package pac

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakePACNet answers DNS from hosts and myIpAddress from local, counting lookups.
func fakePACNet(t *testing.T, hosts map[string][]string, local ...string) *int32 {
	t.Helper()
	var lookups int32
	oldR, oldL := Resolver, LocalIPs
	Resolver = func(_ context.Context, host string) ([]net.IP, error) {
		atomic.AddInt32(&lookups, 1)
		var ips []net.IP
		for _, s := range hosts[host] {
			ips = append(ips, net.ParseIP(s))
		}
		if len(ips) == 0 {
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		return ips, nil
	}
	LocalIPs = func() []net.IP {
		var ips []net.IP
		for _, s := range local {
			ips = append(ips, net.ParseIP(s))
		}
		return ips
	}
	ForgetLookups()
	t.Cleanup(func() {
		Resolver, LocalIPs = oldR, oldL
		ForgetLookups()
	})
	return &lookups
}

const corporatePAC = `
/*
 * Example Corp proxy configuration
 */
var proxies = "PROXY proxy1.corp.example:8080; PROXY proxy2.corp.example:8080; DIRECT";
var direct = "DIRECT"
var intranets = [".corp.example", ".internal.example"]

function inList(host, list) {
	for (var i = 0; i < list.length; i++) {
		if (dnsDomainIs(host, list[i])) return true;
	}
	return false;
}

function isPrivate(ip) {
	return isInNet(ip, "10.0.0.0", "255.0.0.0") ||
		isInNet(ip, "172.16.0.0", "255.240.0.0") ||
		isInNet(ip, "192.168.0.0", "255.255.0.0")
}

function FindProxyForURL(url, host) {
	host = host.toLowerCase();
	if (isPlainHostName(host) || localHostOrDomainIs(host, "intranet.corp.example"))
		return direct;
	if (inList(host, intranets)) return direct   // no semicolon

	var ip = dnsResolve(host)
	if (ip && isPrivate(ip)) return "DIRECT";

	if (shExpMatch(url, "ftp:*")) return "PROXY ftpproxy.corp.example:2121";
	if (shExpMatch(host, "*.video.example") || /^cdn\d+\.example\.net$/.test(host))
		return "SOCKS5 socks.corp.example:1080; " + proxies;
	if (url.substring(0, 6) == "https:")
		return "PROXY sslproxy.corp.example:3128; " + proxies;
	return proxies;
}
`

func TestPACCorporate(t *testing.T) {
	fakePACNet(t, map[string][]string{
		"build.example.com": {"10.1.2.3"},
		"nas.example.com":   {"192.168.7.9"},
		"www.example.org":   {"93.184.216.34"},
	})
	proxies := "PROXY proxy1.corp.example:8080; PROXY proxy2.corp.example:8080; DIRECT"
	cases := []struct{ url, host, want string }{
		{"http://intranet/", "intranet", "DIRECT"},
		{"http://intranet.corp.example/x", "intranet.corp.example", "DIRECT"},
		{"http://wiki.internal.example/", "wiki.internal.example", "DIRECT"},
		{"http://build.example.com/a.zip", "build.example.com", "DIRECT"},
		{"http://NAS.example.com/", "NAS.example.com", "DIRECT"},
		{"http://www.example.org/", "www.example.org", proxies},
		{"http://nowhere.example/", "nowhere.example", proxies},
		{"ftp://ftp.example.org/pub/x.iso", "ftp.example.org", "PROXY ftpproxy.corp.example:2121"},
		{"http://cdn12.example.net/a.mp4", "cdn12.example.net", "SOCKS5 socks.corp.example:1080; " + proxies},
		{"http://live.video.example/", "live.video.example", "SOCKS5 socks.corp.example:1080; " + proxies},
		{"https://secure.example.org/", "secure.example.org", "PROXY sslproxy.corp.example:3128; " + proxies},
	}
	for _, c := range cases {
		got, err := Eval(corporatePAC, c.url, c.host)
		if err != nil || got != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.url, got, err, c.want)
		}
	}
}

// A load-balancing script of the kind WPAD servers hand out: picks a proxy by hashing
// the host, and goes direct from the office network.
const balancingPAC = `
function hash(s) {
	var h = 0
	for (var i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) % 1000003
	return h
}
function FindProxyForURL(url, host) {
	if (isInNet(myIpAddress(), "192.168.50.0", "255.255.255.0")) return "DIRECT"
	var pool = ["PROXY p0.example:3128", "PROXY p1.example:3128", "PROXY p2.example:3128"]
	var first = hash(host) % pool.length
	var out = []
	for (var k = 0; k < pool.length; k++) out.push(pool[(first + k) % pool.length])
	switch (dnsDomainLevels(host)) {
	case 0:
		return "DIRECT"
	case 1:
	case 2:
		return out.join("; ")
	default:
		return out[0] + "; DIRECT"
	}
}
`

func TestPACBalancing(t *testing.T) {
	fakePACNet(t, nil, "fd00::5", "10.20.30.40")
	got, err := Eval(balancingPAC, "http://example.com/", "example.com")
	// "example.com" hashes to 660331, so the list starts at p1
	if err != nil || got != "PROXY p1.example:3128; PROXY p2.example:3128; PROXY p0.example:3128" {
		t.Errorf("got %q, %v", got, err)
	}
	if got, _ := Eval(balancingPAC, "http://a.b.example.com/", "a.b.example.com"); !strings.HasSuffix(got, "; DIRECT") {
		t.Errorf("deep host: %q", got)
	}
	if got, _ := Eval(balancingPAC, "http://printer/", "printer"); got != "DIRECT" {
		t.Errorf("plain host: %q", got)
	}
	fakePACNet(t, nil, "192.168.50.7")
	if got, _ := Eval(balancingPAC, "http://example.com/", "example.com"); got != "DIRECT" {
		t.Errorf("in the office: %q", got)
	}
}

func TestPACFindProxyForURLEx(t *testing.T) {
	fakePACNet(t, map[string][]string{
		"v6.example":   {"2001:db8::10"},
		"dual.example": {"2001:db8::1", "10.0.0.1"},
		"v4.example":   {"203.0.113.9"},
	}, "fd00::5", "192.168.1.20")
	script := `
function FindProxyForURL(url, host) { return "PROXY legacy.example:80" }
function FindProxyForURLEx(url, host) {
	if (!isResolvableEx(host)) return "PROXY unresolved.example:8080"
	if (isInNetEx(host, "2001:db8::/32")) return "DIRECT"
	if (isInNetEx(myIpAddressEx().split(";")[0], "fd00::/8")) return "PROXY ula.example:8080; DIRECT"
	return "PROXY v4.example:8080"
}`
	for host, want := range map[string]string{
		"v6.example":   "DIRECT",
		"dual.example": "DIRECT",
		"v4.example":   "PROXY ula.example:8080; DIRECT",
		"none.example": "PROXY unresolved.example:8080",
	} {
		if got, err := Eval(script, "http://"+host+"/", host); err != nil || got != want {
			t.Errorf("%s: got %q, %v; want %q", host, got, err, want)
		}
	}
}

// pacExpr evaluates JavaScript inside FindProxyForURL, which returns it as a string.
func pacExpr(t *testing.T, body string) string {
	t.Helper()
	got, err := Eval("function FindProxyForURL(url, host) {\n"+body+"\n}", "http://www.example.com/a/b?c=d", "www.example.com")
	if err != nil {
		t.Errorf("%s\n→ error %v", body, err)
	}
	return got
}

func TestPACLanguage(t *testing.T) {
	cases := []struct{ body, want string }{
		{`var a = 1, b = 2; return String(a + b)`, "3"},
		{`return "a" + 1 + 2 + "|" + (1 + 2 + "a")`, "a12|3a"},
		{`x = 5; var x; return String(x)`, "5"},
		{`return typeof y + (function () { return typeof z; var z = 1 })()`, "undefinedundefined"},
		{`return inner(); function inner() { return "hoisted" }`, "hoisted"},
		{`function fib(n) { return n < 2 ? n : fib(n - 1) + fib(n - 2) } return String(fib(15))`, "610"},
		{`function counter() { var c = 0; return function () { return ++c } }
		  var f = counter(); f(); f(); return String(f())`, "3"},
		{`let a = 1; { let a = 2 } const b = a; return String(b)`, "1"},
		{`var s = ""; for (var i = 0; i < 10; i++) { if (i == 2) continue; if (i == 5) break; s += i } return s`, "0134"},
		{`var i = 0, s = ""; while (i < 3) s += i++; do { s += "d" } while (false); return s`, "012d"},
		{`var o = {a: 1, "b": 2, 3: "c"}; var k = []; for (var p in o) k.push(p + "=" + o[p]); return k.join("&")`, "a=1&b=2&3=c"},
		{`var s = 0; for (const x of [1, 2, 3]) s += x; for (var i in ["a", "b"]) s += i; return String(s)`, "601"},
		{`function f(x) { switch (x) { case 1: case 2: return "low"; case "a": return "str"; default: return "other" } }
		  return f(2) + f("a") + f(3) + f("1")`, "lowstrotherother"},
		{`var s = ""; switch (2) { case 1: s += 1; case 2: s += 2; case 3: s += 3; break; case 4: s += 4 } return s`, "23"},
		{`return (0 || "" || null || "x") + (1 && "y") + (null ?? "z") + (0 ?? "w")`, "xyz0"},
		{`return [1 == "1", 1 === "1", null == undefined, null === undefined, NaN == NaN, "" == 0, "0" == false, [1] == 1].join()`,
			"true,false,true,false,false,true,true,true"},
		{`return [typeof 1, typeof "s", typeof true, typeof null, typeof undefined, typeof [], typeof {}, typeof isInNet,
		  typeof function () {}].join()`, "number,string,boolean,object,undefined,object,object,function,function"},
		{`var s = "  Hello.World.Example  ".trim()
		  return [s.toLowerCase(), s.toUpperCase(), s.indexOf("."), s.lastIndexOf("."), s.substring(6, 11), s.substr(-7),
		  s.slice(0, -8), s.charAt(1), s.split(".").length, s.replace(".", "-"), s.startsWith("Hell"), s.endsWith("ple"),
		  s.includes("World"), s.length, s[0], "ab".concat("c", 1), "x".repeat(3), "7".padStart(3, "0")].join("|")`,
			"hello.world.example|HELLO.WORLD.EXAMPLE|5|11|World|Example|Hello.World|e|3|Hello-World.Example|true|true|true|19|H|abc1|xxx|007"},
		{`var m = "cdn42.example.net".match(/^cdn(\d+)\.(.*)$/)
		  return [m[1], m[2], /EXAMPLE/i.test(host), "a-b-c".replace(/-/g, "+"), "x1y22".replace(/(\d+)/, "[$1]"),
		  "a,b;c".split(/[,;]/).join(" "), "abc".search(/c/), "aXbX".match(/X/g).length, "q".match(/z/),
		  new RegExp("^www\\.", "i").test("WWW.x"), "a.b".replace(".", "$&$&"), "AbC".replace(/b/i, function (m) { return m + m })].join("|")`,
			"42|example.net|true|a+b+c|x[1]y22|a b c|2|2||true|a..b|AbbC"},
		{`var a = [3, 1, 2]; a.push(4); a.sort(); var b = a.slice(1, 3)
		  return [a.join(""), b.join(""), a.indexOf(4), a.length, [1, [2, 3]].toString(), a.map(function (x) { return x * 2 }).join(","),
		  a.filter(x => x % 2).join(","), a.some(x => x > 3), a.concat([5], 6).length, a.reverse().join(""), a.pop(), a.shift(),
		  a.includes(2), [10, 9, 1].sort(function (x, y) { return x - y }).join()].join("|")`,
			"1234|23|3|4|1,2,3|2,4,6,8|1,3|true|6|4321|1|4|true|1,9,10"},
		{`return [1 / 3 > 0.33, (0.1 + 0.2).toFixed(2), 10 % 3, -7 % 3, 2 ** 10, 1e21, 1e-7, 123.456, parseInt("42px"),
		  parseInt("0x1F"), parseInt("ff", 16), parseFloat("3.5e2xyz"), Number("12"), isNaN("abc"), Math.max(1, 5, 3),
		  Math.floor(-1.5), (255).toString(16), 5 / 0, -0, 0.1 * 3, 0x10, .5, Number("")].join()`,
			"true,0.30,1,-1,1024,1e+21,1e-7,123.456,42,31,255,350,12,true,5,-2,ff,Infinity,0,0.30000000000000004,16,0.5,0"},
		{`return [convert_addr("192.168.1.1"), convert_addr("10.0.0.1"),
		  (convert_addr("192.168.1.77") & convert_addr("255.255.255.0")) == convert_addr("192.168.1.0"),
		  1 << 31, -1 >>> 0, ~5, 6 & 3, 6 | 3, 6 ^ 3, -16 >> 2].join()`,
			"-1062731519,167772161,true,-2147483648,4294967295,-6,2,7,5,-4"},
		{`var s = ""
		  try { s += "a"; throw "boom"; s += "x" } catch (e) { s += e } finally { s += "f" }
		  try { null.x } catch (e) { s += e.name }
		  try { undefinedFn() } catch (e) { s += e.name }
		  try { try { throw 1 } finally { s += "!" } } catch (e) { s += e }
		  return s`, "aboomfTypeErrorReferenceError!1"},
		{"var a = 1 // no semicolons\nvar b = 2 /* a block\ncomment */ var c = a +\n  b\nreturn \"\" +\n  c", "3"},
		{"function f() { return\n \"x\" }\nreturn String(f())", "undefined"},
		{"var i = 0\ni\n++\ni\nreturn String(i)", "1"},
		{"var a = 1\r// old Mac line endings\rvar b = 2\rreturn String(a + b)", "3"},
		{`var o = {name: "x", list: [1, 2], nested: {k: "v"}}; o.extra = 5; o["list"].push(3); delete o.name
		  return [o.name, o.list.length, o.nested.k, o.extra, o.hasOwnProperty("extra")].join()`, ",3,v,5,true"},
		{`function sum() { var t = 0; for (var i = 0; i < arguments.length; i++) t += arguments[i]; return t }
		  var sq = x => x * x; var add = (a, b) => { return a + b }
		  var fact = function f(n) { return n <= 1 ? 1 : n * f(n - 1) }
		  return [sum(1, 2, 3), sq(4), add(1, 2), fact(5)].join()`, "6,16,3,120"},
		{`return url + " " + host`, "http://www.example.com/a/b?c=d www.example.com"},
		{`var a = 5; a += 2; a *= 3; a -= 1; a /= 4; a %= 3; var b = a++; return [a, b, --a].join()`, "3,2,2"},
		{`return ["a" < "b", "B" < "a", "10" < "9", 10 < 9, "\x41B\t".length, 'it\'s', "l1\nl2".split("\n")[1]].join("|")`,
			"true|true|true|false|3|it's|l2"},
		{`return [String([1, [2, null]]), String({}), String(null), String(undefined), [] + [], 1 + true].join("|")`,
			"1,2,|[object Object]|null|undefined||2"},
		{`function setG() { g = "global" } setG(); return g`, "global"},
		{`function f() { var isPlainHostName = 1; return isPlainHostName } return String(f()) + typeof isPlainHostName`, "1function"},
		{`var a = []; a[3] = "x"; a.length = 5; return a.length + ":" + a.join("-")`, "5:---x-"},
		{`var n = 0; for (var i = 0, j = 10; i < j; i++, j--) n++; return String(n)`, "5"},
		{`if (false) return "a"; else if (0) return "b"; else return "c"`, "c"},
		{`var r = []; [1, 2, 3].forEach(function (x, i) { r.push(x * i) }); return r.join()`, "0,2,6"},
		{`console.log("debug"); alert("hi"); return "ok"`, "ok"},
	}
	for _, c := range cases {
		if got := pacExpr(t, c.body); got != c.want {
			t.Errorf("%s\n→ %q, want %q", c.body, got, c.want)
		}
	}
}

func TestPACBuiltins(t *testing.T) {
	fakePACNet(t, map[string][]string{
		"www.netscape.com": {"198.95.249.79"},
		"dual.example":     {"2001:db8::1", "10.0.0.1"},
	}, "fd00::5", "192.168.1.20")
	cases := []struct{ expr, want string }{
		{`isPlainHostName("www")`, "true"},
		{`isPlainHostName("www.netscape.com")`, "false"},
		{`dnsDomainIs("www.netscape.com", ".netscape.com")`, "true"},
		{`dnsDomainIs("www", ".netscape.com")`, "false"},
		{`localHostOrDomainIs("www.netscape.com", "www.netscape.com")`, "true"},
		{`localHostOrDomainIs("www", "www.netscape.com")`, "true"},
		{`localHostOrDomainIs("www.mcom.com", "www.netscape.com")`, "false"},
		{`localHostOrDomainIs("home.netscape.com", "www.netscape.com")`, "false"},
		{`dnsDomainLevels("www")`, "0"},
		{`dnsDomainLevels("www.netscape.com")`, "2"},
		{`isResolvable("www.netscape.com")`, "true"},
		{`isResolvable("nope.example")`, "false"},
		{`dnsResolve("www.netscape.com")`, "198.95.249.79"},
		{`dnsResolve("dual.example")`, "10.0.0.1"},
		{`dnsResolve("nope.example")`, "null"},
		{`isInNet("www.netscape.com", "198.95.249.79", "255.255.255.255")`, "true"},
		{`isInNet("www.netscape.com", "198.95.0.0", "255.255.0.0")`, "true"},
		{`isInNet("198.95.249.79", "198.94.0.0", "255.255.0.0")`, "false"},
		{`isInNet("nope.example", "0.0.0.0", "0.0.0.0")`, "false"},
		{`myIpAddress()`, "192.168.1.20"},
		{`myIpAddressEx()`, "fd00::5;192.168.1.20"},
		{`dnsResolveEx("dual.example")`, "2001:db8::1;10.0.0.1"},
		{`dnsResolveEx("nope.example")`, ""},
		{`isResolvableEx("dual.example")`, "true"},
		{`isInNetEx("dual.example", "2001:db8::/32")`, "true"},
		{`isInNetEx("10.1.2.3", "10.0.0.0/8")`, "true"},
		{`isInNetEx("10.1.2.3", "bad")`, "false"},
		{`sortIpAddressList("10.0.0.2;2001:db8::2;10.0.0.1;2001:db8::1")`, "2001:db8::1;2001:db8::2;10.0.0.1;10.0.0.2"},
		{`sortIpAddressList("10.0.0.2;junk")`, "false"},
		{`getClientVersion()`, "1.0"},
	}
	for _, c := range cases {
		if got := pacExpr(t, "return String("+c.expr+")"); got != c.want {
			t.Errorf("%s = %q, want %q", c.expr, got, c.want)
		}
	}
}

func TestShExpMatch(t *testing.T) {
	cases := []struct {
		s, pat string
		want   bool
	}{
		{"http://home.netscape.com/people/ari/index.html", "*/ari/*", true},
		{"http://home.netscape.com/people/montulli/index.html", "*/ari/*", false},
		{"www.example.com", "*.example.com", true},
		{"example.com", "*.example.com", false},
		{"a.b", "a?b", true},
		{"ab", "a?b", false},
		{"", "*", true},
		{"abc", "a*b*c", true},
		{"abcbc", "a*bc", true},
		{"abcbd", "a*bc", false},
		{"a[b]", "a[b]", true}, // brackets are just characters
		{"Example.COM", "*.example.com", false},
	}
	for _, c := range cases {
		if got := shExpMatch(c.s, c.pat); got != c.want {
			t.Errorf("shExpMatch(%q, %q) = %v", c.s, c.pat, got)
		}
	}
}

func TestPACTimeFunctions(t *testing.T) {
	// Wednesday 18 March 2026, 14:30:15 at UTC+5:30 (09:00:15 UTC)
	now := time.Date(2026, 3, 18, 14, 30, 15, 0, time.FixedZone("LK", 5*3600+1800))
	if now.Weekday() != time.Wednesday {
		t.Fatal("bad test date")
	}
	old := pacNow
	pacNow = func() time.Time { return now }
	defer func() { pacNow = old }()
	cases := []struct {
		expr string
		want bool
	}{
		{`weekdayRange("MON", "FRI")`, true},
		{`weekdayRange("SAT")`, false},
		{`weekdayRange("WED")`, true},
		{`weekdayRange("FRI", "MON")`, false},
		{`weekdayRange("SUN", "WED")`, true},
		{`weekdayRange("TUE", "THU", "GMT")`, true},
		{`dateRange(18)`, true},
		{`dateRange(1, 15)`, false},
		{`dateRange("MAR")`, true},
		{`dateRange("JAN", "MAR")`, true},
		{`dateRange("NOV", "FEB")`, false},
		{`dateRange("DEC", "MAR")`, true},
		{`dateRange(2026)`, true},
		{`dateRange(2020, 2025)`, false},
		{`dateRange(1, "MAR", 31, "MAR")`, true},
		{`dateRange(1, "JAN", 2026, 17, "MAR", 2026)`, false},
		{`dateRange(1, "JAN", 2026, 18, "MAR", 2026)`, true},
		{`dateRange("MAR", 2026, "APR", 2026)`, true},
		{`dateRange(18, "GMT")`, true},
		{`timeRange(14)`, true},
		{`timeRange(9, 17)`, true},
		{`timeRange(15, 17)`, false},
		{`timeRange(14, 0, 14, 30)`, true},
		{`timeRange(14, 31, 15, 0)`, false},
		{`timeRange(14, 30, 0, 14, 30, 10)`, false},
		{`timeRange(22, 6)`, false},
		{`timeRange(12, 2)`, true},
		{`timeRange(9, "GMT")`, true},
		{`timeRange(14, "GMT")`, false},
	}
	for _, c := range cases {
		want := "false"
		if c.want {
			want = "true"
		}
		if got := pacExpr(t, "return String("+c.expr+")"); got != want {
			t.Errorf("%s = %s", c.expr, got)
		}
	}
	got := pacExpr(t, `var d = new Date(); return [d.getFullYear(), d.getMonth(), d.getDate(), d.getDay(), d.getHours(), d.getUTCHours(), d.getMinutes()].join()`)
	if got != "2026,2,18,3,14,9,30" {
		t.Errorf("Date: %s", got)
	}
}

func TestPACLimits(t *testing.T) {
	cases := []struct {
		body string
		want error
	}{
		{`while (true) {}`, errPACSteps},
		{`for (;;);`, errPACSteps},
		{`var i = 0; do { i++ } while (i > 0)`, errPACSteps},
		{`function f(n) { return f(n + 1) } return f(0)`, errPACDepth},
		{`function a() { return b() } function b() { return a() } return a()`, errPACDepth},
		{`var s = "x"; while (true) s += s`, errPACSize},
		{`var a = []; while (true) a.push(a.length)`, errPACSize},
		{`return "x".repeat(1e9)`, errPACSize},
		{`try { while (true) {} } catch (e) { return "caught" }`, errPACSteps}, // not catchable
		{`function f() { try { return f() } catch (e) { return "caught" } } return f()`, errPACDepth},
		{`var a = []; a.push(a); return "" + a`, nil}, // things holding themselves print, not loop
		{`var e = {}; e.name = e; e.message = e; return "" + e`, nil},
	}
	for _, c := range cases {
		start := time.Now()
		_, err := Eval("function FindProxyForURL(url, host) {\n"+c.body+"\n}", "http://x/", "x")
		if c.want == nil && err == nil {
			err = c.want
		}
		if !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.body, err, c.want)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("%s: took %v", c.body, d)
		}
	}
	// deep nesting in the source is refused, not a crash
	deep := "function FindProxyForURL(url, host) { return " + strings.Repeat("(", 100000) + "1" + strings.Repeat(")", 100000) + " }"
	if _, err := Eval(deep, "http://x/", "x"); err == nil || !strings.Contains(err.Error(), "nested too deeply") {
		t.Errorf("deep nesting: %v", err)
	}
}

func TestPACErrors(t *testing.T) {
	cases := []struct{ script, want string }{
		{"function FindProxyForURL(url, host) {\n  return \"DIRECT\"", "line 2: missing }"},
		{"function FindProxyForURL(url, host) {\n  if (x {\n  }\n}", `line 2: expected ")"`},
		{"var a = ;", `line 1: unexpected ";"`},
		{"function (a) {}", "function name missing"},
		{"return \"DIRECT\"", "return outside a function"},
		{"var s = \"unterminated;\nfunction FindProxyForURL() {}", "line 1: unterminated string"},
		{"var a b", `unexpected "b"`},
		{"function FindProxyForURL() { return `DIRECT` }", "template strings are not supported"},
		{"loop: for (;;) {}", "labels are not supported"},
		{"var x = 3abc", "bad number"},
		{"var x = 1; # comment", "unexpected character"},
		{"/* never closed", "unterminated comment"},
		{"var ok = 1", "no FindProxyForURL function"},
		{"function FindProxyForURL(url, host) { return 42 }", "returned 42, not a string"},
		{"function FindProxyForURL(url, host) { return nope }", "ReferenceError: nope is not defined"},
		{"function FindProxyForURL(url, host) { throw 'bad host' }", "PAC script error: bad host"},
		{"function FindProxyForURL(url, host) { return host.nope() }", "TypeError: host.nope is not a function"},
		{"function FindProxyForURL(url, host) { return /a(?=b)/.test(host) ? 'A' : 'B' }", "SyntaxError: unsupported regular expression"},
	}
	for _, c := range cases {
		_, err := Eval(c.script, "http://x/", "x")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: got %v, want %q", c.script, err, c.want)
		}
	}
	// a regex Go can't run only matters if it's reached
	if got := pacExpr(t, `if (host == "never") return /a(?=b)/.test(host) ? "A" : "B"; return "DIRECT"`); got != "DIRECT" {
		t.Errorf("unused regex: %q", got)
	}
}

func TestPACCaches(t *testing.T) {
	lookups := fakePACNet(t, map[string][]string{"a.example": {"10.0.0.1"}})
	script := `function FindProxyForURL(url, host) {
		return dnsResolve("a.example") + dnsResolve("a.example") + isInNet("a.example", "10.0.0.0", "255.0.0.0")
	}`
	for range 3 {
		if got, err := Eval(script, "http://x/", "x"); err != nil || got != "10.0.0.110.0.0.1true" {
			t.Fatalf("got %q, %v", got, err)
		}
	}
	if n := atomic.LoadInt32(lookups); n != 1 {
		t.Errorf("%d DNS lookups, want 1 (cached)", n)
	}
	a, _ := compilePAC(script)
	b, _ := compilePAC(script)
	if a == nil || a != b {
		t.Error("parsed script not cached")
	}
}

func TestPACConcurrent(t *testing.T) {
	fakePACNet(t, map[string][]string{"www.example.org": {"93.184.216.34"}})
	done := make(chan error)
	for range 8 {
		go func() {
			var err error
			for range 50 {
				var got string
				if got, err = Eval(corporatePAC, "http://www.example.org/", "www.example.org"); err == nil && !strings.HasPrefix(got, "PROXY proxy1") {
					err = errors.New(got)
				}
				if err != nil {
					break
				}
			}
			done <- err
		}()
	}
	for range 8 {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
}
