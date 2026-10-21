package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"zipline/internal/bencode"
)

// sniffPC records the KRPC messages a node sends.
type sniffPC struct {
	net.PacketConn
	mu   sync.Mutex
	sent []map[string]any
}

func (s *sniffPC) WriteTo(b []byte, a net.Addr) (int, error) {
	if v, err := bencode.Decode(b); err == nil {
		if m, ok := v.(map[string]any); ok {
			s.mu.Lock()
			s.sent = append(s.sent, m)
			s.mu.Unlock()
		}
	}
	return s.PacketConn.WriteTo(b, a)
}

func (s *sniffPC) queries() (ro, full int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.sent {
		if bencode.Str(m, "y") == "q" {
			if n, _ := bencode.Int(m, "ro"); n == 1 {
				ro++
			} else {
				full++
			}
		}
	}
	return
}

// testNode starts a node on loopback (network "udp4" or "udp6") that knows only boot.
func testNode(t *testing.T, network string, boot []netip.AddrPort, reachable bool) (*DHT, *sniffPC) {
	host := "127.0.0.1:0"
	if network == "udp6" {
		host = "[::1]:0"
	}
	pc, err := net.ListenPacket(network, host)
	if err != nil {
		t.Skipf("no %s socket: %v", network, err)
	}
	sp := &sniffPC{PacketConn: pc}
	d := makeDHT(sp, network == "udp6", boot, randomNodeID())
	d.boot = nil // (no routers on the internet)
	d.SetReachable(reachable)
	d.start()
	t.Cleanup(d.Close)
	return d, sp
}

func (d *DHT) addr() netip.AddrPort { return addrPortOf(d.pc.LocalAddr()) }

func (d *DHT) tableSize() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.byAddr)
}

func (d *DHT) stored(ih [20]byte) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.store[ih])
}

// The routing table: buckets by shared prefix, eight at most, replacements for nodes that stop answering.
func TestDHTRoutingTable(t *testing.T) {
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	var self [20]byte
	d := makeDHT(pc, false, nil, self) // our id: all zeros
	idWith := func(prefix int, n byte) (id [20]byte) {
		id[prefix/8] = 0x80 >> (prefix % 8) // first set bit at position prefix
		id[19] |= n
		return
	}
	ap := func(n int) netip.AddrPort {
		return netip.AddrPortFrom(netip.AddrFrom4([4]byte{10, 0, byte(n >> 8), byte(n)}), 6881)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if b := d.bucketFor(idWith(0, 0)); b != &d.buckets[0] {
		t.Fatal("an id differing in the first bit goes in bucket 0")
	}
	if b := d.bucketFor(idWith(13, 0)); b != &d.buckets[13] {
		t.Fatal("bucket 13")
	}
	for i := 0; i < 12; i++ {
		d.insert(idWith(3, byte(i+1)), ap(i))
	}
	if b := &d.buckets[3]; len(b.nodes) != dhtK || len(b.repl) != 4 {
		t.Fatalf("bucket 3: %d nodes, %d replacements", len(b.nodes), len(b.repl))
	}
	if d.insert(idWith(3, 1), ap(0)); len(d.buckets[3].nodes) != dhtK {
		t.Fatal("hearing from a known node again changes nothing")
	}
	for k := 0; k < 3; k++ {
		d.failed(ap(2))
	}
	if b := &d.buckets[3]; len(b.nodes) != dhtK || d.byAddr[ap(2)] != nil || d.byAddr[ap(11)] == nil {
		t.Fatal("a node that stops answering gives way to the newest replacement")
	}
	if got := d.closest(idWith(3, 0), 3); len(got) != 3 || got[0].id != idWith(3, 1) {
		t.Fatalf("closest: %v", got)
	}
	var target [20]byte
	rand.Read(target[:])
	for i := 0; i < 160; i++ {
		if d.bucketFor(d.randomIDIn(i)) != &d.buckets[i] {
			t.Fatalf("randomIDIn(%d) isn't in bucket %d", i, i)
		}
	}
}

// Several Zipline nodes on this computer join through one, fill their tables, announce
// a torrent and find it from elsewhere; tokens are checked.
func TestDHTFullNodes(t *testing.T) {
	first, _ := testNode(t, "udp4", nil, true)
	nodes := []*DHT{first}
	for i := 0; i < 9; i++ {
		d, _ := testNode(t, "udp4", []netip.AddrPort{first.addr()}, true)
		nodes = append(nodes, d)
	}
	time.Sleep(300 * time.Millisecond)
	for _, d := range nodes { // a second round, now everyone's there
		d.bootstrap()
	}
	for i, d := range nodes {
		if n := d.tableSize(); n < 5 {
			t.Fatalf("node %d knows only %d others", i, n)
		}
	}
	var ih [20]byte
	rand.Read(ih[:])
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	nodes[3].FindAndAnnounce(ctx, ih, 4444, nil)
	holders := 0
	for _, d := range nodes {
		if d.stored(ih) > 0 {
			holders++
		}
	}
	if holders < 4 {
		t.Fatalf("the announce reached only %d nodes", holders)
	}
	var mu sync.Mutex
	var found []netip.AddrPort
	nodes[7].GetPeers(ctx, ih, func(ps []netip.AddrPort) {
		mu.Lock()
		found = append(found, ps...)
		mu.Unlock()
	})
	if fmt.Sprint(found) != "[127.0.0.1:4444]" {
		t.Fatalf("found %v", found)
	}
	// implied_port: the port the packets come from
	var ih2 [20]byte
	rand.Read(ih2[:])
	nodes[5].FindAndAnnounce(ctx, ih2, nodes[5].Port(), nil)
	found = nil
	nodes[8].GetPeers(ctx, ih2, func(ps []netip.AddrPort) { found = append(found, ps...) })
	if len(found) != 1 || found[0] != nodes[5].addr() {
		t.Fatalf("implied port: found %v, want %v", found, nodes[5].addr())
	}
	// a made-up token is refused; get_peers' token works until two secrets later
	d, to := nodes[2], nodes[4].addr()
	if _, err := d.query(ctx, to, "announce_peer", map[string]any{"info_hash": string(ih[:]), "port": 1, "token": "nope"}); err == nil {
		t.Fatal("a bad token should be refused")
	}
	r, err := d.query(ctx, to, "get_peers", map[string]any{"info_hash": string(ih[:])})
	if err != nil || bencode.Str(r, "token") == "" {
		t.Fatal(err)
	}
	tok := bencode.Str(r, "token")
	nodes[4].mu.Lock()
	nodes[4].rotateSecret()
	nodes[4].mu.Unlock()
	if _, err := d.query(ctx, to, "announce_peer", map[string]any{"info_hash": string(ih[:]), "port": 2, "token": tok}); err != nil {
		t.Fatalf("a token is good for one more secret: %v", err)
	}
	nodes[4].mu.Lock()
	nodes[4].rotateSecret()
	nodes[4].mu.Unlock()
	if _, err := d.query(ctx, to, "announce_peer", map[string]any{"info_hash": string(ih[:]), "port": 3, "token": tok}); err == nil {
		t.Fatal("a token two secrets old should be refused")
	}
	if _, err := d.query(ctx, to, "vote", map[string]any{}); err == nil {
		t.Fatal("an unknown method is an error")
	}
}

// Read-only until reachable (BEP 43): queries say "ro", others don't put the node in their
// tables, and it doesn't answer, until a node it never wrote to reaches it.
func TestDHTReadOnlyUntilReached(t *testing.T) {
	a, _ := testNode(t, "udp4", nil, true)
	b, bs := testNode(t, "udp4", []netip.AddrPort{a.addr()}, false)
	time.Sleep(200 * time.Millisecond)
	if ro, full := bs.queries(); ro == 0 || full != 0 || b.Reachable() {
		t.Fatalf("an unreachable node's queries are read-only: ro %d, full %d", ro, full)
	}
	a.mu.Lock()
	inA := a.byAddr[b.addr()] != nil
	a.mu.Unlock()
	if inA {
		t.Fatal("a read-only node mustn't go into routing tables")
	}
	// a node B never wrote to pings it (as a peer does after a port message): it answers now
	c, cs := testNode(t, "udp4", nil, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.ping(ctx, b.addr()); err != nil || !b.Reachable() {
		t.Fatalf("reached from outside, B should answer: %v", err)
	}
	if ro, _ := cs.queries(); ro != 0 {
		t.Fatal("C is reachable: no ro")
	}
	b.bootstrap()
	if _, full := bs.queries(); full == 0 {
		t.Fatal("reachable now: full queries")
	}
	time.Sleep(100 * time.Millisecond)
	a.mu.Lock()
	inA = a.byAddr[b.addr()] != nil
	a.mu.Unlock()
	if !inA {
		t.Fatal("a full node that asks goes into the table (after a ping)")
	}
	// the port message path: a peer's DHT port gets a ping
	d, _ := testNode(t, "udp4", nil, true)
	b.contact(d.addr())
	time.Sleep(200 * time.Millisecond)
	b.mu.Lock()
	inB := b.byAddr[d.addr()] != nil
	b.mu.Unlock()
	if !inB {
		t.Fatal("a node a peer told us about should be pinged and kept")
	}
}

// memNet is a packet network in memory, for addresses this computer may not have (IPv6).
type memNet struct {
	mu    sync.Mutex
	conns map[netip.AddrPort]*memPC
}

type memPkt struct {
	from netip.AddrPort
	data []byte
}

type memPC struct {
	n      *memNet
	addr   netip.AddrPort
	in     chan memPkt
	closed chan struct{}
	once   sync.Once
}

func (n *memNet) listen(a netip.AddrPort) *memPC {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.conns == nil {
		n.conns = map[netip.AddrPort]*memPC{}
	}
	c := &memPC{n: n, addr: a, in: make(chan memPkt, 256), closed: make(chan struct{})}
	n.conns[a] = c
	return c
}

func (c *memPC) ReadFrom(b []byte) (int, net.Addr, error) {
	select {
	case p := <-c.in:
		return copy(b, p.data), net.UDPAddrFromAddrPort(p.from), nil
	case <-c.closed:
		return 0, nil, net.ErrClosed
	}
}

func (c *memPC) WriteTo(b []byte, a net.Addr) (int, error) {
	c.n.mu.Lock()
	to := c.n.conns[addrPortOf(a)]
	c.n.mu.Unlock()
	if to != nil {
		select {
		case to.in <- memPkt{c.addr, append([]byte(nil), b...)}:
		default: // (a full queue drops, as UDP does)
		}
	}
	return len(b), nil
}

func (c *memPC) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}
func (c *memPC) LocalAddr() net.Addr              { return net.UDPAddrFromAddrPort(c.addr) }
func (c *memPC) SetDeadline(time.Time) error      { return nil }
func (c *memPC) SetReadDeadline(time.Time) error  { return nil }
func (c *memPC) SetWriteDeadline(time.Time) error { return nil }

// IPv6 (BEP 32): nodes on an IPv6 network find each other's announces (18-byte peers,
// nodes6), and an IPv4 node linked to an IPv6 one answers "want": n6.
func TestDHTIPv6(t *testing.T) {
	var mn memNet
	start := func(i int, boot []netip.AddrPort) *DHT {
		d := makeDHT(mn.listen(netip.MustParseAddrPort(fmt.Sprintf("[2001:db8::%d]:6881", i))), true, boot, randomNodeID())
		d.boot = nil
		d.SetReachable(true)
		d.start()
		t.Cleanup(d.Close)
		return d
	}
	first := start(1, nil)
	var nodes []*DHT
	for i := 2; i < 8; i++ {
		nodes = append(nodes, start(i, []netip.AddrPort{first.addr()}))
	}
	time.Sleep(200 * time.Millisecond)
	for _, d := range nodes {
		d.bootstrap()
	}
	if n := nodes[0].tableSize(); n < 3 {
		t.Fatalf("an IPv6 node knows only %d others", n)
	}
	var ih [20]byte
	rand.Read(ih[:])
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	nodes[0].FindAndAnnounce(ctx, ih, 5555, nil)
	var found []netip.AddrPort
	nodes[5].GetPeers(ctx, ih, func(ps []netip.AddrPort) { found = append(found, ps...) })
	if fmt.Sprint(found) != "[[2001:db8::2]:5555]" {
		t.Fatalf("found %v", found)
	}
	// an IPv4 node linked to an IPv6 one
	pc4, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	v4 := makeDHT(pc4, false, nil, randomNodeID())
	v6 := makeDHT(mn.listen(netip.MustParseAddrPort("[2001:db8::99]:6881")), true, []netip.AddrPort{first.addr()}, randomNodeID())
	v4.boot, v6.boot = nil, nil
	v4.other, v6.other = v6, v4
	v4.SetReachable(true)
	v6.SetReachable(true)
	v4.start()
	v6.start()
	defer v4.Close()
	defer v6.Close()
	time.Sleep(200 * time.Millisecond)
	asker, _ := testNode(t, "udp4", nil, true)
	r, err := asker.query(ctx, v4.addr(), "find_node", map[string]any{"target": string(ih[:]), "want": []any{"n4", "n6"}})
	if err != nil {
		t.Fatal(err)
	}
	if n6 := parseNodes([]byte(bencode.Str(r, "nodes6")), true); len(n6) == 0 || !n6[0].addr.Addr().Is6() {
		t.Fatalf("want n6: %q", bencode.Str(r, "nodes6"))
	}
	if _, ok := r["nodes"]; !ok {
		t.Fatal("want n4: nodes too")
	}
	// lookups from the linked pair run on both families
	found = nil
	var mu sync.Mutex
	v4.GetPeers(ctx, ih, func(ps []netip.AddrPort) { mu.Lock(); found = append(found, ps...); mu.Unlock() })
	if fmt.Sprint(found) != "[[2001:db8::2]:5555]" {
		t.Fatalf("a lookup through the IPv4 node should ask the IPv6 DHT too: %v", found)
	}
}

// The same on a real IPv6 socket, where this computer has one.
func TestDHTIPv6Loopback(t *testing.T) {
	first, _ := testNode(t, "udp6", nil, true)
	second, _ := testNode(t, "udp6", []netip.AddrPort{first.addr()}, true)
	third, _ := testNode(t, "udp6", []netip.AddrPort{first.addr()}, true)
	time.Sleep(200 * time.Millisecond)
	second.bootstrap()
	third.bootstrap()
	var ih [20]byte
	rand.Read(ih[:])
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	second.FindAndAnnounce(ctx, ih, 5555, nil)
	var found []netip.AddrPort
	third.GetPeers(ctx, ih, func(ps []netip.AddrPort) { found = append(found, ps...) })
	if fmt.Sprint(found) != "[[::1]:5555]" {
		t.Fatalf("found %v", found)
	}
}

// dht.json keeps the node ids and the good nodes; the old list form still reads.
func TestDHTSavedState(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "dht.json")
	os.WriteFile(p, []byte(`["1.2.3.4:6881","[2001:db8::1]:6881","junk"]`), 0o644)
	sv := readDHTFile(p)
	if len(sv.nodes) != 2 || sv.id == ([20]byte{}) {
		t.Fatalf("old form: %+v", sv)
	}
	a, _ := testNode(t, "udp4", nil, true)
	b, _ := testNode(t, "udp4", []netip.AddrPort{a.addr()}, true)
	time.Sleep(200 * time.Millisecond)
	b.bootstrap()
	data := b.savedState()
	if data == nil {
		t.Fatal("nothing to save")
	}
	os.WriteFile(p, data, 0o644)
	sv = readDHTFile(p)
	if sv.id != b.id || len(sv.nodes) == 0 || sv.nodes[0] != a.addr() {
		t.Fatalf("saved %s, read back %x %v", data, sv.id, sv.nodes)
	}
	if d := makeDHT(b.pc, false, append(sv.nodes, netip.MustParseAddrPort("[::1]:1")), sv.id); len(d.saved) != 1 {
		t.Fatal("an IPv4 node starts from IPv4 nodes only")
	}
}

// Zipline announces itself in the DHT: a libtorrent leecher that knows nothing but a
// libtorrent DHT node finds Zipline there and downloads from it.
func TestDHTLibtorrentFindsZipline(t *testing.T) {
	py := ltPython(t)
	node := ltStart(t, py, "dhtnode")
	boot := fmt.Sprintf("127.0.0.1:%d", int(node.wait(t, "port", 20*time.Second)["port"].(float64)))
	old := dhtBootstrap
	dhtBootstrap = []string{boot}
	defer func() { dhtBootstrap = old }()
	sw := newSwarm(t, py, "")
	m := torrentManager(t)
	m.mu.Lock()
	m.settings.Torrent.NoDHT = false
	m.mu.Unlock()
	d := seedFromDisk(t, m, sw)
	runningTorrent(t, m, d.ID)
	time.Sleep(3 * time.Second) // Zipline's first lookup and announce
	ih, _ := hex.DecodeString(sw.hash)
	leech := ltStart(t, py, "leech", magnetLink([20]byte(ih), "", nil), t.TempDir(), "-", "--dht="+boot)
	if r := leech.wait(t, "done", 120*time.Second); r["done"] != true {
		t.Fatal("libtorrent should find Zipline through the DHT")
	}
	m.mu.Lock()
	dht := m.dht
	m.mu.Unlock()
	if dht == nil || dht.tableSize() == 0 {
		t.Fatal("Zipline's DHT node should know the libtorrent node")
	}
}
