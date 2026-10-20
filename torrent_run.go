package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// Running torrents inside Zipline: a torrent is a download like any other (queues, speed
// limits, data cap, free hours and metered pauses apply), with extras: the file list can
// come from peers (magnet links), and when it's done it keeps sharing for a while.

type TorrentSettings struct {
	Port      int     `json:"port"`      // listening port for other peers (0 = pick one)
	MaxPeers  int     `json:"maxPeers"`  // connections per torrent
	UpKB      int     `json:"upKB"`      // upload limit, KB/s (0 = none)
	SeedRatio float64 `json:"seedRatio"` // keep sharing until uploaded/size reaches this (0 = don't share after finishing)
	SeedMins  int     `json:"seedMins"`  // …or for at most this long
	NoDHT     bool    `json:"noDHT"`
	NoBrowser bool    `json:"noBrowser"` // the extension leaves magnet links and .torrent files to the browser
	MaxSeeds  int     `json:"maxSeeds"`  // at most this many finished torrents share at once
	MetaMins  int     `json:"metaMins"`  // a magnet link gives up on its file list after this long (-1 = never)
	CheckDone bool    `json:"checkDone"` // read every piece back once a torrent finishes (trecheck.go)
	NoPortMap bool    `json:"noPortMap"` // don't ask the router to open the port (UPnP, NAT-PMP, PCP: portmap.go)
	Enc       string  `json:"enc"`       // protocol encryption: "" (prefer encrypted), "require" or "plain" (mse.go)
}

func defaultTorrentSettings() TorrentSettings {
	return TorrentSettings{MaxPeers: 60, SeedRatio: 1, SeedMins: 120, MaxSeeds: 5, MetaMins: 15}
}

// TorrentState is what the download list keeps about a torrent.
type TorrentState struct {
	InfoHash   string   `json:"infoHash"`
	Name       string   `json:"name,omitempty"`
	Trackers   []string `json:"trackers,omitempty"`
	WebSeeds   []string `json:"webSeeds,omitempty"`
	Peers      []string `json:"peers,omitempty"`  // addresses from the magnet link
	Select     []int    `json:"select,omitempty"` // files chosen before the file list was known
	Skip       []bool   `json:"skip,omitempty"`   // per file: not downloaded
	Have       []byte   `json:"have,omitempty"`   // pieces we have
	HasMeta    bool     `json:"hasMeta,omitempty"`
	Uploaded   int64    `json:"uploaded,omitempty"`
	Downloaded int64    `json:"downloaded,omitempty"`
	Private    bool     `json:"private,omitempty"`
	Category   string   `json:"category,omitempty"` // chosen by you: kept when the file list arrives

	Prio    []int8      `json:"prio,omitempty"`    // per file: -1 low, 0 normal, 1 high (Skip still says which aren't downloaded)
	Seq     bool        `json:"seq,omitempty"`     // download in order (to watch while it downloads)
	Ends    bool        `json:"ends,omitempty"`    // each file's first and last pieces first
	Stamps  []fileStamp `json:"stamps,omitempty"`  // per file: size and modified time when Have was saved (trecheck.go)
	Recheck bool        `json:"recheck,omitempty"` // check every piece before going on ("Check files", Redownload)
	torrentExtras
}

func (ts *TorrentState) hash() (ih [20]byte, ok bool) {
	b, err := hex.DecodeString(ts.InfoHash)
	if err != nil || len(b) != 20 {
		return ih, false
	}
	copy(ih[:], b)
	return ih, true
}

// ---------------------------------------------------------------- .torrent files on disk

func (m *Manager) torrentPath(hexHash string) string {
	return filepath.Join(m.dataDir, "torrents", strings.ToLower(hexHash)+".torrent")
}

// saveTorrentFile keeps a torrent's file list (so a magnet link only needs peers once).
func (m *Manager) saveTorrentFile(mi *Metainfo, raw []byte) {
	if raw == nil {
		top := map[string]any{"info": bencodedRaw(mi.Info)}
		if tr := mi.AllTrackers(); len(tr) > 0 {
			top["announce"] = tr[0]
			var tiers []any
			for _, t := range mi.Trackers {
				tiers = append(tiers, t)
			}
			top["announce-list"] = tiers
		}
		if len(mi.WebSeeds) > 0 {
			top["url-list"] = mi.WebSeeds
		}
		raw = bencode(top)
	}
	p := m.torrentPath(mi.HashHex())
	os.MkdirAll(filepath.Dir(p), 0o700)
	writeAtomic(p, raw)
}

var torrentCache = struct {
	m map[[20]byte]*Metainfo
}{m: map[[20]byte]*Metainfo{}}

// loadTorrent returns a torrent's file list from memory or disk. Caller holds m.mu.
func (m *Manager) loadTorrent(ih [20]byte) *Metainfo {
	if mi := torrentCache.m[ih]; mi != nil {
		return mi
	}
	b, err := os.ReadFile(m.torrentPath(hex.EncodeToString(ih[:])))
	if err != nil {
		return nil
	}
	mi, err := parseTorrent(b)
	if err != nil || mi.InfoHash != ih {
		return nil
	}
	torrentCache.m[ih] = mi
	return mi
}

// rememberTorrent keeps a parsed torrent for the Add dialog and the download. Caller holds m.mu.
func (m *Manager) rememberTorrent(mi *Metainfo, raw []byte) {
	torrentCache.m[mi.InfoHash] = mi
	m.saveTorrentFile(mi, raw)
}

// ---------------------------------------------------------------- adding

// TorrentAdd is a torrent the Add dialog (or the command line) adds.
type TorrentAdd struct {
	Magnet   string `json:"magnet"` // a magnet link, or
	Hash     string `json:"hash"`   // a torrent already read (from a .torrent file or link)
	Skip     []bool `json:"skip"`   // files not to download
	Dir      string `json:"dir"`
	Start    bool   `json:"start"`
	QueueID  string `json:"queueId"`
	Desc     string `json:"desc"`
	Category string `json:"category"` // chosen in the Add dialog ("" = by the biggest file's type)
	Prio     []int8 `json:"prio"`     // per file: -1 low, 0 normal, 1 high
	Seq      bool   `json:"seq"`      // download in order
	Ends     bool   `json:"ends"`     // first and last pieces first

	WebSeeds []string `json:"-"` // a metalink's mirrors for the torrent's data (mltorrent.go)
}

func (m *Manager) AddTorrent(r TorrentAdd) (*Download, error) {
	m.mu.Lock()
	var ts TorrentState
	var mi *Metainfo
	if r.Magnet != "" {
		mg, err := parseMagnet(r.Magnet)
		if err != nil {
			m.mu.Unlock()
			return nil, err
		}
		ts = TorrentState{InfoHash: hex.EncodeToString(mg.InfoHash[:]), Name: mg.Name, Trackers: mg.Trackers, WebSeeds: mg.WebSeeds, Peers: mg.Peers, Select: mg.Select}
		mi = m.loadTorrent(mg.InfoHash) // the Add dialog may have fetched the file list already
	} else {
		b, err := hex.DecodeString(r.Hash)
		if err != nil || len(b) != 20 {
			m.mu.Unlock()
			return nil, errors.New("unknown torrent")
		}
		mi = m.loadTorrent([20]byte(b))
		if mi == nil {
			m.mu.Unlock()
			return nil, errors.New("the torrent file is gone; open it again")
		}
		ts = TorrentState{InfoHash: mi.HashHex()}
	}
	for _, d := range m.list { // the same torrent twice would fight over its files
		if d.Torrent != nil && strings.EqualFold(d.Torrent.InfoHash, ts.InfoHash) {
			m.mu.Unlock()
			return nil, fmt.Errorf("this torrent is already in the list (%s)", d.FileName)
		}
	}
	magnetTrackers := ts.Trackers
	if mi != nil {
		ts.Name, ts.HasMeta, ts.Private = mi.Name, true, mi.Private
		if len(ts.Trackers) == 0 {
			ts.Trackers = mi.AllTrackers()
		} else {
			for _, t := range mi.AllTrackers() {
				if !containsStr(ts.Trackers, t) {
					ts.Trackers = append(ts.Trackers, t)
				}
			}
		}
		ts.WebSeeds = append(ts.WebSeeds, mi.WebSeeds...)
		for _, w := range r.WebSeeds {
			if !containsStr(ts.WebSeeds, w) {
				ts.WebSeeds = append(ts.WebSeeds, w)
			}
		}
		if len(r.Skip) == len(mi.Files) {
			ts.Skip = append([]bool(nil), r.Skip...)
		}
		if len(r.Prio) == len(mi.Files) {
			ts.Prio = append([]int8(nil), r.Prio...)
		}
	}
	ts.Seq, ts.Ends = r.Seq, r.Ends
	ts.setTiers(initialTiers(magnetTrackers, mi)) // (shuffled here, once)
	ts.Key = newTrackerKey()
	if ts.Name == "" {
		ts.Name = ts.InfoHash
	}
	ih, _ := ts.hash()
	d := &Download{ID: newID(), URL: magnetLink(ih, ts.Name, ts.Trackers), Kind: "torrent", Size: -1, Added: time.Now(),
		Conns: m.settings.Conns, Desc: r.Desc, Status: StPaused, Torrent: &ts, Resumable: true}
	d.FileName = ts.Name
	d.Category = m.categoryFor(torrentMainFile(mi, ts.Name))
	if r.Category != "" && r.Category != d.Category && m.isCategory(r.Category) {
		d.Category, ts.Category = r.Category, r.Category
	}
	d.Dir = r.Dir
	if d.Dir == "" {
		d.Dir = m.dirFor(d.Category)
	}
	if mi != nil {
		var size int64
		for i, f := range mi.Files {
			if !f.Pad && (len(ts.Skip) != len(mi.Files) || !ts.Skip[i]) {
				size += f.Length
			}
		}
		d.Size = size
	}
	if q := m.queueByID(r.QueueID); q != nil && !r.Start {
		d.Queue, d.Status = q.ID, StQueued
	}
	m.list = append(m.list, d)
	m.byID[d.ID] = d
	m.dirty = true
	if r.Start {
		m.startLocked(d)
	}
	m.mu.Unlock()
	m.kick()
	return d, nil
}

// isCategory says whether a category exists. Caller holds m.mu.
func (m *Manager) isCategory(name string) bool {
	if name == "General" {
		return true
	}
	for _, c := range m.settings.Categories {
		if c.Name == name {
			return true
		}
	}
	return false
}

func containsStr(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// torrentMainFile is the biggest file's name (it decides the category).
func torrentMainFile(mi *Metainfo, name string) string {
	if mi == nil {
		return name
	}
	best := mi.Files[0]
	for _, f := range mi.Files {
		if f.Length > best.Length {
			best = f
		}
	}
	return best.Path[len(best.Path)-1]
}

// torrentMetaArrived saves a magnet link's file list once peers have sent it. Caller holds t.mu.
func (m *Manager) torrentMetaArrived(d *Download, mi *Metainfo, skip []bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rememberTorrent(mi, nil)
	if ts := d.Torrent; ts != nil {
		ts.HasMeta, ts.Name, ts.Private = true, mi.Name, mi.Private
		if skip != nil {
			ts.Skip = skip
		}
	}
	if d.FileName == "" || d.FileName == d.Torrent.InfoHash || d.FileName != mi.Name {
		d.FileName = mi.Name
	}
	if d.Torrent == nil || d.Torrent.Category == "" {
		d.Category = m.categoryFor(torrentMainFile(mi, mi.Name))
	}
	m.dirty = true
	m.notify("torrentMeta", d.ID, "Got the file list for "+iso(mi.Name))
}

// ---------------------------------------------------------------- running

// upLimiter is the one upload limit shared by every peer of every torrent. Its rate is set only
// when it changes: setting it resets the limiter's balance, which would let each sender through.
func (m *Manager) upLimiter() *Limiter {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.upLim == nil {
		m.upLim = &Limiter{}
	}
	if r := m.upRateWanted(); r != m.upRate {
		m.upRate = r
		m.upLim.SetRate(r)
	}
	return m.upLim
}

func (m *Manager) downloadTorrent(ctx context.Context, d *Download) error {
	m.mu.Lock()
	ts := d.Torrent
	if ts == nil {
		m.mu.Unlock()
		return permanentErr{errors.New("the torrent's details are missing")}
	}
	ih, ok := ts.hash()
	if !ok {
		m.mu.Unlock()
		return permanentErr{errors.New("the torrent's details are damaged")}
	}
	if old := m.torrents[d.ID]; old != nil { // still sharing from an earlier run
		old.cancel()
	}
	var mi *Metainfo
	if ts.HasMeta {
		mi = m.loadTorrent(ih)
	}
	d.lim.SetRate(int64(d.LimitKB) * 1024) // this download's own limit, kept across restarts
	skip := append([]bool(nil), ts.Skip...)
	have := append(bitfield(nil), ts.Have...)
	trackers := append([]string(nil), ts.Trackers...)
	webseeds := append([]string(nil), ts.WebSeeds...)
	hints := append([]string(nil), ts.Peers...)
	up, down, upKB := ts.Uploaded, ts.Downloaded, ts.UpKB
	planQ := d.Queue == planQueueID
	prio, seq, ends := ts.pickState()
	stamps, recheck := append([]fileStamp(nil), ts.Stamps...), ts.Recheck
	m.mu.Unlock()
	t := m.newTorrent(d, ih, trackers)
	t.setUpKB(upKB)
	t.planQ.Store(planQ) // it uploads only in free hours (gate.go)
	tcancel := t.cancel
	atomic.StoreInt64(&t.uploaded, up)
	atomic.StoreInt64(&t.downloaded, down)
	t.mu.Lock()
	t.setPick(prio, &seq, &ends)
	if mi != nil {
		t.setMeta(mi, skip, have)
		t.dropMissingFiles()
		for _, w := range webseeds {
			t.webseeds = append(t.webseeds, &webSeed{url: w})
		}
		t.checkComplete()
	} else {
		atomic.StoreInt64(&d.done, 0)
	}
	t.mu.Unlock()
	if os.MkdirAll(d.Dir, 0o755) != nil {
		tcancel()
		return permanentErr{fmt.Errorf("couldn't create the folder %s", d.Dir)}
	}
	t.addHints(ctx, hints)

	m.mu.Lock()
	m.torrents[d.ID] = t
	m.setStatusLocked(d, StDownloading)
	m.mu.Unlock()
	if err := t.checkOnStart(ctx, stamps, recheck); err != nil { // (files that changed, or asked to)
		t.stop("stopped")
		return err
	}

	go t.trackerLoop()
	go t.dhtLoop()
	go t.webSeedLoop()
	go t.persistLoop()
	go t.extLoop() // peer exchange and the fast extension's timers (fastext.go)

	ticks := time.NewTicker(time.Second)
	defer ticks.Stop()
	n := 0
	t.tick(n)
	for {
		select {
		case <-ctx.Done(): // paused or removed
			t.stop("stopped")
			return ctx.Err()
		case <-t.ctx.Done():
			t.stop("stopped")
			if t.err != nil {
				return t.err
			}
			return errors.New("the torrent stopped")
		case <-t.finishedCh():
			if t.checkWhenDone(ctx) { // pieces to download again (or paused while checking)
				continue
			}
			t.finish()
			return nil
		case <-ticks.C:
			n++
			t.tick(n)
			if err := t.metaTimedOut(); err != nil { // so its queue can finish
				t.stop("stopped")
				return permanentErr{err}
			}
		}
	}
}

var errNoFileList = errors.New("Couldn't get the file list from other peers — try again later")

// metaTimedOut: a magnet link that still hasn't got its file list after Options → Torrents' wait.
func (t *torrent) metaTimedOut() error {
	t.m.mu.Lock()
	mins, unit := t.m.settings.Torrent.MetaMins, t.m.gate.metaUnit
	t.m.mu.Unlock()
	t.mu.Lock()
	waiting := t.mi == nil
	t.mu.Unlock()
	if waiting && mins > 0 && time.Since(t.started) >= time.Duration(mins)*unit {
		return errNoFileList
	}
	return nil
}

// newTorrent sets up a torrent's engine (not started).
func (m *Manager) newTorrent(d *Download, ih [20]byte, trackers []string) *torrent {
	port := m.ensureTorrentListener()
	tctx, tcancel := context.WithCancel(context.Background())
	t := &torrent{m: m, d: d, ih: ih, peerID: newPeerID(), ctx: tctx, cancel: tcancel, started: time.Now(),
		peers: map[netip.AddrPort]*tpeer{}, cands: map[netip.AddrPort]*tcand{}, metaReady: make(chan struct{}), finished: make(chan struct{}),
		banned: map[netip.Addr]bool{}, bannedAt: map[netip.AddrPort]bool{}}
	t.port.Store(int32(port))
	for _, u := range trackers {
		t.trackers = append(t.trackers, &trackerState{url: u, status: "Waiting"})
	}
	return t
}

// addHints adds the peers a magnet link names (x.pe).
func (t *torrent) addHints(ctx context.Context, hints []string) {
	var hintAddrs []netip.AddrPort
	for _, h := range hints {
		if ap, err := netip.ParseAddrPort(h); err == nil {
			hintAddrs = append(hintAddrs, ap)
		} else if addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", hostOnly(h)); err == nil && len(addrs) > 0 {
			hintAddrs = append(hintAddrs, netip.AddrPortFrom(addrs[0].Unmap(), portOf(h)))
		}
	}
	t.addCands(hintAddrs, "magnet link")
}

func (m *Manager) setStatusLocked(d *Download, s string) {
	if d.Status != StPaused && d.Status != StQueued {
		d.Status = s
	}
	m.dirty = true
}

// dropMissingFiles forgets pieces whose files were deleted or cut short. Caller holds t.mu.
func (t *torrent) dropMissingFiles() {
	mi := t.mi
	for i, f := range mi.Files {
		if f.Pad || f.Length == 0 || t.st.skip[i] {
			continue
		}
		st, err := os.Stat(t.st.filePath(i))
		if err == nil && st.Size() >= f.Length {
			continue
		}
		first, last := int(f.Offset/mi.PieceLen), int((f.Offset+f.Length-1)/mi.PieceLen)
		for p := first; p <= last; p++ {
			if t.have.has(p) {
				t.have.unset(p)
				if t.wantBytes[p] > 0 {
					atomic.AddInt64(&t.d.done, -t.wantBytes[p])
				}
			}
		}
	}
}

// persistLoop copies progress into the download list every few seconds.
func (t *torrent) persistLoop() {
	tk := time.NewTicker(5 * time.Second)
	defer tk.Stop()
	for {
		select {
		case <-t.ctx.Done():
			return
		case <-tk.C:
			t.persist()
		}
	}
}

func (t *torrent) persist() {
	t.mu.Lock()
	have := t.have.clone()
	unchecked := t.uncheckedNow()
	t.mu.Unlock()
	stamps := t.stamps(unchecked)
	t.m.mu.Lock()
	if cur := t.m.torrents[t.d.ID]; cur != nil && cur != t { // a newer run owns the saved state now
		t.m.mu.Unlock()
		return
	}
	if ts := t.d.Torrent; ts != nil {
		ts.Have, ts.Stamps = have, stamps
		ts.Uploaded = atomic.LoadInt64(&t.uploaded)
		ts.Downloaded = atomic.LoadInt64(&t.downloaded)
		t.m.dirty = true
	}
	t.m.mu.Unlock()
}

// stop closes every connection and tells the trackers.
func (t *torrent) stop(event string) {
	t.cancel()
	t.mu.Lock()
	var ps []*tpeer
	for _, p := range t.peers {
		ps = append(ps, p)
	}
	gone := startedTrackers(t.trackers)
	st := t.st
	left := t.wantedLeft()
	key := t.trackerKey()
	t.mu.Unlock()
	for _, p := range ps {
		p.close()
	}
	t.drainWrites() // checked pieces still on their way to disk get there first
	t.mu.Lock()
	t.freePieces()
	t.mu.Unlock()
	if st != nil {
		st.Close()
	}
	t.persist() // (after closing the files: the sizes and times saved are the final ones)
	t.m.mu.Lock()
	if t.m.torrents[t.d.ID] == t {
		delete(t.m.torrents, t.d.ID)
	}
	port := t.lport()
	t.m.mu.Unlock()
	t.tellStopped(gone, announceReq{InfoHash: t.ih, PeerID: t.peerID, Port: port, Uploaded: atomic.LoadInt64(&t.uploaded),
		Downloaded: atomic.LoadInt64(&t.downloaded), Left: left, Event: event, Key: key}) // we're leaving
}

// finish runs when every wanted piece is in: tell the trackers and keep sharing for a while.
func (t *torrent) finish() {
	t.persist()
	t.mu.Lock()
	st := t.st
	t.complete = true
	t.finishedHere = true
	for _, tr := range t.trackers {
		tr.next = time.Now() // announce "completed" now
	}
	t.mu.Unlock()
	m := t.m
	m.mu.Lock()
	src, ref := t.d.URL, t.d.Headers["Referer"]
	m.mu.Unlock()
	if st != nil {
		st.finished(src, ref)
	}
	// Whether it shares is settled and marked in one go, so a Stop sharing pressed meanwhile
	// either finds it sharing (and stops it) or is seen here (StopSharing).
	t.mu.Lock()
	m.mu.Lock()
	ratio, mins := seedTarget(m.settings.Torrent, t.d.Torrent)
	share := ratio > 0 && mins > 0 && !t.d.noSeed // (on a metered connection it shares later: see gate.go)
	if share {
		t.seeding, t.seedAt = true, time.Now()
	}
	m.mu.Unlock()
	t.mu.Unlock()
	if !share {
		t.stop("completed")
		t.st.removeParts()
		return
	}
	go t.seed(ratio, time.Duration(mins)*time.Minute)
}

// seed keeps sharing until the ratio or time is reached, sharing is stopped, or the app quits.
// Time while uploads are paused (a metered connection, outside free hours, a used-up quota)
// doesn't count towards the time limit.
func (t *torrent) seed(ratio float64, limit time.Duration) {
	tk := time.NewTicker(time.Second)
	defer tk.Stop()
	var shared time.Duration
	size := max(t.mi.Total, 1)
	n := 0
	for {
		select {
		case <-t.ctx.Done():
			t.endSeeding()
			return
		case <-t.m.quit:
			t.stop("stopped")
			return
		case <-tk.C:
		}
		n++
		t.tick(n)
		ratio, limit = t.seedNow() // (they may have been changed in Properties)
		if !t.upHeld() {
			shared += time.Second
		}
		up := atomic.LoadInt64(&t.uploaded) - t.shareBase // (sharing again counts from where it began)
		if float64(up)/float64(size) >= ratio || shared >= limit {
			t.stop("stopped")
			t.endSeeding()
			return
		}
	}
}

func (t *torrent) endSeeding() {
	t.mu.Lock()
	t.seeding = false
	st := t.st
	t.mu.Unlock()
	if st != nil && !t.moving.Load() { // (a move takes it along)
		st.removeParts()
	}
	t.persist()
	t.m.mu.Lock()
	if t.m.torrents[t.d.ID] == t {
		delete(t.m.torrents, t.d.ID)
	}
	t.m.dirty = true
	t.m.mu.Unlock()
}

// StopSharing ends sharing a finished torrent. Pressed while it isn't sharing yet (a finished
// torrent fetching a piece again after its files were chosen anew, so its run starts over), it
// doesn't share when it gets there (finish). Starting it again, or Share again, clears that.
func (m *Manager) StopSharing(id string) {
	m.mu.Lock()
	t := m.torrents[id]
	if d := m.byID[id]; d != nil && d.Torrent != nil {
		d.noSeed = true
		if d.Status == StCompleted {
			delete(m.meterPaused, id) // (nor when the connection is unmetered again)
		}
	}
	m.mu.Unlock()
	if t != nil {
		t.mu.Lock()
		seeding := t.seeding
		t.mu.Unlock()
		if seeding {
			t.stop("stopped")
		}
	}
}

// ---------------------------------------------------------------- finding peers

func shortErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i > 0 && len(s) > 80 {
		s = s[i+2:]
	}
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

func (t *torrent) dhtLoop() {
	t.m.mu.Lock()
	off := t.m.settings.Torrent.NoDHT
	t.m.mu.Unlock()
	if off {
		return
	}
	<-time.After(200 * time.Millisecond)
	for {
		t.mu.Lock()
		private := t.mi != nil && t.mi.Private
		t.mu.Unlock()
		if private { // private torrents only use their trackers (BEP 27)
			return
		}
		d := t.m.ensureDHT()
		if d != nil && torrentProxy() == nil { // (not while a proxy is on: tproxy.go)
			ctx, cancel := context.WithTimeout(t.ctx, 60*time.Second)
			d.FindAndAnnounce(ctx, t.ih, t.lport(), func(ps []netip.AddrPort) { t.addCands(ps, "DHT") }) // (and tells the DHT we have it)
			cancel()
		}
		t.mu.Lock()
		wait := 5 * time.Minute
		if len(t.peers) < 5 && !t.complete {
			wait = time.Minute
		}
		if t.complete {
			wait = 15 * time.Minute
		}
		t.mu.Unlock()
		select {
		case <-t.ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// ensureDHT starts the shared DHT node on first use (and an IPv6 one beside it when this
// computer has IPv6: dht6.go).
func (m *Manager) ensureDHT() *DHT {
	if torrentProxy() != nil { // torrents go through the proxy only: no DHT (tproxy.go)
		return nil
	}
	m.ensureTorrentListener() // (the DHT shares its UDP port, the one the router is asked to open)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dht != nil {
		return m.dht
	}
	sv := readDHTFile(filepath.Join(m.dataDir, "dht.json"))
	pc := m.torrentPacketConn() // the torrent port's UDP traffic that isn't uTP (udpmux.go)
	if pc == nil {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{}) // no UDP on the torrent port: a socket of its own
		if err != nil {
			return nil
		}
		pc = c
	}
	d := makeDHT(pc, false, sv.nodes, sv.id)
	if pm := m.tnet.portMapper(); pm != nil && pm.reachable() {
		d.SetReachable(true)
	}
	if hasIPv6() {
		if pc6, err := listenDHT6(d.Port()); err == nil {
			d.other = makeDHT(pc6, true, sv.nodes6, sv.id6)
			d.other.other = d
			d.other.start()
		}
	}
	d.start()
	m.dht = d
	go func() { // save good nodes now and then, for a quicker start next time
		tk := time.NewTicker(10 * time.Minute)
		defer tk.Stop()
		for {
			select {
			case <-m.quit:
				m.saveDHT()
				d.Close()
				if d.other != nil {
					d.other.Close()
				}
				return
			case <-d.done: // dropped (dropDHT): a new one takes over
				return
			case <-tk.C:
				m.saveDHT()
			}
		}
	}()
	return d
}

// dropDHT closes the DHT node (the torrent port changed, or a proxy came on); the next lookup
// starts a fresh one on the new port.
func (m *Manager) dropDHT() {
	m.saveDHT()
	m.mu.Lock()
	d := m.dht
	m.dht = nil
	m.mu.Unlock()
	if d != nil {
		d.Close()
		if d.other != nil {
			d.other.Close()
		}
	}
}

func (m *Manager) saveDHT() {
	m.mu.Lock()
	d := m.dht
	m.mu.Unlock()
	if d == nil {
		return
	}
	if b := d.savedState(); b != nil {
		writeAtomic(filepath.Join(m.dataDir, "dht.json"), b)
	}
}

// ---------------------------------------------------------------- incoming connections

// ensureTorrentListener listens for peers that connect to us and returns the port.
func (m *Manager) ensureTorrentListener() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tlisten != nil {
		return m.tport
	}
	port := m.settings.Torrent.Port
	if port == 0 {
		var b [2]byte
		rand.Read(b[:])
		port = 49160 + int(binary.BigEndian.Uint16(b[:]))%16000
		m.settings.Torrent.Port = port
		m.dirty = true
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		ln, err = listenTorrentAny() // the port is taken: any port will do this time
		if err != nil {
			return 0
		}
	}
	m.tlisten = ln
	m.tport = ln.Addr().(*net.TCPAddr).Port
	go m.acceptPeers(noProxyListener{ln}) // (no incoming peers while a proxy is on: tnet.go)
	m.startTorrentNet(m.tport)            // uTP and the DHT on the same port number, and port mapping
	go func() {
		<-m.quit
		ln.Close()
	}()
	return m.tport
}

// restartTorrentListener moves to the port chosen in Options; running torrents tell their
// trackers the new port the next time they ask for peers.
func (m *Manager) restartTorrentListener() {
	m.mu.Lock()
	ln := m.tlisten
	if ln == nil || m.settings.Torrent.Port == m.tport {
		m.mu.Unlock()
		return
	}
	m.tlisten = nil
	m.mu.Unlock()
	ln.Close()
	m.stopTorrentNet() // the old port's UDP side and router mappings go too
	m.dropDHT()        // (it was on the old port's UDP side)
	port := m.ensureTorrentListener()
	m.mu.Lock()
	ts := make([]*torrent, 0, len(m.torrents))
	for _, t := range m.torrents {
		ts = append(ts, t)
	}
	m.mu.Unlock()
	for _, t := range ts { // (t.mu is never taken while holding m.mu)
		t.port.Store(int32(port))
		t.mu.Lock()
		for _, tr := range t.trackers {
			tr.next = time.Now() // tell the trackers the new port
		}
		t.mu.Unlock()
	}
}

func (m *Manager) acceptPeers(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			conn.SetDeadline(time.Now().Add(15 * time.Second))
			var h [68]byte
			conn, err := m.acceptEnc(conn) // a plain handshake, or the encrypted one first (mse.go)
			if err == nil {
				_, err = io.ReadFull(conn, h[:])
			}
			if err != nil || h[0] != 19 || string(h[1:20]) != "BitTorrent protocol" {
				conn.Close()
				return
			}
			var ih [20]byte
			copy(ih[:], h[28:48])
			m.mu.Lock()
			var t *torrent
			for _, x := range m.torrents {
				if x.ih == ih {
					t = x
				}
			}
			m.mu.Unlock()
			if t == nil || bytes.Equal(h[48:68], t.peerID[:]) || !mseMatches(conn, ih) {
				conn.Close()
				return
			}
			if _, err := conn.Write(handshakeBytes(t.ih, t.peerID)); err != nil {
				conn.Close()
				return
			}
			conn.SetDeadline(time.Time{})
			ap, _ := netip.ParseAddrPort(conn.RemoteAddr().String())
			ap = netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
			t.mu.Lock()
			banned := t.banned[ap.Addr()] // (incoming ports change: only bans by address apply)
			t.mu.Unlock()
			if banned {
				conn.Close()
				return
			}
			t.startPeer(conn, ap, false, h)
		}()
	}
}

// ---------------------------------------------------------------- web seeds (BEP 19)

type webSeed struct {
	url   string
	fails int
	dead  bool
	got   int64
}

// webSeedLoop downloads whole pieces from web servers listed in the torrent, alongside peers.
func (t *torrent) webSeedLoop() {
	<-t.metaReady
	t.mu.Lock()
	seeds := t.webseeds
	if len(seeds) == 0 && t.mi != nil {
		for _, w := range t.mi.WebSeeds {
			seeds = append(seeds, &webSeed{url: w})
		}
		t.webseeds = seeds
	}
	t.mu.Unlock()
	for _, ws := range seeds {
		for k := 0; k < 2; k++ {
			go t.webSeedWorker(ws)
		}
	}
}

func (t *torrent) webSeedWorker(ws *webSeed) {
	for t.ctx.Err() == nil {
		t.mu.Lock()
		if ws.dead || t.complete {
			t.mu.Unlock()
			return
		}
		piece := t.pickForWebSeed()
		ps := t.pieces[piece]
		t.mu.Unlock()
		if piece < 0 {
			select {
			case <-t.ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}
		err := t.fetchWebPiece(ws, piece, ps)
		t.mu.Lock()
		if err != nil {
			if t.pieces[piece] == ps && !ps.checking {
				t.dropPiece(piece) // back to the pool
			}
			ws.fails++
			if ws.fails >= 6 {
				ws.dead = true
			}
		} else {
			ws.fails = 0
		}
		fails := ws.fails
		t.mu.Unlock()
		if err != nil && !sleepCtx(t.ctx, time.Duration(fails*fails)*time.Second) {
			return
		}
	}
}

// pickForWebSeed takes a wanted piece nobody is fetching: the most important first, then in
// order or the common ones (peers have the rare ones; see webSeedBetter). Caller holds t.mu.
func (t *torrent) pickForWebSeed() int {
	if t.checking { // (nothing is fetched while the files are checked)
		return -1
	}
	n := t.mi.NumPieces()
	best := -1
	start := int(time.Now().UnixNano() % int64(n))
	for j := 0; j < n; j++ {
		i := (start + j) % n
		if !t.wanted[i] || t.have.has(i) || t.pieces[i] != nil {
			continue
		}
		if best < 0 || t.webSeedBetter(i, best) {
			best = i
		}
	}
	if best >= 0 {
		ps := t.newPiece(best)
		if ps == nil { // no piece memory left for now
			return -1
		}
		ps.web, ps.free = true, 0
		for b := range ps.blocks {
			ps.blocks[b], ps.asked[b] = 1, 1
		}
	}
	return best
}

func (t *torrent) webSeedURL(ws *webSeed, fi int) string {
	f := t.mi.Files[fi]
	esc := func(parts []string) string {
		var out []string
		for _, p := range parts {
			out = append(out, url.PathEscape(p))
		}
		return strings.Join(out, "/")
	}
	if !t.mi.Multi {
		if strings.HasSuffix(ws.url, "/") {
			return ws.url + url.PathEscape(t.mi.OrigName)
		}
		return ws.url
	}
	base := ws.url
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base + url.PathEscape(t.mi.OrigName) + "/" + esc(f.Orig)
}

func (t *torrent) fetchWebPiece(ws *webSeed, piece int, ps *pieceState) error {
	mi := t.mi
	off := int64(piece) * mi.PieceLen
	size := mi.PieceSize(piece)
	buf := make([]byte, size)
	err := t.st.span(off, int(size), func(fi int, fo int64, a, b int) (err error) {
		if mi.Files[fi].Pad {
			return nil
		}
		u := t.webSeedURL(ws, fi)
		ctx, cancel := context.WithTimeout(t.ctx, 2*time.Minute)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Range", rangeHeader(fo, fo+int64(b-a)-1))
		req.Header.Set("User-Agent", defaultUA)
		resp, err := httpClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusPartialContent && !(resp.StatusCode == 200 && fo == 0 && resp.ContentLength == int64(b-a)) {
			if resp.StatusCode == 404 || resp.StatusCode == 410 {
				t.mu.Lock()
				ws.dead = true
				t.mu.Unlock()
			}
			return fmt.Errorf("web seed replied %s", resp.Status)
		}
		got := 0
		defer func() {
			if err != nil {
				atomic.AddInt64(&t.d.done, -int64(got)) // not kept
			}
		}()
		for got < b-a {
			want, _ := t.m.pace(t.ctx, t.d, b-a-got)
			n, rerr := resp.Body.Read(buf[a+got : a+got+max(want, 1)])
			t.m.unpace(t.d, want, n)
			got += n
			if n > 0 {
				atomic.AddInt64(&t.d.done, int64(n))
				atomic.AddInt64(&t.downloaded, int64(n))
				t.downRate.add(n)
			}
			if rerr != nil {
				if got == b-a {
					break
				}
				return rerr
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !t.webPieceDone(piece, ps, buf, mi) { // checked in memory, then written like a piece from peers
		return errors.New("web seed sent a damaged piece")
	}
	t.mu.Lock()
	ws.got += size
	t.mu.Unlock()
	return nil
}

// removeTorrentFiles deletes a removed torrent's leftovers (and its files, when asked).
func (m *Manager) removeTorrentFiles(d *Download, withFiles bool) {
	time.Sleep(300 * time.Millisecond) // let the connections close
	m.mu.Lock()
	ts := d.Torrent
	var mi *Metainfo
	if ts != nil && ts.HasMeta {
		if ih, ok := ts.hash(); ok {
			mi = m.loadTorrent(ih)
		}
	}
	dir := d.Dir
	m.mu.Unlock()
	if mi == nil {
		return
	}
	st := newStorage(mi, dir, make([]bool, len(mi.Files)), mi.HashHex())
	os.Remove(st.parts)
	if !withFiles {
		return
	}
	for i := range mi.Files {
		os.Remove(st.filePath(i))
	}
	if mi.Multi { // and the folders that are now empty, deepest first
		var dirs []string
		filepath.Walk(st.root, func(p string, info os.FileInfo, err error) error {
			if err == nil && info.IsDir() {
				dirs = append(dirs, p)
			}
			return nil
		})
		for i := len(dirs) - 1; i >= 0; i-- {
			os.Remove(dirs[i])
		}
	}
}
