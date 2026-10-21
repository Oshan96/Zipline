package main

import (
	"net"
	"slices"
	"strings"
	"sync"
	"time"
)

// Network watch. Connections that failed wait longer and longer before trying again, which is
// right for a busy server but slow after the Wi-Fi drops for a minute or the laptop sleeps. So
// Zipline watches the network adapters' addresses and the clock: when an adapter connects, an
// address changes or the computer wakes up, everyone waiting to retry is woken at once.

var (
	netMu    sync.Mutex
	netCh    = make(chan struct{})
	netOnce  sync.Once
	netEvery = 2 * time.Second
	// onlineCheck reports whether some network adapter is connected (replaced in tests). When
	// the adapters can't be listed at all, it says yes: failures then count as usual.
	onlineCheck = func() bool { a, err := listNetAddrs(); return err != nil || len(a) > 0 }
)

// netChanged is closed when the network changes or the computer wakes from sleep (each change
// closes it and puts a new one in its place: take it before waiting).
func netChanged() <-chan struct{} {
	netMu.Lock()
	defer netMu.Unlock()
	return netCh
}

// signalNetChange wakes everyone waiting on netChanged.
func signalNetChange() {
	netMu.Lock()
	close(netCh)
	netCh = make(chan struct{})
	netMu.Unlock()
}

// online: some network adapter other than loopback is up with an address. While it's false a
// failed connection isn't counted against its tries: there's nothing to try.
func online() bool { return onlineCheck() }

// startNetWatch starts the watcher (once for the whole app).
func startNetWatch() { netOnce.Do(func() { go netWatch() }) }

func netWatch() {
	last := strings.Join(netAddrs(), " ")
	prev := time.Now()
	for {
		time.Sleep(netEvery)
		now := time.Now()
		// The monotonic clock stops while the computer sleeps; the wall clock doesn't.
		slept := now.Round(0).Sub(prev.Round(0))-now.Sub(prev) > 5*time.Second
		cur := strings.Join(netAddrs(), " ")
		if slept || cur != last {
			signalNetChange()
		}
		last, prev = cur, now
	}
}

// netAddrs lists the addresses of the adapters that are up (loopback and link-local left out).
func netAddrs() []string {
	a, _ := listNetAddrs()
	return a
}

func listNetAddrs() ([]string, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
				out = append(out, i.Name+"="+ipn.IP.String())
			}
		}
	}
	slices.Sort(out)
	return out, nil
}
