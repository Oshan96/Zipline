package main

import (
	"net"
	"net/netip"
)

// The IPv6 DHT (BEP 32): a second node with its own id on an IPv6 socket, linked to the IPv4
// one so each can answer for the other ("want": n4 / n6) and lookups run on both.

// newDHTConn6 starts an IPv6 node on pc.
func newDHTConn6(pc net.PacketConn, saved []netip.AddrPort) (*DHT, error) {
	d := makeDHT(pc, true, saved, randomNodeID())
	d.start()
	return d, nil
}

// hasIPv6 says whether this computer has an IPv6 address that reaches the internet (else an
// IPv6 node would only talk to itself).
func hasIPv6() bool {
	ifs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, ia := range ifs {
		if ipn, ok := ia.(*net.IPNet); ok {
			if a, ok := netip.AddrFromSlice(ipn.IP); ok && a.Is6() && !a.Is4In6() && a.IsGlobalUnicast() && !a.IsPrivate() {
				return true
			}
		}
	}
	return false
}

// listenDHT6 opens the IPv6 node's socket, on the IPv4 node's port number when it's free (a
// peer that learns our DHT port over IPv6 tries that port).
func listenDHT6(port int) (net.PacketConn, error) {
	if pc, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6unspecified, Port: port}); err == nil {
		return pc, nil
	}
	return net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6unspecified})
}
