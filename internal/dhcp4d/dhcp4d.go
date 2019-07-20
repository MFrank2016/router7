// Copyright 2018 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package dhcp4d implements a DHCPv4 server.
package dhcp4d

import (
	"log"
	"math/rand"
	"net"
	"syscall"
	"time"

	"github.com/rtr7/router7/internal/netconfig"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/krolaw/dhcp4"
	"github.com/mdlayher/raw"
)

type Lease struct {
	Num          int       `json:"num"` // relative to Handler.start
	Addr         net.IP    `json:"addr"`
	HardwareAddr string    `json:"hardware_addr"`
	Hostname     string    `json:"hostname"`
	Expiry       time.Time `json:"expiry"`
}

func (l *Lease) Expired(at time.Time) bool {
	return !l.Expiry.IsZero() && at.After(l.Expiry)
}

type Handler struct {
	serverIP    net.IP
	start       net.IP // first IP address to hand out
	leaseRange  int    // number of IP addresses to hand out
	leasePeriod time.Duration
	options     dhcp4.Options
	leasesHW    map[string]int // points into leasesIP
	leasesIP    map[int]*Lease
	rawConn     net.PacketConn
	iface       *net.Interface

	timeNow func() time.Time

	// Leases is called whenever a new lease is handed out
	Leases func([]*Lease, *Lease)
}

func NewHandler(dir string, iface *net.Interface, ifaceName string, conn net.PacketConn) (*Handler, error) {
	serverIP, err := netconfig.LinkAddress(dir, ifaceName)
	if err != nil {
		return nil, err
	}
	if iface == nil {
		iface, err = net.InterfaceByName(ifaceName)
		if err != nil {
			return nil, err
		}
	}
	if conn == nil {
		conn, err = raw.ListenPacket(iface, syscall.ETH_P_ALL, nil)
		if err != nil {
			return nil, err
		}
	}
	serverIP = serverIP.To4()
	start := make(net.IP, len(serverIP))
	copy(start, serverIP)
	start[len(start)-1] += 1
	return &Handler{
		rawConn:     conn,
		iface:       iface,
		leasesHW:    make(map[string]int),
		leasesIP:    make(map[int]*Lease),
		serverIP:    serverIP,
		start:       start,
		leaseRange:  230,
		leasePeriod: 2 * time.Hour,
		options: dhcp4.Options{
			dhcp4.OptionSubnetMask:       []byte{255, 255, 255, 0},
			dhcp4.OptionRouter:           []byte(serverIP),
			dhcp4.OptionDomainNameServer: []byte(serverIP),
			dhcp4.OptionDomainName:       []byte("lan"),
			dhcp4.OptionDomainSearch:     []byte{0x03, 'l', 'a', 'n', 0x00},
		},
		timeNow: time.Now,
	}, nil
}

// SetLeases overwrites the leases database with the specified leases, typically
// loaded from persistent storage. There is no locking, so SetLeases must be
// called before Serve.
func (h *Handler) SetLeases(leases []*Lease) {
	h.leasesHW = make(map[string]int)
	h.leasesIP = make(map[int]*Lease)
	for _, l := range leases {
		h.leasesHW[l.HardwareAddr] = l.Num
		h.leasesIP[l.Num] = l
	}
}

func (h *Handler) findLease() int {
	now := h.timeNow()
	if len(h.leasesIP) < h.leaseRange {
		// TODO: hash the hwaddr like dnsmasq
		i := rand.Intn(h.leaseRange)
		if l, ok := h.leasesIP[i]; !ok || l.Expired(now) {
			return i
		}
		for i := 0; i < h.leaseRange; i++ {
			if l, ok := h.leasesIP[i]; !ok || l.Expired(now) {
				return i
			}
		}
	}
	return -1
}

func (h *Handler) canLease(reqIP net.IP, hwaddr string) int {
	if len(reqIP) != 4 || reqIP.Equal(net.IPv4zero) {
		return -1
	}

	leaseNum := dhcp4.IPRange(h.start, reqIP) - 1
	if leaseNum < 0 || leaseNum >= h.leaseRange {
		return -1
	}

	l, ok := h.leasesIP[leaseNum]
	if !ok {
		return leaseNum // lease available
	}

	if l.HardwareAddr == hwaddr {
		return leaseNum // lease already owned by requestor
	}

	if l.Expired(h.timeNow()) {
		return leaseNum // lease expired
	}

	return -1 // lease unavailable
}

func (h *Handler) ServeDHCP(p dhcp4.Packet, msgType dhcp4.MessageType, options dhcp4.Options) dhcp4.Packet {
	reply := h.serveDHCP(p, msgType, options)
	if reply == nil {
		return nil // unsupported request
	}
	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{
		ComputeChecksums: true,
		FixLengths:       true,
	}
	destMAC := p.CHAddr()
	destIP := reply.YIAddr()
	if p.Broadcast() {
		destMAC = []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
		destIP = net.IPv4bcast
	}
	ethernet := &layers.Ethernet{
		DstMAC:       destMAC,
		SrcMAC:       h.iface.HardwareAddr,
		EthernetType: layers.EthernetTypeIPv4,
	}

	ip := &layers.IPv4{
		Version:  4,
		TTL:      255,
		SrcIP:    h.serverIP,
		DstIP:    destIP,
		Protocol: layers.IPProtocolUDP,
		Flags:    layers.IPv4DontFragment,
	}
	udp := &layers.UDP{
		SrcPort: 67,
		DstPort: 68,
	}
	udp.SetNetworkLayerForChecksum(ip)
	gopacket.SerializeLayers(buf, opts,
		ethernet,
		ip,
		udp,
		gopacket.Payload(reply))

	if _, err := h.rawConn.WriteTo(buf.Bytes(), &raw.Addr{destMAC}); err != nil {
		log.Printf("WriteTo: %v", err)
	}

	return nil
}

func (h *Handler) leaseHW(hwAddr string) (*Lease, bool) {
	num, ok := h.leasesHW[hwAddr]
	if !ok {
		return nil, false
	}
	l, ok := h.leasesIP[num]
	return l, ok && l.HardwareAddr == hwAddr
}

// TODO: is ServeDHCP always run from the same goroutine, or do we need locking?
func (h *Handler) serveDHCP(p dhcp4.Packet, msgType dhcp4.MessageType, options dhcp4.Options) dhcp4.Packet {
	reqIP := net.IP(options[dhcp4.OptionRequestedIPAddress])
	if reqIP == nil {
		reqIP = net.IP(p.CIAddr())
	}

	switch msgType {
	case dhcp4.Discover:
		free := -1
		hwAddr := p.CHAddr().String()

		// try to offer the requested IP, if any and available
		if !reqIP.To4().Equal(net.IPv4zero) {
			free = h.canLease(reqIP, hwAddr)
			//log.Printf("canLease(%v, %s) = %d", reqIP, hwAddr, free)
		}

		// offer previous lease for this HardwareAddr, if any
		if lease, ok := h.leaseHW(hwAddr); ok && !lease.Expired(h.timeNow()) {
			free = lease.Num
			//log.Printf("h.leasesHW[%s] = %d", hwAddr, free)
		}

		if free == -1 {
			free = h.findLease()
			//log.Printf("findLease = %d", free)
		}

		if free == -1 {
			log.Printf("Cannot reply with DHCPOFFER: no more leases available")
			return nil // no free leases
		}

		return dhcp4.ReplyPacket(p,
			dhcp4.Offer,
			h.serverIP,
			dhcp4.IPAdd(h.start, free),
			h.leasePeriod,
			h.options.SelectOrderOrAll(options[dhcp4.OptionParameterRequestList]))

	case dhcp4.Request:
		if server, ok := options[dhcp4.OptionServerIdentifier]; ok && !net.IP(server).Equal(h.serverIP) {
			return nil // message not for this dhcp server
		}
		leaseNum := h.canLease(reqIP, p.CHAddr().String())
		if leaseNum == -1 {
			return dhcp4.ReplyPacket(p, dhcp4.NAK, h.serverIP, nil, 0, nil)
		}

		lease := &Lease{
			Num:          leaseNum,
			Addr:         make([]byte, 4),
			HardwareAddr: p.CHAddr().String(),
			Expiry:       h.timeNow().Add(h.leasePeriod),
			Hostname:     string(options[dhcp4.OptionHostName]),
		}
		copy(lease.Addr, reqIP.To4())

		if l, ok := h.leaseHW(lease.HardwareAddr); ok {
			if l.Expiry.IsZero() {
				// Retain permanent lease properties
				lease.Expiry = time.Time{}
				lease.Hostname = l.Hostname
			}

			// Release any old leases for this client
			delete(h.leasesIP, l.Num)
		}

		h.leasesIP[leaseNum] = lease
		h.leasesHW[lease.HardwareAddr] = leaseNum
		if h.Leases != nil {
			var leases []*Lease
			for _, l := range h.leasesIP {
				leases = append(leases, l)
			}
			h.Leases(leases, lease)
		}
		return dhcp4.ReplyPacket(p, dhcp4.ACK, h.serverIP, reqIP, h.leasePeriod,
			h.options.SelectOrderOrAll(options[dhcp4.OptionParameterRequestList]))
	}
	return nil
}
