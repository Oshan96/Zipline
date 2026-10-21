package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Metalinks can list a torrent for their files: <metaurl mediatype="torrent"> (RFC 5854), or a
// url of type "bittorrent" in the older format. Those files go to the torrent engine with the
// metalink's HTTP mirrors as the torrent's web seeds (BEP 19), so peers and mirrors serve it at
// once, as aria2 uses both. For a torrent of one file a mirror's address is the file; for one of
// several, the mirror's address minus the file's path in the torrent is the base the engine adds
// each file's path to (a mirror laid out some other way can't be a web seed). When the torrent
// can't be fetched or read, or doesn't hold the metalink's file, the mirrors download it as usual.

// metalinkTorrent picks a metalink file's torrent (the best by priority): its address, and the
// file's path inside it when it holds several.
func metalinkTorrent(f xmlFile) (link, name string) {
	best := 0
	for _, mu := range f.MetaURLs {
		v := strings.TrimSpace(mu.Value)
		if !strings.EqualFold(strings.TrimSpace(mu.MediaType), "torrent") || !webLink(v) {
			continue
		}
		pri := 999999
		if n, err := strconv.Atoi(mu.Priority); err == nil && n > 0 {
			pri = n
		}
		if link == "" || pri < best {
			link, name, best = v, strings.TrimSpace(mu.Name), pri
		}
	}
	for _, u := range f.Resources.URLs { // metalink 3
		if v := strings.TrimSpace(u.Value); link == "" && strings.EqualFold(u.Type, "bittorrent") && webLink(v) {
			link = v
		}
	}
	return link, name
}

// webLink: an http(s) address (what web seeds and the torrent file itself are fetched from).
func webLink(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https")
}

// metalinkTorrents adds the picked files that a torrent holds as torrent downloads. rest are the
// files for the mirrors: those without a torrent and those its torrent couldn't take. err says
// why a file with no mirrors couldn't be added at all.
func (m *Manager) metalinkTorrents(r AddReq, pick []mlFile) (out []*Download, rest []mlFile, err error) {
	groups := map[string][]mlFile{}
	var order []string
	for _, f := range pick {
		if f.Torrent == "" {
			rest = append(rest, f)
			continue
		}
		if groups[f.Torrent] == nil {
			order = append(order, f.Torrent)
		}
		groups[f.Torrent] = append(groups[f.Torrent], f)
	}
	for _, link := range order {
		d, back, terr := m.addMetalinkTorrent(r, link, groups[link])
		if d != nil {
			out = append(out, d)
		}
		if terr != nil {
			logf("metalink torrent %s: %v", link, terr)
			if err == nil {
				err = terr
			}
		}
		for _, f := range back {
			if len(f.URLs) > 0 {
				rest = append(rest, f)
			}
		}
	}
	return out, rest, err
}

// addMetalinkTorrent fetches one torrent and adds it for the metalink files it holds. back are the
// files it doesn't take (the mirrors download them).
func (m *Manager) addMetalinkTorrent(r AddReq, link string, fs []mlFile) (*Download, []mlFile, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	via := &Download{URL: link, Headers: map[string]string{}}
	for k, v := range r.Headers { // (the metalink site's sign-in stays with that site)
		if (strings.EqualFold(k, "Cookie") || strings.EqualFold(k, "Authorization")) && !sameSite(hostOf(r.URL), hostOf(link)) {
			continue
		}
		via.Headers[k] = v
	}
	req, err := buildRequest(ctx, via, link, "")
	if err != nil {
		return nil, fs, err
	}
	req.Header.Set("Accept", "application/x-bittorrent, */*;q=0.1")
	resp, err := doReq(via, req)
	if err != nil {
		return nil, fs, fmt.Errorf("couldn't get its torrent: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fs, fmt.Errorf("couldn't get its torrent: %w", statusErr{resp.StatusCode, resp.Status, 0})
	}
	mi, err := m.torrentFromBody(resp.Body)
	if err != nil {
		return nil, fs, fmt.Errorf("its torrent can't be read: %w", err)
	}
	idx, in, back := torrentFiles(mi, fs)
	if len(in) == 0 {
		return nil, fs, errors.New("its torrent doesn't hold the metalink's files")
	}
	var skip []bool
	if mi.Multi { // only the files picked from the metalink
		skip = make([]bool, len(mi.Files))
		for i := range skip {
			skip[i] = true
		}
		for _, j := range idx {
			skip[j] = false
		}
	}
	dir := r.Dir
	if !mi.Multi { // (a torrent of several files has its own folders)
		dir, _ = m.metalinkPlace(dir, in[0].path())
	}
	m.mu.Lock()
	for _, d := range m.list { // already there: adding the mirrors' copy too would fetch it twice
		if d.Torrent != nil && strings.EqualFold(d.Torrent.InfoHash, mi.HashHex()) {
			m.mu.Unlock()
			return nil, nil, fmt.Errorf("this torrent is already in the list (%s)", d.FileName)
		}
	}
	m.mu.Unlock()
	q := r.QueueID
	if q == "" && r.Queue {
		q = "main"
	}
	d, err := m.AddTorrent(TorrentAdd{Hash: mi.HashHex(), Skip: skip, Dir: dir, Start: r.Start, QueueID: q, Desc: r.Desc, WebSeeds: webSeedsFor(mi, in, idx)})
	if err != nil {
		return nil, fs, err
	}
	logf("metalink: %s goes to the torrent engine (%d of its files)", mi.Name, len(in))
	return d, back, nil
}

// torrentFiles finds the metalink files in the torrent: in are those it holds (idx: where), back
// the others. A file is found by the path the metalink gives it inside the torrent, else by its
// own path (with or without the torrent's folder), else by a name and size only one file has.
func torrentFiles(mi *Metainfo, fs []mlFile) (idx []int, in, back []mlFile) {
	used := map[int]bool{}
	for _, f := range fs {
		j := -1
		if !mi.Multi {
			if len(in) == 0 && (f.Size <= 0 || f.Size == mi.Total) {
				j = 0
			}
		} else {
			j = torrentFileOf(mi, f, used)
		}
		if j < 0 {
			back = append(back, f)
			continue
		}
		used[j] = true
		idx, in = append(idx, j), append(in, f)
	}
	return idx, in, back
}

func torrentFileOf(mi *Metainfo, f mlFile, used map[int]bool) int {
	fits := func(j int) bool {
		tf := mi.Files[j]
		return !tf.Pad && !used[j] && (f.Size <= 0 || f.Size == tf.Length)
	}
	for _, want := range []string{f.TorrentName, f.path()} {
		want = strings.Trim(strings.ReplaceAll(want, `\`, "/"), "/")
		if want == "" {
			continue
		}
		for j, tf := range mi.Files {
			orig, safe := strings.Join(tf.Orig, "/"), strings.Join(tf.Path, "/")
			if fits(j) && (want == orig || want == mi.OrigName+"/"+orig || want == safe || want == mi.Name+"/"+safe) {
				return j
			}
		}
	}
	found := -1
	for j, tf := range mi.Files {
		if fits(j) && strings.EqualFold(tf.Path[len(tf.Path)-1], f.Name) {
			if found >= 0 {
				return -1 // two of that name: can't tell which
			}
			found = j
		}
	}
	return found
}

// webSeedsFor turns the files' HTTP mirrors into the torrent's web seeds (at most ten, best first).
func webSeedsFor(mi *Metainfo, in []mlFile, idx []int) []string {
	var seeds []string
	for k, f := range in {
		for _, u := range f.URLs {
			pu, err := url.Parse(u.URL)
			if err != nil || !webLink(u.URL) { // (web seeds speak HTTP)
				continue
			}
			seed := u.URL
			if mi.Multi { // BEP 19: the base the torrent's folder and the file's path are added to
				suffix := "/" + mi.OrigName + "/" + strings.Join(mi.Files[idx[k]].Orig, "/")
				if pu.RawQuery != "" || !strings.HasSuffix(pu.Path, suffix) {
					continue
				}
				pu.Path, pu.RawPath, pu.Fragment = strings.TrimSuffix(pu.Path, suffix)+"/", "", ""
				seed = pu.String()
			}
			if !containsStr(seeds, seed) && len(seeds) < 10 {
				seeds = append(seeds, seed)
			}
		}
	}
	return seeds
}
