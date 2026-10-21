package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func mustURL(t testing.TB, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// The example robots.txt of RFC 9309 section 5.1, asked about by each of the crawlers it names.
func TestRobotsRFC9309Groups(t *testing.T) {
	const file = `User-Agent: *
Disallow: *.gif$
Disallow: /example/
Allow: /publications/

User-Agent: foobot
Disallow:/
Allow:/example/page.html
Allow:/example/allowed.gif

User-Agent: barbot
User-Agent: bazbot
Disallow: /example/page.html

User-Agent: quxbot
`
	cases := []struct {
		agent, path string
		want        bool
	}{
		{"Zipline", "/other.html", true}, // no group of its own: the * group
		{"Zipline", "/example/page.html", false},
		{"Zipline", "/pic.gif", false},
		{"Zipline", "/pic.gif?x=1", true},          // ($ means the end of the address, query included)
		{"Zipline", "/publications/pic.gif", true}, // (the longer Allow wins)
		{"Zipline", "/publications/list.html", true},
		{"foobot", "/other.html", false},
		{"foobot", "/example/page.html", true},
		{"foobot", "/example/allowed.gif", true},
		{"foobot", "/example/other.gif", false},
		{"FOOBOT", "/example/page.html", true}, // (names match in any case)
		{"barbot", "/example/page.html", false},
		{"bazbot", "/example/page.html", false},
		{"barbot", "/example/other.html", true}, // (the * group is not added to a named one)
		{"barbot", "/pic.gif", true},
		{"quxbot", "/example/page.html", true}, // an empty group allows everything
		{"quxbot", "/", true},
	}
	for _, c := range cases {
		if got := parseRobots(file, c.agent).allowed(mustURL(t, "http://example.com"+c.path)); got != c.want {
			t.Errorf("%s %s: allowed=%v, want %v", c.agent, c.path, got, c.want)
		}
	}
}

// Several groups for the same crawler are joined; "Zipline/1.0" in a User-agent line is the product Zipline.
func TestRobotsGroupsJoinAndProductToken(t *testing.T) {
	r := parseRobots("User-agent: zipline/2.0\nDisallow: /a/\n\nUser-agent: *\nDisallow: /everyone/\n\nUser-agent: ZIPLINE\nDisallow: /b/\n", "Zipline")
	for p, want := range map[string]bool{"/a/x": false, "/b/x": false, "/everyone/x": true, "/c": true} {
		if got := r.allowed(mustURL(t, "http://h"+p)); got != want {
			t.Errorf("%s: %v, want %v", p, got, want)
		}
	}
	r = parseRobots("User-agent: ziplinebot\nDisallow: /\n", "Zipline") // another crawler's name that starts the same
	if !r.allowed(mustURL(t, "http://h/x")) {
		t.Error("ziplinebot is not Zipline")
	}
}

// The path-matching examples (RFC 9309 section 2.2.3 and the Robots Exclusion documentation).
func TestRobotsPathMatching(t *testing.T) {
	cases := []struct {
		pattern string
		match   []string
		miss    []string
	}{
		{"/fish", []string{"/fish", "/fish.html", "/fish/salmon.html", "/fishheads", "/fishheads/yummy.html", "/fish.php?id=anything"},
			[]string{"/Fish.asp", "/catfish", "/?id=fish", "/desert/fish"}},
		{"/fish*", []string{"/fish", "/fish.html", "/fish/salmon.html", "/fishheads", "/fish.php?id=anything"},
			[]string{"/Fish.asp", "/catfish", "/?id=fish"}},
		{"/fish/", []string{"/fish/", "/fish/?id=anything", "/fish/salmon.htm"}, []string{"/fish", "/fish.html", "/Fish/Salmon.asp"}},
		{"/*.php", []string{"/filename.php", "/folder/filename.php", "/folder/filename.php?parameters", "/folder/any.php.file.html", "/filename.php/"},
			[]string{"/", "/windows.PHP"}},
		{"/*.php$", []string{"/filename.php", "/folder/filename.php"}, []string{"/filename.php?parameters", "/filename.php/", "/filename.php5", "/windows.PHP"}},
		{"/fish*.php", []string{"/fish.php", "/fishheads/catfish.php?parameters"}, []string{"/Fish.PHP"}},
		{"/a*b*c$", []string{"/abc", "/aXbYc", "/abcbc"}, []string{"/abcd", "/ab", "/aXbY"}},
		{"/*?id=", []string{"/x?id=1", "/y/z?id=2"}, []string{"/x"}},
	}
	for _, c := range cases {
		r := parseRobots("User-agent: *\nDisallow: "+c.pattern+"\n", "Zipline")
		for _, p := range c.match {
			if r.allowed(mustURL(t, "http://h"+p)) {
				t.Errorf("Disallow: %s should block %s", c.pattern, p)
			}
		}
		for _, p := range c.miss {
			if !r.allowed(mustURL(t, "http://h"+p)) {
				t.Errorf("Disallow: %s should not block %s", c.pattern, p)
			}
		}
	}
}

// The longest rule wins, and Allow wins when two are as long.
func TestRobotsLongestMatchWins(t *testing.T) {
	cases := []struct {
		rules, path string
		want        bool
	}{
		{"Allow: /p\nDisallow: /", "/page", true},
		{"Disallow: /\nAllow: /p", "/page", true}, // (the order doesn't matter)
		{"Allow: /folder\nDisallow: /folder", "/folder/page", true},
		{"Disallow: /folder\nAllow: /folder", "/folder/page", true},
		{"Allow: /page\nDisallow: /*.htm", "/page.htm", false}, // the Disallow is longer
		{"Allow: /$\nDisallow: /", "/", true},
		{"Allow: /$\nDisallow: /", "/page.htm", false},
		{"Allow: /page\nDisallow: /*.ph", "/page.php5", true}, // as long: Allow
		{"Disallow:", "/anything", true},                      // an empty Disallow blocks nothing
		{"Disallow: /private\nAllow: /private/open/", "/private/open/a", true},
		{"Disallow: /private\nAllow: /private/open/", "/private/closed/a", false},
	}
	for _, c := range cases {
		if got := parseRobots("User-agent: *\n"+c.rules+"\n", "Zipline").allowed(mustURL(t, "http://h"+c.path)); got != c.want {
			t.Errorf("%q on %s: %v, want %v", c.rules, c.path, got, c.want)
		}
	}
}

// Both sides are compared percent-encoded the same way (RFC 9309 section 2.2.2).
func TestRobotsPercentEncoding(t *testing.T) {
	cases := []struct{ rule, path string }{
		{"/foo/bar?baz=quz", "/foo/bar?baz=quz"},
		{"/foo/bar/ツ", "/foo/bar/%E3%83%84"}, // the rule in UTF-8, the address encoded
		{"/foo/bar/%E3%83%84", "/foo/bar/ツ"},
		{"/foo/bar/%e3%83%84", "/foo/bar/%E3%83%84"},
		{"/foo/bar/%62%61%7A", "/foo/bar/baz"}, // encoded letters are the letters
		{"/foo/bar/baz", "/foo/bar/%62%61%7A"},
		{"/a%2Fb", "/a%2fb"}, // (a slash stays encoded: it is not the same as a path separator)
	}
	for _, c := range cases {
		r := parseRobots("User-agent: *\nDisallow: "+c.rule+"\n", "Zipline")
		if r.allowed(mustURL(t, "http://h"+c.path)) {
			t.Errorf("Disallow: %s should block %s", c.rule, c.path)
		}
	}
	r := parseRobots("User-agent: *\nDisallow: /a%2Fb\n", "Zipline")
	if !r.allowed(mustURL(t, "http://h/a/b")) {
		t.Error("an encoded slash is not a slash")
	}
}

func TestRobotsCrawlDelayAndSitemaps(t *testing.T) {
	r := parseRobots("Sitemap: http://h/s1.xml\nUser-agent: *\nCrawl-delay: 2.5\nDisallow: /x\n\nUser-agent: Zipline\nCrawl-delay: 10\n# c\nSitemap: http://h/s2.xml\n", "Zipline")
	if r.delay != 10*time.Second || len(r.sitemaps) != 2 {
		t.Fatalf("delay %v, sitemaps %v", r.delay, r.sitemaps)
	}
	if !r.allowed(mustURL(t, "http://h/x")) { // (the Zipline group has no rules; * isn't joined to it)
		t.Fatal("only the Zipline group counts")
	}
	if r = parseRobots("User-agent: *\nCrawl-delay: 2.5\n", "Zipline"); r.delay != 2500*time.Millisecond {
		t.Fatalf("delay %v", r.delay)
	}
	if r = parseRobots("User-agent: *\r\nCrawl-delay: soon\r\nDisallow: /y\r\n", "Zipline"); r.delay != 0 || r.allowed(mustURL(t, "http://h/y")) {
		t.Fatal("CRLF lines, and a delay that isn't a number is ignored")
	}
}

// What the answer to the robots.txt request means (RFC 9309 section 2.3.1).
func TestRobotsAnswers(t *testing.T) {
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/ok/robots.txt", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "User-agent: *\nDisallow: /secret\n") })
	for _, code := range []int{401, 403, 404, 410} {
		mux.HandleFunc(fmt.Sprintf("/c%d/robots.txt", code), func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", code) })
	}
	for _, code := range []int{500, 502, 503} {
		mux.HandleFunc(fmt.Sprintf("/c%d/robots.txt", code), func(w http.ResponseWriter, r *http.Request) { http.Error(w, "broken", code) })
	}
	mux.HandleFunc("/c429/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		http.Error(w, "slow down", 429)
	})
	// redirects: /hopN/… goes N times through the redirect before the file
	for _, hops := range []int{5, 6} {
		mux.HandleFunc(fmt.Sprintf("/hop%d/robots.txt", hops), func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, fmt.Sprintf("/chain/%d/%d", hops, 1), 302)
		})
	}
	mux.HandleFunc("/chain/", func(w http.ResponseWriter, r *http.Request) {
		var hops, n int
		fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/chain/"), "%d/%d", &hops, &n)
		if n < hops {
			http.Redirect(w, r, fmt.Sprintf("/chain/%d/%d", hops, n+1), 302)
			return
		}
		fmt.Fprint(w, "User-agent: *\nDisallow: /\n")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	hits.Store(0)

	read := func(dir string) (*robotsEntry, *hostPace) {
		pace := newHostPace()
		// the "site" is srv.URL + dir: readRobots asks origin + /robots.txt, so give it a base that ends in the folder
		e := readRobots(context.Background(), &Download{}, srv.URL+dir, pace)
		return e, pace
	}
	blocked := mustURL(t, srv.URL+"/secret")

	e, _ := read("/ok")
	if e.down || e.rules == nil || e.allowed(blocked) || !e.allowed(mustURL(t, srv.URL+"/fine")) {
		t.Fatalf("200: %+v", e)
	}
	for _, c := range []string{"/c401", "/c403", "/c404", "/c410"} { // no file: everything is allowed
		e, _ := read(c)
		if e.down || e.rules != nil || !e.allowed(blocked) {
			t.Errorf("%s should allow everything: %+v", c, e)
		}
	}
	for _, c := range []string{"/c500", "/c502", "/c503"} { // the server can't say: nothing is allowed
		e, _ := read(c)
		if !e.down || e.allowed(mustURL(t, srv.URL+"/fine")) || e.why == "" {
			t.Errorf("%s should disallow everything: %+v", c, e)
		}
	}
	e, pace := read("/c429") // asked to slow down: nothing for now, and the site is left alone as long as it says
	if !e.down {
		t.Errorf("429: %+v", e)
	}
	if until := pace.leftAlone(hostKey(mustURL(t, srv.URL))); time.Until(until) < 5*time.Second || time.Until(until) > 8*time.Second {
		t.Errorf("429 with Retry-After: 7 should hold the site back about 7 s, until %v", time.Until(until))
	}
	if e, _ = read("/hop5"); e.down || e.rules == nil || e.allowed(mustURL(t, srv.URL+"/x")) {
		t.Errorf("five redirects are followed: %+v", e)
	}
	if e, _ = read("/hop6"); e.down || e.rules != nil {
		t.Errorf("six redirects: the file is as good as missing: %+v", e)
	}
	// nobody there at all
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	if e = readRobots(context.Background(), &Download{}, deadURL, newHostPace()); !e.down {
		t.Errorf("no connection should disallow everything: %+v", e)
	}
}

func TestRobotsEntryExpiry(t *testing.T) {
	st := newRobotsStore()
	e := &robotsEntry{at: time.Now().Add(-robotsTTL + time.Minute)}
	st.put("o", e)
	if st.get("o") == nil {
		t.Fatal("an answer is kept for 24 hours")
	}
	e.at = time.Now().Add(-robotsTTL - time.Minute)
	if st.get("o") != nil {
		t.Fatal("…and not longer")
	}
	old := robotsDownTTL
	robotsDownTTL = time.Hour
	defer func() { robotsDownTTL = old }()
	st.put("d", &robotsEntry{down: true, at: time.Now()})
	if st.get("d") == nil {
		t.Fatal("a server that couldn't answer is remembered for a moment")
	}
}
