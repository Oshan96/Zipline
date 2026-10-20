package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCleanURLAndDedupKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"HTTP://Example.COM:80/a/b?utm_source=x&id=3&fbclid=abc#frag", "http://example.com/a/b?id=3"},
		{"https://example.com:443/", "https://example.com/"},
		{"https://example.com:8443/x", "https://example.com:8443/x"},
		{"http://example.com", "http://example.com/"},
		{"ftp://h:21/f", "ftp://h/f"},
		{"http://[::1]:80/x", "http://[::1]/x"},
		{"http://x/a;jsessionid=ABC123?x=1", "http://x/a?x=1"},
		{"http://x/a;JSESSIONID=ABC123/b", "http://x/a/b"},
		{"http://x/a?PHPSESSID=abcdef&q=1", "http://x/a?q=1"},
		{"http://x/a?jsessionid=abcdef", "http://x/a"},
		{"http://x/a?utm_medium=a&utm_campaign=b", "http://x/a"},
		{"http://x/a?mc_cid=1&mc_eid=2&gclid=3&msclkid=4&x=", "http://x/a?x="},
		{"http://x/a?b=2&a=1", "http://x/a?b=2&a=1"}, // (the order the page had is what the server gets)
		// "sid" only when its value looks like a session id
		{"http://x/a?sid=0123456789abcdef0123456789abcdef&q=1", "http://x/a?q=1"},
		{"http://x/a?sid=ab12cd34ef56gh78ij90", "http://x/a"},
		{"http://x/a?sid=12", "http://x/a?sid=12"},
		{"http://x/a?sid=abc", "http://x/a?sid=abc"},
		{"http://x/a?sid=abcdefghijklmnopqrstuvwxyz", "http://x/a?sid=abcdefghijklmnopqrstuvwxyz"},
		{"http://x/song?id=5&utm_term=z", "http://x/song?id=5"},
		{"http://x/a%20b/c.pdf?v=2", "http://x/a%20b/c.pdf?v=2"},
	}
	for _, c := range cases {
		if got := cleanURL(mustURL(t, c.in)).String(); got != c.want {
			t.Errorf("cleanURL(%s) = %s, want %s", c.in, got, c.want)
		}
	}
	same := [][2]string{
		{"http://x/a?b=2&a=1", "http://X/a?a=1&b=2#top"},
		{"http://x/a?b=2&a=1&utm_source=z", "http://x:80/a?a=1&b=2"},
		{"http://x/a;jsessionid=Q?k=v", "http://x/a?k=v"},
	}
	for _, p := range same {
		if dedupKey(mustURL(t, p[0])) != dedupKey(mustURL(t, p[1])) {
			t.Errorf("%s and %s should be one address (%s / %s)", p[0], p[1], dedupKey(mustURL(t, p[0])), dedupKey(mustURL(t, p[1])))
		}
	}
	if dedupKey(mustURL(t, "http://x/a?a=1")) == dedupKey(mustURL(t, "http://x/a?a=2")) {
		t.Error("different values are different addresses")
	}
}

// One file under several spellings is listed once, and the link it is downloaded from is the tidied one.
func TestScanListsAFileOnceWhateverTheSpelling(t *testing.T) {
	site := newRecSite(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			htmlPage(w, `<a href="/files/doc.pdf?utm_source=a">1</a> <a href="/files/doc.pdf">2</a> <a href="/files/doc.pdf#page=3">3</a>
				<a href="/files/two.pdf?b=1&amp;a=2">4</a> <a href="/files/two.pdf?a=2&amp;b=1&amp;fbclid=zz">5</a> <a href="/files/three.pdf;jsessionid=XYZ">6</a>`)
			return
		}
		http.NotFound(w, r)
	})
	m := newTestManager(t)
	j := scan(t, m, GrabOptions{URL: site.URL + "/", Types: []string{"Documents"}, Wait: 1}, 20*time.Second)
	if got := strings.Join(names(j), ","); got != "doc.pdf,three.pdf,two.pdf" {
		t.Fatalf("found %s", got)
	}
	for _, f := range j.Files {
		if strings.Contains(f.URL, "utm_") || strings.Contains(f.URL, "fbclid") || strings.Contains(f.URL, "jsessionid") || strings.Contains(f.URL, "#") {
			t.Errorf("the link should be tidied: %s", f.URL)
		}
	}
}

// A link with no extension, a script, or a ?id= in it is looked at with a HEAD request:
// a page to follow, or a file named by the server.
func TestUnknownLinksDecidedByHEAD(t *testing.T) {
	var other *recSite
	site := newRecSite(t, func(w http.ResponseWriter, r *http.Request) {
		attach := func(ct, name string) {
			w.Header().Set("Content-Type", ct)
			if name != "" {
				w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
			}
			if r.Method != "HEAD" {
				fmt.Fprint(w, "file body")
			}
		}
		switch r.URL.Path {
		case "/":
			htmlPage(w, `<a href="/download.php?id=3">dl</a> <a href="/get?file=7">get</a> <a href="/view?id=1">view</a>
				<a href="/nohead?id=9">nohead</a> <a href="/redir/5">redir</a> <a href="/about">about</a> <a href="/image?id=2">img</a>
				<a href="/style.css">css</a> <a href="/app.js">js</a> <a href="`+other.URL+`/elsewhere.php?id=1">out</a>
				<a href="`+other.URL+`/dl.php?id=1">dl out</a> <a href="`+other.URL+`/twitter">tw</a>`)
		case "/download.php":
			attach("application/pdf", "report-3.pdf")
		case "/get":
			attach("application/octet-stream", "bundle.zip")
		case "/view":
			htmlPage(w, `<a href="/files/manual.pdf">manual</a>`)
		case "/about":
			htmlPage(w, `<a href="/files/about.pdf">about</a>`)
		case "/nohead":
			if r.Method == "HEAD" {
				http.Error(w, "no HEAD here", 405)
				return
			}
			w.Header().Set("Content-Type", "application/pdf")
			w.Header().Set("Content-Disposition", `attachment; filename="nohead.pdf"`)
			w.Header().Set("Content-Range", "bytes 0-0/9999")
			w.WriteHeader(206)
			fmt.Fprint(w, "x")
		case "/redir/5":
			http.Redirect(w, r, "/files/archive.zip", 302)
		case "/files/archive.zip":
			attach("application/zip", "")
		case "/image":
			attach("image/jpeg", "")
		default:
			http.NotFound(w, r)
		}
	})
	other = newRecSite(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", `attachment; filename="outside.pdf"`)
	})
	m := newTestManager(t)
	o := GrabOptions{URL: site.URL + "/", Depth: 1, Types: []string{"Documents", "Compressed", "Images"}, Robots: true, Wait: 1}
	j := scan(t, m, o, 30*time.Second)
	want := "about.pdf,archive.zip,bundle.zip,image.jpg,manual.pdf,nohead.pdf,report-3.pdf"
	if got := strings.Join(names(j), ","); got != want {
		t.Fatalf("found %s\nwant  %s", got, want)
	}
	byName := map[string]GrabFile{}
	for _, f := range j.Files {
		byName[f.Name] = f
	}
	if f := byName["report-3.pdf"]; f.URL != site.URL+"/download.php?id=3" || f.Ext != "pdf" || f.Category != "Documents" {
		t.Errorf("the file is downloaded from the link on the page: %+v", f)
	}
	if f := byName["image.jpg"]; f.Category != "Images" {
		t.Errorf("a file matched by its Content-Type: %+v", f)
	}
	site.mu.Lock()
	defer site.mu.Unlock()
	var heads, gets, ranged int
	for _, r := range site.reqs {
		switch {
		case r.method == "HEAD":
			heads++
		case r.rng == "bytes=0-0":
			ranged++
		case strings.HasPrefix(r.path, "/download.php") || strings.HasPrefix(r.path, "/get?"):
			gets++
		}
		if r.path == "/style.css" || r.path == "/app.js" {
			t.Errorf("%s has a known ending that nobody asked for; it should be left alone", r.path)
		}
	}
	if gets != 0 {
		t.Errorf("a file found by a HEAD request is not fetched: %d GETs", gets)
	}
	if heads < 6 || ranged != 1 {
		t.Errorf("%d HEADs and %d ranged GETs", heads, ranged)
	}
	// other sites are only asked about when the link looks like a download (and the user allows other sites)
	other.mu.Lock()
	defer other.mu.Unlock()
	if len(other.reqs) != 0 {
		t.Errorf("another site was visited though other sites weren't allowed: %v", other.reqs)
	}
}

func TestUnknownLinksOnOtherSites(t *testing.T) {
	other := newRecSite(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", `attachment; filename="outside.pdf"`)
	})
	site := newRecSite(t, func(w http.ResponseWriter, r *http.Request) {
		htmlPage(w, `<a href="`+other.URL+`/dl.php?id=1">dl out</a> <a href="`+other.URL+`/twitter">tw</a> <a href="`+other.URL+`/profile?id=4">p</a>`)
	})
	m := newTestManager(t)
	j := scan(t, m, GrabOptions{URL: site.URL + "/", Depth: 1, Types: []string{"Documents"}, CrossSite: true, Wait: 1}, 30*time.Second)
	if got := strings.Join(names(j), ","); got != "outside.pdf" {
		t.Fatalf("found %s", got)
	}
	other.mu.Lock()
	defer other.mu.Unlock()
	if len(other.reqs) != 1 || !strings.HasPrefix(other.reqs[0].path, "/dl.php") {
		t.Fatalf("only the download-looking link should be checked: %v", other.reqs)
	}
}

// Checks are requests like the rest: they wait their turn, and the site's Crawl-delay counts.
func TestChecksArePaced(t *testing.T) {
	site := newRecSite(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			var b strings.Builder
			for i := 1; i <= 6; i++ {
				fmt.Fprintf(&b, `<a href="/dl?id=%d">%d</a> `, i, i)
			}
			htmlPage(w, b.String())
			return
		}
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="f%s.pdf"`, r.URL.Query().Get("id")))
	})
	site.robots = "User-agent: *\nCrawl-delay: 0.2\n"
	m := newTestManager(t)
	j := scan(t, m, GrabOptions{URL: site.URL + "/", Types: []string{"Documents"}, Robots: true, Wait: 1}, 30*time.Second)
	if len(names(j)) != 6 {
		t.Fatalf("found %v", names(j))
	}
	ts := site.times()
	if len(ts) != 8 { // robots.txt, the page and six HEADs
		t.Fatalf("%d requests", len(ts))
	}
	for i := 1; i < len(ts); i++ {
		if gap := ts[i].Sub(ts[i-1]); gap < 190*time.Millisecond {
			t.Errorf("requests %d and %d were %v apart", i-1, i, gap)
		}
	}
}

func gz(t *testing.T, s string) []byte {
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	zw.Write([]byte(s))
	zw.Close()
	return b.Bytes()
}

// Sitemaps listed in robots.txt and /sitemap.xml seed the scan: an index, gzip-compressed files,
// pages nothing links to, within the scope.
func TestSitemapsSeedTheScan(t *testing.T) {
	var site *recSite
	site = newRecSite(t, func(w http.ResponseWriter, r *http.Request) {
		xml := func(s string) { w.Header().Set("Content-Type", "application/xml"); fmt.Fprint(w, s) }
		urlset := func(locs ...string) string {
			var b strings.Builder
			b.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
			for _, l := range locs {
				b.WriteString("<url><loc>" + l + "</loc></url>")
			}
			return b.String() + "</urlset>"
		}
		switch r.URL.Path {
		case "/":
			htmlPage(w, `<p>nothing to see</p>`)
		case "/maps/index.xml.gz":
			w.Header().Set("Content-Type", "application/gzip")
			w.Write(gz(t, `<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"><sitemap><loc>`+site.URL+`/maps/pages.xml</loc></sitemap><sitemap><loc>`+site.URL+`/maps/more.xml.gz</loc></sitemap></sitemapindex>`))
		case "/maps/pages.xml":
			xml(urlset(site.URL+"/s1.html", site.URL+"/other/s2.html", "http://localhost:"+strings.Split(site.URL, ":")[2]+"/offsite.html", site.URL+"/files/direct.pdf"))
		case "/maps/more.xml.gz":
			w.Write(gz(t, urlset(site.URL+"/s3.html")))
		case "/sitemap.xml":
			xml(urlset(site.URL + "/s4.html"))
		case "/s1.html", "/other/s2.html", "/s3.html", "/s4.html", "/offsite.html":
			htmlPage(w, fmt.Sprintf(`<a href="/files%s.pdf">f</a>`, strings.TrimSuffix(r.URL.Path, ".html")))
		default:
			http.NotFound(w, r)
		}
	})
	site.robots = "User-agent: *\nDisallow: /nothing\nSitemap: " + site.URL + "/maps/index.xml.gz\n"
	m := newTestManager(t)
	o := GrabOptions{URL: site.URL + "/", Depth: 1, Types: []string{"Documents"}, Robots: true, Sitemap: true, Wait: 1}
	j := scan(t, m, o, 30*time.Second)
	if got := strings.Join(names(j), ","); got != "direct.pdf,s1.pdf,s2.pdf,s3.pdf,s4.pdf" {
		t.Fatalf("found %s (%s)", got, j.Error)
	}
	site.mu.Lock()
	for _, r := range site.reqs {
		if r.path == "/offsite.html" && r.host != strings.TrimPrefix(site.URL, "http://") {
			t.Errorf("a sitemap address on another site was visited: %s%s", r.host, r.path)
		}
	}
	n := len(site.reqs)
	site.mu.Unlock()

	// off by default, and not with "this page only"
	for _, o := range []GrabOptions{
		{URL: site.URL + "/", Depth: 1, Types: []string{"Documents"}, Robots: true, Wait: 1},
		{URL: site.URL + "/", Depth: 0, Types: []string{"Documents"}, Robots: true, Sitemap: true, Wait: 1},
	} {
		if j = scan(t, m, o, 30*time.Second); len(names(j)) != 0 {
			t.Errorf("%+v found %v", o, names(j))
		}
	}
	site.mu.Lock()
	for _, r := range site.reqs[n:] {
		if strings.Contains(r.path, "map") {
			t.Errorf("a sitemap was read though it wasn't asked for: %s", r.path)
		}
	}
	site.mu.Unlock()
}

// Pictures and fonts named in style sheets, media in <meta> tags and JSON-LD.
func TestStyleSheetsMetaAndJSONLD(t *testing.T) {
	site := newRecSite(t, func(w http.ResponseWriter, r *http.Request) {
		css := func(s string) { w.Header().Set("Content-Type", "text/css"); fmt.Fprint(w, s) }
		switch r.URL.Path {
		case "/":
			htmlPage(w, `<head>
				<meta property="og:video" content="/v/clip.mp4"><meta name="twitter:player:stream" content="/v/stream.webm">
				<meta property="og:image" content="/img/og.jpg"><meta property="og:title" content="not a link">
				<link rel="stylesheet" href="/css/site.css"><link rel="icon" href="/favicon.ico">
				<style>@import url("/css/inline-import.css"); body{background:url(/img/inline-bg.png)}</style>
				<script type="application/ld+json">{"@context":"https://schema.org","@type":"VideoObject","name":"x","contentUrl":"/v/ld.mp4","embedUrl":"/embed/xyz","thumbnailUrl":"/img/thumb.jpg"}</script>
				<script type="application/ld+json">{"@graph":[{"@type":"ImageObject","contentUrl":"/img/ld-img.png"},{"@type":["MediaObject","AudioObject"],"contentUrl":"/a/ld.mp3"},{"@type":"Person","contentUrl":"/img/not-media.png"}]}</script>
				<script type="application/ld+json">{ this is not json </script>
				</head><body></body>`)
		case "/css/site.css":
			css(`.a{background:url(../img/css-bg.webp)} @font-face{src:url("/fonts/f.woff2") format("woff2")} @import "/css/imported.css";`)
		case "/css/imported.css":
			css(`.b{background:url('/img/imported.gif')}`)
		case "/css/inline-import.css":
			css(`.c{background:url(/img/via-inline-import.svg)}`)
		default:
			http.NotFound(w, r)
		}
	})
	m := newTestManager(t)
	o := GrabOptions{URL: site.URL + "/", Types: []string{"Images", "Video", "Music"}, Exts: "woff2", Robots: true, Wait: 1}
	j := scan(t, m, o, 30*time.Second)
	want := "clip.mp4,css-bg.webp,f.woff2,imported.gif,inline-bg.png,ld-img.png,ld.mp3,ld.mp4,og.jpg,stream.webm,via-inline-import.svg"
	if got := strings.Join(names(j), ","); got != want {
		t.Fatalf("found %s\nwant  %s", got, want)
	}
	// with no picture or font type wanted the style sheets aren't read at all
	n := site.count("/css/")
	j = scan(t, m, GrabOptions{URL: site.URL + "/", Types: []string{"Video"}, Robots: true, Wait: 1}, 30*time.Second)
	if got := strings.Join(names(j), ","); got != "clip.mp4,ld.mp4,stream.webm" {
		t.Fatalf("video only: %s", got)
	}
	if site.count("/css/") != n {
		t.Fatalf("style sheets were read with no picture or font wanted")
	}
}
