package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"maps"
	"net"
	"net/netip"
	"sort"
	"sync"
	"time"
)

// The BitTorrent DHT (BEP 5): a distributed table, run by millions of clients, that says
// which peers have a torrent. It's what makes magnet links without trackers work. Zipline
// asks it for peers and announces its torrents there; while it can't be reached from outside
// it says so in its queries (read-only, BEP 43), and once it can it serves as a full node
// (dhtnode.go). IPv6 runs a second node on its own socket (BEP 32, dht6.go). A node runs on
// any net.PacketConn, its own socket or one shared with other UDP traffic.

var dhtBootstrap = []string{"router.bittorrent.com:6881", "dht.transmissionbt.com:6881", "router.utorrent.com:6881", "dht.libtorrent.org:25401", "dht.aelitis.com:6881"}

type dhtNode struct {
	id    [20]byte
	addr  netip.AddrPort
	seen  time.Time // last answer (or query) from it
	fails int       // queries in a row it didn't answer
}

type dhtWait struct {
	ch chan map[string]any
	to netip.AddrPort
}

type DHT struct {
	pc    net.PacketConn
	v6    bool
	id    [20]byte
	boot  []string         // routers to start from (a copy of dhtBootstrap)
	saved []netip.AddrPort // nodes from last time
	other *DHT             // the other address family's node, if any (BEP 32)
	every time.Duration    // how often the table is looked after
	done  chan struct{}

	mu       sync.Mutex
	pending  map[string]dhtWait
	tid      uint16
	dhtState // the routing table, tokens, stored peers, reachability (dhtnode.go)
}

// newDHT starts an IPv4 node on a socket of its own.
func newDHT(saved []netip.AddrPort) (*DHT, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return nil, err
	}
	return newDHTConn(conn, saved)
}

// newDHTConn starts an IPv4 node on pc (which it closes when it's closed).
func newDHTConn(pc net.PacketConn, saved []netip.AddrPort) (*DHT, error) {
	d := makeDHT(pc, false, saved, randomNodeID())
	d.start()
	return d, nil
}

func randomNodeID() (id [20]byte) {
	rand.Read(id[:])
	return
}

// makeDHT sets a node up without starting it (so two can be linked first).
func makeDHT(pc net.PacketConn, v6 bool, saved []netip.AddrPort, id [20]byte) *DHT {
	d := &DHT{pc: pc, v6: v6, id: id, boot: append([]string(nil), dhtBootstrap...), every: time.Minute,
		pending: map[string]dhtWait{}, done: make(chan struct{})}
	for _, a := range saved {
		if a = unmapAP(a); a.IsValid() && a.Addr().Is4() != v6 {
			d.saved = append(d.saved, a)
		}
	}
	d.dhtState.setup()
	return d
}

func (d *DHT) start() {
	go d.readLoop()
	go d.maintain()
}

func (d *DHT) Close() {
	select {
	case <-d.done:
	default:
		close(d.done)
		d.pc.Close()
	}
}

// Port is the node's UDP port.
func (d *DHT) Port() int {
	if u, ok := d.pc.LocalAddr().(*net.UDPAddr); ok {
		return u.Port
	}
	return int(addrPortOf(d.pc.LocalAddr()).Port())
}

func unmapAP(a netip.AddrPort) netip.AddrPort { return netip.AddrPortFrom(a.Addr().Unmap(), a.Port()) }

func addrPortOf(a net.Addr) netip.AddrPort {
	if u, ok := a.(*net.UDPAddr); ok {
		return unmapAP(u.AddrPort())
	}
	ap, _ := netip.ParseAddrPort(a.String())
	return unmapAP(ap)
}

// ours: an address of this node's family (a shared socket may carry both).
func (d *DHT) ours(a netip.AddrPort) bool {
	return a.IsValid() && a.Port() != 0 && a.Addr().Is4() != d.v6 && !a.Addr().IsUnspecified()
}

func (d *DHT) readLoop() {
	buf := make([]byte, 64<<10)
	for {
		n, src, err := d.pc.ReadFrom(buf)
		if err != nil {
			select {
			case <-d.done:
				return
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		from := addrPortOf(src)
		if !d.ours(from) {
			continue
		}
		v, err := bdecode(buf[:n])
		if err != nil {
			continue
		}
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		switch bstr(m, "y") {
		case "q":
			d.serve(from, m)
		case "r", "e":
			t := bstr(m, "t")
			d.mu.Lock()
			w, ok := d.pending[t]
			if ok && w.to == from { // (an answer from anyone else is a forgery)
				delete(d.pending, t)
			}
			d.mu.Unlock()
			if ok && w.to == from {
				w.ch <- m
			}
		}
	}
}

func (d *DHT) send(pkt []byte, to netip.AddrPort) error {
	_, err := d.pc.WriteTo(pkt, net.UDPAddrFromAddrPort(to))
	return err
}

var errDHTTimeout = errors.New("no answer")

// query sends one KRPC query and waits for the reply's "r" dictionary. A node that answers
// goes into the routing table; one that doesn't counts a failure.
func (d *DHT) query(ctx context.Context, to netip.AddrPort, q string, args map[string]any) (map[string]any, error) {
	d.mu.Lock()
	d.tid++
	t := string([]byte{byte(d.tid >> 8), byte(d.tid)})
	ch := make(chan map[string]any, 1)
	d.pending[t] = dhtWait{ch, to}
	d.sentTo[to] = time.Now()
	ro := !d.reachableLocked()
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.pending, t)
		d.mu.Unlock()
	}()
	args["id"] = string(d.id[:])
	if d.other != nil && (q == "find_node" || q == "get_peers") {
		args["want"] = []any{"n4", "n6"}
	}
	msg := map[string]any{"t": t, "y": "q", "q": q, "a": args, "v": "ZL10"}
	if ro {
		msg["ro"] = 1 // BEP 43: don't put us in your routing table
	}
	if err := d.send(bencode(msg), to); err != nil {
		return nil, err
	}
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case m := <-ch:
		if bstr(m, "y") == "e" {
			return nil, errors.New("error reply")
		}
		r := bdict(m, "r")
		id := bstr(r, "id")
		if r == nil || len(id) != 20 {
			return nil, errors.New("bad reply")
		}
		d.mu.Lock()
		d.insert([20]byte([]byte(id)), to)
		d.mu.Unlock()
		return r, nil
	case <-timer.C:
		d.mu.Lock()
		d.failed(to)
		d.mu.Unlock()
		return nil, errDHTTimeout
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-d.done:
		return nil, errors.New("closed")
	}
}

func (d *DHT) ping(ctx context.Context, a netip.AddrPort) error {
	_, err := d.query(ctx, a, "ping", map[string]any{})
	return err
}

func xorDist(a, b [20]byte) (out [20]byte) {
	for i := range a {
		out[i] = a[i] ^ b[i]
	}
	return
}

// parseNodes reads compact node info: 26 bytes each over IPv4, 38 over IPv6.
func parseNodes(b []byte, v6 bool) []dhtNode {
	size := 26
	if v6 {
		size = 38
	}
	var out []dhtNode
	for i := 0; i+size <= len(b); i += size {
		var n dhtNode
		copy(n.id[:], b[i:i+20])
		var ip netip.Addr
		if v6 {
			ip = netip.AddrFrom16([16]byte(b[i+20 : i+36]))
		} else {
			ip = netip.AddrFrom4([4]byte(b[i+20 : i+24]))
		}
		port := binary.BigEndian.Uint16(b[i+size-2:])
		if port == 0 || ip.IsUnspecified() || ip.Is4In6() {
			continue
		}
		n.addr = netip.AddrPortFrom(ip, port)
		out = append(out, n)
	}
	return out
}

// GoodNodes lists nodes that answered lately (saved for a quicker start next time).
func (d *DHT) GoodNodes() []netip.AddrPort {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []netip.AddrPort
	for _, n := range d.all() {
		if !n.seen.IsZero() && time.Since(n.seen) < 2*time.Hour && n.fails == 0 {
			out = append(out, n.addr)
		}
		if len(out) >= 300 {
			break
		}
	}
	return out
}

type lookupHit struct {
	addr  netip.AddrPort
	id    [20]byte
	token string
}

// lookup walks towards target: it asks the closest nodes not asked yet, eight at a time,
// until nothing new is closer than the eight closest that answered, and returns those (with
// the tokens get_peers gave). onReply sees every answer.
func (d *DHT) lookup(ctx context.Context, target [20]byte, q string, args map[string]any, onReply func(r map[string]any)) []lookupHit {
	type cand struct {
		lookupHit
		known    bool
		asked    bool
		answered bool
	}
	var mu sync.Mutex
	list := map[netip.AddrPort]*cand{}
	add := func(a netip.AddrPort, id [20]byte, known bool) {
		if !d.ours(a) {
			return
		}
		mu.Lock()
		if c := list[a]; c == nil {
			list[a] = &cand{lookupHit: lookupHit{addr: a, id: id}, known: known}
		} else if known && !c.known {
			c.id, c.known = id, true
		}
		mu.Unlock()
	}
	d.mu.Lock()
	for _, n := range d.closest(target, 16) {
		add(n.addr, n.id, true)
	}
	d.mu.Unlock()
	if len(list) < 8 {
		for _, a := range d.saved {
			add(a, [20]byte{}, false)
		}
	}
	if len(list) < 8 {
		family := "ip4"
		if d.v6 {
			family = "ip6"
		}
		for _, h := range d.boot {
			addrs, err := net.DefaultResolver.LookupNetIP(ctx, family, hostOnly(h))
			if err != nil {
				continue
			}
			for _, ip := range addrs {
				add(netip.AddrPortFrom(ip.Unmap(), portOf(h)), [20]byte{}, false)
			}
		}
	}
	closest := func(n int, onlyUnasked bool) []*cand {
		mu.Lock()
		defer mu.Unlock()
		var cs []*cand
		for _, c := range list {
			if onlyUnasked && c.asked || !onlyUnasked && !c.answered {
				continue
			}
			cs = append(cs, c)
		}
		sort.Slice(cs, func(i, j int) bool {
			if cs[i].known != cs[j].known {
				return cs[i].known // nodes we know the id of come first; bootstrap routers after
			}
			di, dj := xorDist(cs[i].id, target), xorDist(cs[j].id, target)
			return bytes.Compare(di[:], dj[:]) < 0
		})
		if len(cs) > n {
			cs = cs[:n]
		}
		return cs
	}
	for round := 0; round < 24 && ctx.Err() == nil; round++ {
		batch := closest(8, true)
		if len(batch) == 0 {
			break
		}
		// stop once nothing new is closer than the eight closest that answered
		if best := closest(8, false); len(best) == 8 {
			far := xorDist(best[7].id, target)
			closer := false
			for _, c := range batch {
				dd := xorDist(c.id, target)
				if !c.known || bytes.Compare(dd[:], far[:]) < 0 {
					closer = true
				}
			}
			if !closer {
				break
			}
		}
		var wg sync.WaitGroup
		for _, c := range batch {
			mu.Lock()
			c.asked = true
			mu.Unlock()
			wg.Add(1)
			go func(c *cand) {
				defer wg.Done()
				r, err := d.query(ctx, c.addr, q, maps.Clone(args))
				if err != nil {
					return
				}
				mu.Lock()
				c.answered, c.id, c.known, c.token = true, [20]byte([]byte(bstr(r, "id"))), true, bstr(r, "token")
				mu.Unlock()
				for _, n := range parseNodes([]byte(bstr(r, d.nodesKey())), d.v6) {
					add(n.addr, n.id, true)
				}
				if d.other != nil { // nodes of the other family go to its node (it checks them first)
					for _, n := range parseNodes([]byte(bstr(r, d.other.nodesKey())), d.other.v6) {
						d.other.heard(n.id, n.addr)
					}
				}
				if onReply != nil {
					onReply(r)
				}
			}(c)
		}
		wg.Wait()
	}
	var hits []lookupHit
	for _, c := range closest(8, false) {
		hits = append(hits, c.lookupHit)
	}
	return hits
}

func (d *DHT) nodesKey() string {
	if d.v6 {
		return "nodes6"
	}
	return "nodes"
}

// GetPeers looks the torrent up (over IPv6 too, when that node runs) and reports peers as
// they're found.
func (d *DHT) GetPeers(ctx context.Context, ih [20]byte, found func([]netip.AddrPort)) {
	d.FindAndAnnounce(ctx, ih, 0, found)
}

// FindAndAnnounce looks the torrent up and then, when port isn't 0, tells the closest nodes
// that we have it (announce_peer, with the tokens they gave). implied_port asks them to use
// the port our packets come from, right when the DHT shares the peers' port (a NAT may change
// it on the way out).
func (d *DHT) FindAndAnnounce(ctx context.Context, ih [20]byte, port int, found func([]netip.AddrPort)) {
	var wg sync.WaitGroup
	for _, n := range []*DHT{d, d.other} {
		if n == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			n.findAndAnnounce(ctx, ih, port, found)
		}()
	}
	wg.Wait()
}

func (d *DHT) findAndAnnounce(ctx context.Context, ih [20]byte, port int, found func([]netip.AddrPort)) {
	var mu sync.Mutex
	seen := map[netip.AddrPort]bool{}
	size := 6
	if d.v6 {
		size = 18
	}
	hits := d.lookup(ctx, ih, "get_peers", map[string]any{"info_hash": string(ih[:])}, func(r map[string]any) {
		var fresh []netip.AddrPort
		mu.Lock()
		for _, v := range blist(r, "values") {
			if s, ok := v.(string); ok && len(s) == size {
				for _, p := range compactPeers([]byte(s), size) {
					if !seen[p] {
						seen[p] = true
						fresh = append(fresh, p)
					}
				}
			}
		}
		mu.Unlock()
		if len(fresh) > 0 && found != nil {
			found(fresh)
		}
	})
	if port <= 0 || ctx.Err() != nil {
		return
	}
	implied := 0
	if d.Port() == port {
		implied = 1
	}
	var wg sync.WaitGroup
	for _, h := range hits {
		if h.token == "" {
			continue
		}
		wg.Add(1)
		go func(h lookupHit) {
			defer wg.Done()
			d.query(ctx, h.addr, "announce_peer", map[string]any{"info_hash": string(ih[:]), "port": port, "token": h.token, "implied_port": implied})
		}(h)
	}
	wg.Wait()
}

func hostOnly(hp string) string {
	h, _, err := net.SplitHostPort(hp)
	if err != nil {
		return hp
	}
	return h
}

func portOf(hp string) uint16 {
	_, p, err := net.SplitHostPort(hp)
	if err != nil {
		return 0
	}
	var n int
	for _, c := range p {
		n = n*10 + int(c-'0')
	}
	return uint16(n)
}
