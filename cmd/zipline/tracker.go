package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"zipline/internal/bencode"
)

// Trackers: HTTP(S) (BEP 3, compact peer lists from BEP 23 and BEP 7) and UDP (BEP 15).

type announceReq struct {
	InfoHash   [20]byte
	PeerID     [20]byte
	Port       int
	Uploaded   int64
	Downloaded int64
	Left       int64
	Event      string // "started", "completed", "stopped" or ""
	NumWant    int
	Key        uint32
	TrackerID  string // echoed back when the tracker gave one
}

type announceResp struct {
	Interval    time.Duration
	MinInterval time.Duration // don't ask again sooner than this
	Peers       []netip.AddrPort
	Seeders     int
	Leechers    int
	TrackerID   string
	Warning     string
}

func announce(ctx context.Context, tracker string, r announceReq) (*announceResp, error) {
	u, err := url.Parse(tracker)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "http", "https":
		return announceHTTP(ctx, u, r)
	case "udp":
		return announceUDP(ctx, u.Host, r)
	}
	return nil, fmt.Errorf("unsupported tracker %q", u.Scheme)
}

func announceHTTP(ctx context.Context, u *url.URL, r announceReq) (*announceResp, error) {
	q := fmt.Sprintf("info_hash=%s&peer_id=%s&port=%d&uploaded=%d&downloaded=%d&left=%d&compact=1&numwant=%d&key=%08x",
		url.QueryEscape(string(r.InfoHash[:])), url.QueryEscape(string(r.PeerID[:])), r.Port, r.Uploaded, r.Downloaded, r.Left, r.NumWant, r.Key)
	if r.Event != "" {
		q += "&event=" + r.Event
	}
	if r.TrackerID != "" {
		q += "&trackerid=" + url.QueryEscape(r.TrackerID)
	}
	full := *u
	if full.RawQuery != "" {
		full.RawQuery += "&" + q
	} else {
		full.RawQuery = q
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", full.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Zipline/"+appVersion)
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	v, err := bencode.Decode(body)
	d, ok := v.(map[string]any)
	if f := bencode.Str(d, "failure reason"); err == nil && f != "" { // (some trackers send it with an error status)
		return nil, newTrackerFailure(d, f)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("tracker replied %s", resp.Status)
	}
	if err != nil || !ok {
		return nil, errors.New("the tracker's reply isn't valid")
	}
	out := &announceResp{Interval: 30 * time.Minute}
	if n, ok := bencode.Int(d, "interval"); ok && n > 0 {
		out.Interval = time.Duration(n) * time.Second
	}
	if n, ok := bencode.Int(d, "min interval"); ok && n > 0 {
		out.MinInterval = time.Duration(n) * time.Second
	}
	out.TrackerID, out.Warning = bencode.Str(d, "tracker id"), bencode.Str(d, "warning message")
	if n, ok := bencode.Int(d, "complete"); ok {
		out.Seeders = int(n)
	}
	if n, ok := bencode.Int(d, "incomplete"); ok {
		out.Leechers = int(n)
	}
	switch p := d["peers"].(type) {
	case string:
		out.Peers = append(out.Peers, compactPeers([]byte(p), 6)...)
	case []any:
		for _, e := range p {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			port, _ := bencode.Int(m, "port")
			if ip, err := netip.ParseAddr(bencode.Str(m, "ip")); err == nil && port > 0 && port < 65536 {
				out.Peers = append(out.Peers, netip.AddrPortFrom(ip.Unmap(), uint16(port)))
			}
		}
	}
	if p6 := bencode.Str(d, "peers6"); p6 != "" {
		out.Peers = append(out.Peers, compactPeers([]byte(p6), 18)...)
	}
	return out, nil
}

// compactPeers reads 6-byte (IPv4) or 18-byte (IPv6) peer entries.
func compactPeers(b []byte, size int) []netip.AddrPort {
	var out []netip.AddrPort
	for i := 0; i+size <= len(b); i += size {
		var ip netip.Addr
		if size == 6 {
			ip = netip.AddrFrom4([4]byte(b[i : i+4]))
		} else {
			ip = netip.AddrFrom16([16]byte(b[i : i+16])).Unmap()
		}
		port := binary.BigEndian.Uint16(b[i+size-2:])
		if port != 0 && ip.IsValid() && !ip.IsUnspecified() {
			out = append(out, netip.AddrPortFrom(ip, port))
		}
	}
	return out
}

func compactPeer(ap netip.AddrPort) []byte {
	ip := ap.Addr().Unmap()
	var b []byte
	if ip.Is4() {
		a := ip.As4()
		b = a[:]
	} else {
		a := ip.As16()
		b = a[:]
	}
	return binary.BigEndian.AppendUint16(append([]byte(nil), b...), ap.Port())
}

const udpProtocolID = 0x41727101980

var udpTrackerTimeouts = []time.Duration{4 * time.Second, 8 * time.Second}

func announceUDP(ctx context.Context, hostport string, r announceReq) (*announceResp, error) {
	if !strings.Contains(hostport, ":") {
		return nil, errors.New("the tracker's address has no port")
	}
	conn, err := trackerUDPDial(ctx, hostport) // (through the proxy's relay when torrents use one: tproxy.go)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	v6 := false
	if ua, ok := conn.RemoteAddr().(*net.UDPAddr); ok && ua.IP.To4() == nil {
		v6 = true
	}
	tid := func() uint32 {
		var b [4]byte
		rand.Read(b[:])
		return binary.BigEndian.Uint32(b[:])
	}
	// exchange sends a packet and waits for the reply to it (retrying with longer waits).
	exchange := func(pkt []byte, t uint32, min int) ([]byte, error) {
		buf := make([]byte, 64<<10)
		for _, wait := range udpTrackerTimeouts {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if _, err := conn.Write(pkt); err != nil {
				return nil, err
			}
			deadline := time.Now().Add(wait)
			if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
				deadline = dl
			}
			conn.SetReadDeadline(deadline)
			for {
				n, err := conn.Read(buf)
				if err != nil {
					var ne net.Error
					if errors.As(err, &ne) && ne.Timeout() {
						break // send again
					}
					return nil, err
				}
				if n < 8 || binary.BigEndian.Uint32(buf[4:]) != t {
					continue
				}
				if binary.BigEndian.Uint32(buf) == 3 {
					return nil, fmt.Errorf("tracker: %s", strings.TrimSpace(string(buf[8:n])))
				}
				if n < min {
					return nil, errors.New("the tracker's reply is too short")
				}
				return append([]byte(nil), buf[:n]...), nil
			}
		}
		return nil, errors.New("the tracker didn't answer")
	}
	t := tid()
	pkt := binary.BigEndian.AppendUint64(nil, udpProtocolID)
	pkt = binary.BigEndian.AppendUint32(pkt, 0)
	pkt = binary.BigEndian.AppendUint32(pkt, t)
	resp, err := exchange(pkt, t, 16)
	if err != nil {
		return nil, err
	}
	connID := binary.BigEndian.Uint64(resp[8:])
	event := map[string]uint32{"completed": 1, "started": 2, "stopped": 3}[r.Event]
	t = tid()
	pkt = binary.BigEndian.AppendUint64(nil, connID)
	pkt = binary.BigEndian.AppendUint32(pkt, 1)
	pkt = binary.BigEndian.AppendUint32(pkt, t)
	pkt = append(pkt, r.InfoHash[:]...)
	pkt = append(pkt, r.PeerID[:]...)
	pkt = binary.BigEndian.AppendUint64(pkt, uint64(r.Downloaded))
	pkt = binary.BigEndian.AppendUint64(pkt, uint64(r.Left))
	pkt = binary.BigEndian.AppendUint64(pkt, uint64(r.Uploaded))
	pkt = binary.BigEndian.AppendUint32(pkt, event)
	pkt = binary.BigEndian.AppendUint32(pkt, 0) // our address: the one the packet comes from
	pkt = binary.BigEndian.AppendUint32(pkt, r.Key)
	want := int32(-1)
	if r.NumWant > 0 {
		want = int32(r.NumWant)
	}
	pkt = binary.BigEndian.AppendUint32(pkt, uint32(want))
	pkt = binary.BigEndian.AppendUint16(pkt, uint16(r.Port))
	resp, err = exchange(pkt, t, 20)
	if err != nil {
		return nil, err
	}
	out := &announceResp{
		Interval: time.Duration(binary.BigEndian.Uint32(resp[8:])) * time.Second,
		Leechers: int(binary.BigEndian.Uint32(resp[12:])),
		Seeders:  int(binary.BigEndian.Uint32(resp[16:])),
	}
	if out.Interval < time.Minute {
		out.Interval = 30 * time.Minute
	}
	size := 6
	if v6 {
		size = 18
	}
	out.Peers = compactPeers(resp[20:], size)
	return out, nil
}

// trackerName shortens a tracker address for the window.
func trackerName(t string) string {
	if u, err := url.Parse(t); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return t
}

var _ = strconv.Itoa
