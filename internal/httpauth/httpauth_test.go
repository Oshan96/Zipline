package httpauth

import (
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestParseChallenges(t *testing.T) {
	type ch = Challenge
	type m = map[string]string
	for _, c := range []struct {
		in   []string
		want []Challenge
	}{
		{[]string{`Basic realm="simple"`}, []ch{{"basic", m{"realm": "simple"}, ""}}},
		// RFC 9110 §11.6.1's example: two challenges in one header, an escaped quote
		{[]string{`Newauth realm="apps", type=1, title="Login to \"apps\"", Basic realm="simple"`}, []ch{
			{"newauth", m{"realm": "apps", "type": "1", "title": `Login to "apps"`}, ""},
			{"basic", m{"realm": "simple"}, ""},
		}},
		{[]string{`Negotiate YIIBhgYGKwYBBQUCoIIBejCCAX==, NTLM`, `Bearer abc/def+ghi=`}, []ch{
			{"negotiate", nil, "YIIBhgYGKwYBBQUCoIIBejCCAX=="},
			{"ntlm", nil, ""},
			{"bearer", nil, "abc/def+ghi="},
		}},
		{[]string{`Bearer realm="example", error="invalid_token", error_description="The access token expired"`}, []ch{
			{"bearer", m{"realm": "example", "error": "invalid_token", "error_description": "The access token expired"}, ""},
		}},
		// case, spaces around "=", commas inside quotes, a bare base64 nonce, a repeated param
		{[]string{`DIGEST REALM = "a,b" ,Nonce=abc/d==,qop="auth,auth-int", realm="dup"`}, []ch{
			{"digest", m{"realm": "a,b", "nonce": "abc/d==", "qop": "auth,auth-int"}, ""},
		}},
		{[]string{`Basic realm="unterminated`}, []ch{{"basic", m{"realm": "unterminated"}, ""}}},
		{[]string{`, ,`, `"junk", Basic realm=x`, `=x, Digest`}, []ch{
			{"basic", m{"realm": "x"}, ""},
			{"digest", nil, ""},
		}},
		{[]string{`realm=orphan, Basic`}, []ch{{"basic", nil, ""}}},
		{nil, nil},
	} {
		if got := ParseChallenges(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseChallenges(%q)\n got %#v\nwant %#v", c.in, got, c.want)
		}
	}
}

// RFC 7616 §3.9.1: a server offering SHA-256 and MD5.
var rfc7616Challenges = []string{
	`Digest realm="http-auth@example.org", qop="auth, auth-int", algorithm=SHA-256, nonce="7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v", opaque="FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS"`,
	`Digest realm="http-auth@example.org", qop="auth, auth-int", algorithm=MD5, nonce="7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v", opaque="FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS"`,
}

func fixedCnonce(s string) func() string { return func() string { return s } }

func TestDigestRFC7616(t *testing.T) {
	chs := ParseChallenges(rfc7616Challenges)
	const tail = `, nonce="7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v", nc=00000001, cnonce="f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ", qop=auth, response="%s", opaque="FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS"`
	for i, c := range []struct{ alg, resp string }{
		{"SHA-256", "753927fa0e85d155564e2e272a28d1802ca10daf4496794697cf8db5856cb6c1"},
		{"MD5", "8ca523f5e9506fed4657c9700eebdbec"},
	} {
		d, err := newDigest(chs[i])
		if err != nil {
			t.Fatal(err)
		}
		d.newCnonce = fixedCnonce("f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ")
		want := `Digest username="Mufasa", realm="http-auth@example.org", uri="/dir/index.html", algorithm=` + c.alg + fmt.Sprintf(tail, c.resp)
		if got := d.Authorize("GET", "/dir/index.html", "Mufasa", "Circle of Life"); got != want {
			t.Errorf("%s:\n got %s\nwant %s", c.alg, got, want)
		}
		if got := d.Authorize("GET", "/dir/index.html", "Mufasa", "Circle of Life"); !strings.Contains(got, "nc=00000002,") {
			t.Errorf("%s: second request %s", c.alg, got)
		}
	}
	if d, err := BestDigest(chs); err != nil || d.algorithm != "SHA-256" {
		t.Errorf("bestDigest = %v, %v", d, err)
	}
	u, _ := url.Parse("http://example.org/dir/index.html")
	if s := Choose(append(chs, Challenge{Scheme: "basic"}), u); s != "Digest" {
		t.Errorf("chooseAuth = %q", s)
	}
}

// digestParams reads an Authorization value back (it has a challenge's syntax).
func digestParams(t *testing.T, h string) map[string]string {
	t.Helper()
	chs := ParseChallenges([]string{h})
	if len(chs) != 1 || chs[0].Scheme != "digest" {
		t.Fatalf("bad header %s", h)
	}
	return chs[0].Params
}

func TestDigestRFC2617(t *testing.T) {
	chs := ParseChallenges([]string{`Digest realm="testrealm@host.com", qop="auth,auth-int", nonce="dcd98b7102dd2f0e8b11d0f600bfb0c093", opaque="5ccc069c403ebaf9f0171e9517f40e41"`})
	d, err := newDigest(chs[0])
	if err != nil {
		t.Fatal(err)
	}
	d.newCnonce = fixedCnonce("0a4f113b")
	p := digestParams(t, d.Authorize("GET", "/dir/index.html", "Mufasa", "Circle Of Life"))
	want := map[string]string{"username": "Mufasa", "realm": "testrealm@host.com", "nonce": "dcd98b7102dd2f0e8b11d0f600bfb0c093",
		"uri": "/dir/index.html", "qop": "auth", "nc": "00000001", "cnonce": "0a4f113b",
		"response": "6629fae49393a05397450978507c4ef1", "opaque": "5ccc069c403ebaf9f0171e9517f40e41"}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("got %v\nwant %v", p, want)
	}

	// RFC 2069 style, no qop: no nc or cnonce, and a shorter response (RFC 2069's example, as corrected)
	chs = ParseChallenges([]string{`Digest realm="testrealm@host.com", nonce="dcd98b7102dd2f0e8b11d0f600bfb0c093", opaque="5ccc069c403ebaf9f0171e9517f40e41"`})
	if d, err = newDigest(chs[0]); err != nil {
		t.Fatal(err)
	}
	h := d.Authorize("GET", "/dir/index.html", "Mufasa", "CircleOfLife")
	if want := `Digest username="Mufasa", realm="testrealm@host.com", uri="/dir/index.html", nonce="dcd98b7102dd2f0e8b11d0f600bfb0c093", response="1949323746fe6a43ef61f9606e7febea", opaque="5ccc069c403ebaf9f0171e9517f40e41"`; h != want {
		t.Errorf("no qop:\n got %s\nwant %s", h, want)
	}
}

// -sess keys the whole session to its first client nonce. Expected values worked out
// separately with Python's hashlib.
func TestDigestSess(t *testing.T) {
	for _, c := range []struct{ alg, r1, r2 string }{
		{"MD5-sess", "e783283f46242139c486a698fec7211d", "6914b51e16f9459d9abc967ad41c4599"},
		{"SHA-256-sess", "2fd51b3a77ad75bad6afad6003e818d767133c46d9e2749e7f5232ae1ea3efd7", "6bb0010aa4bdf46422a798c509ea32e256f27bd37de5cc3bdf8ed51e1d77d650"},
	} {
		ch := ParseChallenges([]string{strings.Replace(rfc7616Challenges[1], "MD5", c.alg, 1)})[0]
		d, err := newDigest(ch)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		d.newCnonce = func() string { n++; return "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ" }
		p1 := digestParams(t, d.Authorize("GET", "/dir/index.html", "Mufasa", "Circle of Life"))
		p2 := digestParams(t, d.Authorize("GET", "/dir/index.html", "Mufasa", "Circle of Life"))
		if p1["response"] != c.r1 || p2["response"] != c.r2 || p2["nc"] != "00000002" || p1["algorithm"] != c.alg || n != 1 {
			t.Errorf("%s: %v / %v (cnonces made: %d)", c.alg, p1, p2, n)
		}
	}
}

// RFC 7616 §3.9.2: SHA-512-256, userhash, and a non-ASCII user name. The hashes printed in the
// RFC don't reproduce (a known erratum); these were checked separately with Python's hashlib.
func TestDigestUserhash(t *testing.T) {
	const ch = `Digest realm="api@example.org", qop="auth", algorithm=SHA-512-256, nonce="5TsQWLVdgBdmrQ0XsxbDODV+57QdFR34I9HAbC/RVvkK", opaque="HRPCssKJSGjCrkzDg8OhwpzCiGPChXYjwrI2QmXDnsOS", charset=UTF-8, userhash=true`
	d, err := newDigest(ParseChallenges([]string{ch})[0])
	if err != nil {
		t.Fatal(err)
	}
	d.newCnonce = fixedCnonce("NTg6RKcb9boFIAS3KrFK9BGeh+iDa/sm6jUMp2wds69v")
	user, pass := "J\u00e4s\u00f8n Doe", "Secret, or not?"
	want := `Digest username="793263caabb707a56211940d90411ea4a575adeccb7e360aeb624ed06ece9b0b", realm="api@example.org", uri="/doe.json", algorithm=SHA-512-256, nonce="5TsQWLVdgBdmrQ0XsxbDODV+57QdFR34I9HAbC/RVvkK", nc=00000001, cnonce="NTg6RKcb9boFIAS3KrFK9BGeh+iDa/sm6jUMp2wds69v", qop=auth, response="3798d4131c277846293534c3edc11bd8a5e4cdcbff78b05db9d95eeb1cec68a5", opaque="HRPCssKJSGjCrkzDg8OhwpzCiGPChXYjwrI2QmXDnsOS", userhash=true`
	if got := d.Authorize("GET", "/doe.json", user, pass); got != want {
		t.Errorf("userhash:\n got %s\nwant %s", got, want)
	}
	// without userhash the name goes as username* (RFC 8187), and isn't sent twice
	d.userhash = false
	got := d.Authorize("GET", "/doe.json", user, pass)
	if !strings.HasPrefix(got, `Digest username*=UTF-8''J%C3%A4s%C3%B8n%20Doe, realm=`) || strings.Contains(got, "userhash") {
		t.Errorf("username*: %s", got)
	}
	// quotes in values are escaped
	d.utf8 = false
	if got := d.Authorize("GET", `/a"b`, `x"y\z`, pass); !strings.HasPrefix(got, `Digest username="x\"y\\z", realm="api@example.org", uri="/a\"b"`) {
		t.Errorf("escaping: %s", got)
	}
}

func TestDigestRefusals(t *testing.T) {
	for _, h := range []string{
		`Digest realm="r", nonce="n", qop="auth-int"`, // would need the body hashed
		`Digest realm="r", nonce="n", algorithm=SHA-1`,
		`Digest realm="r", nonce="n", algorithm=MD5-sess`, // -sess needs qop
		`Digest realm="r"`,
		`Basic realm="r"`,
	} {
		if d, err := newDigest(ParseChallenges([]string{h})[0]); err == nil {
			t.Errorf("newDigest(%s) = %+v", h, d)
		}
	}
	d, err := newDigest(ParseChallenges([]string{`digest realm="r", nonce="n2", qop="auth-int, AUTH", algorithm=sha-256, stale=TRUE`})[0])
	if err != nil || d.qop != "auth" || !d.Stale || d.rank != 2 {
		t.Errorf("newDigest = %+v, %v", d, err)
	}
	if _, err := BestDigest(ParseChallenges([]string{`Basic realm="r", Digest realm="r", nonce="n", qop="auth-int"`})); err == nil {
		t.Error("bestDigest found one")
	}
}

// Connections share a Digest: every request must get its own nonce count.
func TestDigestConcurrent(t *testing.T) {
	d, err := newDigest(ParseChallenges(rfc7616Challenges[1:])[0])
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				nc := ParseChallenges([]string{d.Authorize("GET", "/", "u", "p")})[0].Params["nc"]
				mu.Lock()
				seen[nc] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != 1000 || !seen["00000001"] || !seen[fmt.Sprintf("%08x", 1000)] {
		t.Errorf("%d distinct nonce counts", len(seen))
	}
}

func TestBasicAllowed(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://example.com/f":          true,
		"HTTPS://example.com/f":          true,
		"http://example.com/f":           false,
		"http://8.8.8.8/f":               false,
		"http://[2001:db8::1]/f":         false,
		"http://100.64.0.1/f":            false, // carrier-grade NAT is the ISP's network
		"ftp://example.com/f":            false,
		"http://localhost:8080/f":        true,
		"http://127.0.0.1/f":             true,
		"http://[::1]:8080/f":            true,
		"http://10.1.2.3/f":              true,
		"http://172.16.0.9/f":            true,
		"http://192.168.1.20/f":          true,
		"http://169.254.10.1/f":          true,
		"http://[fd00::5]/f":             true,
		"http://[fe80::1%25eth0]/f":      true,
		"http://[::ffff:192.168.1.1]/f":  true,
		"http://nas/f":                   true,
		"http://NAS./f":                  true,
		"http://printer.local/f":         true,
		"http://router.home.arpa/f":      true,
		"http://app.localhost/f":         true,
		"http://local.example.com/f":     false,
		"http://example.local.evil.com/": false,
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := BasicAllowed(u); got != want {
			t.Errorf("basicAllowed(%s) = %v", raw, got)
		}
	}
}

func TestChooseAuth(t *testing.T) {
	web, _ := url.Parse("http://example.com/f")
	tls, _ := url.Parse("https://example.com/f")
	basic := ParseChallenges([]string{`Basic realm="r"`})
	authInt := ParseChallenges([]string{`Digest realm="r", nonce="n", qop="auth-int", Basic realm="r"`})
	for _, c := range []struct {
		chs  []Challenge
		u    *url.URL
		want string
	}{
		{basic, tls, "Basic"},
		{basic, web, ""}, // the password would cross the internet in the clear
		{authInt, tls, "Basic"},
		{authInt, web, ""},
		{ParseChallenges([]string{`Basic realm="r"`, `Digest realm="r", nonce="n"`}), web, "Digest"},
		{ParseChallenges([]string{`Negotiate, NTLM`}), tls, ""},
		{nil, tls, ""},
	} {
		if got := Choose(c.chs, c.u); got != c.want {
			t.Errorf("chooseAuth(%v, %s) = %q, want %q", c.chs, c.u, got, c.want)
		}
	}
}
