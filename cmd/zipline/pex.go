package main

import (
	"net/netip"
	"sort"
	"time"

	"zipline/internal/bencode"
	"zipline/internal/utp"
)

// Sending peer exchange (BEP 11; receiving is in handleExt): once a minute each peer that speaks
// ut_pex hears which peers we've connected to since the last message and which we've lost, at
// most 50 of each, as libtorrent does. Never for private torrents (BEP 27).

const (
	pexEvery    = time.Minute
	pexMaxAdded = 50
	pexPrefEnc  = 0x01 // added.f flags
	pexSeed     = 0x02
	pexUTP      = 0x04
	pexReach    = 0x10
)

// extHandshakeMore adds our encryption preference to the extension handshake. Caller holds t.mu.
func (t *torrent) extHandshakeMore(eh map[string]any) {
	if t.m.encMode() != encPlain {
		eh["e"] = 1
	}
}

// gotExtHandshake keeps what peer exchange needs from a peer's extension handshake. Caller holds t.mu.
func (t *torrent) gotExtHandshake(p *tpeer, d map[string]any) {
	x := p.xs()
	if n, ok := bencode.Int(d, "p"); ok && n > 0 && n < 65536 {
		x.lport = int(n)
	}
	if n, ok := bencode.Int(d, "e"); ok {
		x.encPref = n == 1
	}
	x.pexNext = time.Now().Add(2 * time.Second) // the first list soon, once we've met a few peers
}

// peerUsesUTP: the peer's connection runs over uTP (internal/utp).
func peerUsesUTP(p *tpeer) bool { return utp.IsConn(p.conn) }

// pexAddr is the address other peers can reach a peer at: where we dialled it, or the
// listening port it told us. Caller holds t.mu.
func (p *tpeer) pexAddr() (netip.AddrPort, bool) {
	if p.outgoing {
		return p.addr, true
	}
	if lp := p.xs().lport; lp > 0 {
		return netip.AddrPortFrom(p.addr.Addr(), uint16(lp)), true
	}
	return netip.AddrPort{}, false
}

func (p *tpeer) pexFlags() byte {
	var f byte
	if _, obf := connEncryption(p.conn); obf || p.xs().encPref {
		f |= pexPrefEnc
	}
	if p.seed {
		f |= pexSeed
	}
	if peerUsesUTP(p) {
		f |= pexUTP
	}
	if p.outgoing {
		f |= pexReach // we got through to it
	}
	return f
}

// sendPex sends the peers that are due their peer exchange message. Caller holds t.mu.
func (t *torrent) sendPex(now time.Time) {
	if t.mi == nil || t.mi.Private { // (a magnet link's torrent may turn out private)
		return
	}
	type entry struct {
		p    *tpeer
		addr netip.AddrPort
	}
	var cur []entry
	for _, p := range t.peers {
		if a, ok := p.pexAddr(); ok && !p.closed {
			cur = append(cur, entry{p, a})
		}
	}
	sort.Slice(cur, func(i, j int) bool { return cur[i].p.unchokedAt.After(cur[j].p.unchokedAt) }) // the ones we trade with first
	for _, to := range t.peers {
		id, ok := to.ext["ut_pex"]
		x := to.xs()
		if !ok || to.closed || x.pexNext.IsZero() || now.Before(x.pexNext) {
			continue
		}
		toAddr, _ := to.pexAddr()
		live := map[netip.AddrPort]bool{}
		var added, added6, flags, flags6 []byte
		n := 0
		for _, e := range cur {
			if e.p == to || e.addr == toAddr {
				continue
			}
			live[e.addr] = true
			if x.pexSent[e.addr] || n >= pexMaxAdded {
				continue
			}
			n++
			x.pexSent[e.addr] = true
			if e.addr.Addr().Is4() {
				added, flags = append(added, compactPeer(e.addr)...), append(flags, e.p.pexFlags())
			} else {
				added6, flags6 = append(added6, compactPeer(e.addr)...), append(flags6, e.p.pexFlags())
			}
		}
		var dropped, dropped6 []byte
		gone := 0
		for a := range x.pexSent {
			if !live[a] && gone < pexMaxAdded {
				gone++
				delete(x.pexSent, a)
				if a.Addr().Is4() {
					dropped = append(dropped, compactPeer(a)...)
				} else {
					dropped6 = append(dropped6, compactPeer(a)...)
				}
			}
		}
		x.pexNext = now.Add(pexEvery)
		if n == 0 && gone == 0 {
			continue
		}
		to.sendLocked(extMsg(byte(id), bencode.Encode(map[string]any{"added": added, "added.f": flags, "added6": added6, "added6.f": flags6,
			"dropped": dropped, "dropped6": dropped6})))
	}
}
