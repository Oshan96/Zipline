package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"math/bits"
	mrand "math/rand/v2"
	"net/netip"
	"os"
	"sort"
	"time"
)

// The DHT's full-node side: a Kademlia routing table (k-buckets of 8, refreshed), answers to
// ping, find_node, get_peers and announce_peer, tokens that prove an announcer asked us first
// (a secret that changes every 5 minutes; tokens stay good for 10), and the peers announced to
// us (bounded, forgotten after 30 minutes). Until someone reaches us unasked (or the port is
// known to be mapped: SetReachable), the node is read-only (BEP 43): it marks its queries "ro"
// and doesn't answer.

const (
	dhtK          = 8
	dhtStale      = 15 * time.Minute // a node not heard from in this long is questionable
	dhtSecretLife = 5 * time.Minute
	dhtPeerTTL    = 30 * time.Minute
	dhtMaxHashes  = 2000 // torrents we keep peers for
	dhtMaxPeers   = 100  // …and peers per torrent
	dhtReachFor   = time.Hour
)

type kbucket struct {
	nodes   []*dhtNode // at most dhtK
	repl    []*dhtNode // replacements for when one goes bad (newest last)
	changed time.Time
}

// dhtState is the node's own data, guarded by DHT.mu.
type dhtState struct {
	buckets     [160]kbucket // buckets[i]: ids sharing exactly i leading bits with ours
	byAddr      map[netip.AddrPort]*dhtNode
	secret      [2][16]byte // the current token secret and the one before
	secretAt    time.Time
	store       map[[20]byte]map[netip.AddrPort]time.Time
	sentTo      map[netip.AddrPort]time.Time // whom we've sent to lately (their queries aren't proof of reachability)
	reachUntil  time.Time
	reachForced bool
	pinging     map[netip.AddrPort]bool
	lastBoot    time.Time
}

func (s *dhtState) setup() {
	s.byAddr = map[netip.AddrPort]*dhtNode{}
	s.store = map[[20]byte]map[netip.AddrPort]time.Time{}
	s.sentTo = map[netip.AddrPort]time.Time{}
	s.pinging = map[netip.AddrPort]bool{}
	rand.Read(s.secret[0][:])
	s.secret[1] = s.secret[0]
	s.secretAt = time.Now()
}

// ---------------------------------------------------------------- the routing table

func (d *DHT) bucketFor(id [20]byte) *kbucket {
	for i := range id {
		if x := id[i] ^ d.id[i]; x != 0 {
			return &d.buckets[i*8+bits.LeadingZeros8(x)]
		}
	}
	return &d.buckets[159]
}

// insert puts a node that answered us into its bucket: fresh again if known, added while
// there's room or a bad node to replace, else kept as a replacement while the stalest node is
// checked. Caller holds d.mu.
func (d *DHT) insert(id [20]byte, a netip.AddrPort) {
	if id == d.id || !d.ours(a) {
		return
	}
	now := time.Now()
	if n := d.byAddr[a]; n != nil {
		if n.id == id {
			n.seen, n.fails = now, 0
			d.bucketFor(id).changed = now
			return
		}
		d.remove(n) // the address has a new id now
	}
	b := d.bucketFor(id)
	for _, n := range b.nodes {
		if n.id == id {
			return // the same id from another address: keep the one we know
		}
	}
	n := &dhtNode{id: id, addr: a, seen: now}
	b.changed = now
	if len(b.nodes) < dhtK {
		b.nodes = append(b.nodes, n)
		d.byAddr[a] = n
		return
	}
	for i, x := range b.nodes {
		if x.fails >= 2 {
			delete(d.byAddr, x.addr)
			b.nodes[i] = n
			d.byAddr[a] = n
			return
		}
	}
	for i, x := range b.repl {
		if x.addr == a || x.id == id {
			b.repl = append(b.repl[:i], b.repl[i+1:]...)
			break
		}
	}
	if b.repl = append(b.repl, n); len(b.repl) > dhtK {
		b.repl = b.repl[1:]
	}
	old := b.nodes[0]
	for _, x := range b.nodes {
		if x.seen.Before(old.seen) {
			old = x
		}
	}
	if now.Sub(old.seen) > dhtStale { // the stalest gets a ping: if it's gone, the replacement moves in
		d.pingLater(old.addr)
	}
}

// failed counts a query a node didn't answer; after three it gives way to a replacement.
// Caller holds d.mu.
func (d *DHT) failed(a netip.AddrPort) {
	if n := d.byAddr[a]; n != nil {
		if n.fails++; n.fails >= 3 {
			d.remove(n)
		}
	}
}

func (d *DHT) remove(n *dhtNode) {
	delete(d.byAddr, n.addr)
	b := d.bucketFor(n.id)
	for i, x := range b.nodes {
		if x == n {
			b.nodes = append(b.nodes[:i], b.nodes[i+1:]...)
			if k := len(b.repl); k > 0 {
				r := b.repl[k-1]
				b.repl = b.repl[:k-1]
				if d.byAddr[r.addr] == nil {
					b.nodes = append(b.nodes, r)
					d.byAddr[r.addr] = r
				}
			}
			return
		}
	}
}

// all lists the table's nodes. Caller holds d.mu.
func (d *DHT) all() []*dhtNode {
	var out []*dhtNode
	for i := range d.buckets {
		out = append(out, d.buckets[i].nodes...)
	}
	return out
}

// closest is the table's n nodes nearest target, bad ones left out. Caller holds d.mu.
func (d *DHT) closest(target [20]byte, n int) []*dhtNode {
	var out []*dhtNode
	for _, x := range d.all() {
		if x.fails < 2 {
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		di, dj := xorDist(out[i].id, target), xorDist(out[j].id, target)
		return bytes.Compare(di[:], dj[:]) < 0
	})
	return out[:min(n, len(out))]
}

// heard: a node we haven't checked (in another node's list, or asking us something). It
// gets a ping when its bucket could take it, and goes in if it answers.
func (d *DHT) heard(id [20]byte, a netip.AddrPort) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.ours(a) || id == d.id || d.byAddr[a] != nil {
		return
	}
	b := d.bucketFor(id)
	room := len(b.nodes) < dhtK
	for _, x := range b.nodes {
		room = room || x.fails >= 2
	}
	if room {
		d.pingLater(a)
	}
}

// pingLater pings a node in the background (a few at a time). Caller holds d.mu.
func (d *DHT) pingLater(a netip.AddrPort) {
	if d.pinging[a] || len(d.pinging) >= 16 {
		return
	}
	d.pinging[a] = true
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		d.ping(ctx, a)
		cancel()
		d.mu.Lock()
		delete(d.pinging, a)
		d.mu.Unlock()
	}()
}

// contact: a peer told us its DHT port (BEP 5's port message).
func (d *DHT) contact(a netip.AddrPort) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.byAddr[a] == nil {
		d.pingLater(a)
	}
}

// ---------------------------------------------------------------- reachability

// SetReachable says the node's port is reachable from outside (mapped in the router, say):
// it serves as a full node from then on.
func (d *DHT) SetReachable(on bool) {
	d.mu.Lock()
	d.reachForced = on
	d.mu.Unlock()
}

func (d *DHT) reachableLocked() bool { return d.reachForced || time.Now().Before(d.reachUntil) }

// Reachable: the node serves others (not read-only).
func (d *DHT) Reachable() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reachableLocked()
}

// ---------------------------------------------------------------- serving others

func (d *DHT) token(ip netip.Addr, secret [16]byte) string {
	b, _ := ip.MarshalBinary()
	h := sha1.Sum(append(secret[:], b...))
	return string(h[:8])
}

func (d *DHT) rotateSecret() {
	d.secret[1] = d.secret[0]
	rand.Read(d.secret[0][:])
	d.secretAt = time.Now()
}

// serve answers a query. Caller holds no locks.
func (d *DHT) serve(from netip.AddrPort, m map[string]any) {
	tid, q, a := bstr(m, "t"), bstr(m, "q"), bdict(m, "a")
	d.mu.Lock()
	now := time.Now()
	if at, ok := d.sentTo[from]; !ok || now.Sub(at) > dhtStale {
		d.reachUntil = now.Add(dhtReachFor) // it reached us unasked: others can too
	}
	serving := d.reachableLocked()
	d.mu.Unlock()
	if !serving {
		return // read-only nodes don't answer (BEP 43)
	}
	reply := func(r map[string]any) {
		d.send(bencode(map[string]any{"t": tid, "y": "r", "r": r, "ip": string(compactPeer(from)), "v": "ZL10"}), from)
	}
	fail := func(code int, msg string) {
		d.send(bencode(map[string]any{"t": tid, "y": "e", "e": []any{code, msg}}), from)
	}
	id := bstr(a, "id")
	if a == nil || len(id) != 20 || tid == "" {
		fail(203, "Protocol Error")
		return
	}
	if ro, _ := bint(m, "ro"); ro != 1 {
		d.heard([20]byte([]byte(id)), from) // (read-only nodes never go in the table)
	}
	r := map[string]any{"id": string(d.id[:])}
	switch q {
	case "ping":
	case "find_node":
		target := bstr(a, "target")
		if len(target) != 20 {
			fail(203, "Protocol Error")
			return
		}
		d.addNodes(r, [20]byte([]byte(target)), a)
	case "get_peers":
		ih := bstr(a, "info_hash")
		if len(ih) != 20 {
			fail(203, "Protocol Error")
			return
		}
		d.mu.Lock()
		r["token"] = d.token(from.Addr(), d.secret[0])
		if vals := d.peersFor([20]byte([]byte(ih))); len(vals) > 0 {
			r["values"] = vals
		}
		d.mu.Unlock()
		d.addNodes(r, [20]byte([]byte(ih)), a)
	case "announce_peer":
		ih, tok := bstr(a, "info_hash"), bstr(a, "token")
		port, _ := bint(a, "port")
		if implied, _ := bint(a, "implied_port"); implied == 1 {
			port = int64(from.Port())
		}
		if len(ih) != 20 || port <= 0 || port > 65535 {
			fail(203, "Protocol Error")
			return
		}
		d.mu.Lock()
		ok := tok == d.token(from.Addr(), d.secret[0]) || tok == d.token(from.Addr(), d.secret[1])
		if ok {
			d.storePeer([20]byte([]byte(ih)), netip.AddrPortFrom(from.Addr(), uint16(port)))
		}
		d.mu.Unlock()
		if !ok {
			fail(203, "Bad token")
			return
		}
	default:
		fail(204, "Method Unknown")
		return
	}
	reply(r)
}

// addNodes puts the nodes closest to target in an answer: our own family's, or what the
// asker wants (BEP 32: "n4", "n6").
func (d *DHT) addNodes(r map[string]any, target [20]byte, a map[string]any) {
	want := map[string]bool{}
	for _, w := range blist(a, "want") {
		if s, ok := w.(string); ok {
			want[s] = true
		}
	}
	if len(want) == 0 {
		want[map[bool]string{false: "n4", true: "n6"}[d.v6]] = true
	}
	for _, n := range []*DHT{d, d.other} {
		if n == nil || !want[map[bool]string{false: "n4", true: "n6"}[n.v6]] {
			continue
		}
		n.mu.Lock()
		var b []byte
		for _, x := range n.closest(target, dhtK) {
			b = append(append(b, x.id[:]...), compactPeer(x.addr)...)
		}
		n.mu.Unlock()
		r[n.nodesKey()] = string(b)
	}
}

// storePeer keeps a peer announced to us. Caller holds d.mu.
func (d *DHT) storePeer(ih [20]byte, a netip.AddrPort) {
	m := d.store[ih]
	if m == nil {
		if len(d.store) >= dhtMaxHashes {
			return
		}
		m = map[netip.AddrPort]time.Time{}
		d.store[ih] = m
	}
	if _, ok := m[a]; !ok && len(m) >= dhtMaxPeers {
		var oldest netip.AddrPort
		for p, at := range m {
			if !oldest.IsValid() || at.Before(m[oldest]) {
				oldest = p
			}
		}
		delete(m, oldest)
	}
	m[a] = time.Now()
}

// peersFor is up to 50 (IPv6: 30) of a torrent's announced peers, in compact form. Caller holds d.mu.
func (d *DHT) peersFor(ih [20]byte) []any {
	var out []any
	for p, at := range d.store[ih] {
		if time.Since(at) < dhtPeerTTL {
			out = append(out, string(compactPeer(p)))
		}
	}
	mrand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out[:min(len(out), map[bool]int{false: 50, true: 30}[d.v6])]
}

// ---------------------------------------------------------------- upkeep

// maintain joins the network, then looks after the table: tokens rotate, stored peers and
// old send records expire, quiet buckets are refreshed and stale nodes pinged.
func (d *DHT) maintain() {
	d.bootstrap()
	tk := time.NewTicker(d.every)
	defer tk.Stop()
	for {
		select {
		case <-d.done:
			return
		case <-tk.C:
		}
		now := time.Now()
		d.mu.Lock()
		if now.Sub(d.secretAt) >= dhtSecretLife {
			d.rotateSecret()
		}
		for ih, m := range d.store {
			for p, at := range m {
				if now.Sub(at) >= dhtPeerTTL {
					delete(m, p)
				}
			}
			if len(m) == 0 {
				delete(d.store, ih)
			}
		}
		for a, at := range d.sentTo {
			if now.Sub(at) > dhtStale {
				delete(d.sentTo, a)
			}
		}
		few := len(d.byAddr) < dhtK
		var refresh [][20]byte
		var stale []netip.AddrPort
		for i := range d.buckets {
			b := &d.buckets[i]
			if len(b.nodes) > 0 && now.Sub(b.changed) > dhtStale && len(refresh) < 2 {
				refresh = append(refresh, d.randomIDIn(i))
				b.changed = now
			}
			for _, n := range b.nodes {
				if now.Sub(n.seen) > dhtStale && len(stale) < 8 {
					stale = append(stale, n.addr)
				}
			}
		}
		for _, a := range stale {
			d.pingLater(a)
		}
		d.mu.Unlock()
		if few && now.Sub(d.lastBootAt()) > 5*time.Minute {
			d.bootstrap()
		}
		for _, id := range refresh {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			d.lookup(ctx, id, "find_node", map[string]any{"target": string(id[:])}, nil)
			cancel()
		}
	}
}

func (d *DHT) lastBootAt() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastBoot
}

// bootstrap fills the table: a lookup of our own id, from the saved nodes and the routers.
func (d *DHT) bootstrap() {
	d.mu.Lock()
	d.lastBoot = time.Now()
	d.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() {
		select {
		case <-d.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	d.lookup(ctx, d.id, "find_node", map[string]any{"target": string(d.id[:])}, nil)
}

// randomIDIn is an id in bucket i: our first i bits, the next one flipped, the rest random.
func (d *DHT) randomIDIn(i int) [20]byte {
	id := randomNodeID()
	for b := 0; b < i; b++ {
		mask := byte(0x80 >> (b % 8))
		id[b/8] = id[b/8]&^mask | d.id[b/8]&mask
	}
	if i < 160 {
		mask := byte(0x80 >> (i % 8))
		id[i/8] = id[i/8]&^mask | ^d.id[i/8]&mask
	}
	return id
}

// ---------------------------------------------------------------- the engine's side

// dhtPort is the port to tell peers in BEP 5's port message (0: no DHT running).
func (m *Manager) dhtPort() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dht == nil {
		return 0
	}
	return m.dht.Port()
}

// dhtContact pings a node a peer told us about (its port message).
func (m *Manager) dhtContact(a netip.AddrPort) {
	m.mu.Lock()
	d := m.dht
	m.mu.Unlock()
	a = unmapAP(a)
	if d != nil && !d.ours(a) {
		d = d.other
	}
	if d != nil && d.ours(a) {
		d.contact(a)
	}
}

// ---------------------------------------------------------------- kept between runs

// dhtFile is dht.json: the node ids and the nodes that answered lately, for a quicker start
// (older versions saved just a list of nodes).
type dhtFile struct {
	ID     string   `json:"id"`
	Nodes  []string `json:"nodes"`
	ID6    string   `json:"id6,omitempty"`
	Nodes6 []string `json:"nodes6,omitempty"`
}

type dhtSaved struct {
	id, id6       [20]byte
	nodes, nodes6 []netip.AddrPort
}

func readDHTFile(path string) dhtSaved {
	sv := dhtSaved{id: randomNodeID(), id6: randomNodeID()}
	b, err := os.ReadFile(path)
	if err != nil {
		return sv
	}
	var f dhtFile
	if json.Unmarshal(b, &f) != nil {
		json.Unmarshal(b, &f.Nodes) // the old form
	}
	parse := func(list []string) (out []netip.AddrPort) {
		for _, s := range list {
			if a, err := netip.ParseAddrPort(s); err == nil {
				out = append(out, unmapAP(a))
			}
		}
		return
	}
	sv.nodes, sv.nodes6 = parse(f.Nodes), parse(f.Nodes6)
	if id, err := hex.DecodeString(f.ID); err == nil && len(id) == 20 {
		sv.id = [20]byte(id)
	}
	if id, err := hex.DecodeString(f.ID6); err == nil && len(id) == 20 {
		sv.id6 = [20]byte(id)
	}
	return sv
}

// savedState is dht.json's contents (nil when there's nothing worth keeping yet).
func (d *DHT) savedState() []byte {
	f := dhtFile{ID: hex.EncodeToString(d.id[:])}
	for _, a := range d.GoodNodes() {
		f.Nodes = append(f.Nodes, a.String())
	}
	if d.other != nil {
		f.ID6 = hex.EncodeToString(d.other.id[:])
		for _, a := range d.other.GoodNodes() {
			f.Nodes6 = append(f.Nodes6, a.String())
		}
	}
	if len(f.Nodes)+len(f.Nodes6) == 0 {
		return nil
	}
	b, _ := json.Marshal(f)
	return b
}
