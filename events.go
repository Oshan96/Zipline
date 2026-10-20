package main

import (
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Live updates for the window (/api/events, server-sent events). Each event has a sequence number
// and the state outside the list (queues, speed, notices, prompts, plan, cap, power, profile,
// quota…) in full. The list comes whole only in a connection's first event ("full": true) and
// when the window asks for it again; after that an event carries just the downloads that are new
// or whose view changed ("downloads"), the IDs that are gone ("removed") and, when the order
// changed other than by those, the whole order ("order"). With thousands of downloads that keeps
// each event, and the window's work on it, small (like qBittorrent's sync API).
//
// A window that falls behind (it missed an event) or reconnects gets the whole list again: a new
// connection starts with it, and POST /api/events/view {"client", "resync": true} asks for it on
// an open one. The same call tells the stream which progress windows are open ("segs": their
// segments are sent, nobody else's) and whether the window is hidden ("slow": an event every 2 s).

const (
	eventEvery     = 400 * time.Millisecond
	eventEverySlow = 2 * time.Second // while the window is hidden or minimized
)

// listEvent is one event. Snapshot.Downloads is the whole list when Full, else the changed rows.
type listEvent struct {
	Seq     int64    `json:"seq"`
	Full    bool     `json:"full,omitempty"`
	Removed []string `json:"removed,omitempty"`
	Order   []string `json:"order,omitempty"`
	Snapshot
}

// eventStream is one window's connection: what it asked for and what it has been sent.
type eventStream struct {
	mu     sync.Mutex
	segs   map[string]bool // progress windows open (replaced, never changed in place)
	slow   bool
	resync bool
	wake   chan struct{} // the options changed: send now

	seq   int64              // only the stream's own goroutine uses these
	sent  map[string]*DLView // the window's copy of each download
	order []string           // and their order
}

func newEventStream(segs []string, slow bool) *eventStream {
	return &eventStream{segs: idSet(segs), slow: slow, wake: make(chan struct{}, 1)}
}

func idSet(ids []string) map[string]bool {
	set := map[string]bool{}
	for _, id := range ids {
		if id != "" {
			set[id] = true
		}
	}
	return set
}

func (c *eventStream) every() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.slow {
		return eventEverySlow
	}
	return eventEvery
}

// next builds the stream's next event.
func (c *eventStream) next(m *Manager) listEvent {
	c.mu.Lock()
	segs, full := c.segs, c.resync || c.sent == nil
	c.resync = false
	c.mu.Unlock()
	s := m.Snapshot(segs)
	c.seq++
	ev := listEvent{Seq: c.seq, Snapshot: s}
	if full {
		ev.Full = true
		c.sent = make(map[string]*DLView, len(s.Downloads))
		c.order = make([]string, len(s.Downloads))
		for i := range s.Downloads { // (rows point into this snapshot until they change)
			c.sent[s.Downloads[i].ID] = &s.Downloads[i]
			c.order[i] = s.Downloads[i].ID
		}
		return ev
	}
	ev.Downloads, ev.Removed, ev.Order = c.diff(s.Downloads)
	return ev
}

// diff compares the list with the window's copy and brings the copy up to date: the rows to send
// (new or changed, in list order), the IDs that are gone, and the whole order when it changed
// other than by removing those and adding the new ones at the end.
func (c *eventStream) diff(list []DLView) (rows []DLView, removed, order []string) {
	rows = []DLView{}
	var added map[string]bool
	kept := 0
	for i := range list {
		v := &list[i]
		old := c.sent[v.ID]
		if old != nil {
			kept++
			if sameView(old, v) {
				continue
			}
		} else {
			if added == nil {
				added = map[string]bool{}
			}
			added[v.ID] = true
		}
		rows = append(rows, *v)
		cp := *v // a copy: pointing into the list would keep each event's whole list alive
		c.sent[v.ID] = &cp
	}
	var gone map[string]bool
	if kept < len(c.sent)-len(added) { // some downloads the window has are gone
		here := make(map[string]bool, len(list))
		for i := range list {
			here[list[i].ID] = true
		}
		gone = map[string]bool{}
		for _, id := range c.order {
			if !here[id] {
				removed = append(removed, id)
				gone[id] = true
				delete(c.sent, id)
			}
		}
	}
	if !sameOrder(c.order, list, gone, added) {
		order = make([]string, len(list))
		for i := range list {
			order[i] = list[i].ID
		}
		c.order = order
	} else if len(gone) > 0 || len(added) > 0 {
		c.order = c.order[:0]
		for i := range list {
			c.order = append(c.order, list[i].ID)
		}
	}
	return rows, removed, order
}

// sameOrder reports whether list is old without the gone IDs and with the added ones at the end,
// which is what the window gets by applying an event that has no "order".
func sameOrder(old []string, list []DLView, gone, added map[string]bool) bool {
	i := 0
	for _, id := range old {
		if gone[id] {
			continue
		}
		if i >= len(list) || list[i].ID != id {
			return false
		}
		i++
	}
	for ; i < len(list); i++ {
		if !added[list[i].ID] {
			return false
		}
	}
	return true
}

// sameView reports whether a download looks the same to the window, so it needn't be sent again.
// (TestSameViewSeesEveryField fails when DLView gets a field this doesn't compare.)
func sameView(a, b *DLView) bool {
	return a.ID == b.ID && a.URL == b.URL && a.Name == b.Name && a.Dir == b.Dir && a.Category == b.Category &&
		a.Size == b.Size && a.Done == b.Done && a.Speed == b.Speed && a.Status == b.Status && a.Error == b.Error &&
		a.Resumable == b.Resumable && a.Added.Equal(b.Added) && a.Finished.Equal(b.Finished) && a.LastTry.Equal(b.LastTry) &&
		a.InQueue == b.InQueue && a.Queue == b.Queue && a.LimitKB == b.LimitKB && a.Scan == b.Scan && a.ScanState == b.ScanState &&
		a.ScanNote == b.ScanNote && a.SyncNote == b.SyncNote && a.Metered == b.Metered && a.Checked.Equal(b.Checked) &&
		a.Kind == b.Kind && a.Parts == b.Parts && a.PartsDone == b.PartsDone && a.Est == b.Est && a.AParts == b.AParts &&
		a.ADone == b.ADone && a.Prog == b.Prog && a.Note == b.Note && a.Stage == b.Stage && a.StagePct == b.StagePct &&
		a.Workers == b.Workers && a.Conns == b.Conns && a.Desc == b.Desc && a.Link == b.Link && a.Page == b.Page &&
		a.Expires == b.Expires && a.Pieces == b.Pieces && a.Waiting == b.Waiting && a.Challenge == b.Challenge &&
		a.PiecesOK == b.PiecesOK && a.PiecesBad == b.PiecesBad &&
		a.Retrying == b.Retrying && a.RetryAt == b.RetryAt && a.TryAgain == b.TryAgain &&
		a.Live == b.Live && a.Rec == b.Rec && a.Private == b.Private &&
		(a.Move == b.Move || a.Move != nil && b.Move != nil && *a.Move == *b.Move) &&
		slices.Equal(a.Segs, b.Segs) && slices.Equal(a.Mirrors, b.Mirrors) && slices.Equal(a.Sources, b.Sources) &&
		(a.Check == b.Check || a.Check != nil && b.Check != nil && *a.Check == *b.Check) &&
		(a.Torrent == b.Torrent || reflect.DeepEqual(a.Torrent, b.Torrent)) // (only torrents pay for this one)
}

// events streams the live updates to a window.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no stream", 500)
		return
	}
	atomic.AddInt32(&sseClients, 1)
	defer atomic.AddInt32(&sseClients, -1)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	q := r.URL.Query()
	c := newEventStream(strings.Split(q.Get("segs"), ","), q.Get("slow") == "1")
	if id := q.Get("client"); id != "" { // the window's name for this stream (/api/events/view)
		s.streams.Store(id, c)
		defer s.streams.CompareAndDelete(id, c)
	}
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		b, _ := json.Marshal(c.next(s.m))
		w.Write([]byte("data: "))
		w.Write(b)
		w.Write([]byte("\n\n"))
		fl.Flush()
		t.Reset(c.every())
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
		case <-c.wake:
		}
	}
}

// eventsView changes what an open stream sends: {"client", "segs": [ids], "slow", "resync"}.
func (s *Server) eventsView(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Client string   `json:"client"`
		Segs   []string `json:"segs"`
		Slow   bool     `json:"slow"`
		Resync bool     `json:"resync"`
	}
	if r.Method != "POST" || readJSON(r, &req) != nil || req.Client == "" {
		http.Error(w, "bad request", 400)
		return
	}
	v, ok := s.streams.Load(req.Client)
	if !ok { // (it reconnects with what it needs)
		writeJSON(w, map[string]any{"error": "not connected"})
		return
	}
	c := v.(*eventStream)
	c.mu.Lock()
	c.segs, c.slow = idSet(req.Segs), req.Slow
	c.resync = c.resync || req.Resync
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
	writeJSON(w, map[string]any{"ok": true})
}

// expiryCache keeps linkExpiry's answer for each address: the list is built for the window
// several times a second, and parsing thousands of addresses each time added up.
var expiryCache struct {
	sync.Mutex
	m map[string]time.Time
}

func cachedExpiry(u string) time.Time {
	if u == "" {
		return time.Time{}
	}
	expiryCache.Lock()
	defer expiryCache.Unlock()
	t, ok := expiryCache.m[u]
	if !ok {
		if len(expiryCache.m) > 50000 || expiryCache.m == nil { // (addresses of removed downloads go too)
			expiryCache.m = map[string]time.Time{}
		}
		t = linkExpiry(u)
		expiryCache.m[u] = t
	}
	return t
}
