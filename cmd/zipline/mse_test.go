package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"io"
	"math/big"
	"net"
	"net/netip"
	"testing"
	"time"
)

// A reference MSE implementation written straight from the spec (its own RC4 and DH, no code
// shared with mse.go), to check Zipline's handshake from both ends.

type refRC4 struct {
	s    [256]byte
	i, j uint8
}

func newRefRC4(key []byte) *refRC4 {
	c := &refRC4{}
	for i := range c.s {
		c.s[i] = byte(i)
	}
	var j uint8
	for i := 0; i < 256; i++ {
		j += c.s[i] + key[i%len(key)]
		c.s[i], c.s[j] = c.s[j], c.s[i]
	}
	c.crypt(make([]byte, 1024)) // RC4-drop1024
	return c
}

func (c *refRC4) crypt(b []byte) []byte {
	for k := range b {
		c.i++
		c.j += c.s[c.i]
		c.s[c.i], c.s[c.j] = c.s[c.j], c.s[c.i]
		b[k] ^= c.s[c.s[c.i]+c.s[c.j]]
	}
	return b
}

const refPrime = "FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B22514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A63A36210000000000090563"

func refSHA1(parts ...[]byte) []byte {
	h := sha1.New()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

type refDH struct{ x, p *big.Int }

func newRefDH() (*refDH, []byte) {
	p, _ := new(big.Int).SetString(refPrime, 16)
	xb := make([]byte, 20)
	rand.Read(xb)
	d := &refDH{x: new(big.Int).SetBytes(xb), p: p}
	y := new(big.Int).Exp(big.NewInt(2), d.x, p)
	out := make([]byte, 96)
	y.FillBytes(out)
	return d, out
}

func (d *refDH) secret(y []byte) []byte {
	s := new(big.Int).Exp(new(big.Int).SetBytes(y), d.x, d.p)
	out := make([]byte, 96)
	return s.FillBytes(out)
}

func refPad(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

// refStream is the payload stream after the handshake.
type refStream struct {
	c        net.Conn
	enc, dec *refRC4 // nil = plaintext
	pre      []byte
}

func (s *refStream) write(b []byte) error {
	b = append([]byte(nil), b...)
	if s.enc != nil {
		s.enc.crypt(b)
	}
	_, err := s.c.Write(b)
	return err
}

func (s *refStream) read(n int) ([]byte, error) {
	out := make([]byte, n)
	k := copy(out, s.pre)
	s.pre = s.pre[k:]
	if _, err := io.ReadFull(s.c, out[k:]); err != nil {
		return nil, err
	}
	if s.dec != nil {
		s.dec.crypt(out[k:])
	}
	return out, nil
}

func readUntil(c net.Conn, want []byte, max int) error {
	var got []byte
	b := make([]byte, 1)
	for len(got) < max+len(want) {
		if _, err := io.ReadFull(c, b); err != nil {
			return err
		}
		got = append(got, b[0])
		if bytes.HasSuffix(got, want) {
			return nil
		}
	}
	return errors.New("no sync")
}

// refB answers an MSE handshake (the receiving side), choosing sel; it reports what A offered and its IA.
func refB(c net.Conn, ih [20]byte, sel uint32) (s *refStream, provide uint32, ia []byte, err error) {
	ya := make([]byte, 96)
	if _, err = io.ReadFull(c, ya); err != nil {
		return
	}
	dh, yb := newRefDH()
	c.Write(append(yb, refPad(77)...))
	S := dh.secret(ya)
	if err = readUntil(c, refSHA1([]byte("req1"), S), 512); err != nil {
		return
	}
	x := make([]byte, 20)
	io.ReadFull(c, x)
	want := refSHA1([]byte("req2"), ih[:])
	r3 := refSHA1([]byte("req3"), S)
	for i := range want {
		want[i] ^= r3[i]
	}
	if !bytes.Equal(x, want) {
		return nil, 0, nil, errors.New("req2 doesn't match")
	}
	dec, enc := newRefRC4(refSHA1([]byte("keyA"), S, ih[:])), newRefRC4(refSHA1([]byte("keyB"), S, ih[:]))
	hd := make([]byte, 14)
	io.ReadFull(c, hd)
	dec.crypt(hd)
	if !bytes.Equal(hd[:8], make([]byte, 8)) {
		return nil, 0, nil, errors.New("bad VC")
	}
	provide = binary.BigEndian.Uint32(hd[8:])
	padC := make([]byte, binary.BigEndian.Uint16(hd[12:])+2)
	io.ReadFull(c, padC)
	dec.crypt(padC)
	ia = make([]byte, binary.BigEndian.Uint16(padC[len(padC)-2:]))
	io.ReadFull(c, ia)
	dec.crypt(ia)
	padD := refPad(33)
	out := make([]byte, 14)
	binary.BigEndian.PutUint32(out[8:], sel)
	binary.BigEndian.PutUint16(out[12:], uint16(len(padD)))
	c.Write(enc.crypt(append(out, padD...)))
	s = &refStream{c: c}
	if sel == 2 {
		s.enc, s.dec = enc, dec
	}
	return s, provide, ia, nil
}

// refA starts an MSE handshake (the connecting side) offering provide, with ia as the initial payload.
func refA(c net.Conn, ih [20]byte, provide uint32, ia []byte) (*refStream, uint32, error) {
	dh, ya := newRefDH()
	c.Write(append(ya, refPad(200)...))
	yb := make([]byte, 96)
	if _, err := io.ReadFull(c, yb); err != nil {
		return nil, 0, err
	}
	S := dh.secret(yb)
	enc, dec := newRefRC4(refSHA1([]byte("keyA"), S, ih[:])), newRefRC4(refSHA1([]byte("keyB"), S, ih[:]))
	x := refSHA1([]byte("req2"), ih[:])
	r3 := refSHA1([]byte("req3"), S)
	for i := range x {
		x[i] ^= r3[i]
	}
	padC := refPad(9)
	body := make([]byte, 14)
	binary.BigEndian.PutUint32(body[8:], provide)
	binary.BigEndian.PutUint16(body[12:], uint16(len(padC)))
	body = append(body, padC...)
	body = binary.BigEndian.AppendUint16(body, uint16(len(ia)))
	body = append(body, ia...)
	msg := append(refSHA1([]byte("req1"), S), x...)
	c.Write(append(msg, enc.crypt(body)...))
	vc := dec.crypt(make([]byte, 8))
	if err := readUntil(c, vc, 512); err != nil {
		return nil, 0, err
	}
	hd := make([]byte, 6)
	if _, err := io.ReadFull(c, hd); err != nil {
		return nil, 0, err
	}
	dec.crypt(hd)
	sel := binary.BigEndian.Uint32(hd)
	padD := make([]byte, binary.BigEndian.Uint16(hd[4:]))
	io.ReadFull(c, padD)
	dec.crypt(padD)
	s := &refStream{c: c}
	if sel == 2 {
		s.enc, s.dec = enc, dec
	}
	return s, sel, nil
}

func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ch := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		ch <- c
	}()
	a, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	b := <-ch
	t.Cleanup(func() { a.Close(); b.Close() })
	a.SetDeadline(time.Now().Add(10 * time.Second))
	b.SetDeadline(time.Now().Add(10 * time.Second))
	return a, b
}

func TestMSEPrime(t *testing.T) {
	ref, _ := new(big.Int).SetString(refPrime, 16)
	if mseP.Cmp(ref) != 0 || mseP.BitLen() != 768 || !mseP.ProbablyPrime(20) {
		t.Fatal("the MSE prime is wrong")
	}
}

// Zipline dials (A), the reference answers (B): RC4 when B picks it, plaintext after the header otherwise.
func TestMSEDialAgainstReference(t *testing.T) {
	var ih [20]byte
	rand.Read(ih[:])
	for _, tc := range []struct {
		mode        encMode
		sel         uint32
		wantProvide uint32
	}{{encPrefer, 2, 3}, {encPrefer, 1, 3}, {encRequire, 2, 2}} {
		a, b := tcpPair(t)
		type res struct {
			s       *refStream
			provide uint32
			err     error
		}
		ch := make(chan res, 1)
		go func() {
			s, provide, ia, err := refB(b, ih, tc.sel)
			if err == nil && len(ia) != 0 {
				err = errors.New("Zipline sends no IA")
			}
			ch <- res{s, provide, err}
		}()
		c, err := mseDial(a, ih, tc.mode)
		if err != nil {
			t.Fatalf("mode %d: %v", tc.mode, err)
		}
		r := <-ch
		if r.err != nil || r.provide != tc.wantProvide {
			t.Fatalf("mode %d: reference B: %v, provide %d", tc.mode, r.err, r.provide)
		}
		if rc, obf := connEncryption(c); rc != (tc.sel == 2) || !obf {
			t.Fatalf("rc4 %v obfuscated %v", rc, obf)
		}
		msg := bytes.Repeat([]byte("zipline to reference "), 300)
		go c.Write(msg)
		got, err := r.s.read(len(msg))
		if err != nil || !bytes.Equal(got, msg) {
			t.Fatalf("A→B stream: %v", err)
		}
		back := bytes.Repeat([]byte("and back "), 999)
		go r.s.write(back)
		got = make([]byte, len(back))
		if _, err := io.ReadFull(c, got); err != nil || !bytes.Equal(got, back) {
			t.Fatalf("B→A stream: %v", err)
		}
		if tc.sel == 1 { // plaintext after the handshake: what goes over the wire is the message itself
			go c.Write([]byte("PLAIN"))
			raw := make([]byte, 5)
			io.ReadFull(b, raw)
			if string(raw) != "PLAIN" {
				t.Fatalf("plaintext stream: %q", raw)
			}
		}
	}
}

// The reference dials (A, with the BitTorrent handshake as IA), Zipline answers (B).
func TestMSEAcceptAgainstReference(t *testing.T) {
	var ih, other [20]byte
	rand.Read(ih[:])
	rand.Read(other[:])
	lookup := func(req2 [20]byte) ([20]byte, bool) {
		for _, h := range [][20]byte{other, ih} {
			if bytes.Equal(refSHA1([]byte("req2"), h[:]), req2[:]) {
				return h, true
			}
		}
		return [20]byte{}, false
	}
	hs := handshakeBytes(ih, newPeerID())
	for _, tc := range []struct {
		mode    encMode
		provide uint32
		want    uint32 // 0: refused
	}{{encPrefer, 3, 2}, {encPrefer, 1, 1}, {encRequire, 3, 2}, {encRequire, 1, 0}} {
		a, b := tcpPair(t)
		type res struct {
			s   *refStream
			sel uint32
			err error
		}
		ch := make(chan res, 1)
		go func() {
			s, sel, err := refA(a, ih, tc.provide, hs)
			ch <- res{s, sel, err}
		}()
		c, got, err := mseAccept(b, tc.mode, lookup)
		if tc.want == 0 {
			if err == nil {
				t.Fatalf("mode %d provide %d should be refused", tc.mode, tc.provide)
			}
			b.Close()
			<-ch
			continue
		}
		if err != nil || got != ih {
			t.Fatalf("mode %d provide %d: %v", tc.mode, tc.provide, err)
		}
		r := <-ch
		if r.err != nil || r.sel != tc.want {
			t.Fatalf("reference A: %v, select %d", r.err, r.sel)
		}
		h := make([]byte, 68)
		if _, err := io.ReadFull(c, h); err != nil || !bytes.Equal(h, hs) || !mseMatches(c, ih) || mseMatches(c, other) {
			t.Fatalf("the IA should come out first: %v", err)
		}
		msg := bytes.Repeat([]byte{1, 2, 3, 250}, 5000)
		go r.s.write(msg)
		buf := make([]byte, len(msg))
		if _, err := io.ReadFull(c, buf); err != nil || !bytes.Equal(buf, msg) {
			t.Fatalf("A→B stream: %v", err)
		}
		go c.Write(msg[:777])
		if back, err := r.s.read(777); err != nil || !bytes.Equal(back, msg[:777]) {
			t.Fatalf("B→A stream: %v", err)
		}
	}
	// an unknown torrent is refused
	a, b := tcpPair(t)
	var nobody [20]byte
	rand.Read(nobody[:])
	go refA(a, nobody, 3, nil)
	if _, _, err := mseAccept(b, encPrefer, lookup); err == nil {
		t.Fatal("an encrypted connection for another torrent should be refused")
	}
}

// Zipline on both ends, and plain handshakes going straight through (or refused).
func TestMSEPlainAndBothEnds(t *testing.T) {
	var ih [20]byte
	rand.Read(ih[:])
	lookup := func([20]byte) ([20]byte, bool) { return ih, true }
	a, b := tcpPair(t)
	go a.Write(handshakeBytes(ih, newPeerID()))
	c, _, err := mseAccept(b, encPrefer, lookup)
	h := make([]byte, 68)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(c, h); err != nil || string(h[1:20]) != "BitTorrent protocol" || !mseMatches(c, [20]byte{}) {
		t.Fatalf("a plain handshake should pass through: %v %q", err, h[:20])
	}
	if rc, obf := connEncryption(c); rc || obf {
		t.Fatal("plain is plain")
	}
	a, b = tcpPair(t)
	go a.Write(handshakeBytes(ih, newPeerID()))
	if _, _, err := mseAccept(b, encRequire, lookup); err == nil {
		t.Fatal("plain should be refused when encryption is required")
	}
	a, b = tcpPair(t)
	go mseDial(a, ih, encPrefer)
	if _, _, err := mseAccept(b, encPlain, lookup); err == nil {
		t.Fatal("encrypted should be refused in plain-only mode")
	}
	a, b = tcpPair(t)
	ch := make(chan net.Conn, 1)
	go func() {
		c, _, _ := mseAccept(b, encRequire, lookup)
		ch <- c
	}()
	ca, err := mseDial(a, ih, encPrefer)
	if err != nil {
		t.Fatal(err)
	}
	cb := <-ch
	if cb == nil {
		t.Fatal("Zipline to Zipline failed")
	}
	if rc, _ := connEncryption(ca); !rc {
		t.Fatal("Zipline to Zipline should pick RC4")
	}
	go ca.Write([]byte("hello, encrypted world"))
	got := make([]byte, 22)
	if _, err := io.ReadFull(cb, got); err != nil || string(got) != "hello, encrypted world" {
		t.Fatalf("%q %v", got, err)
	}
}

// A peer that only speaks plain BitTorrent hangs up on the encrypted attempt: Zipline dials
// again plainly, and next time goes straight to plain.
func TestMSEFallsBackToPlain(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var ih [20]byte
	rand.Read(ih[:])
	conns := make(chan bool, 10)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				h := make([]byte, 68)
				if _, err := io.ReadFull(c, h); err != nil || string(h[1:20]) != "BitTorrent protocol" {
					conns <- false
					return // not BitTorrent: hang up
				}
				conns <- true
				var id [20]byte
				copy(id[:], "-XX0001-plainplainpl")
				c.Write(handshakeBytes(ih, id))
				io.Copy(io.Discard, c)
			}(c)
		}
	}()
	m := torrentManager(t)
	tr := &torrent{m: m, ih: ih, peerID: newPeerID()}
	ap, _ := netip.ParseAddrPort(ln.Addr().String())
	redial := func() (net.Conn, error) { return net.Dial("tcp4", ap.String()) }
	c0, _ := redial()
	c, h, err := tr.encHandshake(c0, ap, redial)
	if err != nil || string(h[48:56]) != "-XX0001-" {
		t.Fatalf("fallback: %v", err)
	}
	c.Close()
	if <-conns || !<-conns || !isPlainPeer(ap) {
		t.Fatal("first an encrypted try, then plain")
	}
	c0, _ = redial()
	if c, _, err = tr.encHandshake(c0, ap, redial); err != nil || !<-conns {
		t.Fatalf("the second time goes straight to plain: %v", err)
	}
	c.Close()
	m.mu.Lock()
	m.settings.Torrent.Enc = "require"
	m.mu.Unlock()
	c0, _ = redial()
	if _, _, err = tr.encHandshake(c0, ap, redial); err == nil {
		t.Fatal("encryption required: a plain peer is no peer")
	}
}
