package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/rc4"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"io"
	"math/big"
	"net"
	"net/netip"
	"sync"
	"time"
)

// Message Stream Encryption (MSE, also called PE): a Diffie-Hellman key exchange before the
// BitTorrent handshake, then RC4 on everything after it, so networks that slow BitTorrent
// down can't recognise it and peers that only take encrypted connections let us in.
// A dials, B answers:
//
//	A→B  Ya, PadA
//	B→A  Yb, PadB
//	A→B  HASH('req1', S), HASH('req2', SKEY) xor HASH('req3', S), ENCRYPT(VC, crypto_provide, len(PadC), PadC, len(IA)), ENCRYPT(IA)
//	B→A  ENCRYPT(VC, crypto_select, len(PadD), PadD), then the stream in the chosen method
//
// S is the shared secret, SKEY the info hash, VC eight zero bytes, and ENCRYPT RC4 with the first
// 1024 bytes of key stream thrown away (keyA = HASH('keyA', S, SKEY) one way, keyB the other).

// encMode is Options → Torrents → Encryption.
type encMode int

const (
	encPrefer  encMode = iota // try encrypted first, take plain peers too (the default)
	encRequire                // encrypted (RC4) connections only
	encPlain                  // never encrypt
)

const (
	mseCryptoPlain = 1 // crypto_provide / crypto_select bits
	mseCryptoRC4   = 2
	mseMaxPad      = 512
)

var (
	mseP, _ = new(big.Int).SetString("FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B22514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A63A36210000000000090563", 16)
	mseG    = big.NewInt(2)

	errMSEHungUp = errors.New("the peer hung up on the encrypted handshake")
	errMSE       = errors.New("bad encrypted handshake")
)

func (m *Manager) encMode() encMode {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch m.settings.Torrent.Enc {
	case "require":
		return encRequire
	case "plain":
		return encPlain
	}
	return encPrefer
}

// mseConn is a connection after the MSE handshake (or an incoming plain one): it hands on what
// the handshake read ahead, then decrypts and encrypts the stream when RC4 was chosen.
type mseConn struct {
	net.Conn
	pre        []byte      // read already, plaintext, not yet handed on
	dec, enc   *rc4.Cipher // nil: plaintext stream
	obfuscated bool        // the MSE handshake ran (even if the stream is plaintext after it)
	ih         [20]byte    // the torrent B found from req2 (incoming)
	wmu        sync.Mutex
	wbuf       []byte
}

// NetConn is the connection underneath (uTP or TCP: utp.IsConn looks through).
func (c *mseConn) NetConn() net.Conn { return c.Conn }

func (c *mseConn) Read(b []byte) (int, error) {
	if len(c.pre) > 0 {
		n := copy(b, c.pre)
		c.pre = c.pre[n:]
		return n, nil
	}
	n, err := c.Conn.Read(b)
	if c.dec != nil && n > 0 {
		c.dec.XORKeyStream(b[:n], b[:n])
	}
	return n, err
}

func (c *mseConn) Write(b []byte) (int, error) {
	if c.enc == nil {
		return c.Conn.Write(b)
	}
	c.wmu.Lock() // the key stream has to go out in order
	defer c.wmu.Unlock()
	if cap(c.wbuf) < len(b) {
		c.wbuf = make([]byte, len(b))
	}
	out := c.wbuf[:len(b)]
	c.enc.XORKeyStream(out, b) // (never in place: a Write mustn't change the caller's bytes)
	return c.Conn.Write(out)
}

// connEncryption says whether a peer's stream is RC4-encrypted (and whether MSE ran at all).
func connEncryption(c net.Conn) (rc4, obfuscated bool) {
	if e, ok := c.(*mseConn); ok {
		return e.enc != nil, e.obfuscated
	}
	return false, false
}

func mseHash(parts ...[]byte) []byte {
	h := sha1.New()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

// mseKeys makes a private key (160 random bits) and its public key, 96 bytes big-endian.
func mseKeys() (*big.Int, []byte) {
	var b [20]byte
	rand.Read(b[:])
	x := new(big.Int).SetBytes(b[:])
	return x, mseBytes(new(big.Int).Exp(mseG, x, mseP))
}

func mseBytes(n *big.Int) []byte {
	out := make([]byte, 96)
	return n.FillBytes(out)
}

// mseSecret is S from the other side's public key; a key outside 2…P-2 is refused.
func mseSecret(their []byte, x *big.Int) ([]byte, error) {
	y := new(big.Int).SetBytes(their)
	if y.Cmp(big.NewInt(1)) <= 0 || y.Cmp(new(big.Int).Sub(mseP, big.NewInt(1))) >= 0 {
		return nil, errMSE
	}
	return mseBytes(new(big.Int).Exp(y, x, mseP)), nil
}

// mseRC4 is the cipher for one direction, its first 1024 bytes of key stream discarded.
func mseRC4(name string, s []byte, skey [20]byte) *rc4.Cipher {
	c, _ := rc4.NewCipher(mseHash([]byte(name), s, skey[:]))
	var drop [1024]byte
	c.XORKeyStream(drop[:], drop[:])
	return c
}

func msePad() []byte {
	var n [2]byte
	rand.Read(n[:])
	pad := make([]byte, int(binary.BigEndian.Uint16(n[:]))%(mseMaxPad+1))
	rand.Read(pad)
	return pad
}

// mseSync reads until want turns up within the next max bytes (the other side's padding comes first).
func mseSync(r *bufio.Reader, want []byte, max int) error {
	got := make([]byte, 0, max+len(want))
	for len(got) < max+len(want) {
		c, err := r.ReadByte()
		if err != nil {
			return err
		}
		if got = append(got, c); bytes.HasSuffix(got, want) {
			return nil
		}
	}
	return errMSE
}

// mseRest is what the handshake's reader holds beyond the handshake, decrypted if the stream is.
func mseRest(r *bufio.Reader, dec *rc4.Cipher) []byte {
	rest, _ := r.Peek(r.Buffered())
	rest = append([]byte(nil), rest...)
	if dec != nil {
		dec.XORKeyStream(rest, rest)
	}
	return rest
}

// mseDial runs the encrypted handshake as A on an outgoing connection and returns the connection
// to send the BitTorrent handshake on. It offers RC4 and plaintext (only RC4 when encryption is
// required). errMSEHungUp means the peer doesn't speak MSE: dial again and talk plainly.
// The caller sets the deadline.
func mseDial(conn net.Conn, ih [20]byte, mode encMode) (net.Conn, error) {
	x, ya := mseKeys()
	if _, err := conn.Write(append(ya, msePad()...)); err != nil {
		return nil, errMSEHungUp
	}
	r := bufio.NewReaderSize(conn, 1024)
	yb := make([]byte, 96)
	if _, err := io.ReadFull(r, yb); err != nil {
		return nil, errMSEHungUp // closed, reset or silent: it doesn't speak MSE
	}
	if bytes.HasPrefix(yb, []byte("\x13BitTorrent protocol")) {
		return nil, errMSEHungUp // a plain peer answered our key with its handshake
	}
	s, err := mseSecret(yb, x)
	if err != nil {
		return nil, err
	}
	enc, dec := mseRC4("keyA", s, ih), mseRC4("keyB", s, ih)
	provide := uint32(mseCryptoRC4 | mseCryptoPlain)
	if mode == encRequire {
		provide = mseCryptoRC4
	}
	msg := mseHash([]byte("req1"), s)
	req2, req3 := mseHash([]byte("req2"), ih[:]), mseHash([]byte("req3"), s)
	for i := range req2 {
		req2[i] ^= req3[i]
	}
	msg = append(msg, req2...)
	tail := make([]byte, 8+4+2+2) // VC, crypto_provide, len(PadC) = 0, len(IA) = 0
	binary.BigEndian.PutUint32(tail[8:], provide)
	enc.XORKeyStream(tail, tail)
	if _, err := conn.Write(append(msg, tail...)); err != nil {
		return nil, err
	}
	// B's answer starts with VC encrypted, after up to 512 bytes of PadB
	vc := make([]byte, 8)
	dec.XORKeyStream(vc, vc)
	if err := mseSync(r, vc, mseMaxPad); err != nil {
		return nil, err
	}
	var hd [6]byte
	if _, err := io.ReadFull(r, hd[:]); err != nil {
		return nil, err
	}
	dec.XORKeyStream(hd[:], hd[:])
	sel, padD := binary.BigEndian.Uint32(hd[:]), int(binary.BigEndian.Uint16(hd[4:]))
	if sel&provide == 0 || sel&(sel-1) != 0 || padD > mseMaxPad {
		return nil, errMSE
	}
	pad := make([]byte, padD)
	if _, err := io.ReadFull(r, pad); err != nil {
		return nil, err
	}
	dec.XORKeyStream(pad, pad)
	c := &mseConn{Conn: conn, obfuscated: true}
	if sel == mseCryptoRC4 {
		c.enc, c.dec = enc, dec
	}
	c.pre = mseRest(r, c.dec)
	return c, nil
}

// mseAccept reads the start of an incoming connection. A plain BitTorrent handshake (0x13
// "BitTorrent protocol") is handed back unread; anything else is taken as MSE, answered as B,
// with the torrent found by req2 among the running ones (lookup gets HASH('req2', SKEY)).
// The connection returned replays what was read ahead (the BitTorrent handshake when A sent it
// as IA). The caller sets the deadline.
func mseAccept(conn net.Conn, mode encMode, lookup func(req2 [20]byte) ([20]byte, bool)) (net.Conn, [20]byte, error) {
	var ih [20]byte
	r := bufio.NewReaderSize(conn, 1024)
	head, err := r.Peek(20)
	if err != nil {
		return nil, ih, err
	}
	if bytes.Equal(head, []byte("\x13BitTorrent protocol")) {
		if mode == encRequire {
			return nil, ih, errors.New("plain connection refused: encryption is required")
		}
		return &mseConn{Conn: conn, pre: mseRest(r, nil)}, ih, nil
	}
	if mode == encPlain {
		return nil, ih, errors.New("encrypted connection refused: plain only")
	}
	ya := make([]byte, 96)
	if _, err := io.ReadFull(r, ya); err != nil {
		return nil, ih, err
	}
	x, yb := mseKeys()
	if _, err := conn.Write(append(yb, msePad()...)); err != nil {
		return nil, ih, err
	}
	s, err := mseSecret(ya, x)
	if err != nil {
		return nil, ih, err
	}
	if err := mseSync(r, mseHash([]byte("req1"), s), mseMaxPad); err != nil {
		return nil, ih, err
	}
	var req [20]byte
	if _, err := io.ReadFull(r, req[:]); err != nil {
		return nil, ih, err
	}
	req3 := mseHash([]byte("req3"), s)
	for i := range req {
		req[i] ^= req3[i]
	}
	ih, ok := lookup(req)
	if !ok {
		return nil, ih, errors.New("encrypted connection for a torrent we don't have")
	}
	dec, enc := mseRC4("keyA", s, ih), mseRC4("keyB", s, ih)
	var hd [14]byte // VC, crypto_provide, len(PadC)
	if _, err := io.ReadFull(r, hd[:]); err != nil {
		return nil, ih, err
	}
	dec.XORKeyStream(hd[:], hd[:])
	provide, padC := binary.BigEndian.Uint32(hd[8:]), int(binary.BigEndian.Uint16(hd[12:]))
	if !bytes.Equal(hd[:8], make([]byte, 8)) || padC > mseMaxPad {
		return nil, ih, errMSE
	}
	pad := make([]byte, padC+2) // PadC, len(IA)
	if _, err := io.ReadFull(r, pad); err != nil {
		return nil, ih, err
	}
	dec.XORKeyStream(pad, pad)
	ia := make([]byte, binary.BigEndian.Uint16(pad[padC:]))
	if _, err := io.ReadFull(r, ia); err != nil {
		return nil, ih, err
	}
	dec.XORKeyStream(ia, ia) // (IA is always RC4, whatever is chosen for the stream)
	var sel uint32
	switch {
	case provide&mseCryptoRC4 != 0:
		sel = mseCryptoRC4
	case provide&mseCryptoPlain != 0 && mode != encRequire:
		sel = mseCryptoPlain
	default:
		return nil, ih, errors.New("no encryption method in common")
	}
	out := make([]byte, 8+4+2) // VC, crypto_select, len(PadD) = 0
	binary.BigEndian.PutUint32(out[8:], sel)
	enc.XORKeyStream(out, out)
	if _, err := conn.Write(out); err != nil {
		return nil, ih, err
	}
	c := &mseConn{Conn: conn, obfuscated: true, ih: ih}
	if sel == mseCryptoRC4 {
		c.enc, c.dec = enc, dec
	}
	c.pre = append(ia, mseRest(r, c.dec)...)
	return c, ih, nil
}

// ---------------------------------------------------------------- wired into the engine

// plainPeers remembers peers that hung up on an encrypted attempt, so the next try is plain.
var plainPeers = struct {
	sync.Mutex
	m map[netip.AddrPort]time.Time
}{m: map[netip.AddrPort]time.Time{}}

func isPlainPeer(a netip.AddrPort) bool {
	plainPeers.Lock()
	defer plainPeers.Unlock()
	at, ok := plainPeers.m[a]
	return ok && time.Since(at) < time.Hour
}

func forgetPlainPeer(a netip.AddrPort) {
	plainPeers.Lock()
	defer plainPeers.Unlock()
	delete(plainPeers.m, a)
}

func markPlainPeer(a netip.AddrPort) {
	plainPeers.Lock()
	defer plainPeers.Unlock()
	if len(plainPeers.m) > 5000 {
		clear(plainPeers.m)
	}
	plainPeers.m[a] = time.Now()
}

// encHandshake runs the encrypted handshake (unless Options says plain) and then the BitTorrent
// one on an outgoing connection. When the peer hangs up on the encrypted attempt it's dialled
// again with redial and greeted plainly (unless encryption is required); once that works it's
// remembered, and the next time goes straight to plain. On an error the connection is closed.
func (t *torrent) encHandshake(conn net.Conn, a netip.AddrPort, redial func() (net.Conn, error)) (net.Conn, [68]byte, error) {
	mode := t.m.encMode()
	retried := false
	if mode == encRequire || mode == encPrefer && !isPlainPeer(a) {
		conn.SetDeadline(time.Now().Add(15 * time.Second))
		c, err := mseDial(conn, t.ih, mode)
		if err == nil {
			h, err := t.handshake(c)
			if err != nil {
				conn.Close()
			}
			return c, h, err
		}
		conn.Close()
		tdbg("peer %s: encrypted handshake: %v", a, err)
		if mode == encRequire || err != errMSEHungUp {
			return nil, [68]byte{}, err
		}
		if conn, err = redial(); err != nil {
			return nil, [68]byte{}, err
		}
		retried = true
	}
	h, err := t.handshake(conn)
	if err != nil {
		conn.Close()
		if mode == encPrefer {
			forgetPlainPeer(a) // (maybe it was only down: encrypted first again next time)
		}
		return nil, h, err
	}
	if retried {
		markPlainPeer(a)
	}
	return conn, h, nil
}

// acceptEnc reads the start of an incoming connection: a plain handshake, or the encrypted one
// for one of the running torrents. On an error it returns conn as it was, for closing.
func (m *Manager) acceptEnc(conn net.Conn) (net.Conn, error) {
	c, _, err := mseAccept(conn, m.encMode(), func(req2 [20]byte) ([20]byte, bool) {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, t := range m.torrents {
			if bytes.Equal(mseHash([]byte("req2"), t.ih[:]), req2[:]) {
				return t.ih, true
			}
		}
		return [20]byte{}, false
	})
	if err != nil {
		return conn, err
	}
	return c, nil
}

// mseMatches: an encrypted connection's BitTorrent handshake names the torrent MSE found.
func mseMatches(c net.Conn, ih [20]byte) bool {
	e, ok := c.(*mseConn)
	return !ok || !e.obfuscated || e.ih == ih
}
