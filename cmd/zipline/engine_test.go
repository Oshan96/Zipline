package main

import (
	"bytes"
	"crypto/sha256"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// throttled per-connection server: each response is capped at perConn bytes/sec
func testServer(data []byte, perConn int, ranges bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ranges {
			r.Header.Del("Range")
		}
		tw := &slowWriter{w: w, rate: perConn, start: time.Now()}
		http.ServeContent(tw, r, "file.bin", time.Now(), bytes.NewReader(data))
	}))
}

type slowWriter struct {
	w     http.ResponseWriter
	rate  int
	start time.Time
	n     int
}

func (s *slowWriter) Header() http.Header { return s.w.Header() }
func (s *slowWriter) WriteHeader(c int)   { s.w.WriteHeader(c) }
func (s *slowWriter) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		c := 32 * 1024
		if c > len(p) {
			c = len(p)
		}
		n, err := s.w.Write(p[:c])
		total += n
		s.n += n
		if err != nil {
			return total, err
		}
		p = p[c:]
		if s.rate > 0 {
			want := time.Duration(float64(s.n) / float64(s.rate) * float64(time.Second))
			if el := time.Since(s.start); el < want {
				time.Sleep(want - el)
			}
		}
	}
	return total, nil
}

func waitFor(t *testing.T, m *Manager, d *Download, st string, max time.Duration) {
	dl := time.Now().Add(max)
	for time.Now().Before(dl) {
		m.mu.Lock()
		s := d.Status
		m.mu.Unlock()
		if s == st {
			return
		}
		if s == StError && st != StError {
			t.Fatalf("download error: %s", d.Error)
		}
		time.Sleep(50 * time.Millisecond)
	}
	m.mu.Lock()
	cur := d.Status
	m.mu.Unlock()
	t.Fatalf("timeout waiting for %s (status %s)", st, cur)
}

func TestMultiConnSpeedAndResume(t *testing.T) {
	data := make([]byte, 24<<20)
	rand.Read(data)
	want := sha256.Sum256(data)
	srv := testServer(data, 2<<20, true) // 2 MB/s per connection
	defer srv.Close()

	m := newTestManager(t) // (not t.TempDir: the list is saved in the background, racing its cleanup)
	m.settings.Conns = 8

	// single connection baseline estimate: 24MB @ 2MB/s = 12s. With 8 conns expect ~1.5-2.5s.
	t0 := time.Now()
	d := m.Add(AddReq{URL: srv.URL + "/file.bin", Start: true})
	time.Sleep(700 * time.Millisecond)
	m.Pause([]string{d.ID})
	waitFor(t, m, d, StPaused, 5*time.Second)
	for {
		m.mu.Lock()
		r := d.running
		m.mu.Unlock()
		if !r {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	part := atomic.LoadInt64(&d.done)
	d.mu.Lock()
	t.Logf("paused at %d bytes, segments=%d", part, len(d.Segments))
	d.mu.Unlock()
	if part == 0 || part >= int64(len(data)) {
		t.Fatalf("unexpected pause point %d", part)
	}
	m.Start([]string{d.ID})
	waitFor(t, m, d, StCompleted, 30*time.Second)
	el := time.Since(t0)
	got, err := os.ReadFile(filepath.Join(d.Dir, d.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(got) != want {
		t.Fatal("checksum mismatch")
	}
	t.Logf("24MB with 8 conns (2MB/s each) in %v → %.1f MB/s", el, 24/el.Seconds())
	if el > 6*time.Second {
		t.Fatalf("too slow: %v", el)
	}
}

func TestNoRangeServer(t *testing.T) {
	data := make([]byte, 3<<20)
	rand.Read(data)
	srv := testServer(data, 0, false)
	defer srv.Close()
	m := newTestManager(t) // (not t.TempDir: the list is saved in the background)
	d := m.Add(AddReq{URL: srv.URL + "/x/file.bin", Start: true})
	waitFor(t, m, d, StCompleted, 10*time.Second)
	got, _ := os.ReadFile(filepath.Join(d.Dir, d.FileName))
	if !bytes.Equal(got, data) {
		t.Fatal("mismatch")
	}
}

func TestDynamicSplitUnevenConnections(t *testing.T) {
	// One very slow segment should get split up by the fast workers.
	data := make([]byte, 16<<20)
	rand.Read(data)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := atomic.AddInt32(&calls, 1)
		rate := 8 << 20
		if c == 2 {
			rate = 100 << 10 // second connection crawls
		}
		http.ServeContent(&slowWriter{w: w, rate: rate, start: time.Now()}, r, "f.bin", time.Now(), bytes.NewReader(data))
	}))
	defer srv.Close()
	m := newTestManager(t) // (not t.TempDir: the list is saved in the background)
	m.settings.Conns = 4
	t0 := time.Now()
	d := m.Add(AddReq{URL: srv.URL + "/f.bin", Start: true})
	waitFor(t, m, d, StCompleted, 60*time.Second)
	got, _ := os.ReadFile(filepath.Join(d.Dir, d.FileName))
	if !bytes.Equal(got, data) {
		t.Fatal("mismatch")
	}
	t.Logf("uneven: %v, requests=%d", time.Since(t0), calls)
}
