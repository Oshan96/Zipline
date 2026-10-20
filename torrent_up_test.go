package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// torrentRow is the torrent view the window gets for download id (nil when there isn't one).
func torrentRow(m *Manager, id string) (*TorrentView, string) {
	for _, v := range m.Snapshot(nil).Downloads {
		if v.ID == id {
			return v.Torrent, v.Status
		}
	}
	return nil, ""
}

// fakeSeeds makes n seeds of mt that tr will dial: each has every piece, unchokes at once and
// answers every request straight away, so only Zipline's own limits slow a download from them.
func fakeSeeds(t *testing.T, tr *torrent, mt *memTorrent, n int) {
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})
	have := newBitfield(mt.mi.NumPieces())
	for i := 0; i < mt.mi.NumPieces(); i++ {
		have.set(i)
	}
	var addrs []netip.AddrPort
	for i := 0; i < n; i++ {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		ap, _ := netip.ParseAddrPort(ln.Addr().String())
		addrs = append(addrs, ap)
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			var h [68]byte
			if _, err := io.ReadFull(c, h[:]); err != nil {
				return
			}
			mine := handshakeBytes(mt.mi.InfoHash, [20]byte([]byte("-XX0001-fakeseedseed")))
			mine[27] = 0 // (plain messages: no fast extension)
			c.Write(mine)
			send := func(msg []byte) {
				c.Write(binary.BigEndian.AppendUint32(nil, uint32(len(msg))))
				c.Write(msg)
			}
			send(append([]byte{msgBitfield}, have...))
			send([]byte{msgUnchoke})
			for {
				var lb [4]byte
				if _, err := io.ReadFull(c, lb[:]); err != nil {
					return
				}
				msg := make([]byte, binary.BigEndian.Uint32(lb[:]))
				if _, err := io.ReadFull(c, msg); err != nil {
					return
				}
				if len(msg) == 13 && msg[0] == msgRequest {
					k, n := readBlockKey(msg[1:])
					send(append(blockMsg(msgPiece, k, 0)[:9], mt.block(k, n)...))
				}
			}
		}()
	}
	tr.addCands(addrs, "test")
}

// limitedDownload starts a download of mt from many seeds under a 100 KB/s speed limit.
func limitedDownload(t *testing.T, mt *memTorrent, seeds int) (*Manager, *Download, *torrent, int) {
	m := torrentManager(t)
	m.mu.Lock()
	m.settings.Torrent.Enc = "plain"
	m.settings.SpeedLimitOn, m.settings.SpeedLimitKB = true, 100
	m.applyLimit()
	m.rememberTorrent(mt.mi, mt.raw)
	m.mu.Unlock()
	d, err := m.AddTorrent(TorrentAdd{Hash: mt.mi.HashHex(), Dir: filepath.Join(m.settings.DownloadDir, "got"), Start: true})
	if err != nil {
		t.Fatal(err)
	}
	tr := runningTorrent(t, m, d.ID)
	fakeSeeds(t, tr, mt, seeds)
	m.mu.Lock()
	port := m.tport
	m.mu.Unlock()
	return m, d, tr, port
}

// A download held to a speed limit still answers the peers that ask it for pieces at once. The
// limit used to be applied to every read from a peer, so with many peers (24 here) a message from
// any of them, a request or the unchoke before it, waited its turn behind each of their reads:
// seconds for every step, and a peer that asks for a block and waits can't take much from us.
func TestLimitedDownloadAnswersAtOnce(t *testing.T) {
	mt := newMemTorrent(t, 6<<20, 32<<10)
	_, _, tr, port := limitedDownload(t, mt, 24)
	piece := -1
	for end := time.Now().Add(40 * time.Second); piece < 0 && time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		tr.mu.Lock()
		for i := 0; i < mt.mi.NumPieces() && piece < 0; i++ {
			if tr.have.has(i) {
				piece = i
			}
		}
		tr.mu.Unlock()
	}
	if piece < 0 {
		t.Fatal("nothing downloaded to share")
	}
	p := dialRawPeer(t, port, mt.mi.InfoHash)
	start := time.Now()
	p.send(msgInterested)
	if !p.waitFor(msgUnchoke, 15*time.Second) {
		t.Fatal("not unchoked")
	}
	p.request(piece, 0, blockSize)
	if !p.waitFor(msgPiece, 15*time.Second) {
		t.Fatal("no block")
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("unchoke and block took %v while downloading at the speed limit; want well under a second", took)
	}
}

// A torrent that is still downloading shares the pieces it already has, and the window shows it:
// the upload rate and the bytes shared both grow before the download is done. A libtorrent peer
// that knows only us takes them as they come in; held up by the speed limit (see above) it got a
// tenth of what we had.
func TestLeechingTorrentUploads(t *testing.T) {
	py := ltPython(t)
	mt := newMemTorrent(t, 3<<20, 32<<10)
	m, d, _, port := limitedDownload(t, mt, 24)
	file := filepath.Join(t.TempDir(), "fast.torrent")
	if err := os.WriteFile(file, mt.raw, 0o644); err != nil {
		t.Fatal(err)
	}
	ltStart(t, py, "leech", file, t.TempDir(), fmt.Sprintf("127.0.0.1:%d", port), "--nopex", "--notracker")

	for end := time.Now().Add(45 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		row, status := torrentRow(m, d.ID)
		if row == nil || status != StDownloading || atomic.LoadInt64(&d.done) >= d.Size {
			break // (finished before enough was shared)
		}
		if row.Up > 0 && row.Uploaded >= 1<<20 {
			return
		}
	}
	row, status := torrentRow(m, d.ID)
	var up float64
	var shared int64
	if row != nil {
		up, shared = row.Up, row.Uploaded
	}
	t.Fatalf("download %s: the window shows %.0f B/s up and %d bytes shared; want a rate above 0 and 1 MiB shared before it's done", status, up, shared)
}
