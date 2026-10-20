package filename

import (
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"
)

// The tc2231 suite (greenbytes.de/tech/tc2231) with the names Chrome picks, plus a few
// cases from Chrome's own http_content_disposition_unittest.cc. "" means no name, so the
// download falls back to the URL. Where we knowingly differ from Chrome, the comment says so.
var tc2231 = []struct{ id, header, want string }{
	{"inlonly", `inline`, ""},
	{"inlonlyquoted", `"inline"`, ""},
	{"inlwithasciifilename", `inline; filename="foo.html"`, "foo.html"},
	{"inlwithfnattach", `inline; filename="Not an attachment!"`, "Not an attachment!"},
	{"inlwithasciifilenamepdf", `inline; filename="foo.pdf"`, "foo.pdf"},
	{"attonly", `attachment`, ""},
	{"attonlyquoted", `"attachment"`, ""},
	{"attonlyucase", `ATTACHMENT`, ""},
	{"attwithasciifilename", `attachment; filename="foo.html"`, "foo.html"},
	{"attwithasciifilename25", `attachment; filename="0000000000111111111122222"`, "0000000000111111111122222"},
	{"attwithasciifnescapedchar", `attachment; filename="f\oo.html"`, "foo.html"},
	{"attwithasciifnescapedquote", `attachment; filename="\"quoting\" tested.html"`, `"quoting" tested.html`},
	{"attwithquotedsemicolon", `attachment; filename="Here's a semicolon;.html"`, "Here's a semicolon;.html"},
	{"attwithfilenameandextparam", `attachment; foo="bar"; filename="foo.html"`, "foo.html"},
	{"attwithfilenameandextparamescaped", `attachment; foo="\"\\";filename="foo.html"`, "foo.html"},
	{"attwithasciifilenameucase", `attachment; FILENAME="foo.html"`, "foo.html"},
	{"attwithasciifilenamenq", `attachment; filename=foo.html`, "foo.html"},
	{"attwithtokfncommanq", `attachment; filename=foo,bar.html`, "foo,bar.html"},
	{"attwithasciifilenamenqs", `attachment; filename=foo.html ;`, "foo.html"},
	{"attemptyparam", `attachment; ;filename=foo`, "foo"},
	{"attwithasciifilenamenqws", `attachment; filename=foo bar.html`, "foo bar.html"},
	{"attwithfntokensq", `attachment; filename='foo.bar'`, "'foo.bar'"},
	{"attwithisofnplain", "attachment; filename=\"foo-\xe4.html\"", "foo-ä.html"},
	{"attwithutf8fnplain", "attachment; filename=\"foo-\xc3\xa4.html\"", "foo-ä.html"},
	{"attwithfnrawpctenca", `attachment; filename="foo-%41.html"`, "foo-A.html"},
	{"attwithfnusingpct", `attachment; filename="50%.html"`, "50%.html"},
	{"attwithfnrawpctencaq", `attachment; filename="foo-%\41.html"`, "foo-A.html"},
	{"attwithnamepct", `attachment; name="foo-%41.html"`, ""},
	{"attwithfilenamepctandiso", "attachment; filename=\"\xe4-%41.html\"", "ä-%41.html"},
	{"attwithfnrawpctenclong", `attachment; filename="foo-%c3%a4-%e2%82%ac.html"`, "foo-ä-€.html"},
	{"attwithasciifilenamews1", `attachment; filename ="foo.html"`, "foo.html"},
	{"attwith2filenames", `attachment; filename="foo.html"; filename="bar.html"`, "foo.html"},
	{"attfnbrokentoken", `attachment; filename=foo[1](2).html`, "foo[1](2).html"},
	{"attfnbrokentokeniso", "attachment; filename=foo-\xe4.html", "foo-ä.html"},
	{"attfnbrokentokenutf", "attachment; filename=foo-\xc3\xa4.html", "foo-ä.html"},
	{"attmissingdisposition", `filename=foo.html`, "foo.html"},
	{"attmissingdisposition2", `x=y; filename=foo.html`, "foo.html"},
	{"attmissingdisposition3", `"foo; filename=bar;baz"; filename=qux`, "qux"}, // Chrome stops at the broken parameter: none
	{"attmissingdisposition4", `filename=foo.html, filename=bar.html`, "foo.html, filename=bar.html"},
	{"emptydisposition", `; filename=foo.html`, "foo.html"},
	{"doublecolon", `: inline; attachment; filename=foo.html`, "foo.html"}, // Chrome: none
	{"attandinline", `inline; attachment; filename=foo.html`, "foo.html"},  // Chrome: none
	{"attandinline2", `attachment; inline; filename=foo.html`, "foo.html"}, // Chrome: none
	{"attbrokenquotedfn", `attachment; filename="foo.html".txt`, `foo.html".txt`},
	{"attbrokenquotedfn2", `attachment; filename="bar`, "bar"},
	{"attbrokenquotedfn3", `attachment; filename=foo"bar;baz"qux`, `foo"bar;baz"qux`},
	{"attmultinstances", `attachment; filename=foo.html, attachment; filename=bar.html`, "foo.html, attachment"},
	{"attconfusedparam", `attachment; xfilename=foo.html`, ""},
	{"attabspath", `attachment; filename="/foo.html"`, "foo.html"},     // Chrome: _foo.html
	{"attabspathwin", `attachment; filename="\\foo.html"`, "foo.html"}, // Chrome: _foo.html
	{"attcdate", `attachment; creation-date="Wed, 12 Feb 1997 16:29:51 -0500"`, ""},
	{"dispext", `foobar`, ""},
	{"dispextbadfn", `attachment; example="filename=example.txt"`, ""},
	{"attwithisofn2231iso", `attachment; filename*=iso-8859-1''foo-%E4.html`, "foo-ä.html"},
	{"attwithfn2231utf8", `attachment; filename*=UTF-8''foo-%c3%a4-%e2%82%ac.html`, "foo-ä-€.html"},
	{"attwithfn2231noc", `attachment; filename*=''foo-%c3%a4-%e2%82%ac.html`, ""},
	{"attwithfn2231utf8comp", `attachment; filename*=UTF-8''foo-a%cc%88.html`, "foo-a\u0308.html"}, // Chrome NFC-composes: foo-ä.html
	{"attwithfn2231utf8-bad", `attachment; filename*=iso-8859-1''foo-%c3%a4-%e2%82%ac.html`, "foo-Ã¤-â‚¬.html"},
	{"attwithfn2231iso-bad", `attachment; filename*=utf-8''foo-%E4.html`, ""},
	{"attwithfn2231ws1", `attachment; filename *=UTF-8''foo-%c3%a4.html`, ""},
	{"attwithfn2231ws2", `attachment; filename*= UTF-8''foo-%c3%a4.html`, "foo-ä.html"},
	{"attwithfn2231ws3", `attachment; filename* =UTF-8''foo-%c3%a4.html`, "foo-ä.html"},
	{"attwithfn2231quot", `attachment; filename*="UTF-8''foo-%c3%a4.html"`, ""},
	{"attwithfn2231quot2", `attachment; filename*="foo%20bar.html"`, ""},
	{"attwithfn2231singleqmissing", `attachment; filename*=UTF-8'foo-%c3%a4.html`, ""},
	{"attwithfn2231nbadpct1", `attachment; filename*=UTF-8''foo%`, "foo%"},
	{"attwithfn2231nbadpct2", `attachment; filename*=UTF-8''f%oo.html`, "f%oo.html"},
	{"attwithfn2231dpct", `attachment; filename*=UTF-8''A-%2541.html`, "A-%41.html"},
	{"attwithfn2231abspathdisguised", `attachment; filename*=UTF-8''%5cfoo.html`, "foo.html"}, // Chrome: _foo.html
	{"attfncont", `attachment; filename*0="foo."; filename*1="html"`, "foo.html"},             // Chrome has no continuations: none
	{"attfncontqs", `attachment; filename*0="foo"; filename*1="\b\a\r.html"`, "foobar.html"},
	{"attfncontenc", `attachment; filename*0*=UTF-8''foo-%c3%a4; filename*1=".html"`, "foo-ä.html"},
	{"attfncontlz", `attachment; filename*0="foo"; filename*01="bar"`, "foo"},
	{"attfncontnc", `attachment; filename*0="foo"; filename*2="bar"`, "foo"},
	{"attfnconts1", `attachment; filename*1="foo."; filename*2="html"`, ""},
	{"attfncontord", `attachment; filename*1="bar"; filename*0="foo"`, "foobar"},
	{"attfnboth", `attachment; filename="foo-ae.html"; filename*=UTF-8''foo-%c3%a4.html`, "foo-ä.html"},
	{"attfnboth2", `attachment; filename*=UTF-8''foo-%c3%a4.html; filename="foo-ae.html"`, "foo-ä.html"},
	{"attfnboth3", `attachment; filename*0*=ISO-8859-15''euro-sign%3d%a4; filename*=ISO-8859-1''currency-sign%3d%a4`, "currency-sign=¤"},
	{"attnewandfn", `attachment; foobar=x; filename="foo.html"`, "foo.html"},
	{"attrfc2047token", `attachment; filename==?ISO-8859-1?Q?foo-=E4.html?=`, "foo-ä.html"},
	{"attrfc2047quoted", `attachment; filename="=?ISO-8859-1?Q?foo-=E4.html?="`, "foo-ä.html"},

	// from Chrome's unit tests
	{"chrome-whitespace", "inline; filename=\"abc  \t\nde.pdf\"", "abc    de.pdf"},
	{"chrome-pct-utf8", `attachment; filename=%E5%B2%A1%E3%80%80%E5%B2%A1.txt`, "岡\u3000岡.txt"},
	{"chrome-pct-not-utf8", `attachment; filename=%B7%DD%BD%D1.png`, "%B7%DD%BD%D1.png"}, // Chrome: none
	{"chrome-2047-utf8", `attachment; filename==?utf-8?Q?=E8=8A=B8=E8=A1=93_3=2Epng?=`, "芸術 3.png"},
	{"chrome-2047-iso88591", `inline; filename="=?iso88591?Q?caf=e9_=2epng?="`, "café .png"},
	{"chrome-2047-two-words", `inline; filename="=?iso88591?Q?caf=E9_?= =?utf-8?Q?=2epng?="`, "café .png"},
	{"chrome-2047-bad-hex", `attachment; filename==?iso88591?Q?caf=EG?=`, ""},
	{"chrome-2047-unterminated", `attachment; filename==?iso88591?Q?caf=E3?`, ""},
	{"chrome-2047-trailing", `attachment; filename==?iso88591?Q?caf=E3?==`, ""},
	{"chrome-2047-euc-kr", `attachment; filename==?eUc-Kr?b?v7m8+iAzLnBuZw==?=`, ""}, // Chrome reads EUC-KR; we only know UTF-8 and Latin-1
	{"chrome-ext-lang", `attachment; filename*=utf-8'en'%E5%B2%A1%E3%80%80%E5%B2%A1.txt`, "岡\u3000岡.txt"},
	{"chrome-ext-space", `inline; filename*=iso88591''cafe foo.png`, "cafe foo.png"},
	{"chrome-ext-1252", `attachment; filename*=windows-1252''caf%E9.png`, "café.png"},
	{"chrome-ext-raw-byte", "attachment; filename*=iso-8859-1'es'f\xfa.txt", ""},
	{"chrome-ext-no-quotes", `attachment; filename*=foo.html`, ""},
	{"chrome-ext-bogus-charset", `attachment; filename*=bogus_charset''foo.html`, ""},
	{"chrome-ext-extra-quote", `attachment; filename*=''foo'.html`, ""},
	{"chrome-name-not-filename", `inline; name="abcde.pdf"`, ""},
	{"chrome-name-and-filename", `attachment; name="foo"; filename="bar"`, "bar"},
	{"chrome-ext-wins", `attachment; filename="bar"; filename*=utf-8''baz`, "baz"},
	{"chrome-unbalanced", `attachment; filename="abcdef.pdf`, "abcdef.pdf"},
	{"chrome-spaced-equals", "filename \t\t  =abcde.pdf", "abcde.pdf"},
}

func TestContentDispositionTC2231(t *testing.T) {
	for _, c := range tc2231 {
		got, ok := ParseContentDisposition(c.header)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("%s: %q → %q %v, want %q", c.id, c.header, got, ok, c.want)
		}
	}
}

// Headers seen from real servers that Go's mime.ParseMediaType rejects or misreads.
func TestContentDispositionRealWorld(t *testing.T) {
	for _, c := range []struct{ header, want string }{
		{`attachment; filename=My Report 2024.pdf`, "My Report 2024.pdf"},
		{`attachment; filename="report.zip"; filename*=UTF-8''report.zip`, "report.zip"}, // S3
		{`attachment; filename=foo.zip; filename*=UTF-8''foo.zip`, "foo.zip"},            // GitHub
		{`attachment; filename="disk image.img"; filename*=UTF-8''disk%20image.img`, "disk image.img"},
		{`attachment; filename="otchet.pdf"; filename*=UTF-8''%D0%9E%D1%82%D1%87%D1%91%D1%82.pdf`, "Отчёт.pdf"},
		{`attachment; filename*=UTF-8''%E6%97%A5%E6%9C%AC%E8%AA%9E.txt`, "日本語.txt"},
		{`attachment; filename="?????.pdf"; filename*=UTF-8''%E2%82%AC%20rates.pdf`, "€ rates.pdf"},
		{`Attachment; FileName*=utf-8''R%C3%A9sum%C3%A9.pdf`, "Résumé.pdf"},
		{`attachment; filename="my%20file.zip"`, "my file.zip"},
		{`attachment; filename="資料.pdf"`, "資料.pdf"},
		{`attachment; filename="=?utf-8?B?0J7RgtGH0ZHRgi5wZGY=?="`, "Отчёт.pdf"},
		{"attachment; filename=\x93quoted\x94 \x80.txt", "“quoted” €.txt"}, // raw Windows-1252
		{`attachment;filename="foo.zip";`, "foo.zip"},
		{`  attachment ;; ; filename = "a b.txt" ;; `, "a b.txt"},
		{`attachment; filename=C:\Users\me\a.txt`, "a.txt"},
		{`attachment; filename="../../etc/passwd"`, "passwd"},
		{`attachment; filename="dir/"`, "dir"},
		{`attachment; filename=""; filename="b.txt"`, "b.txt"},
		{`attachment; filename*=UTF-8''%FF.txt; filename="ok.txt"`, "ok.txt"},
		{`attachment; filename*=UTF-8''a.txt; filename*=UTF-8''b.txt`, "a.txt"},
		{`attachment; filename=".."; filename*=UTF-8''..`, ""},
		{`attachment; filename=""`, ""},
		{``, ""},
	} {
		got, ok := ParseContentDisposition(c.header)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("%q → %q %v, want %q", c.header, got, ok, c.want)
		}
	}
	if n, _ := ParseContentDisposition(`attachment; filename="\"quoting\" tested.html"`); Clean(n) != "_quoting_ tested.html" {
		t.Fatal(Clean(n))
	}
}

// Names the cleaner has always handled this way (downloads are named through it).
var sanitizeCases = []struct{ in, want string }{
	{"plain.txt", "plain.txt"},
	{"  spaced out.txt  ", "spaced out.txt"},
	{`a<b>c:d"e/f\g|h?i*j.txt`, "a_b_c_d_e_f_g_h_i_j.txt"},
	{"tab\there\x00nul\x1f.bin", "tab_here_nul_.bin"},
	{"del\x7f.bin", "del_.bin"},
	{"c1\u0085\u009f.bin", "c1__.bin"},
	{"invoice\u202efdp.exe", "invoicefdp.exe"},
	{"a\u200bb\ufeffc\u2066d\u00ade.txt", "abcde.txt"},
	{"family 👨\u200d👩\u200d👧.jpg", "family 👨\u200d👩\u200d👧.jpg"},
	{"می\u200cخواهم.txt", "می\u200cخواهم.txt"},
	{"...hidden", "hidden"},
	{"trailing.txt. . .", "trailing.txt"},
	{"ok\xffbyte.txt", "ok\ufffdbyte.txt"},
	{"..", "download"},
	{" . . ", "download"},
	{"", "download"},
	{"\u202e", "download"},
	{"日本語.txt", "日本語.txt"},
}

func TestCleanFileName(t *testing.T) {
	cases := append([]struct{ in, want string }{
		// Windows devices, with or without an extension, in any case
		{"con", "_con"}, {"CON.txt", "_CON.txt"}, {"Aux", "_Aux"}, {"nul.tar.gz", "_nul.tar.gz"},
		{"prn", "_prn"}, {"com0", "_com0"}, {"COM9.log", "_COM9.log"}, {"lpt5", "_lpt5"},
		{"COM¹.txt", "_COM¹.txt"}, {"lpt³", "_lpt³"}, {"CONIN$", "_CONIN$"}, {"conout$.log", "_conout$.log"},
		{"con .txt", "_con .txt"}, {"clock$", "_clock$"}, {"Desktop.ini", "_Desktop.ini"}, {"thumbs.db", "_thumbs.db"},
		{"con\u202e.txt", "_con.txt"},
		// …but not names that only start like one
		{"console.txt", "console.txt"}, {"com10.txt", "com10.txt"}, {"lpt", "lpt"}, {"aux_file.txt", "aux_file.txt"},
		{"nul-1", "nul-1"}, {"_con", "_con"}, {"desktop.ini.txt", "desktop.ini.txt"},
		// endings the shell acts on
		{"evil.lnk", "evil.download"}, {"x.LNK", "x.download"}, {"site.url", "site.download"},
		{"cmd.scf", "cmd.download"}, {"app.exe.local", "app.exe.download"}, {"run.desktop", "run.download"},
		{"a.{20D04FE0-3AEA-1069-A2D8-08002B30309D}", "a.download"}, {"evil.lnk. . ", "evil.download"},
		{"con.lnk", "_con.download"}, {"lnk", "lnk"}, {"file.lnk.txt", "file.lnk.txt"}, {"x.urls", "x.urls"},
		// spaces of any kind at the ends
		{"\u00a0nbsp\u3000", "nbsp"},
	}, sanitizeCases...)
	for _, c := range cases {
		if got := Clean(c.in); got != c.want {
			t.Errorf("cleanFileName(%q) = %q, want %q", c.in, got, c.want)
		}
		if again := Clean(c.want); again != c.want {
			t.Errorf("cleaning %q again gave %q", c.want, again)
		}
	}
}

func longNames() []string {
	a := strings.Repeat("a", 300)
	return []string{a + ".mp4", strings.Repeat("é", 150) + ".mkv", a, strings.Repeat("a", 199) + "€bbb",
		strings.Repeat("a", 250) + "." + strings.Repeat("x", 30), strings.Repeat("日本", 60) + ".tar.gz"}
}

func TestCleanFileNameLength(t *testing.T) {
	for _, n := range append(longNames(), strings.Repeat("a", 195)+"     "+strings.Repeat("b", 50), "con."+strings.Repeat("a", 300)) {
		got := Clean(n)
		if len(got) > 200 || !utf8.ValidString(got) || Clean(got) != got {
			t.Errorf("%d bytes, valid %v: %q", len(got), utf8.ValidString(got), got)
		}
	}
	for in, want := range map[string]string{
		strings.Repeat("a", 300) + ".mp4":                            strings.Repeat("a", 196) + ".mp4",
		strings.Repeat("a", 199) + "€bbb":                            strings.Repeat("a", 199), // € would be split
		strings.Repeat("a", 195) + "     " + strings.Repeat("b", 50): strings.Repeat("a", 195), // no trailing spaces
		"con." + strings.Repeat("a", 300):                            "_con." + strings.Repeat("a", 195),
	} {
		if got := Clean(in); got != want {
			t.Errorf("cleanFileName(%d bytes) = %q", len(in), got)
		}
	}
	if got := Clean(strings.Repeat("é", 150) + ".mkv"); !strings.HasSuffix(got, ".mkv") || len(got) != 200 {
		t.Errorf("%d bytes: %q", len(got), got)
	}
}

func TestNameFromURLPath(t *testing.T) {
	for in, want := range map[string]string{
		"/files/report.pdf":              "report.pdf",
		"/files/my%20report.pdf":         "my report.pdf",
		"/%E6%97%A5%E6%9C%AC.txt":        "日本.txt",
		"/caf%E9.png":                    "caf%E9.png", // not UTF-8: kept as sent
		"/x/%zz.bin":                     "%zz.bin",
		"/dir/":                          "dir",
		"/dir//":                         "dir",
		"/x/..":                          "x",
		"/x/%20":                         "x",
		"/download.php?id=3&next=/y.zip": "download.php",
		"/a/b.zip#part/2":                "b.zip",
		"/file.zip;jsessionid=ABC123":    "file.zip",
		"/dir/;jsessionid=ABC":           "dir",
		"/a%2Fb.zip":                     "a/b.zip", // Clean makes it a_b.zip
		`/a\b.txt`:                       "b.txt",
		"file.iso":                       "file.iso",
		"/":                              "",
		"":                               "",
	} {
		if got := FromURLPath(in); got != want {
			t.Errorf("nameFromURLPath(%q) = %q, want %q", in, got, want)
		}
	}
	for raw, want := range map[string]string{
		"https://h/p/na%C3%AFve%20file.zip?dl=1": "naïve file.zip",
		"https://h/p/caf%E9.png":                 "caf%E9.png",
		"https://h/a%2Fb.zip":                    "a/b.zip",
		"https://h/50%25.txt":                    "50%.txt",
		"https://h/":                             "",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := FromURLPath(u.EscapedPath()); got != want {
			t.Errorf("%s → %q, want %q", raw, got, want)
		}
	}
}

// Whatever a server sends, a name comes back clean: one component, valid UTF-8, short.
func FuzzContentDisposition(f *testing.F) {
	for _, c := range tc2231 {
		f.Add(c.header)
	}
	f.Fuzz(func(t *testing.T, h string) {
		n, ok := ParseContentDisposition(h)
		if !ok {
			return
		}
		c := Clean(n)
		if strings.ContainsAny(n, `/\`) || c == "" || len(c) > 200 || !utf8.ValidString(c) || Clean(c) != c {
			t.Fatalf("%q → %q → %q", h, n, c)
		}
	})
}
