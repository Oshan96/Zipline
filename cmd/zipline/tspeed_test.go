package main

import (
	"bytes"
	"crypto/sha256"
	"io"
	mrand "math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// G6: a fast libtorrent seed on this computer. The download has to be quick and its memory
// bounded: pieces are put together in memory, but the engine never holds more than
// pieceMemory of them (tdisk.go). ZL_SPEED_MB sets the size (default 160 MiB) and
// ZL_SPEED_PIECE the piece size in KiB (default 1024).
func TestTorrentSpeedFromLocalSeed(t *testing.T) {
	if testing.Short() {
		t.Skip("a big download")
	}
	py := ltPython(t)
	envInt := func(k string, def int) int {
		if n, err := strconv.Atoi(os.Getenv(k)); err == nil && n > 0 {
			return n
		}
		return def
	}
	size, piece := int64(envInt("ZL_SPEED_MB", 160))<<20, envInt("ZL_SPEED_PIECE", 1024)<<10
	dir := t.TempDir()
	root := filepath.Join(dir, "Big")
	os.MkdirAll(root, 0o755)
	// two files, so pieces cross a file boundary
	first := size*2/3 + 12345
	rnd := mrand.NewChaCha8([32]byte{7})
	for name, n := range map[string]int64{"one.bin": first, "two.bin": size - first} {
		f, err := os.Create(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.CopyN(f, rnd, n); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	tr := newTestTracker(t)
	mk := ltStart(t, py, "make", root, tr.httpURL(), "-", strconv.Itoa(piece))
	info := mk.wait(t, "torrent", 2*time.Minute)
	seeder := ltStart(t, py, "seed", info["torrent"].(string), dir)
	if r := seeder.wait(t, "port", time.Minute); r["seeding"] != true {
		t.Fatalf("the libtorrent seeder isn't seeding: %v", r)
	}
	m := torrentManager(t)
	m.mu.Lock()
	m.settings.Torrent.SeedRatio = 0 // done when it's downloaded
	m.mu.Unlock()

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peakHeap, peakSys atomic.Uint64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // memory, sampled while it downloads
		defer wg.Done()
		tk := time.NewTicker(50 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
			}
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			if ms.HeapInuse > peakHeap.Load() {
				peakHeap.Store(ms.HeapInuse)
			}
			if ms.Sys > peakSys.Load() {
				peakSys.Store(ms.Sys)
			}
		}
	}()
	d := addTorrentFile(t, m, info["torrent"].(string), nil)
	var began time.Time
	for dl := time.Now().Add(time.Minute); began.IsZero() && time.Now().Before(dl); time.Sleep(5 * time.Millisecond) {
		if atomic.LoadInt64(&d.done) > 0 {
			began = time.Now()
		}
	}
	waitFor(t, m, d, StCompleted, 5*time.Minute)
	took := time.Since(began)
	close(stop)
	wg.Wait()

	for _, name := range []string{"one.bin", "two.bin"} {
		if !bytes.Equal(fileHash(t, filepath.Join(root, name)), fileHash(t, filepath.Join(m.settings.DownloadDir, "got", "Big", name))) {
			t.Fatalf("%s differs", name)
		}
	}
	mb := func(n uint64) uint64 { return n >> 20 }
	speed := float64(size) / took.Seconds() / 1e6
	heap := peakHeap.Load() - min(peakHeap.Load(), base.HeapInuse)
	t.Logf("%d MiB in %d KiB pieces: %v, %.1f MB/s; heap in use grew by at most %d MiB (peak %d MiB), memory from the system peaked at %d MiB",
		size>>20, piece>>10, took.Round(time.Millisecond), speed, mb(heap), mb(peakHeap.Load()), mb(peakSys.Load()))
	if limit := uint64(pieceMemoryLimit() + 64<<20); heap > limit {
		t.Errorf("heap grew by %d MiB, more than the %d MiB the pieces may use plus room for the rest", mb(heap), mb(limit))
	}
	if speed < 15 {
		t.Errorf("%.1f MB/s from a seed on this computer is too slow", speed)
	}
}

func fileHash(t *testing.T, path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	io.Copy(h, f)
	return h.Sum(nil)
}
