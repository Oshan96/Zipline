package main

import (
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
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const errURL = "https://files.example.com/big.iso"

// osCode picks the Unix or the Windows code for the same failure.
func osCode(unix, win syscall.Errno) syscall.Errno {
	if runtime.GOOS == "windows" {
		return win
	}
	return unix
}

// netErr builds a failed request the way net/http and net report one.
func netErr(op, call string, code syscall.Errno) error {
	addr := &net.TCPAddr{IP: net.IPv4(203, 0, 113, 7), Port: 443}
	return &url.Error{Op: "Get", URL: errURL, Err: &net.OpError{Op: op, Net: "tcp", Addr: addr, Err: os.NewSyscallError(call, code)}}
}

// noJargon fails if a message still shows Go's framing.
func noJargon(t *testing.T, msg string) {
	t.Helper()
	for _, bad := range []string{"dial tcp", "read tcp", "connectex", "wsarecv", "Get \"", "203.0.113.7", "syscall", "errno"} {
		if strings.Contains(msg, bad) {
			t.Errorf("%q shows %q", msg, bad)
		}
	}
}

func TestFriendlyErrorStatus(t *testing.T) {
	for code, want := range map[int]string{
		400: "the server didn't accept the request (400)",
		403: "the server refused access (403)",
		404: "the file isn't on the server (404)",
		410: "the file has been removed from the server (410)",
		416: "the server couldn't send the rest of the file (416)",
		429: "the server says too many requests — waiting a while helps",
		500: "the server had an internal error (500)",
		502: "the server is having trouble (502)",
		503: "the server is busy or down for maintenance (503)",
		504: "the server took too long to respond (504)",
		418: "the server replied 418 I'm a teapot",
	} {
		se := statusErr{code: code, status: fmt.Sprintf("%d %s", code, http.StatusText(code))}
		for _, err := range []error{se, permanentErr{se}, fmt.Errorf("probe: %w", se)} {
			if got := friendlyError(err, errURL); !strings.HasPrefix(got, want) {
				t.Errorf("%d: got %q, want %q…", code, got, want)
			}
		}
		retry := code == 429 || code >= 500
		if isTransient(se) != retry || isTransient(permanentErr{se}) || isNetworkDown(se) {
			t.Errorf("%d: transient=%v (want %v), permanent=%v, down=%v", code, isTransient(se), retry, isTransient(permanentErr{se}), isNetworkDown(se))
		}
	}
	for _, code := range []int{408, 425} {
		if !isTransient(statusErr{code: code}) {
			t.Errorf("%d should be retried", code)
		}
	}
}

// Every OS error code, built as Go reports it (the Windows ones only run on Windows).
func TestFriendlyErrorCodes(t *testing.T) {
	file := filepath.Join(t.TempDir(), "big.iso.part")
	fileErr := func(op string, code syscall.Errno) error {
		return permanentErr{&os.PathError{Op: op, Path: file, Err: code}}
	}
	cases := []struct {
		name      string
		err       error
		want      string
		transient bool
		down      bool
	}{
		{"refused", netErr("dial", "connect", osCode(syscall.ECONNREFUSED, 10061)), "couldn't connect to files.example.com (connection refused) — try again later", true, false},
		{"reset", netErr("read", "read", osCode(syscall.ECONNRESET, 10054)), "files.example.com closed the connection — try again later", true, false},
		{"aborted", netErr("read", "read", osCode(syscall.ECONNABORTED, 10053)), "the connection to files.example.com was cut off on this computer — a firewall, antivirus or VPN may be blocking it", true, false},
		{"timed out", netErr("dial", "connect", osCode(syscall.ETIMEDOUT, 10060)), "files.example.com didn't answer in time — check your internet connection", true, false},
		{"stalled", netErr("read", "read", osCode(syscall.ETIMEDOUT, 10060)), "files.example.com stopped sending data — check your internet connection", true, false},
		{"net unreachable", netErr("dial", "connect", osCode(syscall.ENETUNREACH, 10051)), "no network connection — check your internet connection", true, true},
		{"net down", netErr("dial", "connect", osCode(syscall.ENETDOWN, 10050)), "no network connection — check your internet connection", true, true},
		{"host unreachable", netErr("dial", "connect", osCode(syscall.EHOSTUNREACH, 10065)), "couldn't reach files.example.com — check your internet connection", true, true},
		{"firewall", netErr("dial", "connect", osCode(syscall.EPERM, 10013)), "the connection to files.example.com was blocked on this computer — a firewall or security program may be stopping Zipline", false, false},
		{"disk full", fileErr("write", osCode(syscall.ENOSPC, 112)), "the disk is full — free up space or choose another folder", false, false},
		{"quota", fileErr("write", osCode(syscall.EDQUOT, 1295)), "your disk quota is used up — free up space or choose another folder", false, false},
		{"denied", fileErr("open", osCode(syscall.EACCES, 5)), "Zipline isn't allowed to save to " + file + " — choose another folder", false, false},
		{"read-only", fileErr("open", osCode(syscall.EROFS, 19)), "the disk is read-only — choose another folder", false, false},
		{"in use", fileErr("open", osCode(syscall.ETXTBSY, 32)), "couldn't use " + file + " — the file may be open in another program", false, false},
		{"path too long", fileErr("open", osCode(syscall.ENAMETOOLONG, 206)), "the file's path is too long — choose a shorter folder or file name", false, false},
		{"no folder", fileErr("open", osCode(syscall.ENOENT, 3)), "couldn't find " + file + " — check that the folder exists and its drive is connected", false, false},
		{"too many files", fileErr("open", osCode(syscall.EMFILE, 4)), "too many files are open at once — pause some downloads and try again", false, false},
		{"rename in use", &os.LinkError{Op: "rename", Old: file, New: file + ".done", Err: osCode(syscall.EBUSY, 32)}, "couldn't use " + file + ".done — the file may be open in another program", false, false},
	}
	if runtime.GOOS == "windows" {
		dns := func(code syscall.Errno) error { // Go keeps a lookup's cause only as text
			return &url.Error{Op: "Get", URL: errURL, Err: &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{
				Err: os.NewSyscallError("getaddrinfow", code).Error(), Name: "files.example.com"}}}
		}
		cases = append(cases, []struct {
			name      string
			err       error
			want      string
			transient bool
			down      bool
		}{
			{"lock", fileErr("write", 33), "couldn't use " + file + " — the file may be open in another program", false, false},
			{"handle disk full", fileErr("write", 39), "the disk is full — free up space or choose another folder", false, false},
			{"path not found", fileErr("open", 3), "couldn't find " + file + " — check that the folder exists and its drive is connected", false, false},
			{"mapped", fileErr("open", 1224), file + " is in use by another program (often antivirus) — try again in a moment", false, false},
			{"virus", fileErr("open", 225), "your antivirus blocked " + file, false, false},
			{"name dropped", netErr("read", "wsarecv", 64), "files.example.com closed the connection — try again later", true, false},
			{"semaphore timeout", netErr("dial", "connectex", 121), "files.example.com didn't answer in time — check your internet connection", true, false},
			{"try again", dns(11002), "couldn't look up files.example.com right now — check your internet connection", true, true},
			{"host not found", dns(11001), "couldn't find files.example.com — check the link or your internet connection", true, false},
		}...)
	}
	for _, c := range cases {
		got := friendlyError(c.err, errURL)
		if got != c.want {
			t.Errorf("%s: got %q\nwant %q", c.name, got, c.want)
		}
		noJargon(t, got)
		if isTransient(c.err) != c.transient || isNetworkDown(c.err) != c.down {
			t.Errorf("%s: transient=%v down=%v, want %v %v", c.name, isTransient(c.err), isNetworkDown(c.err), c.transient, c.down)
		}
		// the engine passes disk errors on as text ("disk: " cut off): the code is still found
		if _, ok := c.err.(permanentErr); ok {
			flat := errors.New(strings.TrimPrefix(fmt.Sprintf("disk: %v", c.err), "disk: "))
			if got := friendlyError(flat, errURL); got != c.want {
				t.Errorf("%s as text: got %q\nwant %q", c.name, got, c.want)
			}
		}
	}
}

func TestFriendlyErrorDNS(t *testing.T) {
	cases := []struct {
		err  *net.DNSError
		want string
		down bool
	}{
		{&net.DNSError{Err: "no such host", Name: "files.example.com", IsNotFound: true}, "couldn't find files.example.com — check the link or your internet connection", false},
		{&net.DNSError{Err: "i/o timeout", Name: "files.example.com", IsTimeout: true}, "couldn't look up files.example.com right now — check your internet connection", true},
		{&net.DNSError{Err: "server misbehaving", Name: "files.example.com", IsTemporary: true}, "couldn't look up files.example.com (its name server reported a problem) — try again later", false},
	}
	if runtime.GOOS != "windows" {
		// the pure-Go resolver when the computer is offline
		cases = append(cases, struct {
			err  *net.DNSError
			want string
			down bool
		}{&net.DNSError{Err: "dial udp 8.8.8.8:53: connect: " + syscall.ENETUNREACH.Error(), Name: "files.example.com", IsTemporary: true}, "no network connection — check your internet connection", true})
	}
	for _, c := range cases {
		err := &url.Error{Op: "Get", URL: errURL, Err: &net.OpError{Op: "dial", Net: "tcp", Err: c.err}}
		if got := friendlyError(err, errURL); got != c.want {
			t.Errorf("%v: got %q, want %q", c.err, got, c.want)
		}
		if isNetworkDown(err) != c.down || !isTransient(err) {
			t.Errorf("%v: down=%v transient=%v", c.err, isNetworkDown(err), isTransient(err))
		}
	}
	// a real lookup of a name that can't exist
	_, err := net.DefaultResolver.LookupHost(context.Background(), "nothing.invalid")
	var dns *net.DNSError
	if !errors.As(err, &dns) || !dns.IsNotFound {
		t.Logf("no NXDOMAIN for .invalid here (%v); skipping the real lookup", err)
		return
	}
	if got := friendlyError(err, "http://nothing.invalid/a.zip"); got != "couldn't find nothing.invalid — check the link or your internet connection" {
		t.Errorf("real lookup: %q", got)
	}
}

// directClient talks to local test servers without the sandbox's or the user's proxy.
func directClient(cfg *tls.Config) *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 10 * time.Second}
}

func TestFriendlyErrorRealNetwork(t *testing.T) {
	// nothing listening
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + ln.Addr().String() + "/f.bin"
	ln.Close()
	_, err = directClient(nil).Get(closed)
	if got, want := friendlyError(err, closed), "couldn't connect to "+ln.Addr().String()+" (connection refused) — try again later"; got != want {
		t.Errorf("refused: %q (from %v)", got, err)
	}
	if !isTransient(err) || isNetworkDown(err) {
		t.Errorf("refused: transient=%v down=%v", isTransient(err), isNetworkDown(err))
	}

	srv := tlsServer(t, nil)
	trusted := x509.NewCertPool()
	trusted.AddCert(srv.Certificate())
	host := strings.TrimPrefix(srv.URL, "https://")
	check := func(name string, cfg *tls.Config, rawURL, want string) {
		t.Helper()
		_, err := directClient(cfg).Get(rawURL)
		got := friendlyError(err, rawURL)
		if got != want {
			t.Errorf("%s: got %q\nwant %q (from %v)", name, got, want, err)
		}
		if isTransient(err) {
			t.Errorf("%s: shouldn't be retried", name)
		}
	}
	check("untrusted", &tls.Config{RootCAs: x509.NewCertPool()}, srv.URL,
		"the secure connection to "+host+" failed (its certificate isn't trusted)")
	check("wrong name", &tls.Config{RootCAs: trusted, ServerName: "other.test"}, srv.URL,
		"the secure connection to "+host+" failed (its certificate is for a different site)")
	old := tls12Server(t)
	check("TLS version", &tls.Config{RootCAs: trusted, MinVersion: tls.VersionTLS13}, old,
		"the secure connection to "+strings.TrimPrefix(old, "https://")+" failed (the server turned it down)")

	exp, pool := expiredServer(t)
	check("expired", &tls.Config{RootCAs: pool}, exp,
		"the secure connection to "+strings.TrimPrefix(exp, "https://")+" failed (its certificate has expired or isn't valid yet) — check your computer's date and time")

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer plain.Close()
	plainHost := strings.TrimPrefix(plain.URL, "http://")
	check("https to http", nil, "https://"+plainHost+"/f.bin",
		plainHost+" doesn't offer a secure connection — try the link with http:// instead")

	// the server stops answering
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer slow.Close()
	c := directClient(nil)
	c.Timeout = 200 * time.Millisecond
	_, err = c.Get(slow.URL)
	if got := friendlyError(err, slow.URL); got != strings.TrimPrefix(slow.URL, "http://")+" didn't answer in time — check your internet connection" || !isTransient(err) {
		t.Errorf("timeout: %q (from %v)", got, err)
	}

	// the server hangs up part-way through the body
	short := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.Write([]byte("only a bit"))
	}))
	defer short.Close()
	resp, err := directClient(nil).Get(short.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if got := friendlyError(err, short.URL); got != "the connection to "+strings.TrimPrefix(short.URL, "http://")+" closed early — try again" || !isTransient(err) {
		t.Errorf("short body: %q (from %v)", got, err)
	}
}

// tlsServer is an https test server that keeps the handshake failures we cause out of the log.
func tlsServer(t *testing.T, cfg *tls.Config) *httptest.Server {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.TLS = cfg
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// tls12Server only speaks TLS 1.2, so a client that insists on 1.3 gets an alert.
func tls12Server(t *testing.T) string {
	return tlsServer(t, &tls.Config{MaxVersion: tls.VersionTLS12}).URL
}

// expiredServer serves https with a self-signed certificate that ran out yesterday.
func expiredServer(t *testing.T) (string, *x509.CertPool) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-72 * time.Hour),
		NotAfter:              time.Now().Add(-24 * time.Hour),
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	srv := tlsServer(t, &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})
	return srv.URL, pool
}

func TestFriendlyErrorProxy(t *testing.T) {
	defer setProxySettings(ProxySettings{Mode: "system"})

	// the proxy isn't there
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gone := "http://" + ln.Addr().String()
	ln.Close()
	setProxySettings(ProxySettings{Mode: "manual", URL: gone})
	pu, _ := url.Parse(gone)
	tr := &http.Transport{Proxy: http.ProxyURL(pu)}
	_, err = (&http.Client{Transport: tr}).Get("http://files.test/f.bin")
	if got, want := friendlyError(err, "http://files.test/f.bin"), "couldn't reach the proxy "+ln.Addr().String()+" — check Options → Proxy"; got != want {
		t.Errorf("proxy down: %q (from %v)", got, err)
	}

	// the proxy won't open an https tunnel
	var code atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(int(code.Load())) }))
	defer proxy.Close()
	pu, _ = url.Parse(proxy.URL)
	tr = &http.Transport{Proxy: http.ProxyURL(pu)}
	for c, want := range map[int]string{
		407: "the proxy needs a user name and password (Options → Proxy)",
		502: "the proxy wouldn't connect to files.test (it replied 502 Bad Gateway) — check Options → Proxy",
	} {
		code.Store(int32(c))
		_, err = (&http.Client{Transport: tr}).Get("https://files.test/f.bin")
		if got := friendlyError(err, "https://files.test/f.bin"); got != want {
			t.Errorf("CONNECT %d: %q (from %v)", c, got, err)
		}
		tr.CloseIdleConnections()
	}
}

func TestFriendlyErrorFiles(t *testing.T) {
	if runtime.GOOS == "linux" { // a real "disk full"
		if f, err := os.OpenFile("/dev/full", os.O_WRONLY, 0); err == nil {
			_, err = f.Write([]byte("x"))
			f.Close()
			if got := friendlyError(err, errURL); got != "the disk is full — free up space or choose another folder" {
				t.Errorf("/dev/full: %q (from %v)", got, err)
			}
		}
	}
	// a real read-only folder (root and Windows ignore the permission)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	p := filepath.Join(dir, "f.part")
	if f, err := os.Create(p); err == nil {
		f.Close()
		t.Log("the folder stayed writable here; the code tests cover permissions")
	} else if got := friendlyError(err, errURL); got != "Zipline isn't allowed to save to "+p+" — choose another folder" {
		t.Errorf("read-only folder: %q (from %v)", got, err)
	}
}

func TestFriendlyErrorPlain(t *testing.T) {
	addr := &net.TCPAddr{IP: net.IPv4(203, 0, 113, 7), Port: 443}
	odd := errors.New("something odd happened")
	for _, c := range []struct {
		err  error
		want string
	}{
		// the engine's own messages pass straight through
		{permanentErr{errors.New("the site needs a user name and password (save one in Options → Sites)")}, "the site needs a user name and password (save one in Options → Sites)"},
		{permanentErr{errors.New("OneDrive didn't hand out the file for this link")}, "OneDrive didn't hand out the file for this link"},
		{permanentErr{errors.New("FTP server doesn't support resuming (REST)")}, "FTP server doesn't support resuming (REST)"},
		{permanentErr{fmt.Errorf("file not found on FTP server (%s)", "550 /pub/x: no such file")}, "file not found on FTP server (550 /pub/x: no such file)"},
		{errLinkExpired, "the download link has expired"},
		// Go's framing comes off
		{&url.Error{Op: "Get", URL: errURL, Err: &net.OpError{Op: "dial", Net: "tcp", Addr: addr, Err: odd}}, "something odd happened"},
		{errors.New(`Get "https://files.example.com/big.iso": dial tcp 203.0.113.7:443: something odd happened`), "something odd happened"},
		{errors.New(`read tcp 10.0.0.2:51000->203.0.113.7:443: something odd happened`), "something odd happened"},
		{errors.New(`Get "https://files.example.com/big.iso": net/http: request canceled while waiting`), "request canceled while waiting"},
		{&url.Error{Op: "Get", URL: errURL, Err: errors.New("tls: server selected unsupported protocol version 301")}, "the secure connection to files.example.com failed (server selected unsupported protocol version 301)"},
		{&url.Error{Op: "Get", URL: errURL, Err: io.EOF}, "files.example.com closed the connection — try again later"},
		{&url.Error{Op: "Get", URL: errURL, Err: context.Canceled}, "the download was paused"},
		{&url.Error{Op: "Get", URL: errURL, Err: context.DeadlineExceeded}, "files.example.com didn't answer in time — check your internet connection"},
		{&net.OpError{Op: "read", Net: "tcp", Addr: addr, Err: os.ErrDeadlineExceeded}, "files.example.com stopped sending data — check your internet connection"},
		{tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}, "files.example.com doesn't offer a secure connection — try the link with http:// instead"},
		{&net.OpError{Op: "remote error", Err: tls.AlertError(40)}, "the secure connection to files.example.com failed (the server turned it down)"},
		{&net.OpError{Op: "socks connect", Net: "tcp", Addr: addr, Err: odd}, "the proxy couldn't connect to files.example.com — check Options → Proxy"},
	} {
		got := friendlyError(c.err, errURL)
		if got != c.want {
			t.Errorf("%v:\n got %q\nwant %q", c.err, got, c.want)
		}
		noJargon(t, got)
	}
	if friendlyError(nil, errURL) != "" || friendlyError(odd, "") != "something odd happened" {
		t.Error("nil error or empty URL")
	}
	if got := friendlyError(netErr("dial", "connect", osCode(syscall.ECONNREFUSED, 10061)), "not a url"); !strings.HasPrefix(got, "couldn't connect to the server") {
		t.Errorf("no host: %q", got)
	}
	// an OS message we don't know: the code's own words, in our style
	unknown := osCode(syscall.ENOTSUP, 1392) // ERROR_FILE_CORRUPT on Windows
	got := friendlyError(netErr("read", "wsarecv", unknown), errURL)
	if want := lowerFirst(strings.TrimSuffix(unknown.Error(), ".")); got != want || strings.HasSuffix(got, ".") {
		t.Errorf("unknown code: %q, want %q", got, want)
	}
	if lowerFirst("The device is not ready") != "the device is not ready" || lowerFirst("A connection attempt failed") != "a connection attempt failed" ||
		lowerFirst("OneDrive x") != "OneDrive x" || lowerFirst("DRM y") != "DRM y" {
		t.Error("lowerFirst")
	}
	// retry decisions for errors without an OS code
	if !isTransient(&url.Error{Op: "Get", URL: errURL, Err: io.ErrUnexpectedEOF}) || isTransient(odd) ||
		isTransient(&url.Error{Op: "Get", URL: errURL, Err: context.Canceled}) || isTransient(permanentErr{netErr("dial", "connect", osCode(syscall.ECONNREFUSED, 10061))}) ||
		isTransient(x509.UnknownAuthorityError{}) || isNetworkDown(odd) || isNetworkDown(nil) || isTransient(nil) {
		t.Error("retry decisions")
	}
}
