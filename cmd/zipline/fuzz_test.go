package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Fuzz tests: whatever a server sends, the readers must return an error, never crash.
// Run with: go test -fuzz FuzzTS -fuzztime 60s   (the seeds below also run as normal tests)

// watchdog fails the run (so the fuzzer saves the input) if work takes far too long.
func watchdog(t *testing.T, data []byte, work func()) {
	done := make(chan struct{})
	go func() { defer close(done); work() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		os.WriteFile(filepath.Join(os.TempDir(), "zipline-fuzz-hang.bin"), data, 0o644)
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("still running after 10s:\n%s", buf[:n])
	}
}

func fuzzFile(t *testing.T, data []byte) string {
	p := filepath.Join(t.TempDir(), "in")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func seedFiles(f *testing.F, globs ...string) {
	for _, g := range globs {
		files, _ := filepath.Glob(g)
		for _, p := range files {
			if b, err := os.ReadFile(p); err == nil && len(b) < 400<<10 {
				f.Add(b)
			}
		}
	}
}

func FuzzTS(f *testing.F) {
	pkt := make([]byte, 188*4)
	for i := 0; i < 4; i++ {
		pkt[i*188] = 0x47
	}
	f.Add(pkt)
	seedFiles(f, "testdata/fuzz-seed/*.ts")
	f.Fuzz(func(t *testing.T, data []byte) {
		in := fuzzFile(t, data)
		watchdog(t, data, func() {
			remuxFiles(context.Background(), in+".mp4", []string{in}, nil, nil) // no recover: a crash must show up here
		})
	})
}

func FuzzFMP4(f *testing.F) {
	f.Add([]byte("\x00\x00\x00\x10ftypisom\x00\x00\x00\x00\x00\x00\x00\x08moov"))
	seedFiles(f, "testdata/fuzz-seed/*.mp4")
	f.Fuzz(func(t *testing.T, data []byte) {
		in := fuzzFile(t, data)
		remuxFiles(context.Background(), in+".mp4", []string{in}, nil, nil) // no recover: a crash must show up here
		readFMP4(context.Background(), in, nil)                             // straight call, no recover
	})
}

func FuzzMKV(f *testing.F) {
	f.Add([]byte{0x1A, 0x45, 0xDF, 0xA3, 0x80, 0x18, 0x53, 0x80, 0x67, 0xFF})
	seedFiles(f, "testdata/fuzz-seed/*.webm")
	f.Fuzz(func(t *testing.T, data []byte) {
		in := fuzzFile(t, data)
		readMKV(context.Background(), in, nil)
		muxFiles(context.Background(), &muxJob{out: in + ".mkv", ins: []muxIn{{path: in}}}, nil) // no recover
	})
}

func FuzzPackedAudio(f *testing.F) {
	f.Add(append(id3Timestamp(126000), bytes.Repeat([]byte{0xFF, 0xF1, 0x50, 0x80, 0x02, 0x1F, 0xFC}, 4)...))
	f.Fuzz(func(t *testing.T, data []byte) {
		in := fuzzFile(t, data)
		(&packedSource{path: in}).scan(context.Background(), nil)
	})
}

func FuzzMPD(f *testing.F) {
	f.Add(`<?xml version="1.0"?><MPD type="static" mediaPresentationDuration="PT6S"><Period><AdaptationSet contentType="video" mimeType="video/mp4"><SegmentTemplate media="v$Number%05d$.m4s" initialization="i.mp4" duration="2" timescale="1"><SegmentTimeline><S t="0" d="2" r="-1"/></SegmentTimeline></SegmentTemplate><Representation id="v" bandwidth="1" width="2" height="2"/></AdaptationSet></Period></MPD>`)
	f.Fuzz(func(t *testing.T, body string) {
		m := &Manager{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // no network: a manifest that needs it just fails
		m.resolveDASH(ctx, &Download{}, "http://127.0.0.1:1/x.mpd", body, 0)
	})
}
