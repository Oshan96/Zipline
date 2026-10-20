package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// NAT-PMP (RFC 6886) and PCP (RFC 6887): small UDP requests to the gateway's port 5351. The first
// time both are sent at once: a PCP MAP request and a NAT-PMP address request. A router that
// speaks PCP answers the first (and is used that way, as RFC 6887 asks); one that only speaks
// NAT-PMP answers the second, and the first with "unsupported version".

type pmpClient struct {
	gw       netip.AddrPort
	known    bool // which of the two it speaks is known
	pcp      bool
	nonce    [12]byte // PCP: names our mappings
	local    netip.Addr
	external netip.Addr
	ext      [2]int // the router's ports for TCP and UDP
}

// pmpWaits are the retransmission times (RFC 6886 starts at 250 ms and doubles).
var pmpWaits = []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second}

var pcpResults = map[int]string{1: "unsupported version", 2: "not allowed", 3: "malformed request", 4: "unsupported request", 7: "network failure",
	8: "out of resources", 9: "unsupported protocol", 10: "too many mappings", 11: "can't give that port", 12: "address mismatch", 13: "too many peers"}

var natpmpResults = map[int]string{1: "unsupported version", 2: "not allowed", 3: "network failure", 4: "out of resources", 5: "unsupported request"}

// pmpExchange sends the requests until a reply settles it: accept returns true when it's done,
// false to keep listening. It reports whether anything at all came back.
func pmpExchange(ctx context.Context, conn *net.UDPConn, reqs [][]byte, accept func([]byte) bool) (answered bool, err error) {
	buf := make([]byte, 1100)
	for _, wait := range pmpWaits {
		for _, r := range reqs {
			if _, err := conn.Write(r); err != nil {
				return answered, err
			}
		}
		end := time.Now().Add(wait)
		if d, ok := ctx.Deadline(); ok && d.Before(end) {
			end = d
		}
		conn.SetReadDeadline(end)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				break // (a timeout, or "port unreachable": no NAT-PMP here)
			}
			answered = true
			if accept(buf[:n]) {
				return true, nil
			}
		}
		if ctx.Err() != nil {
			return answered, ctx.Err()
		}
	}
	return answered, nil
}

func pcpMapReq(local netip.Addr, nonce [12]byte, proto byte, port, ext int, life uint32) []byte {
	b := make([]byte, 60)
	b[0], b[1] = 2, 1 // version 2, MAP
	binary.BigEndian.PutUint32(b[4:], life)
	a := local.As16() // (IPv4 as ::ffff:a.b.c.d)
	copy(b[8:24], a[:])
	copy(b[24:36], nonce[:])
	b[36] = proto
	binary.BigEndian.PutUint16(b[40:], uint16(port))
	binary.BigEndian.PutUint16(b[42:], uint16(ext))
	b[54], b[55] = 0xff, 0xff // any IPv4 address outside: ::ffff:0.0.0.0
	return b
}

func natpmpMapReq(op byte, port, ext int, life uint32) []byte {
	b := make([]byte, 12)
	b[1] = op // 1 UDP, 2 TCP
	binary.BigEndian.PutUint16(b[4:], uint16(port))
	binary.BigEndian.PutUint16(b[6:], uint16(ext))
	binary.BigEndian.PutUint32(b[8:], life)
	return b
}

func (c *pmpClient) dial() (*net.UDPConn, error) {
	conn, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(c.gw))
	if err != nil {
		return nil, err
	}
	c.local = conn.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap()
	return conn, nil
}

// add maps (or renews) the port for TCP and, when udp, UDP.
func (c *pmpClient) add(ctx context.Context, port int, udp bool) (pmResult, error) {
	conn, err := c.dial()
	if err != nil {
		return pmResult{}, err
	}
	defer conn.Close()
	if c.nonce == [12]byte{} {
		rand.Read(c.nonce[:])
	}
	protos := []byte{6} // TCP
	if udp {
		protos = append(protos, 17)
	}
	var lease time.Duration
	for i, proto := range protos {
		var life time.Duration
		var err error
		if !c.known {
			life, err = c.detect(ctx, conn, port)
		} else {
			life, err = c.mapOne(ctx, conn, proto, port, uint32(pmLease/time.Second))
		}
		if err != nil {
			if i > 0 { // TCP is mapped: UDP is a bonus (uTP and the DHT also work going out)
				break
			}
			return pmResult{}, err
		}
		lease = shorterLease(lease, life)
	}
	via := "NAT-PMP"
	if c.pcp {
		via = "PCP"
	}
	return pmResult{via: via, ext: c.ext[0], external: c.external, lease: lease}, nil
}

// detect asks both ways at once and maps TCP in the same go when the router speaks PCP.
func (c *pmpClient) detect(ctx context.Context, conn *net.UDPConn, port int) (time.Duration, error) {
	life := uint32(pmLease / time.Second)
	pcpReq := pcpMapReq(c.local, c.nonce, 6, port, port, life)
	var pcpReply []byte
	natpmp, noPCP := false, false
	_, err := pmpExchange(ctx, conn, [][]byte{pcpReq, {0, 0}}, func(b []byte) bool {
		switch {
		case len(b) >= 60 && b[0] == 2 && b[1] == 0x81 && string(b[24:36]) == string(c.nonce[:]):
			pcpReply = append([]byte(nil), b...)
			return true
		case len(b) >= 8 && b[0] == 0 && binary.BigEndian.Uint16(b[2:]) == 1: // "unsupported version": to the PCP request
			noPCP = true
		case len(b) >= 12 && b[0] == 0 && b[1] == 128 && binary.BigEndian.Uint16(b[2:]) == 0:
			natpmp = true
			c.external = netip.AddrFrom4([4]byte(b[8:12]))
		}
		return natpmp && noPCP
	})
	switch {
	case pcpReply != nil:
		c.known, c.pcp = true, true
		return c.pcpResult(pcpReply, 0)
	case natpmp:
		c.known, c.pcp = true, false
		return c.mapOne(ctx, conn, 6, port, life)
	case err != nil:
		return 0, err
	}
	return 0, errPMNoAnswer
}

// mapOne maps one protocol (6 TCP, 17 UDP) by the way the router speaks; life 0 removes it.
func (c *pmpClient) mapOne(ctx context.Context, conn *net.UDPConn, proto byte, port int, life uint32) (time.Duration, error) {
	slot := 0
	if proto == 17 {
		slot = 1
	}
	ext := port
	if c.ext[slot] != 0 {
		ext = c.ext[slot]
	}
	var req []byte
	if c.pcp {
		req = pcpMapReq(c.local, c.nonce, proto, port, ext, life)
	} else {
		op := byte(2)
		if proto == 17 {
			op = 1
		}
		if life == 0 {
			ext = 0 // RFC 6886: a deletion suggests no port
		}
		req = natpmpMapReq(op, port, ext, life)
	}
	var reply []byte
	answered, err := pmpExchange(ctx, conn, [][]byte{req}, func(b []byte) bool {
		if c.pcp && len(b) >= 60 && b[0] == 2 && b[1] == 0x81 && string(b[24:36]) == string(c.nonce[:]) && b[36] == proto ||
			!c.pcp && len(b) >= 16 && b[0] == 0 && b[1] == 128+req[1] && int(binary.BigEndian.Uint16(b[8:])) == port {
			reply = append([]byte(nil), b...)
			return true
		}
		return false
	})
	switch {
	case reply != nil && c.pcp:
		return c.pcpResult(reply, slot)
	case reply != nil:
		if code := int(binary.BigEndian.Uint16(reply[2:])); code != 0 {
			return 0, &pmRefused{msg: fmt.Sprintf("NAT-PMP: %s", resultText(natpmpResults, code))}
		}
		c.ext[slot] = int(binary.BigEndian.Uint16(reply[10:]))
		return time.Duration(binary.BigEndian.Uint32(reply[12:])) * time.Second, nil
	case err != nil:
		return 0, err
	case answered:
		return 0, errors.New("the router's answer didn't match")
	}
	return 0, errPMNoAnswer
}

// pcpResult reads a PCP MAP answer.
func (c *pmpClient) pcpResult(b []byte, slot int) (time.Duration, error) {
	if code := int(b[3]); code != 0 {
		return 0, &pmRefused{msg: "PCP: " + resultText(pcpResults, code), conflict: code == 11}
	}
	c.ext[slot] = int(binary.BigEndian.Uint16(b[42:]))
	if ip := netip.AddrFrom16([16]byte(b[44:60])).Unmap(); ip.IsValid() && !ip.IsUnspecified() {
		c.external = ip
	}
	return time.Duration(binary.BigEndian.Uint32(b[4:])) * time.Second, nil
}

func resultText(m map[int]string, code int) string {
	if s, ok := m[code]; ok {
		return s
	}
	return fmt.Sprintf("error %d", code)
}

func (c *pmpClient) del(ctx context.Context, port int, udp bool) {
	if !c.known {
		return
	}
	conn, err := c.dial()
	if err != nil {
		return
	}
	defer conn.Close()
	c.mapOne(ctx, conn, 6, port, 0)
	if udp && c.ext[1] != 0 {
		c.mapOne(ctx, conn, 17, port, 0)
	}
}
