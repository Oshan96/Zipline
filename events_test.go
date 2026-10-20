package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var viewN atomic.Int64

// addViews puts n stopped downloads in the list (nothing in them changes by itself).
func addViews(m *Manager, n int) []*Download {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Download
	for i := 0; i < n; i++ {
		k := viewN.Add(1)
		d := &Download{ID: fmt.Sprintf("v%05d", k), URL: fmt.Sprintf("https://example.com/f/%d.zip?expires=1999999999", k),
			FileName: fmt.Sprintf("file-%d.zip", k), Dir: m.settings.DownloadDir, Category: "Compressed", Size: 1 << 20,
			Status: []string{StPaused, StCompleted}[i%2], Added: time.Unix(1700000000+k, 0), Conns: 4,
			Segments: []*Segment{{Start: 0, End: 1<<20 - 1, Pos: 1000}}}
		m.list = append(m.list, d)
		m.byID[d.ID] = d
		out = append(out, d)
	}
	return out
}

func viewIDs(l []DLView) []string {
	var ids []string
	for _, v := range l {
		ids = append(ids, v.ID)
	}
	return ids
}

func TestEventsSendOnlyChanges(t *testing.T) {
	m := newTestManager(t)
	ds := addViews(m, 5)
	c := newEventStream(nil, false)
	ev := c.next(m)
	if ev.Seq != 1 || !ev.Full || len(ev.Downloads) != 5 || len(ev.Queues) == 0 {
		t.Fatalf("first event: seq %d full %v %d downloads %d queues; want the whole list", ev.Seq, ev.Full, len(ev.Downloads), len(ev.Queues))
	}
	ev = c.next(m)
	if ev.Seq != 2 || ev.Full || len(ev.Downloads) != 0 || ev.Removed != nil || ev.Order != nil || len(ev.Queues) == 0 {
		t.Fatalf("nothing changed: got seq %d full %v rows %v removed %v order %v", ev.Seq, ev.Full, viewIDs(ev.Downloads), ev.Removed, ev.Order)
	}
	if b, _ := json.Marshal(ev); !bytes.Contains(b, []byte(`"downloads":[]`)) {
		t.Fatalf("an event without changes should still say so: %s", b)
	}

	// one changes: only it is sent
	m.mu.Lock()
	ds[2].Desc = "changed"
	m.mu.Unlock()
	atomic.AddInt64(&ds[3].done, 5)
	ev = c.next(m)
	if got := viewIDs(ev.Downloads); !slices.Equal(got, []string{ds[2].ID, ds[3].ID}) || ev.Downloads[0].Desc != "changed" || ev.Removed != nil || ev.Order != nil {
		t.Fatalf("changed rows: got %v removed %v order %v", got, ev.Removed, ev.Order)
	}

	// one removed, one added: the window drops it and puts the new one at the end, no order needed
	m.Remove([]string{ds[1].ID}, false)
	added := addViews(m, 1)[0]
	ev = c.next(m)
	if got := viewIDs(ev.Downloads); !slices.Equal(got, []string{added.ID}) || !slices.Equal(ev.Removed, []string{ds[1].ID}) || ev.Order != nil {
		t.Fatalf("add and remove: rows %v removed %v order %v", got, ev.Removed, ev.Order)
	}
	ev = c.next(m)
	if len(ev.Downloads) != 0 || ev.Removed != nil || ev.Order != nil {
		t.Fatalf("after add and remove nothing changed, got rows %v removed %v order %v", viewIDs(ev.Downloads), ev.Removed, ev.Order)
	}

	// moved: the new order, and no rows (their views didn't change)
	m.Move([]string{ds[4].ID}, -1)
	ev = c.next(m)
	want := []string{ds[0].ID, ds[2].ID, ds[4].ID, ds[3].ID, added.ID}
	if !slices.Equal(ev.Order, want) || len(ev.Downloads) != 0 || ev.Removed != nil {
		t.Fatalf("moved: order %v (want %v) rows %v removed %v", ev.Order, want, viewIDs(ev.Downloads), ev.Removed)
	}

	// added in the middle (not at the end): rows plus the order
	m.mu.Lock()
	mid := &Download{ID: "mid", URL: "https://example.com/mid.zip", FileName: "mid.zip", Status: StPaused}
	m.list = append(m.list[:1], append([]*Download{mid}, m.list[1:]...)...)
	m.byID[mid.ID] = mid
	m.mu.Unlock()
	ev = c.next(m)
	if !slices.Equal(viewIDs(ev.Downloads), []string{"mid"}) || len(ev.Order) != 6 || ev.Order[1] != "mid" {
		t.Fatalf("inserted: rows %v order %v", viewIDs(ev.Downloads), ev.Order)
	}

	// asked to resync: the whole list again, and the count goes on
	c.resync = true
	ev = c.next(m)
	if !ev.Full || len(ev.Downloads) != 6 || ev.Seq != 8 {
		t.Fatalf("resync: full %v %d rows seq %d", ev.Full, len(ev.Downloads), ev.Seq)
	}
	if ev = c.next(m); ev.Full || len(ev.Downloads) != 0 {
		t.Fatalf("after a resync: full %v rows %v", ev.Full, viewIDs(ev.Downloads))
	}
}

// Segments (and the other progress-window details) only for the downloads asked for.
func TestEventsSegmentsOnlyWhenAsked(t *testing.T) {
	m := newTestManager(t)
	ds := addViews(m, 4)
	c := newEventStream([]string{ds[0].ID, ""}, false)
	ev := c.next(m)
	for _, v := range ev.Downloads {
		if (v.Segs != nil) != (v.ID == ds[0].ID) {
			t.Fatalf("%s has segments %v; only %s should", v.ID, v.Segs, ds[0].ID)
		}
	}
	c.segs = idSet([]string{ds[1].ID})
	ev = c.next(m)
	if got := viewIDs(ev.Downloads); !slices.Equal(got, []string{ds[0].ID, ds[1].ID}) || ev.Downloads[0].Segs != nil || ev.Downloads[1].Segs == nil {
		t.Fatalf("switching the progress window: rows %v", got)
	}
	if ev = c.next(m); len(ev.Downloads) != 0 {
		t.Fatalf("segments that didn't change are sent again: %v", viewIDs(ev.Downloads))
	}
	m.mu.Lock()
	ds[1].Segments[0].Pos += 100
	m.mu.Unlock()
	if ev = c.next(m); !slices.Equal(viewIDs(ev.Downloads), []string{ds[1].ID}) {
		t.Fatalf("its segments moved on: rows %v", viewIDs(ev.Downloads))
	}
}

// TestSameViewSeesEveryField: a field sameView doesn't compare would never reach the window.
func TestSameViewSeesEveryField(t *testing.T) {
	typ := reflect.TypeOf(DLView{})
	for i := 0; i < typ.NumField(); i++ {
		var a, b DLView
		f := reflect.ValueOf(&b).Elem().Field(i)
		switch f.Kind() {
		case reflect.String:
			f.SetString("x")
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Int, reflect.Int32, reflect.Int64:
			f.SetInt(1)
		case reflect.Float64:
			f.SetFloat(1)
		case reflect.Slice:
			f.Set(reflect.MakeSlice(f.Type(), 1, 1))
		case reflect.Pointer:
			f.Set(reflect.New(f.Type().Elem()))
		case reflect.Struct:
			f.Set(reflect.ValueOf(time.Unix(1, 0)))
		default:
			t.Fatalf("%s: a %s field; teach this test and sameView about it", typ.Field(i).Name, f.Kind())
		}
		if sameView(&a, &b) || sameView(&b, &a) {
			t.Errorf("sameView doesn't compare DLView.%s", typ.Field(i).Name)
		}
		c := b
		if !sameView(&b, &c) {
			t.Errorf("sameView: copies differ in DLView.%s", typ.Field(i).Name)
		}
	}
}

// sseStream reads a /api/events stream.
type sseStream struct {
	t      *testing.T
	r      *bufio.Reader
	cancel func()
}

func openEvents(t *testing.T, base, query string) *sseStream {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/events?token=t&"+query, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if res.StatusCode != 200 {
		cancel()
		t.Fatalf("events: %s", res.Status)
	}
	s := &sseStream{t: t, r: bufio.NewReader(res.Body), cancel: func() { cancel(); res.Body.Close() }}
	t.Cleanup(s.cancel)
	return s
}

func (s *sseStream) next() listEvent {
	s.t.Helper()
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			s.t.Fatal(err)
		}
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var ev listEvent
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				s.t.Fatal(err)
			}
			return ev
		}
	}
}

func postView(t *testing.T, base string, body any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", base+"/api/events/view", bytes.NewReader(b))
	req.Header.Set("X-Token", "t")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return out
}

func TestEventsStream(t *testing.T) {
	m := newTestManager(t)
	ds := addViews(m, 3)
	srv := httptest.NewServer((&Server{m: m, token: "t"}).routes())
	t.Cleanup(srv.Close) // (after the streams close)

	es := openEvents(t, srv.URL, "client=w1&segs="+ds[0].ID)
	ev := es.next()
	if ev.Seq != 1 || !ev.Full || len(ev.Downloads) != 3 || ev.Downloads[0].Segs == nil || ev.Downloads[1].Segs != nil {
		t.Fatalf("first event: seq %d full %v rows %d", ev.Seq, ev.Full, len(ev.Downloads))
	}
	seq := ev.Seq
	next := func() listEvent { // the next event, checking the count goes up by one
		t.Helper()
		ev := es.next()
		if ev.Seq != seq+1 {
			t.Fatalf("event %d after %d", ev.Seq, seq)
		}
		seq = ev.Seq
		for _, v := range ev.Downloads {
			if v.Segs != nil && v.ID != ds[1].ID && v.ID != ds[0].ID {
				t.Fatalf("segments for %s, which nobody asked for", v.ID)
			}
		}
		return ev
	}

	// the progress window changes: sent straight away
	start := time.Now()
	if r := postView(t, srv.URL, map[string]any{"client": "w1", "segs": []string{ds[1].ID}}); r["ok"] != true {
		t.Fatalf("view: %v", r)
	}
	var got []DLView
	for len(got) == 0 {
		got = next().Downloads
	}
	if time.Since(start) > 300*time.Millisecond || !slices.Equal(viewIDs(got), []string{ds[0].ID, ds[1].ID}) || got[0].Segs != nil || got[1].Segs == nil {
		t.Fatalf("after asking for %s's segments (%v): rows %v", ds[1].ID, time.Since(start), viewIDs(got))
	}

	// a resync on the open stream
	postView(t, srv.URL, map[string]any{"client": "w1", "segs": []string{ds[1].ID}, "resync": true})
	if ev := next(); !ev.Full {
		if ev = next(); !ev.Full {
			t.Fatal("no whole list after asking for a resync")
		}
	}

	// hidden: an event every 2 s; shown again: one at once, then every 0.4 s
	postView(t, srv.URL, map[string]any{"client": "w1", "slow": true})
	next() // (the one the change itself sends)
	t0 := time.Now()
	next()
	if d := time.Since(t0); d < 1500*time.Millisecond {
		t.Fatalf("hidden window: an event after %v", d)
	}
	t0 = time.Now()
	postView(t, srv.URL, map[string]any{"client": "w1", "slow": false})
	next()
	next()
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("shown again: two events took %v", d)
	}

	// a reconnect starts over with the whole list
	es.cancel()
	es = openEvents(t, srv.URL, "client=w1")
	if ev := es.next(); ev.Seq != 1 || !ev.Full || len(ev.Downloads) != 3 {
		t.Fatalf("reconnected: seq %d full %v rows %d", ev.Seq, ev.Full, len(ev.Downloads))
	}

	if r := postView(t, srv.URL, map[string]any{"client": "nobody"}); r["error"] == nil {
		t.Fatalf("a stream that isn't open: %v", r)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/api/events/view?token=t", nil)
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != 400 {
		t.Fatalf("GET view: %v %v", res.StatusCode, err)
	}
}

// BenchmarkEvents5000 is one live update of a 5,000-download list where a few downloads changed:
// building the list, comparing it with what the window has and encoding the event.
func BenchmarkEvents5000(b *testing.B) {
	m := NewManager(b.TempDir())
	ds := addViews(m, 5000)
	c := newEventStream(nil, false)
	c.next(m)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, d := range ds[:6] {
			atomic.AddInt64(&d.done, 1000)
		}
		json.Marshal(c.next(m))
	}
}

// BenchmarkEventsWhole5000 is the same update sent as the whole list (how it was before).
func BenchmarkEventsWhole5000(b *testing.B) {
	m := NewManager(b.TempDir())
	ds := addViews(m, 5000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, d := range ds[:6] {
			atomic.AddInt64(&d.done, 1000)
		}
		json.Marshal(m.Snapshot(nil))
	}
}
