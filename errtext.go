package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unicode"
	"unicode/utf8"
)

// Errors are told apart by their Go types and OS error codes, not their wording: Windows
// words them differently from macOS and Linux, and in the system's own language.

// errKind is what went wrong, shared by the messages and the retry logic. The OS error
// codes behind each kind are listed per OS in errtext_windows.go and errtext_other.go.
type errKind int

const (
	ekNone errKind = iota

	ekRefused     // nothing accepted the connection
	ekReset       // the server dropped the connection
	ekAborted     // something on this computer dropped it (firewall, antivirus, VPN)
	ekTimedOut    // no answer
	ekNetUnreach  // no route to any network: this computer is offline
	ekNetDown     // the network adapter is down
	ekHostUnreach // no route to that server
	ekNoHost      // the name doesn't exist (also what some systems say when offline)
	ekDNSAgain    // the name lookup got no answer this time
	ekDNSFailed   // the name server answered, with a failure

	ekDiskFull
	ekQuota
	ekDenied // a file permission, or (for a connection) a firewall
	ekReadOnly
	ekInUse // open or locked in another program
	ekPathLong
	ekNoPath // the folder, or its drive, isn't there
	ekTooManyFiles
	ekMapped    // another program (often antivirus) has the file mapped
	ekVirus     // the antivirus blocked the file
	ekDiskError // the drive itself failed
)

// friendlyError turns an error into one plain sentence for the user, naming the server
// from rawURL and ending with what to do about it when there's something to do.
func friendlyError(err error, rawURL string) string {
	if err == nil {
		return ""
	}
	host := errHost(rawURL)
	var se statusErr
	if errors.As(err, &se) {
		return statusText(se)
	}
	k := kindOf(err)
	if k == ekNetUnreach || k == ekNetDown { // a proxy can't help with that either
		return "no network connection — check your internet connection"
	}
	if s := proxyText(err, host, rawURL); s != "" {
		return s
	}
	if s := tlsText(err, host, rawURL); s != "" {
		return s
	}
	if k != ekNone {
		return kindText(k, err, host)
	}
	var dns *net.DNSError
	var ue *url.Error
	var pe *os.PathError
	switch {
	case errors.As(err, &dns):
		return "couldn't look up " + host + " (" + plainText(errors.New(dns.Err), host) + ")"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "the connection to " + host + " closed early — try again"
	case isTimeout(err):
		return timeoutText(err, host)
	case errors.Is(err, context.Canceled):
		return "the download was paused"
	case errors.As(err, &ue) && errors.Is(err, io.EOF):
		return host + " closed the connection — try again later"
	case errors.As(err, &pe) && pe.Err != nil:
		return "couldn't " + fileVerb(pe.Op) + " " + pe.Path + " (" + osWords(pe.Err.Error()) + ")"
	}
	return plainText(err, host)
}

// isNetworkDown reports errors that mean this computer has no working network, so the
// retry logic waits for it to come back instead of using up its tries.
func isNetworkDown(err error) bool {
	if err == nil {
		return false
	}
	switch kindOf(err) {
	case ekNetUnreach, ekNetDown, ekHostUnreach, ekDNSAgain:
		return true
	}
	return false
}

// isTransient reports errors worth retrying: network trouble and the HTTP replies that
// mean "not now" (408, 425, 429 and 5xx). A permanentErr never is.
func isTransient(err error) bool {
	var pe permanentErr
	if err == nil || errors.As(err, &pe) || errors.Is(err, context.Canceled) {
		return false
	}
	var se statusErr
	if errors.As(err, &se) {
		return se.code == 408 || se.code == 425 || se.code == 429 || se.code >= 500 && se.code < 600
	}
	switch kindOf(err) {
	case ekNone:
	case ekRefused, ekReset, ekAborted, ekTimedOut, ekNetUnreach, ekNetDown, ekHostUnreach,
		ekNoHost, ekDNSAgain, ekDNSFailed: // "not found" too: offline Windows and macOS say that
		return true
	default:
		return false // disk and file problems need the user
	}
	if tlsText(err, "", "") != "" {
		return false // a certificate or security setting won't fix itself
	}
	var op *net.OpError
	var dns *net.DNSError
	return isTimeout(err) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) ||
		errors.As(err, &op) || errors.As(err, &dns)
}

// kindOf finds the OS error code inside err. Errors that were passed on as text (disk errors
// from the engine, the cause inside a DNS error) are matched against this OS's own wording
// for each code, which is the same whatever the system language.
func kindOf(err error) errKind {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errnoKinds[errno]
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		if e, _ := errnoInText(dns.Err); e != 0 {
			return errnoKinds[e]
		}
		switch {
		case dns.IsNotFound:
			return ekNoHost
		case dns.IsTimeout:
			return ekDNSAgain
		case dns.IsTemporary && dns.Err == "server misbehaving": // Go's fixed words for a SERVFAIL reply
			return ekDNSFailed
		case dns.IsTemporary:
			return ekDNSAgain
		}
		return ekNone
	}
	if e, _ := errnoInText(err.Error()); e != 0 {
		return errnoKinds[e]
	}
	return ekNone
}

// errnoWords is each known code's wording on this computer (on Windows each one is a
// system call, so they're looked up once).
var errnoWords = sync.OnceValue(func() map[syscall.Errno]string {
	m := make(map[syscall.Errno]string, len(errnoKinds))
	for e := range errnoKinds {
		m[e] = e.Error()
	}
	return m
})

// errnoInText finds which known OS error msg ends with, and what comes before it.
func errnoInText(msg string) (syscall.Errno, string) {
	var best syscall.Errno
	var bestText string
	for e, t := range errnoWords() {
		if len(t) > len(bestText) && strings.HasSuffix(msg, t) {
			best, bestText = e, t
		}
	}
	if best == 0 {
		return 0, ""
	}
	return best, strings.TrimSuffix(strings.TrimSuffix(msg, bestText), ": ")
}

func kindText(k errKind, err error, host string) string {
	p := errPath(err)
	the := func(what string) string { // the path, or a stand-in when there's none
		if p == "" {
			return what
		}
		return p
	}
	var op *net.OpError
	switch k {
	case ekRefused:
		return "couldn't connect to " + host + " (connection refused) — try again later"
	case ekReset:
		return host + " closed the connection — try again later"
	case ekAborted:
		return "the connection to " + host + " was cut off on this computer — a firewall, antivirus or VPN may be blocking it"
	case ekTimedOut:
		return timeoutText(err, host)
	case ekHostUnreach:
		return "couldn't reach " + host + " — check your internet connection"
	case ekNoHost:
		return "couldn't find " + host + " — check the link or your internet connection"
	case ekDNSAgain:
		return "couldn't look up " + host + " right now — check your internet connection"
	case ekDNSFailed:
		return "couldn't look up " + host + " (its name server reported a problem) — try again later"
	case ekDenied:
		if errors.As(err, &op) {
			return "the connection to " + host + " was blocked on this computer — a firewall or security program may be stopping Zipline"
		}
		return "Zipline isn't allowed to save to " + the("that folder") + " — choose another folder"
	case ekDiskFull:
		return "the disk is full — free up space or choose another folder"
	case ekQuota:
		return "your disk quota is used up — free up space or choose another folder"
	case ekReadOnly:
		return "the disk is read-only — choose another folder"
	case ekInUse:
		return "couldn't use " + the("the file") + " — the file may be open in another program"
	case ekPathLong:
		return "the file's path is too long — choose a shorter folder or file name"
	case ekNoPath:
		return "couldn't find " + the("the folder") + " — check that the folder exists and its drive is connected"
	case ekTooManyFiles:
		return "too many files are open at once — pause some downloads and try again"
	case ekMapped:
		return the("the file") + " is in use by another program (often antivirus) — try again in a moment"
	case ekVirus:
		return "your antivirus blocked " + the("the file")
	case ekDiskError:
		return "the disk reported an error reading or writing " + the("the file") + " — check the drive"
	}
	return plainText(err, host)
}

func statusText(se statusErr) string {
	switch se.code {
	case 400:
		return "the server didn't accept the request (400) — the link may be incomplete"
	case 401:
		return "the site needs a user name and password — save one in Options → Sites"
	case 403:
		return "the server refused access (403) — the link may have expired or need a sign-in"
	case 404:
		return "the file isn't on the server (404)"
	case 408:
		return "the server gave up waiting (408) — try again"
	case 410:
		return "the file has been removed from the server (410)"
	case 416:
		return "the server couldn't send the rest of the file (416) — it may have changed; use Redownload"
	case 429:
		return "the server says too many requests — waiting a while helps"
	case 500:
		return "the server had an internal error (500) — try again later"
	case 502:
		return "the server is having trouble (502) — try again later"
	case 503:
		return "the server is busy or down for maintenance (503) — try again later"
	case 504:
		return "the server took too long to respond (504) — try again later"
	}
	if s := strings.TrimSpace(se.status); s != "" {
		return "the server replied " + s
	}
	return "the server replied " + strconv.Itoa(se.code)
}

// proxyText explains a request the proxy couldn't carry.
func proxyText(err error, host, rawURL string) string {
	var op *net.OpError
	if errors.As(err, &op) {
		if op.Op == "proxyconnect" {
			if p := proxyName(err, rawURL); p != "" {
				return "couldn't reach the proxy " + p + " — check Options → Proxy"
			}
			return "couldn't reach the proxy — check Options → Proxy"
		}
		if strings.HasPrefix(op.Op, "socks ") {
			return "the proxy couldn't connect to " + host + " — check Options → Proxy"
		}
	}
	// A proxy that won't open a tunnel (CONNECT) for https comes back from Go as just the
	// words of its status line, e.g. "Proxy Authentication Required".
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil && errors.Unwrap(ue.Err) == nil {
		for code := 400; code < 600; code++ {
			if t := http.StatusText(code); t != "" && t == ue.Err.Error() {
				if code == http.StatusProxyAuthRequired {
					return "the proxy needs a user name and password (Options → Proxy)"
				}
				return "the proxy wouldn't connect to " + host + " (it replied " + strconv.Itoa(code) + " " + t + ") — check Options → Proxy"
			}
		}
	}
	return ""
}

// proxyName is the proxy a request to rawURL goes through, or the address that failed.
func proxyName(err error, rawURL string) string {
	if u, e := url.Parse(rawURL); e == nil && u.Host != "" {
		if pu, _ := proxyFor(u); pu != nil {
			return pu.Host
		}
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return dns.Name
	}
	for e := err; e != nil; e = errors.Unwrap(e) {
		if op, ok := e.(*net.OpError); ok && op.Addr != nil {
			return op.Addr.String()
		}
	}
	return ""
}

// tlsText explains a failed secure (https) connection.
func tlsText(err error, host, rawURL string) string {
	failed := "the secure connection to " + host + " failed"
	var ci x509.CertificateInvalidError
	var hn x509.HostnameError
	var ua x509.UnknownAuthorityError
	var sr x509.SystemRootsError
	var cv *tls.CertificateVerificationError
	var rh tls.RecordHeaderError
	var ae tls.AlertError
	var op *net.OpError
	switch {
	case errors.As(err, &ci) && ci.Reason == x509.Expired:
		return failed + " (its certificate has expired or isn't valid yet) — check your computer's date and time"
	case errors.As(err, &hn):
		return failed + " (its certificate is for a different site)"
	case errors.As(err, &ua):
		return failed + " (its certificate isn't trusted)"
	case errors.As(err, &ci), errors.As(err, &sr), errors.As(err, &cv):
		return failed + " (certificate problem)"
	case errors.Is(err, http.ErrSchemeMismatch), errors.As(err, &rh):
		if strings.HasPrefix(strings.ToLower(rawURL), "https:") {
			return host + " doesn't offer a secure connection — try the link with http:// instead"
		}
		return host + " didn't answer with a secure connection"
	case errors.As(err, &ae), errors.As(err, &op) && op.Op == "remote error": // an alert from the server
		return failed + " (the server turned it down)"
	}
	return ""
}

// timeoutText tells a server that never answered from one that stopped part-way.
func timeoutText(err error, host string) string {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "read" {
		return host + " stopped sending data — check your internet connection"
	}
	return host + " didn't answer in time — check your internet connection"
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout()
}

// goFraming is what Go puts before a cause when it prints a network error:
// `Get "https://…": `, `dial tcp 1.2.3.4:443: `, `read tcp 10.0.0.2:5000->1.2.3.4:443: `,
// `lookup example.com on 8.8.8.8:53: `, `connectex: ` and the like.
var goFraming = regexp.MustCompile(`^(?:` +
	`(?:Get|Head|Post|Put|Patch|Delete|Options) "[^"]*": |` +
	`(?:dial|read|write|proxyconnect|socks connect) (?:tcp|udp|ip|unix)[46]?(?: \S+)?: |` +
	`remote error: |lookup \S+(?: on \S+)?: |` +
	`(?:connect|connectex|wsarecv|wsasend|wsarecvfrom|wsasendto|wsaioctl|getaddrinfow|setsockopt|socket|sendfile|transmitfile|read|write): |` +
	`net/http: |disk: )+`)

// plainText is the innermost message of err without Go's framing.
func plainText(err error, host string) string {
	inner := innermost(err)
	msg := inner.Error()
	_, fromOS := inner.(syscall.Errno)
	if s := goFraming.FindString(msg); s != "" {
		msg, fromOS = msg[len(s):], true
	}
	if fromOS {
		msg = osWords(msg)
	}
	if rest, ok := strings.CutPrefix(msg, "tls: "); ok {
		return "the secure connection to " + host + " failed (" + rest + ")"
	}
	if msg == "" {
		return err.Error()
	}
	return msg
}

// innermost drops the wrappers Go puts around a cause (the request, the socket call).
func innermost(err error) error {
	for {
		var next error
		switch e := err.(type) {
		case *url.Error:
			next = e.Err
		case *net.OpError:
			next = e.Err
		case *os.SyscallError:
			next = e.Err
		case permanentErr:
			next = e.error
		}
		if next == nil {
			return err
		}
		err = next
	}
}

// osWords tidies an OS message to match ours: Windows ends its messages with a full stop
// and starts them with a capital.
func osWords(s string) string {
	s = strings.TrimSuffix(strings.TrimSpace(s), ".")
	return lowerFirst(s)
}

// lowerFirst lower-cases a capitalised first word ("The device…" → "the device…") but
// leaves names and abbreviations such as "OneDrive" or "FTP" alone.
func lowerFirst(s string) string {
	w, _, _ := strings.Cut(s, " ")
	r, n := utf8.DecodeRuneInString(w)
	if !unicode.IsUpper(r) || strings.ToLower(w[n:]) != w[n:] {
		return s
	}
	return string(unicode.ToLower(r)) + s[n:]
}

// pathOp is a file error passed on as text: "write /path/to/file.part" (the cause cut off).
var pathOp = regexp.MustCompile(`^(?:disk: )?(?:open|write|read|mkdir|remove|sync|truncate|close|seek|stat|lstat|chmod) (.+)$`)

// errPath is the file or folder an error is about, or "".
func errPath(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Path
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.New
	}
	if _, before := errnoInText(err.Error()); before != "" {
		if m := pathOp.FindStringSubmatch(before); m != nil {
			return m[1]
		}
	}
	return ""
}

func fileVerb(op string) string {
	switch op {
	case "open":
		return "open"
	case "mkdir":
		return "create the folder"
	case "remove":
		return "delete"
	case "read", "seek":
		return "read"
	}
	return "write to"
}

func errHost(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		return u.Host
	}
	return "the server"
}
