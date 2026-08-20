package main

import (
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

var mdnsGroupAddr = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}

// ResolveA resolves an mDNS ("Bonjour") hostname such as "dereks-MacBook-Pro.local"
// to an IPv4 address by sending a one-shot multicast DNS query on the local
// network and waiting for a matching A record reply within timeout.
//
// This talks raw mDNS directly (RFC 6762) instead of shelling out to
// avahi-resolve, so it works on a minimal container without avahi-daemon
// installed. It does require the host to be able to send/receive multicast
// UDP on 224.0.0.251:5353, which means the container's network interface
// must sit on the same L2 broadcast domain as the target machine (a bridged
// LXC NIC on the same VLAN works; a NATed one will not).
func ResolveA(host string, timeout time.Duration) (net.IP, error) {
	fqdn := host
	if !strings.HasSuffix(fqdn, ".") {
		fqdn += "."
	}

	name, err := dnsmessage.NewName(fqdn)
	if err != nil {
		return nil, fmt.Errorf("invalid hostname %q: %w", host, err)
	}

	query := dnsmessage.Message{
		Header: dnsmessage.Header{ID: 0, RecursionDesired: false},
		Questions: []dnsmessage.Question{
			{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET},
		},
	}
	packed, err := query.Pack()
	if err != nil {
		return nil, fmt.Errorf("pack query: %w", err)
	}

	conn, err := net.ListenMulticastUDP("udp4", nil, mdnsGroupAddr)
	if err != nil {
		return nil, fmt.Errorf("listen multicast: %w", err)
	}
	defer conn.Close()

	if _, err := conn.WriteToUDP(packed, mdnsGroupAddr); err != nil {
		return nil, fmt.Errorf("send query: %w", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil, fmt.Errorf("set deadline: %w", err)
	}

	buf := make([]byte, 65535)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return nil, fmt.Errorf("no mdns response for %s within %s: %w", host, timeout, err)
		}
		if ip, ok := parseAAnswer(buf[:n], fqdn); ok {
			return ip, nil
		}
	}
}

// parseAAnswer looks for an A record in an mDNS reply whose name matches
// fqdn (case-insensitively, as DNS names are). Returns ok=false if the
// packet is malformed or doesn't contain a matching answer.
func parseAAnswer(data []byte, fqdn string) (net.IP, bool) {
	var parser dnsmessage.Parser
	if _, err := parser.Start(data); err != nil {
		return nil, false
	}
	if err := parser.SkipAllQuestions(); err != nil {
		return nil, false
	}
	for {
		h, err := parser.AnswerHeader()
		if err != nil {
			return nil, false
		}
		if h.Type != dnsmessage.TypeA || !strings.EqualFold(h.Name.String(), fqdn) {
			if err := parser.SkipAnswer(); err != nil {
				return nil, false
			}
			continue
		}
		res, err := parser.AResource()
		if err != nil {
			return nil, false
		}
		return net.IPv4(res.A[0], res.A[1], res.A[2], res.A[3]), true
	}
}
