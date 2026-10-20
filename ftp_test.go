package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------- an FTP server closer to real ones

// ftpd is a test FTP server where logins land in home folders and names resolve against
// the current folder. It can cap connections, and speaks TLS (implicit, or AUTH TLS) with
// PROT P data connections that must resume their control connection's session, like
// vsftpd's require_ssl_reuse.
type ftpd struct {
	ln        net.Listener
	files     map[string][]byte    // absolute path → contents
	users     map[string][2]string // user → password, home folder; anonymous lands in /
	rate      int                  // bytes/s per data connection, 0 = unlimited
	tls       *tls.Config          // nil: no TLS
	implicit  bool
	noAnonTLS bool   // refuse anonymous logins over TLS (vsftpd's allow_anon_ssl=NO)
	breakData bool   // drop PROT P data connections instead of doing TLS
	freshData bool   // data connections can't resume (other session keys), so they're refused
	abortAt   int    // cut transfers short after this many bytes with "426"
	limit     int    // sessions served at once; the others get busy
	busy      string // reply for those: "421 …" at the greeting, "530 …" after PASS
	busyFirst int    // turn away this many first connections whatever the limit

	mu          sync.Mutex
	conns       int // control connections so far
	active      int
	peak        int // sessions at once (not counting the ones turned away)
	log         []string
	retrs       int
	tlsData     int // PROT P data connections that resumed their control connection's session
	plainData   int
	refused     int // data connections refused for not resuming the session
	plainLogins int // passwords received in the clear
}

func newFTPD(t *testing.T, setup func(*ftpd)) *ftpd {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &ftpd{ln: ln, files: map[string][]byte{}, users: map[string][2]string{}}
	if setup != nil {
		setup(s)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *ftpd) url(scheme, login, p string) string {
	if login != "" {
		login += "@"
	}
	return scheme + "://" + login + s.ln.Addr().String() + p
}

func (s *ftpd) commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.log)
}

func (s *ftpd) count(n *int) {
	s.mu.Lock()
	*n++
	s.mu.Unlock()
}

func (s *ftpd) resolve(cwd, p string) string {
	if strings.HasPrefix(p, "/") {
		return path.Clean(p)
	}
	return path.Join(cwd, p)
}

func (s *ftpd) isDir(p string) bool {
	for f := range s.files {
		if p == "/" || strings.HasPrefix(f, p+"/") {
			return true
		}
	}
	return false
}

func (s *ftpd) serve(raw net.Conn) {
	defer raw.Close()
	s.mu.Lock()
	s.conns++
	full := s.conns <= s.busyFirst || s.limit > 0 && s.active >= s.limit
	if !full {
		s.active++
		s.peak = max(s.peak, s.active)
		defer func() { s.mu.Lock(); s.active--; s.mu.Unlock() }()
	}
	s.mu.Unlock()

	var cfg *tls.Config
	if s.tls != nil {
		cfg = s.tls.Clone()
		var k [32]byte
		rand.Read(k[:])
		cfg.SetSessionTicketKeys([][32]byte{k}) // only this connection's sessions resume
	}
	c := raw
	if s.implicit {
		tc := tls.Server(raw, cfg)
		if tc.Handshake() != nil {
			return
		}
		c = tc
	}
	r := bufio.NewReader(c)
	say := func(f string, a ...any) { fmt.Fprintf(c, f+"\r\n", a...) }
	if full && strings.HasPrefix(s.busy, "421") {
		say("%s", s.busy)
		return
	}
	say("220-Welcome\r\n220 ready")
	user, cwd, prot := "", "/", false
	var pasv net.Listener
	defer func() {
		if pasv != nil {
			pasv.Close()
		}
	}()
	var rest int64
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		cmd, arg, _ := strings.Cut(line, " ")
		cmd = strings.ToUpper(cmd)
		s.mu.Lock()
		s.log = append(s.log, line)
		s.mu.Unlock()
		_, secure := c.(*tls.Conn)
		switch cmd {
		case "AUTH":
			if cfg == nil || s.implicit {
				say("502 no TLS here")
				continue
			}
			say("234 starting TLS")
			tc := tls.Server(c, cfg)
			if tc.Handshake() != nil {
				return
			}
			c, r = tc, bufio.NewReader(tc)
		case "USER":
			user = arg
			say("331 password please")
		case "PASS":
			if full {
				say("%s", s.busy)
				continue
			}
			if !secure {
				s.count(&s.plainLogins)
			}
			u, ok := s.users[user]
			switch {
			case user == "anonymous" && secure && s.noAnonTLS:
				say("530 Anonymous sessions may not use encryption.")
				continue
			case user == "anonymous" && !ok:
				cwd = "/"
			case ok && u[0] == arg:
				cwd = u[1]
			default:
				say("530 Login incorrect.")
				continue
			}
			say("230 logged in")
		case "PBSZ":
			say("200 PBSZ=0")
		case "PROT":
			prot = arg == "P" && secure
			say("200 PROT %s", arg)
		case "TYPE":
			say("200 binary")
		case "PWD":
			say(`257 "%s" is the current folder`, cwd)
		case "CWD":
			p := s.resolve(cwd, arg)
			if !s.isDir(p) {
				say("550 %s: no such folder", arg)
				continue
			}
			cwd = p
			say("250 ok")
		case "SIZE", "MDTM":
			f, ok := s.files[s.resolve(cwd, arg)]
			switch {
			case !ok:
				say("550 no such file")
			case cmd == "SIZE":
				say("213 %d", len(f))
			default:
				say("213 20260101120000")
			}
		case "EPSV", "PASV":
			if pasv != nil {
				pasv.Close()
			}
			pasv, _ = net.Listen("tcp", "127.0.0.1:0")
			pasv.(*net.TCPListener).SetDeadline(time.Now().Add(10 * time.Second))
			p := pasv.Addr().(*net.TCPAddr).Port
			if cmd == "EPSV" {
				say("229 Entering Extended Passive Mode (|||%d|)", p)
			} else {
				say("227 Entering Passive Mode (10,9,9,9,%d,%d)", p/256, p%256)
			}
		case "REST":
			rest, _ = strconv.ParseInt(arg, 10, 64)
			say("350 restarting at %d", rest)
		case "RETR":
			f, ok := s.files[s.resolve(cwd, arg)]
			if !ok || pasv == nil {
				say("550 no such file")
				continue
			}
			say("150 opening data connection")
			dc, err := pasv.Accept()
			pasv.Close()
			pasv = nil
			if err != nil {
				return
			}
			s.count(&s.retrs)
			if prot {
				if s.breakData {
					dc.Close()
					say("425 can't open data connection")
					continue
				}
				dcfg := cfg
				if s.freshData {
					dcfg = s.tls.Clone()
				}
				tc := tls.Server(dc, dcfg)
				if err := tc.Handshake(); err != nil || !tc.ConnectionState().DidResume {
					s.count(&s.refused)
					dc.Close()
					say("522 SSL connection failed: session reuse required")
					continue
				}
				s.count(&s.tlsData)
				dc = tc
			} else {
				s.count(&s.plainData)
			}
			src := f[min(rest, int64(len(f))):]
			rest = 0
			if s.abortAt > 0 && len(src) > s.abortAt {
				s.send(dc, src[:s.abortAt])
				dc.Close()
				say("426 Connection closed; transfer aborted.")
				continue
			}
			s.send(dc, src)
			dc.Close()
			say("226 done")
		case "QUIT":
			say("221 bye")
			return
		default:
			say("502 not implemented")
		}
	}
}

func (s *ftpd) send(dc net.Conn, src []byte) {
	start, sent := time.Now(), 0
	for len(src) > 0 {
		n := min(32<<10, len(src))
		if _, err := dc.Write(src[:n]); err != nil {
			return
		}
		src, sent = src[n:], sent+n
		if s.rate > 0 {
			if want := time.Duration(float64(sent) / float64(s.rate) * float64(time.Second)); time.Since(start) < want {
				time.Sleep(want - time.Since(start))
			}
		}
	}
}

// testCert makes a self-signed ECDSA certificate for 127.0.0.1, localhost and files.test,
// and a pool that trusts it.
func testCert(t *testing.T) (*tls.Config, *x509.CertPool) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "zipline test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		DNSNames:              []string{"localhost", "files.test"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: cert}}}, pool
}

// trustFTPTLS makes the FTP client trust pool, starting from a clean slate of TLS notes
// (an earlier test's server may have had the same port).
func trustFTPTLS(t *testing.T, pool *x509.CertPool) {
	old := ftpTLSConfig
	ftpTLSConfig = func(host string) *tls.Config { return &tls.Config{ServerName: host, RootCAs: pool} }
	forgetFTPTLS()
	t.Cleanup(func() { ftpTLSConfig = old; forgetFTPTLS() })
}

func forgetFTPTLS() {
	ftpTLSNotes.Lock()
	clear(ftpTLSNotes.m)
	ftpTLSNotes.Unlock()
}

func readProbe(t *testing.T, raw string) (*probeResult, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pr, err := probeFTP(ctx, &Download{}, raw, 0)
	if err != nil {
		t.Fatalf("probe %s: %v", raw, err)
	}
	defer pr.body.Close()
	b, err := io.ReadAll(pr.body)
	if err != nil {
		t.Fatalf("reading %s: %v", raw, err)
	}
	return pr, b
}

// ---------------------------------------------------------------- paths

func TestFTPSplitPath(t *testing.T) {
	cases := []struct{ url, dirs, name string }{
		{"ftp://h/file.bin", "", "file.bin"},
		{"ftp://h/pub/linux/file.bin", "pub|linux", "file.bin"},
		{"ftp://h/%2Fsrv/file.bin", "/|srv", "file.bin"},
		{"ftp://h/%2F/file.bin", "/", "file.bin"},
		{"ftp://h//srv/file.bin", "/|srv", "file.bin"},
		{"ftp://h/a//b/file.bin", "a|b", "file.bin"},
		{"ftp://h/my%20files/a%2Fb.bin", "my files|a", "b.bin"},
	}
	for _, c := range cases {
		u, _ := url.Parse(c.url)
		dirs, name, err := ftpSplit(u)
		if err != nil || strings.Join(dirs, "|") != c.dirs || name != c.name {
			t.Errorf("%s: dirs %q name %q err %v, want %q %q", c.url, dirs, name, err, c.dirs, c.name)
		}
	}
	u, _ := url.Parse("ftp://h/a%0D%0ADELE%20x/f")
	if _, _, err := ftpSplit(u); err == nil {
		t.Error("a line break in the path should be refused")
	}
}

// Paths are relative to where the login lands (RFC 1738), so accounts with a home folder work.
func TestFTPHomeFolder(t *testing.T) {
	data, other := randData(3<<20), randData(1000)
	srv := newFTPD(t, func(s *ftpd) {
		s.files["/home/bob/pub/file.bin"] = data
		s.files["/pub/file.bin"] = []byte("the file at the root, not bob's")
		s.files["/srv/other.bin"] = other
		s.users["bob"] = [2]string{"pw", "/home/bob"}
		s.rate = 2 << 20
	})
	m := newTestManager(t)
	m.settings.Conns = 4
	d := m.Add(AddReq{URL: srv.url("ftp", "bob:pw", "/pub/file.bin"), Start: true})
	waitFor(t, m, d, StCompleted, 20*time.Second)
	fileIs(t, d, data)
	log := srv.commands()
	if !slices.Contains(log, "CWD pub") || !slices.Contains(log, "RETR file.bin") || srv.retrs < 2 {
		t.Fatalf("expected CWD pub + RETR file.bin on several connections, got %d RETRs: %q", srv.retrs, log)
	}
	for _, l := range log {
		if strings.HasPrefix(l, "RETR /") || strings.HasPrefix(l, "SIZE /") {
			t.Fatalf("sent a path from the root: %q", l)
		}
	}

	// %2F starts from the root
	n := len(srv.commands())
	pr, got := readProbe(t, srv.url("ftp", "bob:pw", "/%2Fsrv/other.bin"))
	if string(got) != string(other) || pr.size != int64(len(other)) || pr.name != "other.bin" {
		t.Fatalf("absolute path: size %d, name %q, %d bytes", pr.size, pr.name, len(got))
	}
	var walk []string
	for _, l := range srv.commands()[n:] {
		if strings.HasPrefix(l, "CWD") || strings.HasPrefix(l, "SIZE") || strings.HasPrefix(l, "RETR") {
			walk = append(walk, l)
		}
	}
	if strings.Join(walk, ",") != "CWD /,CWD srv,SIZE other.bin,RETR other.bin" {
		t.Fatalf("absolute walk: %q", walk)
	}
}

// ---------------------------------------------------------------- too many connections

func TestFTPTooManyConnections(t *testing.T) {
	old := ftpBusyWait
	ftpBusyWait = 20 * time.Millisecond
	defer func() { ftpBusyWait = old }()
	data := randData(64 << 10)
	ctx := context.Background()
	for _, busy := range []string{
		"421 There are too many connections from your internet address.",
		"530 Sorry, the maximum number of clients (1) from your host are already connected.",
	} {
		srv := newFTPD(t, func(s *ftpd) { s.files["/f.bin"] = data; s.limit, s.busy = 1, busy })
		first, err := openFTPRange(ctx, &Download{}, srv.url("ftp", "", "/f.bin"), 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = openFTPRange(ctx, &Download{}, srv.url("ftp", "", "/f.bin"), 0)
		var pe permanentErr
		if !errors.Is(err, errNoSlot) || errors.As(err, &pe) {
			t.Fatalf("%q: want a non-permanent errNoSlot, got %v", busy, err)
		}
		first.Close()
	}

	// a wrong password stays a hard error
	srv := newFTPD(t, func(s *ftpd) { s.files["/f.bin"] = data; s.users["bob"] = [2]string{"pw", "/"} })
	_, err := openFTPRange(ctx, &Download{}, srv.url("ftp", "bob:nope", "/f.bin"), 0)
	var pe permanentErr
	if errors.Is(err, errNoSlot) || !errors.As(err, &pe) {
		t.Fatalf("wrong password: want a permanent error, got %v", err)
	}
}

func TestFTPProbeWaitsForASlot(t *testing.T) {
	old := ftpBusyWait
	ftpBusyWait = 20 * time.Millisecond
	defer func() { ftpBusyWait = old }()
	data := randData(10 << 10)
	srv := newFTPD(t, func(s *ftpd) { s.files["/f.bin"] = data; s.busyFirst, s.busy = 2, "421 Too many users, try later" })
	if _, got := readProbe(t, srv.url("ftp", "", "/f.bin")); string(got) != string(data) {
		t.Fatal("wrong data after waiting for a slot")
	}
	if srv.conns != 3 {
		t.Fatalf("expected 2 refused connections then one that works, got %d", srv.conns)
	}

	full := newFTPD(t, func(s *ftpd) { s.files["/f.bin"] = data; s.busyFirst, s.busy = 100, "421 Too many users" })
	_, err := probeFTP(context.Background(), &Download{}, full.url("ftp", "", "/f.bin"), 0)
	var pe permanentErr
	if !errors.As(err, &pe) || !errors.Is(err, errNoSlot) || !strings.Contains(err.Error(), "try again later") {
		t.Fatalf("a server that stays full should end in a clear permanent error, got %v", err)
	}
	if full.conns != 4 {
		t.Fatalf("expected 4 tries, got %d", full.conns)
	}
}

// A server that takes fewer connections than asked for: the download settles on what it takes.
func TestFTPConnectionLimitDownload(t *testing.T) {
	data := randData(3 << 20)
	srv := newFTPD(t, func(s *ftpd) {
		s.files["/pub/file.bin"] = data
		s.rate, s.limit, s.busy = 2<<20, 2, "421 Too many connections (2) from this IP"
	})
	m := newTestManager(t)
	m.settings.Conns = 8
	d := m.Add(AddReq{URL: srv.url("ftp", "", "/pub/file.bin"), Start: true})
	waitFor(t, m, d, StCompleted, 30*time.Second)
	fileIs(t, d, data)
	if srv.peak > 2 || srv.conns <= 2 {
		t.Fatalf("peak %d sessions over %d connections", srv.peak, srv.conns)
	}
}

// ---------------------------------------------------------------- FTPS

func TestFTPSImplicit(t *testing.T) {
	cfg, pool := testCert(t)
	trustFTPTLS(t, pool)
	data := randData(3 << 20)
	srv := newFTPD(t, func(s *ftpd) { s.files["/pub/file.bin"] = data; s.tls, s.implicit, s.rate = cfg, true, 2<<20 })
	m := newTestManager(t)
	m.settings.Conns = 4
	d := m.Add(AddReq{URL: srv.url("ftps", "", "/pub/file.bin"), Start: true})
	waitFor(t, m, d, StCompleted, 20*time.Second)
	fileIs(t, d, data)
	if srv.retrs < 2 || srv.tlsData != srv.retrs || srv.refused+srv.plainData+srv.plainLogins != 0 {
		t.Fatalf("retrs %d, TLS data %d, refused %d, plain data %d, plain logins %d", srv.retrs, srv.tlsData, srv.refused, srv.plainData, srv.plainLogins)
	}
	if !d.Resumable {
		t.Fatal("expected resumable")
	}
}

// AUTH TLS on ftp://, with data connections that must resume the control connection's session.
func TestFTPExplicitTLS(t *testing.T) {
	cfg, pool := testCert(t)
	trustFTPTLS(t, pool)
	data := randData(3 << 20)
	srv := newFTPD(t, func(s *ftpd) {
		s.files["/home/bob/file.bin"] = data
		s.users["bob"] = [2]string{"pw", "/home/bob"}
		s.tls, s.rate = cfg, 2<<20
	})
	m := newTestManager(t)
	m.settings.Conns = 4
	d := m.Add(AddReq{URL: srv.url("ftp", "bob:pw", "/file.bin"), Start: true})
	waitFor(t, m, d, StCompleted, 20*time.Second)
	fileIs(t, d, data)
	if srv.retrs < 2 || srv.tlsData != srv.retrs || srv.refused+srv.plainData+srv.plainLogins != 0 {
		t.Fatalf("retrs %d, TLS data %d, refused %d, plain data %d, plain logins %d", srv.retrs, srv.tlsData, srv.refused, srv.plainData, srv.plainLogins)
	}
	if log := srv.commands(); !slices.Contains(log, "AUTH TLS") || !slices.Contains(log, "PROT P") {
		t.Fatalf("commands: %q", log)
	}
	if n := ftpTLSNote(srv.ln.Addr().String()); n != 0 {
		t.Fatalf("nothing should have fallen back, note %d", n)
	}
}

func TestFTPTLSFallbacks(t *testing.T) {
	_, pool := testCert(t)
	trustFTPTLS(t, pool)
	untrusted, _ := testCert(t) // another certificate, which the client doesn't trust
	data := randData(100 << 10)
	auths := func(s *ftpd) (n int) {
		for _, l := range s.commands() {
			if l == "AUTH TLS" {
				n++
			}
		}
		return n
	}

	// no AUTH TLS on the server: plain, and it isn't asked again
	plain := newFTPD(t, func(s *ftpd) { s.files["/f.bin"] = data })
	for range 2 {
		if _, got := readProbe(t, plain.url("ftp", "", "/f.bin")); string(got) != string(data) {
			t.Fatal("plain server: wrong data")
		}
	}
	if n := ftpTLSNote(plain.ln.Addr().String()); n != ftpPlain || auths(plain) != 1 {
		t.Fatalf("plain server: note %d, %d AUTH TLS", n, auths(plain))
	}

	// a certificate that doesn't verify: plain FTP for that server, remembered
	bad := newFTPD(t, func(s *ftpd) { s.files["/f.bin"] = data; s.tls = untrusted })
	for range 2 {
		if _, got := readProbe(t, bad.url("ftp", "", "/f.bin")); string(got) != string(data) {
			t.Fatal("untrusted certificate: wrong data")
		}
	}
	if n := ftpTLSNote(bad.ln.Addr().String()); n != ftpPlain || auths(bad) != 1 || bad.plainData != 2 {
		t.Fatalf("untrusted certificate: note %d, %d AUTH TLS, %d plain transfers", n, auths(bad), bad.plainData)
	}

	// …but ftps:// asked for TLS: no going plain
	badImplicit := newFTPD(t, func(s *ftpd) { s.files["/f.bin"] = data; s.tls, s.implicit = untrusted, true })
	_, err := probeFTP(context.Background(), &Download{}, badImplicit.url("ftps", "", "/f.bin"), 0)
	var pe permanentErr
	if err == nil || !errors.As(err, &pe) || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("ftps:// with an untrusted certificate: %v", err)
	}

	// TLS on data connections fails: they go in the clear, the login stays encrypted
	cfg, pool2 := testCert(t)
	trustFTPTLS(t, pool2)
	broken := newFTPD(t, func(s *ftpd) { s.files["/f.bin"] = data; s.tls, s.breakData = cfg, true })
	if _, got := readProbe(t, broken.url("ftp", "", "/f.bin")); string(got) != string(data) {
		t.Fatal("broken data TLS: wrong data")
	}
	if n := ftpTLSNote(broken.ln.Addr().String()); n != ftpDataPlain || broken.plainData != 1 || broken.plainLogins != 0 {
		t.Fatalf("broken data TLS: note %d, %d plain transfers, %d plain logins", n, broken.plainData, broken.plainLogins)
	}

	// the data handshake works but the server refuses it for not resuming the session: the
	// next connection sends data in the clear
	fresh := newFTPD(t, func(s *ftpd) { s.files["/f.bin"] = data; s.tls, s.freshData = cfg, true })
	ctx := context.Background()
	pr, err := probeFTP(ctx, &Download{}, fresh.url("ftp", "", "/f.bin"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(pr.body); err == nil || !strings.Contains(err.Error(), "522") {
		t.Fatalf("refused data connection should be an error, got %v", err)
	}
	pr.body.Close()
	if _, got := readProbe(t, fresh.url("ftp", "", "/f.bin")); string(got) != string(data) {
		t.Fatal("after a refused data connection: wrong data")
	}
	if n := ftpTLSNote(fresh.ln.Addr().String()); n != ftpDataPlain || fresh.refused != 1 || fresh.plainData != 1 || fresh.plainLogins != 0 {
		t.Fatalf("refused data connection: note %d, refused %d, plain transfers %d, plain logins %d", n, fresh.refused, fresh.plainData, fresh.plainLogins)
	}

	// anonymous logins refused over TLS (vsftpd's default): plain
	noAnon := newFTPD(t, func(s *ftpd) { s.files["/f.bin"] = data; s.tls, s.noAnonTLS = cfg, true })
	if _, got := readProbe(t, noAnon.url("ftp", "", "/f.bin")); string(got) != string(data) {
		t.Fatal("no anonymous TLS: wrong data")
	}
	if n := ftpTLSNote(noAnon.ln.Addr().String()); n != ftpPlain {
		t.Fatalf("no anonymous TLS: note %d", n)
	}
}

// A transfer the server cuts short isn't taken for the end of the file.
func TestFTPAbortedTransfer(t *testing.T) {
	data := randData(256 << 10)
	srv := newFTPD(t, func(s *ftpd) { s.files["/f.bin"] = data; s.abortAt = 100 << 10 })
	pr, err := probeFTP(context.Background(), &Download{}, srv.url("ftp", "", "/f.bin"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer pr.body.Close()
	got, err := io.ReadAll(pr.body)
	if len(got) != 100<<10 || err == nil || !strings.Contains(err.Error(), "426") {
		t.Fatalf("got %d bytes and %v", len(got), err)
	}
}

func TestIsFTP(t *testing.T) {
	for u, want := range map[string]bool{"ftp://h/f": true, "FTPS://h/f": true, "ftpx://h/f": false, "https://h/f": false} {
		if isFTP(u) != want {
			t.Errorf("isFTP(%q) = %v", u, !want)
		}
	}
}
