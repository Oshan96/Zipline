package main

import (
	"bytes"
	"testing"
	"time"
)

// A batch (or any queue) used to open one "Download complete" window per file: the window had no way to
// tell its downloads from one started by hand, since a download leaves its queue as it finishes. The
// completion notice says so now, and the queue counts them for its own tray balloon.
func TestQueueDownloadsAnnouncedWithTheQueue(t *testing.T) {
	srv := testServer(bytes.Repeat([]byte("z"), 64<<10), 0, true)
	defer srv.Close()
	m := newTestManager(t)
	alone := m.Add(AddReq{URL: srv.URL + "/alone.bin", Start: true})
	var batch []*Download
	for _, n := range []string{"/img1.jpg", "/img2.jpg", "/img3.jpg"} {
		batch = append(batch, m.Add(AddReq{URL: srv.URL + n, QueueID: "main"}))
	}
	waitFor(t, m, alone, StCompleted, 10*time.Second)
	m.StartQueue("main", false)
	for _, d := range batch {
		waitFor(t, m, d, StCompleted, 10*time.Second)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.mu.Lock()
		done := false
		for _, n := range m.notices {
			done = done || n.Kind == "queueDone"
		}
		m.mu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	fromQueue := map[string]bool{}
	seen := 0
	for _, n := range m.notices {
		if n.Kind == "complete" {
			fromQueue[n.ID] = n.Queue
			seen++
		}
	}
	if seen != 4 {
		t.Fatalf("%d completion notices, want 4", seen)
	}
	if fromQueue[alone.ID] {
		t.Error("a download started by hand is told on its own")
	}
	for _, d := range batch {
		if !fromQueue[d.ID] {
			t.Errorf("%s finished in the queue: its notice should say so", d.FileName)
		}
		if d.Queue != "" {
			t.Errorf("%s still in the queue after finishing", d.FileName)
		}
	}
	if _, left := m.qDone["main"]; left {
		t.Error("the queue's count is cleared once it has been told")
	}
}

func TestDoneBalloons(t *testing.T) {
	for _, c := range []struct {
		names       []string
		title, text string
	}{
		{[]string{"a.zip"}, "Download complete", "a.zip"},
		{[]string{"a.zip", "b.zip"}, "2 downloads complete", "a.zip and 1 other"},
		{[]string{"a.zip", "b.zip", "c.zip", "d.zip"}, "4 downloads complete", "a.zip and 3 others"},
	} {
		if title, text := doneBalloon(c.names); title != c.title || text != c.text {
			t.Errorf("%v: %q, %q", c.names, title, text)
		}
	}
	for _, c := range []struct {
		done, failed int
		text         string
		warn         bool
	}{
		{50, 0, "50 files downloaded", false},
		{1, 0, "1 file downloaded", false},
		{48, 2, "48 files downloaded, 2 couldn't be", true},
		{0, 3, "3 files couldn't be downloaded", true},
	} {
		title, text, warn := queueBalloon("Main queue", c.done, c.failed)
		if title != "Main queue finished" || text != c.text || warn != c.warn {
			t.Errorf("%d done, %d failed: %q, %q, %v", c.done, c.failed, title, text, warn)
		}
	}
}
