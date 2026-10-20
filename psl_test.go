package main

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
)

var _ cookiejar.PublicSuffixList = pslList{}

// pslOfficial is checkPublicSuffix from the list's own tests/test_psl.txt: the registrable
// domain of each name, "" for null.
var pslOfficial = []struct{ in, want string }{
	{"", ""},
	{"COM", ""},
	{"example.COM", "example.com"},
	{"WwW.example.COM", "example.com"},
	{".com", ""},
	{".example", ""},
	{".example.com", ""},
	{".example.example", ""},
	{"example", ""},
	{"example.example", "example.example"},
	{"b.example.example", "example.example"},
	{"a.b.example.example", "example.example"},
	{"biz", ""},
	{"domain.biz", "domain.biz"},
	{"b.domain.biz", "domain.biz"},
	{"a.b.domain.biz", "domain.biz"},
	{"com", ""},
	{"example.com", "example.com"},
	{"b.example.com", "example.com"},
	{"a.b.example.com", "example.com"},
	{"uk.com", ""},
	{"example.uk.com", "example.uk.com"},
	{"b.example.uk.com", "example.uk.com"},
	{"a.b.example.uk.com", "example.uk.com"},
	{"test.ac", "test.ac"},
	{"mm", ""},
	{"c.mm", ""},
	{"b.c.mm", "b.c.mm"},
	{"a.b.c.mm", "b.c.mm"},
	{"jp", ""},
	{"test.jp", "test.jp"},
	{"www.test.jp", "test.jp"},
	{"ac.jp", ""},
	{"test.ac.jp", "test.ac.jp"},
	{"www.test.ac.jp", "test.ac.jp"},
	{"kyoto.jp", ""},
	{"test.kyoto.jp", "test.kyoto.jp"},
	{"ide.kyoto.jp", ""},
	{"b.ide.kyoto.jp", "b.ide.kyoto.jp"},
	{"a.b.ide.kyoto.jp", "b.ide.kyoto.jp"},
	{"c.kobe.jp", ""},
	{"b.c.kobe.jp", "b.c.kobe.jp"},
	{"a.b.c.kobe.jp", "b.c.kobe.jp"},
	{"city.kobe.jp", "city.kobe.jp"},
	{"www.city.kobe.jp", "city.kobe.jp"},
	{"ck", ""},
	{"test.ck", ""},
	{"b.test.ck", "b.test.ck"},
	{"a.b.test.ck", "b.test.ck"},
	{"www.ck", "www.ck"},
	{"www.www.ck", "www.ck"},
	{"us", ""},
	{"test.us", "test.us"},
	{"www.test.us", "test.us"},
	{"ak.us", ""},
	{"test.ak.us", "test.ak.us"},
	{"www.test.ak.us", "test.ak.us"},
	{"k12.ak.us", ""},
	{"test.k12.ak.us", "test.k12.ak.us"},
	{"www.test.k12.ak.us", "test.k12.ak.us"},
	{"食狮.com.cn", "食狮.com.cn"},
	{"食狮.公司.cn", "食狮.公司.cn"},
	{"www.食狮.公司.cn", "食狮.公司.cn"},
	{"shishi.公司.cn", "shishi.公司.cn"},
	{"公司.cn", ""},
	{"食狮.中国", "食狮.中国"},
	{"www.食狮.中国", "食狮.中国"},
	{"shishi.中国", "shishi.中国"},
	{"中国", ""},
	{"xn--85x722f.com.cn", "xn--85x722f.com.cn"},
	{"xn--85x722f.xn--55qx5d.cn", "xn--85x722f.xn--55qx5d.cn"},
	{"www.xn--85x722f.xn--55qx5d.cn", "xn--85x722f.xn--55qx5d.cn"},
	{"shishi.xn--55qx5d.cn", "shishi.xn--55qx5d.cn"},
	{"xn--55qx5d.cn", ""},
	{"xn--85x722f.xn--fiqs8s", "xn--85x722f.xn--fiqs8s"},
	{"www.xn--85x722f.xn--fiqs8s", "xn--85x722f.xn--fiqs8s"},
	{"shishi.xn--fiqs8s", "shishi.xn--fiqs8s"},
	{"xn--fiqs8s", ""},
}

func TestPSLOfficial(t *testing.T) {
	for _, c := range pslOfficial {
		if got := pslRegistrable(c.in); got != c.want {
			t.Errorf("pslRegistrable(%q) = %q, want %q", c.in, got, c.want)
		}
		// registrableDomain differs only for single labels, which are their own (intranet) site
		want := c.want
		if !strings.Contains(c.in, ".") {
			want = strings.ToLower(c.in)
		}
		if got := registrableDomain(c.in); got != want {
			t.Errorf("registrableDomain(%q) = %q, want %q", c.in, got, want)
		}
	}
}

func TestPublicSuffix(t *testing.T) {
	for _, c := range []struct {
		in, want string
		icann    bool
	}{
		{"www.bbc.co.uk", "co.uk", true},
		{"co.uk", "co.uk", true},
		{"example.com", "com", true},
		{"WWW.Example.COM.", "com", true},
		{"me.github.io", "github.io", false},
		{"x.me.github.io", "github.io", false},
		{"github.io", "github.io", false},
		{"foo.blogspot.com", "blogspot.com", false},
		{"bucket.s3.amazonaws.com", "s3.amazonaws.com", false},
		{"city.kobe.jp", "kobe.jp", true},
		{"a.b.kobe.jp", "b.kobe.jp", true},
		{"example.nosuchtld", "nosuchtld", false},
		{"www.münchen.de", "de", true},
		{"食狮.公司.cn", "公司.cn", true},
		{"xn--85x722f.xn--55qx5d.cn", "xn--55qx5d.cn", true},
		{"中国", "中国", true},
		{".com", "", false},
		{"a..com", "", false},
		{"", "", false},
	} {
		got, icann := publicSuffix(c.in)
		if got != c.want || icann != c.icann {
			t.Errorf("publicSuffix(%q) = %q, %v; want %q, %v", c.in, got, icann, c.want, c.icann)
		}
	}
}

func TestRegistrableDomain(t *testing.T) {
	for in, want := range map[string]string{
		"a.github.io":           "a.github.io",
		"x.a.github.io":         "a.github.io",
		"github.io":             "",
		"www.bbc.co.uk":         "bbc.co.uk",
		"co.uk":                 "",
		"www.example.com.":      "example.com",
		"WWW.EXAMPLE.COM":       "example.com",
		"www.MÜNCHEN.de":        "münchen.de",
		"www.xn--mnchen-3ya.de": "xn--mnchen-3ya.de",
		"a.b.公司.cn":             "b.公司.cn",
		"nas.home.arpa":         "nas.home.arpa",
		"192.168.1.10":          "192.168.1.10",
		"[2001:DB8::1]":         "2001:db8::1",
		"::ffff:10.0.0.1":       "10.0.0.1",
		"fe80::1%eth0":          "fe80::1%eth0",
		"localhost":             "localhost",
		"NAS.":                  "nas",
		"host.123":              "host.123",
		"a..b.com":              "",
		"":                      "",
	} {
		if got := registrableDomain(in); got != want {
			t.Errorf("registrableDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSameSitePSL(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"dl.example.com", "cdn.example.com", true},
		{"a.example.co.uk", "example.co.uk", true},
		{"x.a.github.io", "a.github.io", true},
		{"münchen.de", "www.xn--mnchen-3ya.de", true},
		{"Example.COM.", "example.com", true},
		{"github.io", "github.io", true},
		{"[::1]", "::1", true},
		{"10.0.0.1", "10.0.0.1", true},
		{"nas", "NAS", true},
		{"a.github.io", "b.github.io", false},
		{"github.io", "a.github.io", false},
		{"foo.blogspot.com", "bar.blogspot.com", false},
		{"a.s3.amazonaws.com", "b.s3.amazonaws.com", false},
		{"example.co.uk", "other.co.uk", false},
		{"example.com", "example.org", false},
		{"10.0.0.1", "10.0.0.2", false},
		{"1.2.3.4", "5.2.3.4", false},
		{"nas", "nas.lan", false},
		{"localhost", "example.com", false},
		{"a..example.com", "example.com", false},
	} {
		if got := sameSitePSL(c.a, c.b); got != c.want {
			t.Errorf("sameSitePSL(%q, %q) = %v", c.a, c.b, got)
		}
		if got := sameSitePSL(c.b, c.a); got != c.want {
			t.Errorf("sameSitePSL(%q, %q) = %v", c.b, c.a, got)
		}
	}
}

func TestPunycode(t *testing.T) {
	for in, want := range map[string]string{
		"ascii":   "ascii",
		"münchen": "mnchen-3ya",
		"公司":      "55qx5d",
		"中国":      "fiqs8s",
		"食狮":      "85x722f",
		// RFC 3492 §7.1 samples (A), (B), (D), (I), (L)
		"\u0644\u064A\u0647\u0645\u0627\u0628\u062A\u0643\u0644\u0645\u0648\u0634\u0639\u0631\u0628\u064A\u061F": "egbpdaj6bu4bxfgehfvwxn",
		"他们为什么不说中文":                    "ihqwcrb4cv8a8dqg056pqjye",
		"Pročprostěnemluvíčesky":       "Proprostnemluvesky-uyb24dma41a",
		"почемужеонинеговорятпорусски": "b1abfaaepdrnnbgefbadotcwatmq2g4l",
		"3年B組金八先生":                     "3B-ww4c5e180e575a65lsy2b",
	} {
		if got, ok := punycode(in); !ok || got != want {
			t.Errorf("punycode(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"\xff\xfe", strings.Repeat("é", 64)} {
		if _, ok := punycode(bad); ok {
			t.Errorf("punycode(%q) succeeded", bad)
		}
	}
}

func TestPSLRules(t *testing.T) {
	rules := pslRules()
	if len(rules) < 9000 {
		t.Fatalf("only %d rules", len(rules))
	}
	for key, want := range map[string]uint8{
		"com":           pslExact | pslExact<<3,
		"github.io":     pslExact,
		"city.kobe.jp":  pslExcept | pslExcept<<3,
		"xn--55qx5d.cn": pslExact | pslExact<<3, // 公司.cn
	} {
		if rules[key] != want {
			t.Errorf("rule %q = %b, want %b", key, rules[key], want)
		}
	}
	if rules["kobe.jp"]&pslWild == 0 {
		t.Error("*.kobe.jp missing")
	}
	// every rule in the file must survive parsing (IDN ones via punycode)
	for line := range strings.Lines(pslData) {
		line = strings.TrimSpace(strings.TrimLeft(line, "!*."))
		if line != "" && !strings.HasPrefix(line, "/") {
			if _, ok := idnaASCII(line); !ok {
				t.Errorf("rule %q doesn't encode", line)
			}
		}
	}
	if s := (pslList{}).String(); !strings.HasPrefix(s, "psl ") || len(s) != 16 {
		t.Errorf("String() = %q", s)
	}
}

// The cookie jar must keep each github.io site's cookies to itself.
func TestPSLCookieJar(t *testing.T) {
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: pslList{}})
	if err != nil {
		t.Fatal(err)
	}
	set := func(raw, domain string) {
		u, _ := url.Parse(raw)
		jar.SetCookies(u, []*http.Cookie{{Name: "s" + strings.ReplaceAll(domain, ".", ""), Value: "1", Domain: domain}})
	}
	got := func(raw string) string {
		u, _ := url.Parse(raw)
		var names []string
		for _, c := range jar.Cookies(u) {
			names = append(names, c.Name)
		}
		return strings.Join(names, ",")
	}
	set("https://a.github.io/", "github.io")     // refused: a public suffix
	set("https://x.a.github.io/", "a.github.io") // fine: the site itself
	set("https://www.bbc.co.uk/", "co.uk")       // refused
	set("https://www.bbc.co.uk/", "bbc.co.uk")
	for raw, want := range map[string]string{
		"https://b.github.io/":    "",
		"https://github.io/":      "",
		"https://y.a.github.io/":  "sagithubio",
		"https://news.bbc.co.uk/": "sbbccouk",
		"https://www.itv.co.uk/":  "",
	} {
		if g := got(raw); g != want {
			t.Errorf("cookies for %s = %q, want %q", raw, g, want)
		}
	}
}
