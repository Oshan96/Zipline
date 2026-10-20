package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Opt-in heavy tests: ZL_BENCH_TS=/path/to/big.ts go test -run Bench
func TestRemuxBench(t *testing.T) {
	in := os.Getenv("ZL_BENCH_TS")
	if in == "" {
		t.Skip()
	}
	t0 := time.Now()
	res, err := remuxTS(context.Background(), in, in+".mp4", nil)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(in)
	el := time.Since(t0)
	t.Logf("%+v %d MB in %v = %.0f MB/s", res, st.Size()>>20, el, float64(st.Size())/1e6/el.Seconds())
}

// TestBenchPauseWhileConverting serves a big .ts as an HLS stream of byte ranges, pauses while
// it's being converted, resumes, and checks the result.
func TestBenchPauseWhileConverting(t *testing.T) {
	in := os.Getenv("ZL_BENCH_TS")
	if in == "" {
		t.Skip()
	}
	st, _ := os.Stat(in)
	const pieces = 40
	step := st.Size() / pieces / 188 * 188
	var pl strings.Builder
	pl.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:10\n")
	for i := int64(0); i < pieces; i++ {
		n := step
		if i == pieces-1 {
			n = st.Size() - i*step
		}
		fmt.Fprintf(&pl, "#EXTINF:6,\n#EXT-X-BYTERANGE:%d@%d\nbig.ts\n", n, i*step)
	}
	pl.WriteString("#EXT-X-ENDLIST\n")
	mux := http.NewServeMux()
	mux.HandleFunc("/v.m3u8", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, pl.String()) })
	mux.HandleFunc("/big.ts", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, in) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	orig := ffmpegPath
	ffmpegPath = func() string { return "" }
	defer func() { ffmpegPath = orig }()

	dir, _ := os.MkdirTemp("", "zlbench")
	defer os.RemoveAll(dir) // best effort: the manager may still be saving its list
	m := NewManager(dir)
	m.settings.DownloadDir = dir
	m.settings.Conns = 8
	t0 := time.Now()
	d := m.Add(AddReq{URL: srv.URL + "/v.m3u8", Name: "big.mp4", Start: true})
	var sawStage string
	for time.Since(t0) < 60*time.Second {
		m.mu.Lock()
		stage, pct := d.stage, d.stagePct
		m.mu.Unlock()
		if stage != "" && pct >= 20 {
			sawStage = fmt.Sprintf("%s %d%%", stage, pct)
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if sawStage == "" {
		t.Fatal("never saw the conversion stage")
	}
	dl := time.Since(t0)
	m.Pause([]string{d.ID})
	waitFor(t, m, d, StPaused, 10*time.Second)
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(d.partPath() + ".mp4tmp"); err == nil {
		t.Fatal("temporary file left after pausing")
	}
	if isMP4File(d.partPath()) {
		t.Fatal("part file changed before the conversion finished")
	}
	t1 := time.Now()
	m.Start([]string{d.ID})
	waitFor(t, m, d, StCompleted, 120*time.Second)
	out := filepath.Join(d.Dir, d.FileName)
	fi, _ := os.Stat(out)
	t.Logf("paused at %q after %v; resumed and finished in %v; %s %d MB, note %q", sawStage, dl.Round(time.Millisecond), time.Since(t1).Round(time.Millisecond), d.FileName, fi.Size()>>20, d.Note)
	if !isMP4File(out) || d.Note != "" || d.stage != "" {
		t.Fatalf("result: %s note=%q stage=%q", d.FileName, d.Note, d.stage)
	}
	os.WriteFile(os.Getenv("ZL_BENCH_TS")+".dl.txt", []byte(out), 0o644)
}
