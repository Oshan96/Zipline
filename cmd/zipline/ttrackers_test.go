package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"zipline/internal/bencode"
)

func waitCond(t *testing.T, max time.Duration, what string, f func() bool) {
	t.Helper()
	for end := time.Now().Add(max); time.Now().Before(end); time.Sleep(25 * time.Millisecond) {
		if f() {
			return
		}
	}
	t.Fatal("timed out: " + what)
}

// fakeTracker is an HTTP tracker that writes down what it's asked.
type fakeTracker struct {
	*httptest.Server
	mu    sync.Mutex
	hits  []url.Values
	reply func(q url.Values, n int) (int, []byte)
}

func newFakeTracker(t *testing.T, reply func(q url.Values, n int) (int, []byte)) *fakeTracker {
	f := &fakeTracker{reply: reply}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits = append(f.hits, r.URL.Query())
		n := len(f.hits)
		f.mu.Unlock()
		code, body := f.reply(r.URL.Query(), n)
		w.WriteHeader(code)
		w.Write(body)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeTracker) announce() string { return f.URL + "/announce" }

func (f *fakeTracker) events() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, q := range f.hits {
		out = append(out, q.Get("event"))
	}
	return out
}

func (f *fakeTracker) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.hits)
}

func (f *fakeTracker) hit(i int) url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < len(f.hits) {
		return f.hits[i]
	}
	return url.Values{}
}

func okReply(extra map[string]any) func(url.Values, int) (int, []byte) {
	return func(url.Values, int) (int, []byte) {
		d := map[string]any{"interval": int64(1800), "peers": ""}
		for k, v := range extra {
			d[k] = v
		}
		return 200, bencode.Encode(d)
	}
}

func failReply(why string) func(url.Values, int) (int, []byte) {
	return func(url.Values, int) (int, []byte) { return 200, bencode.Encode(map[string]any{"failure reason": why}) }
}

// idleTorrent is a torrent nobody has (no web seed), with these trackers; it keeps running.
func idleTorrent(t *testing.T, tiers ...[]string) []byte {
	tor, _, _ := webSeedTorrent(t)
	v, _ := bencode.Decode(tor)
	top := v.(map[string]any)
	delete(top, "url-list")
	var list []any
	for _, tier := range tiers {
		list = append(list, tier)
	}
	if len(list) > 0 {
		top["announce"] = tiers[0][0]
		top["announce-list"] = list
	}
	return bencode.Encode(top)
}

// addStopped adds a torrent without starting it, with its trackers in exactly this order.
func addStopped(t *testing.T, m *Manager, tor []byte, tiers [][]string) *Download {
	mi, err := parseTorrent(tor)
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.rememberTorrent(mi, tor)
	m.mu.Unlock()
	d, err := m.AddTorrent(TorrentAdd{Hash: mi.HashHex(), Dir: filepath.Join(m.settings.DownloadDir, "got")})
	if err != nil {
		t.Fatal(err)
	}
	if tiers != nil {
		if _, err := m.SetTorrentTrackers(d.ID, tiers); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func fakeTorrent(urls ...string) *torrent {
	t := &torrent{d: &Download{ID: "x"}, peers: map[netip.AddrPort]*tpeer{}}
	for _, u := range urls {
		t.trackers = append(t.trackers, &trackerState{url: u, status: "Waiting"})
	}
	return t
}

// ---------------------------------------------------------------- the rules

func TestTrackerBackoff(t *testing.T) {
	for _, c := range []struct {
		fails  int
		minGap time.Duration
		want   time.Duration
	}{
		{1, 0, time.Minute}, {2, 0, 4 * time.Minute}, {3, 0, 9 * time.Minute}, {7, 0, 49 * time.Minute},
		{8, 0, time.Hour}, {1000, 0, time.Hour}, // an hour at most
		{1, 30 * time.Minute, 30 * time.Minute}, {2, 5 * time.Minute, 5 * time.Minute}, // never sooner than its min interval
		{1, 3 * time.Hour, time.Hour},
	} {
		if got := trackerBackoff(c.fails, c.minGap); got != c.want {
			t.Errorf("trackerBackoff(%d, %v) = %v, want %v", c.fails, c.minGap, got, c.want)
		}
	}
}

func TestCleanTiers(t *testing.T) {
	got, err := cleanTiers([][]string{{" http://a/announce ", "http://a/announce", ""}, {}, {"udp://b:80", "https://c/x"}, {"http://a/announce"}})
	if err != nil || fmt.Sprint(got) != "[[http://a/announce] [udp://b:80 https://c/x]]" {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{"ftp://x/y", "not a url", "http://", "magnet:?xt=1"} {
		if _, err := cleanTiers([][]string{{bad}}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	var many []string
	for i := range maxTrackers + 1 {
		many = append(many, fmt.Sprintf("http://t%d.example/announce", i))
	}
	if _, err := cleanTiers([][]string{many}); err == nil {
		t.Error("too many trackers accepted")
	}
	if got, err := cleanTiers(nil); err != nil || len(got) != 0 {
		t.Errorf("none: %v %v", got, err)
	}
}

// Each tier is shuffled when the torrent is added, and stays in its place.
func TestInitialTiersShuffleOnce(t *testing.T) {
	var first, other []string
	for i := 0; i < 8; i++ {
		first = append(first, fmt.Sprintf("http://a%d.example/announce", i))
	}
	other = []string{"http://z.example/announce"}
	mi := &Metainfo{Trackers: [][]string{first, other}}
	shuffled := false
	for range 20 {
		tiers := initialTiers([]string{"http://m1.example/x", "http://z.example/announce"}, mi)
		if len(tiers) != 3 || len(tiers[0]) != 8 || tiers[1][0] != other[0] || fmt.Sprint(tiers[2]) != "[http://m1.example/x]" {
			t.Fatalf("tiers: %v", tiers)
		}
		if !slices.Equal(tiers[0], first) {
			shuffled = true
		}
		got := slices.Clone(tiers[0])
		slices.Sort(got)
		if !slices.Equal(got, first) {
			t.Fatalf("a tracker was lost or made up: %v", tiers[0])
		}
	}
	if !shuffled {
		t.Error("the first tier was never shuffled")
	}
}

func TestClientRefusedAndPlainWords(t *testing.T) {
	for _, why := range []string{"Client is not on the whitelist", "client not whitelisted", "Your client is banned", "unknown client", "Unregistered client rejected", "BLACKLISTED"} {
		if !clientRefused(why) {
			t.Errorf("%q should read as a refusal", why)
		}
		if got := trackerErrText(fmt.Errorf("tracker: %s", why)); got != refusedText {
			t.Errorf("%q: %q", why, got)
		}
	}
	for _, why := range []string{"Torrent not registered with this tracker", "info_hash is not allowed", "too many requests", "server is overloaded", ""} {
		if clientRefused(why) {
			t.Errorf("%q isn't about the client", why)
		}
	}
	if got := trackerErrText(&trackerFailure{Reason: "Torrent not  registered\n with this tracker"}); got != "The tracker says: Torrent not registered with this tracker" {
		t.Errorf("one line: %q", got)
	}
	if got := oneLine(strings.Repeat("é", 300), 10); got != strings.Repeat("é", 10)+"…" {
		t.Errorf("cut: %q", got)
	}
	if got := oneLine("a\xffb", 10); got != "ab" {
		t.Errorf("not text: %q", got)
	}
}

// A tracker's failure reason is read even when it comes with an error status; BEP 31's "retry in" too.
func TestTrackerFailureReasonAndRetryIn(t *testing.T) {
	var mu sync.Mutex
	body := bencode.Encode(map[string]any{"failure reason": "busy, try later", "retry in": int64(7)})
	say := func(b []byte) {
		mu.Lock()
		body = b
		mu.Unlock()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(503)
		w.Write(body)
	}))
	defer srv.Close()
	_, err := announce(t.Context(), srv.URL+"/announce", announceReq{NumWant: 5})
	tf := failureOf(err)
	if tf == nil || tf.Reason != "busy, try later" || tf.RetryIn != 7*time.Minute {
		t.Fatalf("%v %+v", err, tf)
	}
	say(bencode.Encode(map[string]any{"failure reason": "gone", "retry in": "never"}))
	_, err = announce(t.Context(), srv.URL+"/announce", announceReq{})
	if tf := failureOf(err); tf == nil || !tf.Never {
		t.Fatalf("never: %v %+v", err, tf)
	}
	say([]byte("<html>Bad gateway</html>")) // not a tracker's reply at all
	if _, err = announce(t.Context(), srv.URL+"/announce", announceReq{}); err == nil || failureOf(err) != nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("plain error status: %v", err)
	}
	// a UDP tracker's error comes as text
	if tf := failureOf(fmt.Errorf("tracker: %s", "nope")); tf == nil || tf.Reason != "nope" {
		t.Fatalf("udp: %+v", tf)
	}
	if failureOf(fmt.Errorf("something else")) != nil {
		t.Fatal("not a failure reason")
	}
}

// ---------------------------------------------------------------- the order trackers are tried in

func TestTrackerWalk(t *testing.T) {
	tor := fakeTorrent("http://a/x", "http://b/x", "http://c/x")
	a, b, c := tor.trackers[0], tor.trackers[1], tor.trackers[2]
	a.tier, b.tier, c.tier = 0, 0, 1
	t0 := time.Now()
	due := func(at time.Duration) []*trackerState { return tor.dueTrackers(t0.Add(at)) }
	ask := func(tr *trackerState, at time.Duration) { tr.asking, tr.last = true, t0.Add(at) }
	one := func(got []*trackerState, want *trackerState, what string) {
		t.Helper()
		if len(got) != 1 || got[0] != want {
			t.Fatalf("%s: got %d trackers, want just %s", what, len(got), want.url)
		}
	}

	one(due(0), a, "to begin with: the first")
	ask(a, 0)
	if got := due(2 * time.Second); len(got) != 0 {
		t.Fatalf("waiting for the first to answer: %v", got)
	}
	// still nothing from the first after a while: the next one may try as well
	one(due(trackerSlow+time.Second), b, "the first is slow")
	ask(b, trackerSlow+time.Second)
	// the first fails: it's left alone, the second is still going
	a.asking = false
	tor.trackerFailed(a, fmt.Errorf("tracker: client not whitelisted"), t0.Add(12*time.Second))
	if got := due(13 * time.Second); len(got) != 0 {
		t.Fatalf("the second is being asked: %v", got)
	}
	if a.status != refusedText || a.retryAt.Sub(t0.Add(12*time.Second)) != time.Hour || a.detail == "" {
		t.Fatalf("refused: %q retry %v detail %q", a.status, a.retryAt, a.detail)
	}
	// the second fails too: the next tier
	b.asking = false
	tor.trackerFailed(b, fmt.Errorf("tracker: server busy"), t0.Add(14*time.Second))
	one(due(15*time.Second), c, "the whole first tier failed")
	if b.retryAt.Sub(t0.Add(14*time.Second)) != time.Minute || b.status != "The tracker says: server busy" || b.detail != "" {
		t.Fatalf("failed: %q %v", b.status, b.retryAt)
	}
	// the third works: nothing more to ask until it's due
	ask(c, 15*time.Second)
	c.asking = false
	if tor.trackerWorked(c, announceReq{Event: "started"}, &announceResp{Interval: 30 * time.Minute, Warning: "  slow\ndown "}, t0.Add(16*time.Second)) {
		t.Fatal("the only tracker of its tier can't move up")
	}
	if c.status != "Working" || c.warning != "slow down" || c.fails != 0 {
		t.Fatalf("worked: %+v", c)
	}
	if got := due(20 * time.Second); len(got) != 0 {
		t.Fatalf("in use and not due: %v", got)
	}
	// the second one's minute is up: it's tried again before the third
	one(due(14*time.Second+time.Minute+time.Second), b, "back off is over")
	// it works now: it stays in front, the third is no longer needed
	ask(b, 80*time.Second)
	b.asking = false
	tor.trackerWorked(b, announceReq{Event: "started"}, &announceResp{Interval: 30 * time.Minute}, t0.Add(81*time.Second))
	if got := due(90 * time.Second); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	// and once more it fails, then fails again: longer each time
	tor.trackerFailed(b, fmt.Errorf("no answer"), t0.Add(time.Hour))
	tor.trackerFailed(b, fmt.Errorf("no answer"), t0.Add(time.Hour))
	if b.fails != 2 || b.retryAt.Sub(t0.Add(time.Hour)) != 4*time.Minute {
		t.Fatalf("fails %d, retry in %v", b.fails, b.retryAt.Sub(t0.Add(time.Hour)))
	}
	// a tracker's own wishes: retry in 10 minutes, a min interval of 20
	tor.trackerFailed(a, &trackerFailure{Reason: "later", RetryIn: 10 * time.Minute}, t0)
	if a.retryAt.Sub(t0) != 10*time.Minute { // (the usual wait would have been 4)
		t.Fatalf("retry in: %v", a.retryAt.Sub(t0))
	}
	b.fails, b.minGap = 0, 20*time.Minute
	tor.trackerFailed(b, fmt.Errorf("no answer"), t0)
	if b.retryAt.Sub(t0) != 20*time.Minute {
		t.Fatalf("min interval: %v", b.retryAt.Sub(t0))
	}
}

// The tracker that answers moves to the front of its tier; the others keep their order.
func TestTrackerPromote(t *testing.T) {
	tor := fakeTorrent("http://a/x", "http://b/x", "http://c/x", "http://d/x")
	for i, tr := range tor.trackers {
		tr.tier = i / 3
	}
	c, d := tor.trackers[2], tor.trackers[3]
	if !tor.promote(c) {
		t.Fatal("c should move up")
	}
	if got := fmt.Sprint(tor.tierURLs()); got != "[[http://c/x http://a/x http://b/x] [http://d/x]]" {
		t.Fatalf("after c: %s", got)
	}
	if tor.promote(c) || tor.promote(d) {
		t.Fatal("already in front")
	}
	if !tor.promote(tor.trackers[2]) { // b
		t.Fatal("b should move up")
	}
	if got := fmt.Sprint(tor.tierURLs()); got != "[[http://b/x http://c/x http://a/x] [http://d/x]]" {
		t.Fatalf("after b: %s", got)
	}
}

// A magnet link's file list is fetched for the Add dialog: there every tracker is asked at once.
func TestTrackerProbeAsksAll(t *testing.T) {
	tor := fakeTorrent("http://a/x", "http://b/x", "http://c/x")
	tor.d.ID = "meta:0123"
	now := time.Now()
	if got := tor.dueTrackers(now); len(got) != 3 {
		t.Fatalf("%d asked", len(got))
	}
	tor.trackers[0].asking = true
	tor.trackers[1].retryAt = now.Add(time.Minute)
	if got := tor.dueTrackers(now); len(got) != 1 || got[0] != tor.trackers[2] {
		t.Fatalf("%v", got)
	}
}

func TestTrackerViewsShowStandbyFailuresAndWarnings(t *testing.T) {
	tor := fakeTorrent("http://a.example/x", "http://b.example:81/x", "udp://c.example:6969")
	for i, tr := range tor.trackers {
		tr.tier = i
	}
	now := time.Now()
	tor.trackerFailed(tor.trackers[0], fmt.Errorf("tracker: not whitelisted client"), now)
	tor.trackerWorked(tor.trackers[1], announceReq{Event: "started"}, &announceResp{Interval: time.Hour, Warning: "careful", Seeders: 3, Leechers: 4}, now)
	v := tor.trackerViews(now.Add(time.Second))
	if len(v) != 3 {
		t.Fatal(v)
	}
	if v[0].Status != refusedText || v[0].State != "refused" || v[0].Detail == "" || v[0].Fails != 1 || v[0].RetryIn < 3500 || v[0].Announce != "http://a.example/x" {
		t.Errorf("refused: %+v", v[0])
	}
	if v[1].Status != "Working" || v[1].Warning != "careful" || v[1].Seeders != 3 || v[1].URL != "http://b.example:81" {
		t.Errorf("working: %+v", v[1])
	}
	if v[2].Status != "Standby" || v[2].State != "standby" {
		t.Errorf("not needed: %+v", v[2])
	}
}

// ---------------------------------------------------------------- with trackers on the wire

// A tracker that refuses Zipline is told apart and left alone; the next one in its tier works and moves up.
// Every announce carries the torrent's one key, and the tracker id comes back.
func TestTrackerEtiquetteEndToEnd(t *testing.T) {
	refuse := newFakeTracker(t, failReply("Your client is not on the whitelist"))
	work := newFakeTracker(t, okReply(map[string]any{"tracker id": "abc", "warning message": "Please update your client"}))
	tor, _, _ := webSeedTorrent(t)
	v, _ := bencode.Decode(tor)
	top := v.(map[string]any)
	top["announce"] = refuse.announce()
	m := torrentManager(t)
	d := addStopped(t, m, bencode.Encode(top), [][]string{{refuse.announce(), work.announce()}})
	m.Start([]string{d.ID})
	waitFor(t, m, d, StCompleted, 60*time.Second)
	waitCond(t, 15*time.Second, "the working tracker hears started and completed", func() bool { return work.count() >= 2 })
	time.Sleep(3 * time.Second) // (a few loops more: nothing else should be sent)

	if got := refuse.events(); fmt.Sprint(got) != "[started]" {
		t.Errorf("the refusing tracker saw %v: asked once, then left alone", got)
	}
	if got := work.events(); fmt.Sprint(got) != "[started completed]" {
		t.Errorf("the working tracker saw %v", got)
	}
	m.mu.Lock()
	key := d.Torrent.Key
	tiers := cloneTiers(d.Torrent.Tiers)
	m.mu.Unlock()
	if key == 0 {
		t.Fatal("the torrent has no key")
	}
	want := fmt.Sprintf("%08x", key)
	for _, c := range []struct {
		f   *fakeTracker
		hit int
	}{{refuse, 0}, {work, 0}, {work, 1}} {
		if got := c.f.hit(c.hit).Get("key"); got != want {
			t.Errorf("announce %d had key %q, want %q", c.hit, got, want)
		}
	}
	if work.hit(0).Get("trackerid") != "" || work.hit(1).Get("trackerid") != "abc" {
		t.Errorf("tracker id: %q then %q", work.hit(0).Get("trackerid"), work.hit(1).Get("trackerid"))
	}
	if len(tiers) != 1 || tiers[0][0] != work.announce() || tiers[0][1] != refuse.announce() {
		t.Errorf("the tracker that answered should be first in its tier: %v", tiers)
	}
	// what the window shows
	tr := runningTorrent(t, m, d.ID)
	views := tr.view(false).Trackers
	if len(views) != 2 || views[0].Status != "Working" || views[0].State != "working" || views[0].Warning != "Please update your client" ||
		views[1].Status != refusedText || views[1].State != "refused" || !strings.Contains(views[1].Arg, "whitelist") || !strings.Contains(views[1].Detail, "whitelist") || views[1].Fails != 1 {
		t.Errorf("views: %+v", views)
	}
	// the key and the order survive a restart: they are in the saved list
	m.mu.Lock()
	b, _ := json.Marshal(d)
	m.mu.Unlock()
	var back Download
	if json.Unmarshal(b, &back) != nil || back.Torrent.Key != key || fmt.Sprint(back.Torrent.Tiers) != fmt.Sprint(tiers) {
		t.Errorf("saved as %s", b)
	}
}

// The next tier is only asked when every tracker of the tier before it failed.
func TestTrackerNextTierOnlyWhenAllFailed(t *testing.T) {
	bad1 := newFakeTracker(t, failReply("down for maintenance"))
	bad2 := newFakeTracker(t, func(url.Values, int) (int, []byte) { return 500, []byte("oops") })
	good := newFakeTracker(t, okReply(nil))
	spare := newFakeTracker(t, okReply(nil))
	m := torrentManager(t)
	d := addStopped(t, m, idleTorrent(t), [][]string{{bad1.announce(), bad2.announce()}, {good.announce()}, {spare.announce()}})
	m.Start([]string{d.ID})
	waitCond(t, 20*time.Second, "the second tier is asked", func() bool { return good.count() >= 1 })
	time.Sleep(2500 * time.Millisecond)
	if bad1.count() != 1 || bad2.count() != 1 || good.count() != 1 || spare.count() != 0 {
		t.Fatalf("asked: %d %d %d %d (want 1 1 1 0)", bad1.count(), bad2.count(), good.count(), spare.count())
	}
	tr := runningTorrent(t, m, d.ID)
	views := tr.view(false).Trackers
	if len(views) != 4 || views[0].Tier != 0 || views[2].Tier != 1 || views[3].Tier != 2 || views[3].Status != "Standby" || views[3].State != "standby" ||
		views[0].State != "says" || views[0].Arg != "down for maintenance" || views[1].State != "error" || views[2].State != "working" ||
		!strings.HasPrefix(views[0].Status, "The tracker says: down for") || views[1].Status != "Error: tracker replied 500 Internal Server Error" {
		t.Fatalf("views: %+v", views)
	}
}

// Trackers added or taken away while the torrent runs: the new ones are used, the old ones are told.
func TestSetTorrentTrackersWhileRunning(t *testing.T) {
	one := newFakeTracker(t, okReply(nil))
	two := newFakeTracker(t, okReply(nil))
	three := newFakeTracker(t, okReply(nil))
	m := torrentManager(t)
	d := addStopped(t, m, idleTorrent(t), [][]string{{one.announce()}})
	m.Start([]string{d.ID})
	waitCond(t, 15*time.Second, "the first tracker hears started", func() bool { return one.count() >= 1 })
	if _, err := m.SetTorrentTrackers(d.ID, [][]string{{two.announce()}, {three.announce(), one.announce() + "?x=1"}}); err != nil {
		t.Fatal(err)
	}
	waitCond(t, 15*time.Second, "the first tracker is told we've gone, the new one asked", func() bool { return one.count() >= 2 && two.count() >= 1 })
	if got := one.events(); fmt.Sprint(got) != "[started stopped]" {
		t.Errorf("the removed tracker saw %v", got)
	}
	if three.count() != 0 {
		t.Errorf("the second tier was asked while the first works")
	}
	tr := runningTorrent(t, m, d.ID)
	tr.mu.Lock()
	got := fmt.Sprint(tr.tierURLs())
	tr.mu.Unlock()
	if want := fmt.Sprint([][]string{{two.announce()}, {three.announce(), one.announce() + "?x=1"}}); got != want {
		t.Errorf("running trackers: %s, want %s", got, want)
	}
	// Properties' view of them (also in the saved list)
	m.mu.Lock()
	saved := fmt.Sprint(d.Torrent.Tiers)
	flat := fmt.Sprint(d.Torrent.Trackers)
	m.mu.Unlock()
	if saved != got || flat != fmt.Sprint([]string{two.announce(), three.announce(), one.announce() + "?x=1"}) {
		t.Errorf("saved %s / %s", saved, flat)
	}
	// none at all is fine too
	if _, err := m.SetTorrentTrackers(d.ID, nil); err != nil {
		t.Fatal(err)
	}
	tr.mu.Lock()
	n := len(tr.trackers)
	tr.mu.Unlock()
	if n != 0 {
		t.Errorf("%d trackers left", n)
	}
}

func TestTorrentTrackersAPI(t *testing.T) {
	m := torrentManager(t)
	h := (&Server{m: m, token: "t"}).routes()
	d := addStopped(t, m, idleTorrent(t, []string{"http://one.example/announce", "http://two.example/announce"}), nil)
	det := apiCall(t, h, "torrent/detail", map[string]any{"id": d.ID})
	tv, _ := det["tiers"].([]any)
	if len(tv) != 1 || len(tv[0].([]any)) != 2 {
		t.Fatalf("tiers in the details: %v", det["tiers"])
	}
	if r := apiCall(t, h, "torrent/trackers", map[string]any{"id": d.ID, "tiers": [][]string{{"ftp://nope"}}}); r["error"] == nil {
		t.Fatalf("a bad address was accepted: %v", r)
	}
	if r := apiCall(t, h, "torrent/trackers", map[string]any{"id": "nope", "tiers": [][]string{}}); r["error"] == nil {
		t.Fatalf("an unknown download: %v", r)
	}
	r := apiCall(t, h, "torrent/trackers", map[string]any{"id": d.ID, "tiers": [][]string{{" udp://t.example:80 "}, {}, {"https://u.example/a", "udp://t.example:80"}}})
	if r["ok"] != true || fmt.Sprint(r["tiers"]) != "[[udp://t.example:80] [https://u.example/a]]" {
		t.Fatalf("saved: %v", r)
	}
	det = apiCall(t, h, "torrent/detail", map[string]any{"id": d.ID})
	if fmt.Sprint(det["tiers"]) != "[[udp://t.example:80] [https://u.example/a]]" || !strings.Contains(fmt.Sprint(det["magnet"]), "tr=udp") {
		t.Fatalf("details: %v", det)
	}
	// the list itself doesn't carry them
	for _, v := range m.Snapshot(nil).Downloads {
		if v.ID == d.ID && v.Torrent.Tiers != nil {
			t.Error("the list carries the tiers")
		}
	}
}

// A saved list from before tiers existed gets them from its torrent file; the key is made when first needed.
func TestOldTorrentGetsTiersAndKey(t *testing.T) {
	m := torrentManager(t)
	tor := idleTorrent(t, []string{"http://one.example/a"}, []string{"http://two.example/a", "http://three.example/a"})
	d := addStopped(t, m, tor, nil)
	m.mu.Lock()
	d.Torrent.Tiers, d.Torrent.Key = nil, 0
	got := m.trackerTiersFor(d.Torrent)
	m.mu.Unlock()
	if len(got) != 2 || got[0][0] != "http://one.example/a" || len(got[1]) != 2 {
		t.Fatalf("%v", got)
	}
	tr := &torrent{m: m, d: d}
	k := tr.trackerKey()
	if k == 0 || tr.trackerKey() != k {
		t.Fatalf("key %d", k)
	}
	b, _ := json.Marshal(&TorrentState{InfoHash: "00"})
	if strings.Contains(string(b), "tiers") || strings.Contains(string(b), "key") || strings.Contains(string(b), "upKB") || strings.Contains(string(b), "seed") {
		t.Fatalf("empty extras are saved: %s", b)
	}
}
