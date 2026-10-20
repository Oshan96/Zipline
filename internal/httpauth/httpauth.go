// Package httpauth answers HTTP login challenges with Basic or Digest authentication.
package httpauth

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"unicode/utf8"
)

// A server asks for a login with 401 and a WWW-Authenticate header (a proxy: 407 and
// Proxy-Authenticate) naming the schemes it takes. Digest proves the password without sending
// it; Basic sends it merely base64-encoded, so it's kept to https and the local network.

// Challenge is one challenge from a WWW-Authenticate or Proxy-Authenticate header.
type Challenge struct {
	Scheme  string            // lower-case: "basic", "digest", "bearer" …
	Params  map[string]string // names lower-cased, quoted values unescaped; nil when none
	Token68 string            // the single blob some schemes send instead of params (Negotiate)
}

// ParseChallenges reads WWW-Authenticate or Proxy-Authenticate values (RFC 9110 §11.6.1). Commas
// separate challenges as well as the params within one, so a token that isn't followed by "="
// starts a new challenge. Malformed pieces are skipped up to the next comma.
func ParseChallenges(values []string) []Challenge {
	var out []Challenge
	for _, s := range values {
		cur := -1 // the challenge that following params belong to
		for i := 0; ; {
			i = skipAuthSpace(s, i, true)
			if i >= len(s) {
				break
			}
			name, j := authToken(s, i)
			k := skipAuthSpace(s, j, false)
			if name != "" && k < len(s) && s[k] == '=' {
				// name=value: another param of the current challenge
				if v, e, ok := authValue(s, skipAuthSpace(s, k+1, false)); ok && cur >= 0 {
					out[cur].setParam(name, v)
					i = e
				}
				i = skipAuthJunk(s, i)
				continue
			}
			if name == "" || j < len(s) && s[j] != ' ' && s[j] != '\t' && s[j] != ',' {
				cur = -1
				i = skipAuthJunk(s, i)
				continue
			}
			out = append(out, Challenge{Scheme: strings.ToLower(name)})
			cur, i = len(out)-1, k
			if i >= len(s) || s[i] == ',' {
				continue
			}
			// after the scheme: its first param, or a token68
			if p, j := authToken(s, i); p != "" {
				if e := skipAuthSpace(s, j, false); e < len(s) && s[e] == '=' {
					if v, e, ok := authValue(s, skipAuthSpace(s, e+1, false)); ok {
						out[cur].setParam(p, v)
						i = skipAuthJunk(s, e)
						continue
					}
				}
			}
			if t, e := authToken68(s, i); t != "" && authListEnd(s, e) {
				out[cur].Token68 = t
				i = e
				continue
			}
			i = skipAuthJunk(s, i)
		}
	}
	return out
}

// setParam records a param; a repeated one keeps its first value.
func (c *Challenge) setParam(name, v string) {
	name = strings.ToLower(name)
	if _, dup := c.Params[name]; dup {
		return
	}
	if c.Params == nil {
		c.Params = map[string]string{}
	}
	c.Params[name] = v
}

func skipAuthSpace(s string, i int, commas bool) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || commas && s[i] == ',') {
		i++
	}
	return i
}

// authListEnd: only spaces lie between i and the next comma or the end.
func authListEnd(s string, i int) bool {
	i = skipAuthSpace(s, i, false)
	return i == len(s) || s[i] == ','
}

// skipAuthJunk moves on to the next comma that isn't inside quotes.
func skipAuthJunk(s string, i int) int {
	for quoted := false; i < len(s); i++ {
		switch c := s[i]; {
		case quoted && c == '\\':
			i++
		case c == '"':
			quoted = !quoted
		case c == ',' && !quoted:
			return i
		}
	}
	return len(s)
}

func authAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// authToken reads an RFC 9110 token.
func authToken(s string, i int) (string, int) {
	j := i
	for j < len(s) && (authAlnum(s[j]) || strings.IndexByte("!#$%&'*+-.^_`|~", s[j]) >= 0) {
		j++
	}
	return s[i:j], j
}

// authToken68 reads a token68: base64-ish characters with "=" padding.
func authToken68(s string, i int) (string, int) {
	j := i
	for j < len(s) && (authAlnum(s[j]) || strings.IndexByte("-._~+/", s[j]) >= 0) {
		j++
	}
	if j == i {
		return "", i
	}
	for j < len(s) && s[j] == '=' {
		j++
	}
	return s[i:j], j
}

// authValue reads a param value: a quoted-string, or a token. Unquoted values run to the next
// space or comma, since some servers send base64 nonces bare; one can't start with "=" though,
// as then it's a token68's padding.
func authValue(s string, i int) (string, int, bool) {
	if i < len(s) && s[i] == '"' {
		var b strings.Builder
		for i++; i < len(s); i++ {
			switch s[i] {
			case '\\':
				if i+1 < len(s) {
					i++
					b.WriteByte(s[i])
				}
			case '"':
				return b.String(), i + 1, true
			default:
				b.WriteByte(s[i])
			}
		}
		return b.String(), i, true // unterminated: take the rest
	}
	j := i
	for j < len(s) && s[j] != ',' && s[j] != ' ' && s[j] != '\t' && s[j] != '"' {
		j++
	}
	if j == i || s[i] == '=' {
		return "", i, false
	}
	return s[i:j], j, true
}

// Digest answers one Digest challenge (RFC 7616, and RFC 2617/2069 servers). Connections to
// the same server share it, each request taking the next nonce count. When a server rejects a
// request with a new challenge, start a fresh state from that.
type Digest struct {
	realm, nonce, opaque string
	algorithm            string // as the server spelled it; "" when it named none (MD5)
	hash                 func() hash.Hash
	rank                 int    // strength of the hash, to pick between challenges
	sess                 bool   // "-sess": the password hash is keyed to the nonces once
	qop                  string // "auth", or "" for an old server without qop
	userhash             bool   // send a hash of the user name rather than the name
	utf8                 bool   // charset=UTF-8: a non-ASCII user name may go as username*
	Stale                bool   // only the nonce had run out: the login itself was accepted

	newCnonce func() string // random; fixed in tests
	mu        sync.Mutex
	nc        uint32
	cnonce    string // a -sess session's client nonce
}

// newDigest checks a Digest challenge is one this client can answer. Only qop=auth is done
// (auth-int would need the request body hashed), with MD5, SHA-256 or SHA-512-256, plain or -sess.
func newDigest(ch Challenge) (*Digest, error) {
	if !strings.EqualFold(ch.Scheme, "digest") {
		return nil, fmt.Errorf("%s isn't Digest", ch.Scheme)
	}
	p := ch.Params
	d := &Digest{
		realm: p["realm"], nonce: p["nonce"], opaque: p["opaque"], algorithm: p["algorithm"],
		userhash:  strings.EqualFold(p["userhash"], "true"),
		utf8:      strings.EqualFold(p["charset"], "UTF-8"),
		Stale:     strings.EqualFold(p["stale"], "true"),
		newCnonce: randomCnonce,
	}
	if d.nonce == "" {
		return nil, errors.New("digest: the server sent no nonce")
	}
	alg := strings.ToUpper(d.algorithm)
	if a, ok := strings.CutSuffix(alg, "-SESS"); ok && a != "" {
		alg, d.sess = a, true
	}
	switch alg {
	case "", "MD5":
		d.hash, d.rank = md5.New, 1
	case "SHA-256":
		d.hash, d.rank = sha256.New, 2
	case "SHA-512-256":
		d.hash, d.rank = sha512.New512_256, 3
	default:
		return nil, fmt.Errorf("digest: algorithm %q not supported", d.algorithm)
	}
	if q := strings.TrimSpace(p["qop"]); q != "" {
		for o := range strings.SplitSeq(q, ",") {
			if strings.EqualFold(strings.TrimSpace(o), "auth") {
				d.qop = "auth"
			}
		}
		if d.qop == "" {
			return nil, fmt.Errorf("digest: qop %q not supported", q)
		}
	} else if d.sess {
		return nil, errors.New("digest: -sess without qop") // it needs a client nonce
	}
	return d, nil
}

// BestDigest picks the strongest Digest challenge this client can answer.
func BestDigest(chs []Challenge) (*Digest, error) {
	var best *Digest
	err := errors.New("no Digest challenge")
	for _, ch := range chs {
		if !strings.EqualFold(ch.Scheme, "digest") {
			continue
		}
		d, e := newDigest(ch)
		if e != nil {
			err = e
		} else if best == nil || d.rank > best.rank {
			best = d
		}
	}
	if best == nil {
		return nil, err
	}
	return best, nil
}

func randomCnonce() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Authorize returns the Authorization (or Proxy-Authorization) value for one request; uri is
// the request target as sent (u.RequestURI()).
func (d *Digest) Authorize(method, uri, user, pass string) string {
	d.mu.Lock()
	d.nc++
	nc := fmt.Sprintf("%08x", d.nc)
	cnonce := d.cnonce
	if cnonce == "" && d.qop != "" {
		cnonce = d.newCnonce()
		if d.sess {
			d.cnonce = cnonce // the session key is made with the first client nonce
		}
	}
	d.mu.Unlock()

	h := func(parts ...string) string {
		x := d.hash()
		x.Write([]byte(strings.Join(parts, ":")))
		return hex.EncodeToString(x.Sum(nil))
	}
	ha1 := h(user, d.realm, pass)
	if d.sess {
		ha1 = h(ha1, d.nonce, cnonce)
	}
	ha2 := h(method, uri)
	var resp string
	if d.qop == "" {
		resp = h(ha1, d.nonce, ha2)
	} else {
		resp = h(ha1, d.nonce, nc, cnonce, d.qop, ha2)
	}

	var b strings.Builder
	switch {
	case d.userhash:
		b.WriteString("Digest username=" + authQuote(h(user, d.realm)))
	case d.utf8 && strings.ContainsFunc(user, func(r rune) bool { return r >= utf8.RuneSelf }):
		b.WriteString("Digest username*=UTF-8''" + authExtValue(user))
	default:
		b.WriteString("Digest username=" + authQuote(user))
	}
	b.WriteString(", realm=" + authQuote(d.realm) + ", uri=" + authQuote(uri))
	if d.algorithm != "" {
		b.WriteString(", algorithm=" + d.algorithm)
	}
	b.WriteString(", nonce=" + authQuote(d.nonce))
	if d.qop != "" {
		b.WriteString(", nc=" + nc + ", cnonce=" + authQuote(cnonce) + ", qop=" + d.qop)
	}
	b.WriteString(", response=" + authQuote(resp))
	if d.opaque != "" {
		b.WriteString(", opaque=" + authQuote(d.opaque))
	}
	if d.userhash {
		b.WriteString(", userhash=true")
	}
	return b.String()
}

var authEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

func authQuote(s string) string { return `"` + authEscaper.Replace(s) + `"` }

// authExtValue percent-encodes UTF-8 text for an RFC 8187 ext-value, as username* takes.
func authExtValue(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if c := s[i]; authAlnum(c) || strings.IndexByte("!#$&+-.^_`|~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// BasicAllowed reports whether a Basic login may be sent to u: over https, or over plain http
// only within this machine or the local network, as anywhere else it'd cross the internet
// readable by anyone on the path.
func BasicAllowed(u *url.URL) bool {
	switch strings.ToLower(u.Scheme) {
	case "https":
		return true
	case "http":
	default:
		return false
	}
	h := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if a, err := netip.ParseAddr(h); err == nil {
		a = a.Unmap()
		return a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast()
	}
	if h == "" {
		return false
	}
	if !strings.Contains(h, ".") {
		return true // "localhost", or an intranet name like "nas"
	}
	for _, s := range []string{".local", ".localhost", ".home.arpa", ".internal"} {
		if strings.HasSuffix(h, s) {
			return true // names reserved for local use (mDNS, RFC 6761, RFC 8375, ICANN)
		}
	}
	return false
}

// Choose picks how to answer a 401 for u: "Digest" when there's a Digest challenge this
// client can answer (BestDigest gives the strongest), else "Basic" if offered and BasicAllowed,
// else "".
func Choose(chs []Challenge, u *url.URL) (scheme string) {
	if _, err := BestDigest(chs); err == nil {
		return "Digest"
	}
	for _, c := range chs {
		if strings.EqualFold(c.Scheme, "basic") && BasicAllowed(u) {
			return "Basic"
		}
	}
	return ""
}
