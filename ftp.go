package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A small FTP client: just enough for fast, resumable, multi-connection downloads.
// Each connection logs in on its own control channel, walks to the file's folder with CWD,
// seeks with REST and streams with RETR. ftps:// is TLS from the first byte (implicit FTPS);
// ftp:// switches to TLS with AUTH TLS when the server offers it and stays plain otherwise.

const ftpCmdTimeout = 30 * time.Second

// ftpBusyWait spaces out the retries of a first login the server turns away as full.
var ftpBusyWait = time.Second

// ftpTLSConfig gives the TLS settings for a server (tests trust their own certificate here).
var ftpTLSConfig = func(host string) *tls.Config { return &tls.Config{ServerName: host} }

// errFTPRetryPlain: TLS turned out not to work with this server. That's noted for the
// session, so connecting again goes without it.
var errFTPRetryPlain = errors.New("FTP over TLS didn't work with this server")

type ftpConn struct {
	c        net.Conn // the control connection (TLS once secured)
	raw      net.Conn // the TCP connection under it
	r        *bufio.Reader
	key      string   // host:port, for what TLS did on this server
	dataHost string   // where data connections go
	proxy    *url.URL // SOCKS5 proxy for every connection, nil = direct
	tlsCfg   *tls.Config
	prot     bool   // data connections are encrypted too (PROT P)
	dir      string // the folders walked into from where the login landed (see target)
}

func isFTP(u string) bool {
	l := strings.ToLower(u)
	return strings.HasPrefix(l, "ftp://") || strings.HasPrefix(l, "ftps://")
}

// ftpCreds: URL user info, then a Basic Authorization header from the Add dialog, else anonymous.
func ftpCreds(u *url.URL, d *Download) (string, string) {
	if u.User != nil {
		p, _ := u.User.Password()
		return u.User.Username(), usableSecret(p)
	}
	if a := d.Headers["Authorization"]; strings.HasPrefix(a, "Basic ") {
		if b, err := base64.StdEncoding.DecodeString(a[6:]); err == nil {
			if i := strings.Index(string(b), ":"); i >= 0 {
				return string(b[:i]), string(b[i+1:])
			}
		}
	}
	if user, pass, ok := siteLogin(u); ok { // saved login (Options → Sites)
		return user, pass
	}
	return "anonymous", "zipline@example.com"
}

// What TLS did on each server (host:port) this session, so later connections skip what failed.
const (
	ftpDataPlain = 1 // PROT P refused or its handshake failed: data connections in the clear
	ftpPlain     = 2 // ftp:// only: no AUTH TLS, or its handshake failed (certificate): plain FTP
)

var ftpTLSNotes = struct {
	sync.Mutex
	m map[string]int
}{m: map[string]int{}}

func ftpTLSNote(key string) int {
	ftpTLSNotes.Lock()
	defer ftpTLSNotes.Unlock()
	return ftpTLSNotes.m[key]
}

func noteFTPTLS(key string, n int) {
	ftpTLSNotes.Lock()
	ftpTLSNotes.m[key] = max(ftpTLSNotes.m[key], n)
	ftpTLSNotes.Unlock()
}

// ftpSession keeps one control connection's TLS session for its data connections to resume:
// strict servers (vsftpd's require_ssl_reuse) refuse data connections that don't. Each control
// connection has its own, as a server only knows the sessions of the connection they came from.
type ftpSession struct {
	mu sync.Mutex
	cs *tls.ClientSessionState
}

func (s *ftpSession) Get(string) (*tls.ClientSessionState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cs, s.cs != nil
}

func (s *ftpSession) Put(_ string, cs *tls.ClientSessionState) {
	s.mu.Lock()
	s.cs = cs
	s.mu.Unlock()
}

// ftpDial connects and logs in. When TLS turns out not to work on an ftp:// server, it's
// noted and the login done again in plain text.
func ftpDial(ctx context.Context, u *url.URL, d *Download) (*ftpConn, error) {
	fc, err := ftpLogin(ctx, u, d)
	if errors.Is(err, errFTPRetryPlain) {
		fc, err = ftpLogin(ctx, u, d)
	}
	return fc, err
}

func ftpLogin(ctx context.Context, u *url.URL, d *Download) (*ftpConn, error) {
	implicit := strings.EqualFold(u.Scheme, "ftps")
	port := u.Port()
	if port == "" {
		port = "21"
		if implicit {
			port = "990"
		}
	}
	fc := &ftpConn{key: net.JoinHostPort(u.Hostname(), port)}
	pu, err := proxyFor(u)
	if err != nil {
		return nil, permanentErr{err}
	}
	// HTTP proxies aren't used for FTP (it goes direct): they'd have to CONNECT to every
	// passive data port, which they rarely allow. SOCKS5 tunnels both kinds of connection.
	if pu != nil && (pu.Scheme == "socks5" || pu.Scheme == "socks5h") {
		fc.proxy = pu
	}
	c, err := fc.dial(ctx, fc.key)
	if err != nil {
		return nil, err
	}
	fc.c, fc.raw, fc.r = c, c, bufio.NewReader(c)
	if sc, ok := c.(*socksConn); ok {
		fc.dataHost = sc.host
	} else {
		fc.dataHost, _, _ = net.SplitHostPort(c.RemoteAddr().String())
	}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	fail := func(err error) (*ftpConn, error) { c.Close(); return nil, err }

	note := ftpTLSNote(fc.key)
	if implicit {
		if err := fc.secure(ctx, u.Hostname()); err != nil {
			var ve *tls.CertificateVerificationError
			if errors.As(err, &ve) {
				err = permanentErr{err}
			}
			return fail(err)
		}
	}
	code, msg, err := fc.read()
	if err != nil {
		return fail(err)
	}
	if code != 220 {
		return fail(ftpLoginErr(code, msg))
	}
	if !implicit && note < ftpPlain {
		// Like FileZilla's "use explicit FTP over TLS if available": encrypt when the server can.
		code, msg, err := fc.cmd("AUTH TLS")
		switch {
		case err != nil:
			return fail(err)
		case code == 234:
			if err := fc.secure(ctx, u.Hostname()); err != nil {
				if ctx.Err() != nil || isTimeout(err) {
					return fail(err)
				}
				noteFTPTLS(fc.key, ftpPlain) // e.g. a self-signed certificate: plain, as for any ftp:// server
				return fail(fmt.Errorf("%w: %v", errFTPRetryPlain, err))
			}
		case code == 421:
			return fail(ftpLoginErr(code, msg))
		default:
			noteFTPTLS(fc.key, ftpPlain)
		}
	}
	secured := fc.tlsCfg != nil
	user, pass := ftpCreds(u, d)
	if strings.ContainsAny(user+pass, "\r\n") {
		return fail(permanentErr{errors.New("bad FTP user name or password")})
	}
	code, msg, err = fc.cmd("USER " + user)
	if err != nil {
		return fail(err)
	}
	if code == 331 {
		code, msg, err = fc.cmd("PASS " + pass)
		if err != nil {
			return fail(err)
		}
	}
	if code != 230 && code != 202 {
		anon := strings.EqualFold(user, "anonymous") || strings.EqualFold(user, "ftp")
		if secured && !implicit && anon && code == 530 && !ftpFull(msg) {
			// Some servers refuse anonymous logins over TLS (vsftpd's allow_anon_ssl=NO).
			noteFTPTLS(fc.key, ftpPlain)
			return fail(fmt.Errorf("%w: %d %s", errFTPRetryPlain, code, msg))
		}
		return fail(ftpLoginErr(code, msg))
	}
	if secured && note < ftpDataPlain {
		if code, _, err := fc.cmd("PBSZ 0"); err != nil {
			return fail(err)
		} else if code == 200 {
			if code, _, err = fc.cmd("PROT P"); err != nil {
				return fail(err)
			}
			fc.prot = code == 200
		}
		if !fc.prot {
			noteFTPTLS(fc.key, ftpDataPlain)
		}
	}
	if code, msg, err = fc.cmd("TYPE I"); err != nil {
		return fail(err)
	} else if code != 200 {
		return fail(ftpErr(code, msg))
	}
	return fc, nil
}

// dial opens a connection to the server: through the SOCKS proxy when there is one, else
// directly (data connections from the same adapter as their control connection).
func (fc *ftpConn) dial(ctx context.Context, addr string) (net.Conn, error) {
	switch {
	case fc.proxy != nil:
		return dialSocks(ctx, fc.proxy, addr)
	case fc.raw != nil:
		return dialFrom(ctx, "tcp", addr, fc.raw)
	}
	return dialNet(ctx, "tcp", addr)
}

// secure switches the control connection to TLS, verifying the certificate as usual.
func (fc *ftpConn) secure(ctx context.Context, host string) error {
	cfg := ftpTLSConfig(host).Clone()
	if cfg.ServerName == "" {
		cfg.ServerName = host
	}
	cfg.ClientSessionCache = &ftpSession{} // data connections use cfg too, so they resume this session
	tc := tls.Client(fc.raw, cfg)
	tc.SetDeadline(time.Now().Add(ftpCmdTimeout))
	if err := tc.HandshakeContext(ctx); err != nil {
		return err
	}
	fc.c, fc.r, fc.tlsCfg = tc, bufio.NewReader(tc), cfg
	return nil
}

func ftpErr(code int, msg string) error {
	e := fmt.Errorf("FTP server replied %d %s", code, strings.TrimSpace(msg))
	if code == 530 { // login refused: don't hammer the server
		return permanentErr{e}
	}
	return e
}

// ftpLoginErr explains a refused greeting or login. "Too many connections" (421, or a 530
// that says so) is errNoSlot to the engine: the download carries on with fewer connections.
func ftpLoginErr(code int, msg string) error {
	if code == 421 || code == 530 && ftpFull(msg) {
		return fmt.Errorf("%w (FTP server replied %d %s)", errNoSlot, code, strings.TrimSpace(msg))
	}
	return ftpErr(code, msg)
}

// ftpFull: a 530 that turns the login away for the number of users, not for the password
// ("Sorry, the maximum number of clients (3) from your host are already connected").
func ftpFull(msg string) bool {
	m := strings.ToLower(msg)
	for _, s := range []string{"too many", "maximum", "no more", "limit", "already connected", "try later", "try again later"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

func (fc *ftpConn) read() (int, string, error) { return fc.readFor(ftpCmdTimeout) }

func (fc *ftpConn) readFor(wait time.Duration) (int, string, error) {
	fc.c.SetReadDeadline(time.Now().Add(wait))
	line, err := fc.r.ReadString('\n')
	if err != nil {
		return 0, "", err
	}
	if len(line) < 4 {
		return 0, "", errors.New("bad FTP reply")
	}
	code, err := strconv.Atoi(line[:3])
	if err != nil {
		return 0, "", errors.New("bad FTP reply")
	}
	msg := strings.TrimRight(line[4:], "\r\n")
	if line[3] == '-' { // multi-line reply ends with "code "
		for {
			l, err := fc.r.ReadString('\n')
			if err != nil {
				return 0, "", err
			}
			if strings.HasPrefix(l, line[:3]+" ") {
				break
			}
		}
	}
	return code, msg, nil
}

func (fc *ftpConn) cmd(line string) (int, string, error) {
	fc.c.SetWriteDeadline(time.Now().Add(ftpCmdTimeout))
	if _, err := io.WriteString(fc.c, line+"\r\n"); err != nil {
		return 0, "", err
	}
	return fc.read()
}

func (fc *ftpConn) Close() error { return fc.c.Close() }

// passive opens a data connection (EPSV, falling back to PASV). The control connection's
// server is always used, which avoids broken NAT addresses in PASV replies.
func (fc *ftpConn) passive(ctx context.Context) (net.Conn, error) {
	port := 0
	if code, msg, err := fc.cmd("EPSV"); err != nil {
		return nil, err
	} else if code == 229 {
		if i, j := strings.Index(msg, "(|||"), strings.LastIndex(msg, "|)"); i >= 0 && j > i+4 {
			port, _ = strconv.Atoi(msg[i+4 : j])
		}
	}
	if port == 0 {
		code, msg, err := fc.cmd("PASV")
		if err != nil {
			return nil, err
		}
		if code != 227 {
			return nil, ftpErr(code, msg)
		}
		i, j := strings.Index(msg, "("), strings.Index(msg, ")")
		if i < 0 || j < i {
			return nil, errors.New("bad PASV reply")
		}
		parts := strings.Split(msg[i+1:j], ",")
		if len(parts) != 6 {
			return nil, errors.New("bad PASV reply")
		}
		p1, _ := strconv.Atoi(strings.TrimSpace(parts[4]))
		p2, _ := strconv.Atoi(strings.TrimSpace(parts[5]))
		port = p1*256 + p2
	}
	c, err := fc.dial(ctx, net.JoinHostPort(fc.dataHost, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	if fc.prot {
		// Same config as the control connection: same server name and session cache, so
		// it resumes that session. The handshake starts at the first read, once the
		// server has answered the transfer command.
		return tls.Client(c, fc.tlsCfg), nil
	}
	return c, nil
}

func (fc *ftpConn) size(p string) int64 {
	code, msg, err := fc.cmd("SIZE " + p)
	if err != nil || code != 213 {
		return -1
	}
	n, err := strconv.ParseInt(strings.TrimSpace(msg), 10, 64)
	if err != nil {
		return -1
	}
	return n
}

func (fc *ftpConn) mdtm(p string) string {
	code, msg, err := fc.cmd("MDTM " + p)
	if err != nil || code != 213 {
		return ""
	}
	return strings.TrimSpace(msg)
}

// ftpSplit reads a URL path as RFC 1738 means it: folders relative to where the login lands,
// with a leading %2F (or a doubled slash) starting from the root, as curl does.
func ftpSplit(u *url.URL) (dirs []string, name string, err error) {
	p := strings.TrimPrefix(u.Path, "/")
	if strings.ContainsAny(p, "\r\n") {
		return nil, "", permanentErr{errors.New("bad FTP link")}
	}
	if strings.HasPrefix(p, "/") {
		dirs = append(dirs, "/")
	}
	parts := strings.Split(p, "/")
	for _, s := range parts[:len(parts)-1] {
		if s != "" { // CWD needs a folder name; servers ignore empty steps anyway
			dirs = append(dirs, s)
		}
	}
	return dirs, parts[len(parts)-1], nil
}

// target walks into the file's folder one CWD at a time (curl's default, which servers of
// every kind accept) and returns the name to ask for there. A server that refuses the walk
// gets the whole path from the root instead, as Zipline used to send it.
func (fc *ftpConn) target(u *url.URL) (string, error) {
	dirs, name, err := ftpSplit(u)
	if err != nil {
		return "", err
	}
	want := strings.Join(dirs, "/")
	if fc.dir == want {
		return name, nil // already there
	}
	rooted := len(dirs) > 0 && dirs[0] == "/"
	rel := dirs
	if rooted {
		rel = dirs[1:]
	}
	full := "/" + strings.Join(append(rel[:len(rel):len(rel)], name), "/")
	if fc.dir != "" && !rooted {
		return full, nil // somewhere else already, and the way back to the login folder is unknown
	}
	fc.dir = "\n" // part-way until the walk completes
	for _, d := range dirs {
		code, _, err := fc.cmd("CWD " + d)
		if err != nil {
			return "", err
		}
		if code != 250 && code != 200 {
			return full, nil
		}
	}
	fc.dir = want
	return name, nil
}

type ftpReader struct {
	data   net.Conn
	fc     *ftpConn
	stop   func() bool
	restOK bool  // the server honoured REST
	size   int64 // from SIZE when asked, else -1
	mod    string
	got    int64
	end    error // set once the data connection has ended
}

func (r *ftpReader) Read(p []byte) (int, error) {
	if r.end != nil {
		return 0, r.end
	}
	n, err := r.data.Read(p)
	r.got += int64(n)
	if err == io.EOF {
		err = r.finish()
		r.end = err
	}
	return n, err
}

// finish reads the server's word on a transfer whose data connection has ended: one it cut
// short mustn't pass for the end of the file.
func (r *ftpReader) finish() error {
	code, msg, err := r.fc.readFor(5 * time.Second)
	if err != nil || code < 400 {
		return io.EOF // done, or it says nothing: the data's end is all there is to go by
	}
	if r.got == 0 && r.fc.prot {
		// TLS on the data connection was refused after all, e.g. vsftpd's "522 SSL connection
		// failed: session reuse required" when the session can't be resumed.
		noteFTPTLS(r.fc.key, ftpDataPlain)
	}
	return ftpErr(code, msg)
}

func (r *ftpReader) Close() error {
	r.stop()
	r.data.Close()
	return r.fc.Close() // control connections aren't reused after a partial transfer
}

// retr starts a transfer of name at offset.
func (fc *ftpConn) retr(ctx context.Context, name string, offset int64) (*ftpReader, error) {
	data, err := fc.passive(ctx)
	if err != nil {
		return nil, err
	}
	restOK := false
	if code, _, err := fc.cmd("REST " + strconv.FormatInt(offset, 10)); err != nil {
		data.Close()
		return nil, err
	} else if code == 350 {
		restOK = true
	} else if offset > 0 {
		data.Close()
		return nil, permanentErr{errors.New("FTP server doesn't support resuming (REST)")}
	}
	code, msg, err := fc.cmd("RETR " + name)
	if err != nil {
		data.Close()
		return nil, err
	}
	if code != 150 && code != 125 {
		data.Close()
		if code == 550 {
			return nil, permanentErr{fmt.Errorf("file not found on FTP server (%s)", strings.TrimSpace(msg))}
		}
		return nil, ftpErr(code, msg)
	}
	if tc, ok := data.(*tls.Conn); ok {
		tc.SetDeadline(time.Now().Add(ftpCmdTimeout))
		if err := tc.HandshakeContext(ctx); err != nil {
			data.Close()
			if ctx.Err() != nil || isTimeout(err) {
				return nil, err
			}
			noteFTPTLS(fc.key, ftpDataPlain)
			return nil, fmt.Errorf("%w (data connection: %v)", errFTPRetryPlain, err)
		}
		tc.SetDeadline(time.Time{})
	}
	r := &ftpReader{data: data, fc: fc, restOK: restOK, size: -1}
	r.stop = context.AfterFunc(ctx, func() { data.Close(); fc.Close() })
	return r, nil
}

// ftpOpen logs in, walks to the file and starts streaming it from pos; stat also reads its
// size and date first. When TLS on data connections fails, it reconnects once without.
func ftpOpen(ctx context.Context, u *url.URL, d *Download, pos int64, stat bool) (*ftpReader, error) {
	r, err := ftpOpenOnce(ctx, u, d, pos, stat)
	if errors.Is(err, errFTPRetryPlain) && ctx.Err() == nil {
		r, err = ftpOpenOnce(ctx, u, d, pos, stat)
	}
	return r, err
}

func ftpOpenOnce(ctx context.Context, u *url.URL, d *Download, pos int64, stat bool) (*ftpReader, error) {
	fc, err := ftpDial(ctx, u, d)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { fc.Close() })
	defer stop()
	fail := func(err error) (*ftpReader, error) { fc.Close(); return nil, err }
	name, err := fc.target(u)
	if err != nil {
		return fail(err)
	}
	size, mod := int64(-1), ""
	if stat {
		size, mod = fc.size(name), fc.mdtm(name)
	}
	r, err := fc.retr(ctx, name, pos)
	if err != nil {
		return fail(err)
	}
	r.size, r.mod = size, mod
	return r, nil
}

// probeFTP logs in, reads size/date and starts streaming from pos.
func probeFTP(ctx context.Context, d *Download, rawURL string, pos int64) (*probeResult, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(u.Path, "/") {
		return nil, permanentErr{errors.New("that's an FTP folder, not a file — use the Site grabber for folders")}
	}
	var r *ftpReader
	for try := 1; ; try++ {
		r, err = ftpOpen(ctx, u, d, pos, true)
		if err == nil || ctx.Err() != nil || !errors.Is(err, errNoSlot) {
			break
		}
		// The server is full: connections of ours or of others may leave in a moment.
		if try == 4 {
			err = permanentErr{fmt.Errorf("%w — try again later", err)}
			break
		}
		if !sleepCtx(ctx, time.Duration(try)*ftpBusyWait) {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	name := path.Base(u.Path)
	if name == "" || name == "/" || name == "." {
		name = "download"
	}
	return &probeResult{body: r, partial: r.restOK, size: r.size, resumable: r.restOK && r.size > 0,
		name: sanitizeName(name), finalURL: rawURL, lastMod: r.mod}, nil
}

// openFTPRange opens a new connection that streams from pos (the worker stops at the segment
// end). A server that turns it away as full returns an error that is errNoSlot.
func openFTPRange(ctx context.Context, d *Download, rawURL string, pos int64) (io.ReadCloser, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, permanentErr{err}
	}
	r, err := ftpOpen(ctx, u, d, pos, false)
	if errors.Is(err, errNoSlot) && sleepCtx(ctx, ftpBusyWait/4) {
		// It may still be counting the connection this worker closed a moment ago.
		r, err = ftpOpen(ctx, u, d, pos, false)
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}
