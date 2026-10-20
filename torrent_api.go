package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"sync/atomic"
	"time"
)

// The window's side of torrents: describing a torrent for the Add dialog, fetching a magnet
// link's file list while the dialog is open, choosing files, and the live view.

// torrentRef is how the window refers to a torrent Zipline has read: "torrent:<hash>".
func torrentRef(ih [20]byte) string { return "torrent:" + hex.EncodeToString(ih[:]) }

func parseTorrentRef(s string) ([20]byte, bool) {
	var ih [20]byte
	if !strings.HasPrefix(s, "torrent:") {
		return ih, false
	}
	b, err := hex.DecodeString(strings.TrimPrefix(s, "torrent:"))
	if err != nil || len(b) != 20 {
		return ih, false
	}
	copy(ih[:], b)
	return ih, true
}

type tfileView struct {
	Index int     `json:"i"` // position in the torrent (padding files aren't listed)
	Path  string  `json:"path"`
	Size  int64   `json:"size"`
	Skip  bool    `json:"skip,omitempty"`
	Done  float64 `json:"done,omitempty"` // 0…1
	Prio  int8    `json:"prio,omitempty"` // -1 low, 0 normal, 1 high
}

// torrentInfo describes a torrent for the Add dialog.
func (m *Manager) torrentInfo(ih [20]byte, name string, mi *Metainfo, select_ []int) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := map[string]any{"kind": "torrent", "hash": hex.EncodeToString(ih[:]), "name": name, "size": int64(-1), "needMeta": mi == nil}
	if mi != nil {
		var files []tfileView
		var total int64
		for i, f := range mi.Files {
			if f.Pad {
				continue
			}
			fv := tfileView{Index: i, Path: strings.Join(f.Path, "/"), Size: f.Length}
			if len(select_) > 0 {
				fv.Skip = !containsInt(select_, i)
			}
			files = append(files, fv)
			total += f.Length
		}
		res["name"], res["size"], res["files"], res["private"], res["fileCount"] = mi.Name, total, files, mi.Private, len(mi.Files)
		res["trackers"], res["webSeeds"], res["pieces"], res["pieceLen"] = len(mi.AllTrackers()), len(mi.WebSeeds), mi.NumPieces(), mi.PieceLen
		if mi.Comment != "" {
			res["comment"] = mi.Comment
		}
		name = mi.Name
	}
	cat := m.categoryFor(torrentMainFile(mi, name))
	res["category"], res["dir"] = cat, m.dirFor(cat)
	for _, d := range m.list {
		if d.Torrent != nil && strings.EqualFold(d.Torrent.InfoHash, hex.EncodeToString(ih[:])) {
			res["exists"] = d.FileName
		}
	}
	return res
}

func containsInt(l []int, x int) bool {
	for _, v := range l {
		if v == x {
			return true
		}
	}
	return false
}

// probeTorrent answers the Add dialog for a magnet link or a torrent Zipline has read.
func (s *Server) probeTorrent(w http.ResponseWriter, raw string) {
	m := s.m
	if ih, ok := parseTorrentRef(raw); ok {
		m.mu.Lock()
		mi := m.loadTorrent(ih)
		m.mu.Unlock()
		if mi == nil {
			writeJSON(w, map[string]any{"error": "the torrent file is gone; open it again"})
			return
		}
		writeJSON(w, m.torrentInfo(ih, mi.Name, mi, nil))
		return
	}
	mg, err := parseMagnet(raw)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	m.mu.Lock()
	mi := m.loadTorrent(mg.InfoHash)
	m.mu.Unlock()
	res := m.torrentInfo(mg.InfoHash, mg.Name, mi, mg.Select)
	res["trackers"] = len(mg.Trackers)
	writeJSON(w, res)
}

// torrentFromBody reads a .torrent file a link served (the probe's body is still open).
func (m *Manager) torrentFromBody(body io.Reader) (*Metainfo, error) {
	b, err := io.ReadAll(io.LimitReader(body, 20<<20+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 20<<20 {
		return nil, errors.New("too big for a torrent file")
	}
	mi, err := parseTorrent(b)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.rememberTorrent(mi, b)
	m.mu.Unlock()
	return mi, nil
}

func looksLikeTorrent(mime, name string, size int64) bool {
	mime = strings.ToLower(mime)
	return (strings.Contains(mime, "bittorrent") || strings.HasSuffix(strings.ToLower(name), ".torrent")) && size < 20<<20
}

// torrentOpen reads a .torrent file the window was given (picked or dropped).
func (s *Server) torrentOpen(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Data string `json:"data"` // base64
		Path string `json:"path"` // or a file on this computer (command line)
	}
	if readJSON(r, &req) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	var b []byte
	var err error
	if req.Path != "" {
		b, err = os.ReadFile(req.Path)
	} else {
		b, err = base64.StdEncoding.DecodeString(req.Data)
	}
	if err != nil {
		writeJSON(w, map[string]any{"error": "couldn't read the file"})
		return
	}
	mi, err := parseTorrent(b)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	s.m.mu.Lock()
	s.m.rememberTorrent(mi, b)
	s.m.mu.Unlock()
	res := s.m.torrentInfo(mi.InfoHash, mi.Name, mi, nil)
	res["url"] = torrentRef(mi.InfoHash)
	writeJSON(w, res)
}

// ---------------------------------------------------------------- file lists for magnet links

type metaSession struct {
	t    *torrent
	poll time.Time
	err  string
}

// torrentMeta fetches a magnet link's file list from peers while the Add dialog waits.
func (s *Server) torrentMeta(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Magnet string `json:"magnet"`
		Stop   bool   `json:"stop"`
	}
	if readJSON(r, &req) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	mg, err := parseMagnet(req.Magnet)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	m := s.m
	key := "meta:" + hex.EncodeToString(mg.InfoHash[:])
	m.mu.Lock()
	if mi := m.loadTorrent(mg.InfoHash); mi != nil {
		m.mu.Unlock()
		if ms := m.stopMeta(key); ms != nil {
			_ = ms
		}
		writeJSON(w, m.torrentInfo(mg.InfoHash, mi.Name, mi, mg.Select))
		return
	}
	ms := m.metaSessions[key]
	if req.Stop {
		m.mu.Unlock()
		m.stopMeta(key)
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	busy := false
	for id := range m.torrents { // it's downloading already: that run gets the list
		if t := m.torrents[id]; t != nil && t.ih == mg.InfoHash && !strings.HasPrefix(id, "meta:") {
			busy = true
		}
	}
	if ms == nil && !busy {
		ms = &metaSession{}
		m.metaSessions[key] = ms
		m.mu.Unlock()
		ms.t = m.startMetaFetch(key, mg)
		m.mu.Lock()
	}
	if ms != nil {
		ms.poll = time.Now()
	}
	m.mu.Unlock()
	res := m.torrentInfo(mg.InfoHash, mg.Name, nil, mg.Select)
	if ms != nil && ms.t != nil {
		if v := ms.t.live.Load(); v != nil {
			res["peers"], res["metaPct"] = v.Peers, v.MetaPct
		}
	}
	writeJSON(w, res)
}

func (m *Manager) stopMeta(key string) *metaSession {
	m.mu.Lock()
	ms := m.metaSessions[key]
	delete(m.metaSessions, key)
	m.mu.Unlock()
	if ms != nil && ms.t != nil {
		ms.t.stop("stopped")
	}
	return ms
}

// startMetaFetch runs a torrent engine just long enough to get the file list.
func (m *Manager) startMetaFetch(key string, mg *Magnet) *torrent {
	d := &Download{ID: key, Kind: "torrent", Dir: os.TempDir(), Size: -1,
		Torrent: &TorrentState{InfoHash: hex.EncodeToString(mg.InfoHash[:]), Name: mg.Name, Trackers: mg.Trackers, Peers: mg.Peers}}
	t := m.newTorrent(d, mg.InfoHash, mg.Trackers)
	m.mu.Lock()
	m.torrents[key] = t
	m.mu.Unlock()
	t.addHints(t.ctx, mg.Peers)
	go t.trackerLoop()
	go t.dhtLoop()
	go func() {
		tk := time.NewTicker(time.Second)
		defer tk.Stop()
		n := 0
		for {
			select {
			case <-t.metaReady:
				m.stopMeta(key)
				return
			case <-t.ctx.Done():
				return
			case <-tk.C:
			}
			n++
			t.tick(n)
			m.mu.Lock()
			ms := m.metaSessions[key]
			idle := ms == nil || time.Since(ms.poll) > 2*time.Minute
			m.mu.Unlock()
			if idle { // the dialog went away
				m.stopMeta(key)
				return
			}
		}
	}()
	return t
}

// ---------------------------------------------------------------- choosing files

// SetTorrentFiles changes which files of a torrent are downloaded.
func (m *Manager) SetTorrentFiles(id string, skip []bool) error {
	m.mu.Lock()
	d := m.byID[id]
	if d == nil || d.Torrent == nil {
		m.mu.Unlock()
		return errors.New("not a torrent")
	}
	ih, _ := d.Torrent.hash()
	mi := m.loadTorrent(ih)
	if mi == nil || len(skip) != len(mi.Files) {
		m.mu.Unlock()
		return errors.New("the file list isn't known yet")
	}
	all := true
	for _, s := range skip {
		all = all && s
	}
	if all {
		m.mu.Unlock()
		return errors.New("choose at least one file")
	}
	old := d.Torrent.Skip
	wasSkipped := func(i int) bool { return len(old) == len(skip) && old[i] }
	// Pieces touching a file whose choice changed can't be trusted any more: a newly chosen file's
	// boundary bytes sit in the hidden parts file, and a newly skipped file's bytes are now read
	// from the parts file (zeros). Forget them so they're fetched again, never served as zeros.
	var redo []int
	added := false
	for i, f := range mi.Files {
		if f.Length == 0 || f.Pad || wasSkipped(i) == skip[i] {
			continue
		}
		if !skip[i] {
			added = true
		}
		for p := int(f.Offset / mi.PieceLen); p <= int((f.Offset+f.Length-1)/mi.PieceLen); p++ {
			redo = append(redo, p)
		}
	}
	for _, p := range redo { // a forgotten piece that's still wanted means fetching again
		for _, i := range mi.pieceFiles(p) {
			if !skip[i] && !mi.Files[i].Pad {
				added = true
			}
		}
	}
	d.Torrent.Skip = append([]bool(nil), skip...)
	if have := bitfield(d.Torrent.Have); len(have) == len(newBitfield(mi.NumPieces())) {
		for _, p := range redo {
			have.unset(p)
		}
	}
	t := m.torrents[id]
	wasDone := d.Status == StCompleted
	m.dirty = true
	m.mu.Unlock()
	if t != nil {
		t.mu.Lock()
		for _, p := range redo {
			t.have.unset(p)
		}
		t.applySkip(append([]bool(nil), skip...))
		t.complete = t.wantedLeft() == 0
		for _, p := range t.peers {
			t.updateInterest(p)
		}
		t.mu.Unlock()
	} else {
		m.mu.Lock()
		var size int64
		for i, f := range mi.Files {
			if !skip[i] && !f.Pad {
				size += f.Length
			}
		}
		d.Size = size
		m.mu.Unlock()
	}
	if wasDone && added { // more to download now
		m.mu.Lock()
		d.Status = StPaused
		m.startLocked(d)
		m.mu.Unlock()
	}
	return nil
}

func (s *Server) torrentFiles(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Skip []bool `json:"skip"`
		Prio []int8 `json:"prio"` // per file: -1 low, 0 normal, 1 high
		Seq  *bool  `json:"seq"`  // download in order
		Ends *bool  `json:"ends"` // first and last pieces first
	}
	if readJSON(r, &req) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if req.Prio != nil || req.Seq != nil || req.Ends != nil {
		if err := s.m.SetTorrentPick(req.ID, req.Prio, req.Seq, req.Ends); err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
	}
	if req.Skip != nil || req.Prio == nil && req.Seq == nil && req.Ends == nil {
		if err := s.m.SetTorrentFiles(req.ID, req.Skip); err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
	}
	writeJSON(w, map[string]any{"ok": true})
}

// torrentDetail is one torrent's full state for its progress and Properties windows: peers,
// trackers, the piece map and the files.
func (s *Server) torrentDetail(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if readJSON(r, &req) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	s.m.mu.Lock()
	d := s.m.byID[req.ID]
	t := s.m.torrents[req.ID]
	s.m.mu.Unlock()
	if d == nil || d.Torrent == nil {
		writeJSON(w, map[string]any{"error": "not a torrent"})
		return
	}
	var live *TorrentLive
	if t != nil { // fresh, not the last tick's (t.mu can't be taken while holding m.mu)
		l := t.view(true)
		live = &l
	}
	s.m.mu.Lock()
	v := s.m.torrentView(d, true)
	if live != nil && s.m.torrents[req.ID] == t {
		v.Peers, v.Seeds, v.Known, v.Up, v.Seeding, v.NeedMeta, v.MetaPct = live.Peers, live.Seeds, live.Known, live.Up, live.Seeding, live.NeedMeta, live.MetaPct
		v.Map, v.Trackers, v.PeerList = live.Map, live.Trackers, live.PeerList
		v.Checking, v.CheckPct = live.Checking, live.CheckPct
	}
	s.m.mu.Unlock()
	writeJSON(w, v)
}

// torrentAssoc reports (and with "on", changes) whether Zipline opens magnet links and
// .torrent files.
func (s *Server) torrentAssoc(w http.ResponseWriter, r *http.Request) {
	var req struct {
		On *bool `json:"on"`
	}
	readJSON(r, &req)
	if req.On != nil {
		if err := setTorrentAssociation(*req.On); err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
	}
	sup, on := torrentAssociation()
	writeJSON(w, map[string]any{"supported": sup, "on": on})
}

func (s *Server) torrentAdd(w http.ResponseWriter, r *http.Request) {
	var req TorrentAdd
	if readJSON(r, &req) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if ih, ok := parseTorrentRef(req.Magnet); ok { // "torrent:<hash>"
		req.Magnet, req.Hash = "", hex.EncodeToString(ih[:])
	}
	d, err := s.m.AddTorrent(req)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	s.m.stopMeta("meta:" + d.Torrent.InfoHash)
	writeJSON(w, map[string]any{"id": d.ID})
}

// ---------------------------------------------------------------- the live view

// TorrentView is a torrent's state in the download list.
type TorrentView struct {
	Hash     string            `json:"hash"`
	Peers    int               `json:"peers"`
	Seeds    int               `json:"seeds"`
	Known    int               `json:"known"`
	Up       float64           `json:"up"`
	Uploaded int64             `json:"uploaded"`
	Ratio    float64           `json:"ratio"`
	Seeding  bool              `json:"seeding"`
	Held     bool              `json:"held,omitempty"` // sharing is paused (a metered connection, outside free hours…)
	NeedMeta bool              `json:"needMeta"`
	MetaPct  int               `json:"metaPct"`
	Private  bool              `json:"private,omitempty"`
	Map      []byte            `json:"map,omitempty"`
	Trackers []TrackerView     `json:"trackers,omitempty"`
	PeerList []TorrentPeerView `json:"peerList,omitempty"`
	Files    []tfileView       `json:"files,omitempty"`
	Count    int               `json:"fileCount,omitempty"`
	Magnet   string            `json:"magnet,omitempty"`
	Checking bool              `json:"checking,omitempty"` // reading its files back: "Checking files · 37%"
	CheckPct int               `json:"checkPct,omitempty"`
	Seq      bool              `json:"seq,omitempty"`  // download in order
	Ends     bool              `json:"ends,omitempty"` // first and last pieces first

	NoShare   *ShareError `json:"noShare,omitempty"`   // a finished torrent that couldn't be shared when asked to (treshare.go)
	StopRatio float64     `json:"stopRatio,omitempty"` // sharing again: the ratio where it stops
	torrentCtlView
}

// torrentView builds a download's torrent view. Caller holds m.mu (only reads the engine's
// published state).
func (m *Manager) torrentView(d *Download, detail bool) *TorrentView {
	ts := d.Torrent
	v := &TorrentView{Hash: ts.InfoHash, Uploaded: ts.Uploaded, NeedMeta: !ts.HasMeta, Private: ts.Private, Seq: ts.Seq, Ends: ts.Ends, torrentCtlView: ctlView(ts, detail)}
	if t := m.torrents[d.ID]; t != nil {
		if detail {
			t.wantPeers.Store(time.Now().Unix() + 3)
		}
		if l := t.live.Load(); l != nil {
			v.Peers, v.Seeds, v.Known, v.Up, v.Seeding, v.NeedMeta, v.MetaPct = l.Peers, l.Seeds, l.Known, l.Up, l.Seeding, l.NeedMeta, l.MetaPct
			v.Held = l.Held
			v.Checking, v.CheckPct = l.Checking && d.running, l.CheckPct
			v.Uploaded = max(v.Uploaded, l.Uploaded)
			if detail {
				v.Map, v.Trackers, v.PeerList = l.Map, l.Trackers, l.PeerList
			}
			if ratio, _ := seedTarget(m.settings.Torrent, ts); l.Seeding && l.Base > 0 && d.Size > 0 {
				v.StopRatio = ratio + float64(l.Base)/float64(d.Size) // (it counts from where sharing began again)
			}
		}
		v.Uploaded = max(v.Uploaded, atomic.LoadInt64(&t.uploaded))
	} else if d.Status == StCompleted {
		v.NoShare = d.shareNote
	}
	if d.Size > 0 {
		v.Ratio = float64(v.Uploaded) / float64(d.Size)
	}
	if ih, ok := ts.hash(); ok {
		v.Magnet = magnetLink(ih, ts.Name, ts.Trackers)
		if ts.HasMeta {
			if mi := m.loadTorrent(ih); mi != nil {
				v.Count = len(mi.Files)
			}
		}
	}
	if detail && ts.HasMeta {
		if ih, ok := ts.hash(); ok {
			if mi := m.loadTorrent(ih); mi != nil {
				v.Count = len(mi.Files)
				have := bitfield(ts.Have)
				if v.Map == nil && len(have) == len(newBitfield(mi.NumPieces())) {
					v.Map = pieceMap(mi, have, ts.Skip, d.Status == StCompleted)
				}
				for i, f := range mi.Files {
					if f.Pad {
						continue
					}
					fv := tfileView{Index: i, Path: strings.Join(f.Path, "/"), Size: f.Length, Skip: len(ts.Skip) == len(mi.Files) && ts.Skip[i]}
					if len(ts.Prio) == len(mi.Files) {
						fv.Prio = ts.Prio[i]
					}
					if f.Length > 0 && len(have) == len(newBitfield(mi.NumPieces())) {
						first, last := int(f.Offset/mi.PieceLen), int((f.Offset+f.Length-1)/mi.PieceLen)
						got := 0
						for p := first; p <= last; p++ {
							if have.has(p) {
								got++
							}
						}
						fv.Done = float64(got) / float64(last-first+1)
					}
					if d.Status == StCompleted && !fv.Skip {
						fv.Done = 1
					}
					v.Files = append(v.Files, fv)
				}
			}
		}
	}
	return v
}

// pieceMap sums up which parts of a torrent that isn't running are here (same buckets as the
// running view: 0…100 percent of each part, 255 = not downloading that part).
func pieceMap(mi *Metainfo, have bitfield, skip []bool, done bool) []byte {
	n := mi.NumPieces()
	if n == 0 {
		return nil
	}
	wanted := make([]bool, n)
	for i, f := range mi.Files {
		if f.Pad || f.Length == 0 || len(skip) == len(mi.Files) && skip[i] {
			continue
		}
		for p := int(f.Offset / mi.PieceLen); p <= int((f.Offset+f.Length-1)/mi.PieceLen); p++ {
			wanted[p] = true
		}
	}
	buckets := min(n, 300)
	out := make([]byte, buckets)
	for b := range out {
		lo, hi := b*n/buckets, (b+1)*n/buckets
		got, want := 0, 0
		for i := lo; i < hi; i++ {
			if wanted[i] {
				want++
				if done || have.has(i) {
					got++
				}
			}
		}
		if want == 0 {
			out[b] = 255
		} else {
			out[b] = byte(got * 100 / want)
		}
	}
	return out
}

var _ = context.Background
var _ = path.Base
