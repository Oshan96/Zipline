package main

import (
	"bytes"
	"context"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

func TestPlanWindows(t *testing.T) {
	night := PlanSettings{On: true, Start: "00:00", End: "07:00", Days: []int{0, 1, 2, 3, 4, 5, 6}, Margin: 5}
	// 2026-10-12 is a Monday
	for when, want := range map[string]bool{"2026-10-12 00:00": true, "2026-10-12 03:30": true, "2026-10-12 06:54": true, "2026-10-12 06:55": false, "2026-10-12 12:00": false, "2026-10-12 23:59": false} {
		if _, _, in := night.windowAt(at(when)); in != want {
			t.Errorf("00:00–07:00 at %s: in=%v", when, in)
		}
	}
	late := PlanSettings{On: true, Start: "22:00", End: "06:00", Days: []int{5, 6}, Margin: 0} // starts Fri and Sat nights
	for when, want := range map[string]bool{
		"2026-10-16 22:30": true,  // Friday night
		"2026-10-17 05:59": true,  // into Saturday morning
		"2026-10-17 06:00": false, // over
		"2026-10-18 01:00": true,  // Saturday night into Sunday
		"2026-10-19 01:00": false, // Sunday night: not a free night
		"2026-10-13 23:00": false, // Tuesday
	} {
		if _, _, in := late.windowAt(at(when)); in != want {
			t.Errorf("Fri/Sat 22:00–06:00 at %s: in=%v", when, in)
		}
	}
	from, to, ok := late.nextWindow(at("2026-10-13 12:00"))
	if !ok || !from.Equal(at("2026-10-16 22:00")) || !to.Equal(at("2026-10-17 06:00")) {
		t.Fatalf("next window %v–%v", from, to)
	}
	allDay := PlanSettings{On: true, Start: "08:00", End: "08:00", Days: []int{6}} // Saturdays
	if _, _, in := allDay.windowAt(at("2026-10-17 20:00")); !in {
		t.Fatal("equal start and end means the whole day")
	}
	// estimates: 7 hours minus 5 minutes of free time a night
	if fin, n, ok := night.estimateFinish(at("2026-10-12 01:00"), 3600*1000, 1000); !ok || n != 1 || !fin.Equal(at("2026-10-12 02:00")) {
		t.Fatalf("tonight: %v %d %v", fin, n, ok)
	}
	// 10 hours of work starting at 01:00: 5h55 tonight, the rest the next night
	fin, n, ok := night.estimateFinish(at("2026-10-12 01:00"), 10*3600*1000, 1000)
	if !ok || n != 2 || !fin.Equal(at("2026-10-13 04:05")) {
		t.Fatalf("two nights: %v %d %v", fin, n, ok)
	}
	if fin, n, _ := night.estimateFinish(at("2026-10-12 15:00"), 1800*1000, 1000); n != 1 || !fin.Equal(at("2026-10-13 00:30")) {
		t.Fatalf("from the afternoon: %v %d", fin, n)
	}
}

// TestPlanRunsQueueInFreeHours: the Free data hours queue starts with the window and stops
// (keeping progress) when it ends.
func TestPlanRunsQueueInFreeHours(t *testing.T) {
	data := make([]byte, 6<<20)
	rand.Read(data)
	srv := testServer(data, 256<<10, true)
	defer srv.Close()
	m := newTestManager(t)
	now := time.Now()
	m.mu.Lock()
	m.settings.Plan = PlanSettings{On: true, Start: now.Add(-10 * time.Minute).Format("15:04"), End: now.Add(50 * time.Minute).Format("15:04"), Days: []int{0, 1, 2, 3, 4, 5, 6}, Margin: 5}
	m.normalizeSettings()
	m.mu.Unlock()
	if m.queueByID(planQueueID) == nil {
		t.Fatal("switching the plan on should create the Free data hours queue")
	}
	d := m.Add(AddReq{URL: srv.URL + "/movie.mkv", QueueID: planQueueID})
	if d.Status != StQueued {
		t.Fatalf("status %s", d.Status)
	}
	m.planTick(time.Now())
	waitFor(t, m, d, StDownloading, 10*time.Second)
	time.Sleep(600 * time.Millisecond)
	// the window ends
	m.mu.Lock()
	m.settings.Plan.End = time.Now().Add(-1 * time.Minute).Format("15:04")
	m.settings.Plan.Start = time.Now().Add(-2 * time.Hour).Format("15:04")
	m.mu.Unlock()
	m.planTick(time.Now())
	waitFor(t, m, d, StQueued, 10*time.Second)
	for i := 0; i < 100; i++ {
		m.mu.Lock()
		r := d.running
		m.mu.Unlock()
		if !r {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if atomic.LoadInt64(&d.done) == 0 {
		t.Fatal("no progress kept")
	}
	m.mu.Lock()
	ended := false
	for _, n := range m.notices {
		ended = ended || n.Kind == "planEnd"
	}
	v := m.planView(time.Now())
	m.mu.Unlock()
	if !ended || v.In || v.Items != 1 || v.Remaining <= 0 || v.Remaining >= int64(len(data)) {
		t.Fatalf("after the window: ended=%v view=%+v", ended, v)
	}
}

func TestMeteredPausesAndResumes(t *testing.T) {
	data := make([]byte, 8<<20)
	rand.Read(data)
	srv := testServer(data, 256<<10, true)
	defer srv.Close()
	m := newTestManager(t)
	var metered atomic.Bool
	defer setMeteredCheck(func() (bool, bool) { return metered.Load(), true })()
	m.mu.Lock()
	m.settings.Plan.Metered = true
	m.mu.Unlock()
	a := m.Add(AddReq{URL: srv.URL + "/a.bin", Start: true})
	waitFor(t, m, a, StDownloading, 10*time.Second)

	metered.Store(true) // the laptop moved to a phone hotspot
	m.meteredTick()
	waitFor(t, m, a, StPaused, 10*time.Second)
	q := m.Add(AddReq{URL: srv.URL + "/b.bin", QueueID: "main"})
	m.StartQueue("main", false)
	time.Sleep(300 * time.Millisecond)
	m.mu.Lock()
	qs := q.Status
	m.mu.Unlock()
	if qs != StQueued {
		t.Fatalf("queues should wait while metered, got %s", qs)
	}
	c := m.Add(AddReq{URL: srv.URL + "/c.bin", Start: true}) // started by hand: allowed
	waitFor(t, m, c, StDownloading, 10*time.Second)
	m.meteredTick()
	time.Sleep(200 * time.Millisecond)
	m.mu.Lock()
	cs := c.Status
	v := m.planView(time.Now())
	m.mu.Unlock()
	if cs != StDownloading || !v.Metered || v.Paused != 1 {
		t.Fatalf("hand-started download %s, view %+v", cs, v)
	}

	metered.Store(false) // back on Wi-Fi
	m.meteredTick()
	waitFor(t, m, a, StDownloading, 10*time.Second)
	waitFor(t, m, q, StDownloading, 10*time.Second)
}

// simulated line for polite mode: capacity C, a queue that fills when demand passes ~95%.
type simLine struct{ capacity float64 }

func (l simLine) delay(load float64) time.Duration {
	over := max(0, load-0.95*l.capacity) / l.capacity
	return 20*time.Millisecond + time.Duration(min(over*8, 1)*float64(400*time.Millisecond))
}

func TestPoliteBacksOffAndRecovers(t *testing.T) {
	line := simLine{capacity: 4 << 20}
	c := &politeCtl{}
	now := time.Now()
	ours := func(limit, other float64) float64 {
		free := line.capacity - other
		if limit > 0 && limit < free {
			return limit
		}
		return max(free, line.capacity*0.3) // TCP still grabs a share
	}
	step := 0
	// run returns our average speed and the average delay seen by everyone over the last half
	run := func(steps int, other float64) (avg float64, delay time.Duration) {
		var sum float64
		var dsum time.Duration
		for i := 0; i < steps; i++ {
			if step%45 == 0 { // the loop's occasional measurement with the line nearly empty
				c.samples = append(c.samples, rttSample{now, line.delay(other + ours(c.rate, other)/12)})
			}
			step++
			thr := ours(c.rate, other)
			now = now.Add(time.Second)
			d := line.delay(thr + other)
			c.observe(now, d, thr)
			if i >= steps/2 {
				sum += thr
				dsum += d
			}
		}
		half := float64(steps - steps/2)
		return sum / half, dsum / time.Duration(steps-steps/2)
	}
	C := line.capacity
	avg0, d0 := run(60, 0)
	t.Logf("alone: %.0f of %.0f KB/s, delay %v", avg0/1024, C/1024, d0)
	if avg0 < 0.85*C {
		t.Fatalf("alone on the line it should use nearly all of it: %.0f of %.0f KB/s", avg0/1024, C/1024)
	}
	// a video call or a stream on another device needs 1.5 MB/s
	other := 1.5 * (1 << 20)
	avg, delay := run(60, other)
	t.Logf("with 1.5 MB/s of other traffic: ours %.0f KB/s, delay %v (unmanaged it would be %v)", avg/1024, delay, line.delay(C))
	if delay > 90*time.Millisecond {
		t.Fatalf("the other traffic still waits %v on average (full speed would be %v)", delay, line.delay(C))
	}
	if avg < 0.8*(C-other) {
		t.Fatalf("backed off too far: %.0f KB/s of the %.0f KB/s left", avg/1024, (C-other)/1024)
	}
	// it stops: back to (nearly) everything
	avg2, d2 := run(60, 0)
	t.Logf("after: %.0f KB/s, delay %v", avg2/1024, d2)
	if avg2 < 0.85*C {
		t.Fatalf("should speed back up: %.0f KB/s", avg2/1024)
	}
}

func TestPoliteLoopLimitsADownload(t *testing.T) {
	data := make([]byte, 16<<20)
	rand.Read(data)
	srv := testServer(data, 512<<10, true)
	defer srv.Close()
	oldProbe, oldEvery := rttProbe, politeInterval
	var calls int32
	rttProbe = func(context.Context, string) (time.Duration, error) {
		if atomic.AddInt32(&calls, 1) < 4 {
			return 15 * time.Millisecond, nil
		}
		return 250 * time.Millisecond, nil // someone else needs the line
	}
	politeInterval = 40 * time.Millisecond
	defer func() { rttProbe, politeInterval = oldProbe, oldEvery }()
	m := newTestManager(t)
	m.mu.Lock()
	m.settings.Polite = true
	m.mu.Unlock()
	go m.politeLoop() // the manager's own loop started before the interval changed
	d := m.Add(AddReq{URL: srv.URL + "/a.bin", Start: true})
	defer func() { m.Pause([]string{d.ID}); m.waitStopped(d) }() // before the server closes
	waitFor(t, m, d, StDownloading, 10*time.Second)
	deadline := time.Now().Add(8 * time.Second)
	for m.Snapshot(nil).Polite.Rate == 0 && time.Now().Before(deadline) { // (the quiet probe also limits it briefly)
		time.Sleep(50 * time.Millisecond)
	}
	if m.politeLim.Rate() == 0 {
		t.Fatal("polite mode never slowed the download down")
	}
	v := m.Snapshot(nil).Polite
	if !v.On || v.Rate <= 0 || v.QueueMs < 100 {
		t.Fatalf("view %+v", v)
	}
	m.mu.Lock()
	m.settings.Polite = false
	m.mu.Unlock()
	deadline = time.Now().Add(5 * time.Second)
	for m.politeLim.Rate() != 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if m.politeLim.Rate() != 0 {
		t.Fatal("switching polite mode off should lift the limit")
	}
}

// loopAdapters pretends 127.0.0.1 and 127.0.0.2 are two network adapters.
func loopAdapters(t *testing.T, extra ...netIf) {
	var lo *net.Interface
	ifs, _ := net.Interfaces()
	for i := range ifs {
		if ifs[i].Flags&net.FlagLoopback != 0 {
			lo = &ifs[i]
			break
		}
	}
	if lo == nil && runtime.GOOS == "windows" {
		lo = &net.Interface{Index: 1} // Windows' loopback is always interface 1 (Wine can't list adapters)
	}
	if lo == nil {
		t.Skip("no loopback interface")
	}
	old := listNetIfs
	listNetIfs = func() []netIf {
		return append([]netIf{{Name: "lo-a", Index: lo.Index, IP: "127.0.0.1", Kind: "ethernet"}, {Name: "lo-b", Index: lo.Index, IP: "127.0.0.2", Kind: "wifi"}}, extra...)
	}
	mdial.mu.Lock()
	mdial.bad, mdial.bytes, mdial.at = map[string]time.Time{}, map[string]*int64{}, time.Time{}
	mdial.mu.Unlock()
	t.Cleanup(func() {
		listNetIfs = old
		mdial.configure(MultiNet{})
	})
}

func TestSeveralConnectionsAtOnce(t *testing.T) {
	data := make([]byte, 12<<20)
	rand.Read(data)
	var mu sync.Mutex
	sources := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, _, _ := net.SplitHostPort(r.RemoteAddr)
		mu.Lock()
		sources[h]++
		mu.Unlock()
		http.ServeContent(&slowWriter{w: w, rate: 2 << 20, start: time.Now()}, r, "f.bin", time.Now(), bytes.NewReader(data))
	}))
	defer srv.Close()
	loopAdapters(t, netIf{Name: "ghost", Index: 0, IP: "192.0.2.55", Kind: "mobile"}) // not connected
	m := newTestManager(t)
	m.mu.Lock()
	m.settings.Multi = MultiNet{On: true}
	m.settings.Conns = 8
	m.applyLimit()
	m.mu.Unlock()
	d := m.Add(AddReq{URL: srv.URL + "/big.bin", Start: true})
	waitFor(t, m, d, StCompleted, 30*time.Second)
	if b, _ := os.ReadFile(filepath.Join(d.Dir, d.FileName)); !bytes.Equal(b, data) {
		t.Fatal("file differs")
	}
	mu.Lock()
	first := map[string]int{}
	for k, v := range sources {
		first[k] = v
		delete(sources, k)
	}
	mu.Unlock()
	if first["127.0.0.1"] == 0 || first["127.0.0.2"] == 0 {
		t.Fatalf("connections should come from both adapters: %v", first)
	}
	views := netStats(MultiNet{On: true})
	got := map[string]NetIfView{}
	for _, v := range views {
		got[v.Name] = v
	}
	if !got["ghost"].Bad || got["lo-a"].Bad || got["lo-b"].Bad {
		t.Fatalf("the adapter without a connection should be marked: %+v", views)
	}
	t.Logf("requests per source address: %v", first)

	// only the adapters ticked in Options are used
	m.mu.Lock()
	m.settings.Multi = MultiNet{On: true, Use: []string{"lo-b", "lo-a"}}
	m.applyLimit()
	m.mu.Unlock()
	mdial.markBad("ghost", false)
	d2 := m.Add(AddReq{URL: srv.URL + "/big2.bin", Start: true})
	waitFor(t, m, d2, StCompleted, 30*time.Second)
	mu.Lock()
	defer mu.Unlock()
	if sources["127.0.0.1"] == 0 || sources["127.0.0.2"] == 0 || len(sources) != 2 {
		t.Fatalf("sources %v", sources)
	}
}

func TestSeveralConnectionsFTP(t *testing.T) {
	data := make([]byte, 2<<20)
	rand.Read(data)
	srv := newTestFTP(t, data, 0, false)
	loopAdapters(t)
	m := newTestManager(t)
	m.mu.Lock()
	m.settings.Multi = MultiNet{On: true}
	m.applyLimit()
	m.mu.Unlock()
	d := m.Add(AddReq{URL: srv.url("/pub/file.bin"), Start: true})
	waitFor(t, m, d, StCompleted, 20*time.Second)
	if b, _ := os.ReadFile(filepath.Join(d.Dir, d.FileName)); !bytes.Equal(b, data) {
		t.Fatal("file differs")
	}
	srv.mu.Lock()
	src := strings.Join(srv.sources, " ")
	srv.mu.Unlock()
	if atomic.LoadInt32(&srv.foreignData) != 0 {
		t.Fatal("an FTP data connection came from a different adapter than its control connection")
	}
	if !strings.Contains(src, "127.0.0.1") || !strings.Contains(src, "127.0.0.2") {
		t.Fatalf("FTP sessions should spread over both adapters: %s", src)
	}
}

// The Add dialog must not start downloading early while the user is saving data.
func TestNoEarlyStartWhileSavingData(t *testing.T) {
	srv := newSiteServer(t, 2<<20, 1<<20)
	m := newTestManager(t)
	h := (&Server{m: m, token: "t"}).routes()
	setPlan := func(from, to time.Time, metered bool) {
		m.mu.Lock()
		m.settings.Plan = PlanSettings{On: true, Start: from.Format("15:04"), End: to.Format("15:04"), Days: []int{0, 1, 2, 3, 4, 5, 6}, Metered: true}
		m.ensurePlanQueue()
		m.metered = metered
		m.mu.Unlock()
	}
	probe := func() string {
		info := apiCall(t, h, "probe", map[string]any{"url": srv.URL + "/movie.mkv", "early": true})
		id, _ := info["early"].(string)
		if id != "" {
			apiCall(t, h, "early", map[string]any{"action": "cancel", "id": id})
		}
		return id
	}
	now := time.Now()
	setPlan(now.Add(3*time.Hour), now.Add(4*time.Hour), false)
	if probe() != "" {
		t.Fatal("started early outside the free data hours")
	}
	setPlan(now.Add(-time.Hour), now.Add(2*time.Hour), false)
	if probe() == "" {
		t.Fatal("inside the free hours the Add dialog should start early")
	}
	setPlan(now.Add(-time.Hour), now.Add(2*time.Hour), true)
	if probe() != "" {
		t.Fatal("started early on a metered connection")
	}
}
