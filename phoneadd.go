package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// What the phone does with a link. Adding from the phone goes through the window's own code, not a
// copy of it: the probe that reads the name, size and kind of a link (a plain file, a stream, a
// .torrent or magnet link, a metalink, a link that matches a download whose link expired), the add
// that follows, the torrent file picker and the per-download speed limit. The phone page asks first
// (probe), shows what the Add dialog shows (name, size, queue, files), and then adds. Only the
// fields it may set are passed on, so it can't send headers, cookies or a folder through them.

// phoneReq is what the phone page may send.
type phoneReq struct {
	Name   string   `json:"name"`
	URL    string   `json:"url"`
	Start  bool     `json:"start"`
	Action string   `json:"action"`
	IDs    []string `json:"ids"`
	ID     string   `json:"id"`
	On     bool     `json:"on"`
	KB     int      `json:"kb"`
	Sure   bool     `json:"sure"` // asked about using normal data outside free hours

	QueueID  string `json:"queueId"`  // where "Download later" puts it
	Size     int64  `json:"size"`     // what the probe read
	Probed   bool   `json:"probed"`   // the page has probed the link already
	Meta     bool   `json:"meta"`     // a metalink
	MetaFile int    `json:"metaFile"` // which of its files (-1: all)
	Stop     bool   `json:"stop"`     // a magnet link's file list is no longer wanted
	Skip     []bool `json:"skip"`     // torrent files not to download
	Prio     []int8 `json:"prio"`     // per file: -1 low, 0 normal, 1 high
	Seq      *bool  `json:"seq"`
	Ends     *bool  `json:"ends"`
}

// phoneTor is what the phone's list needs to know about a torrent.
type phoneTor struct {
	Seeding bool `json:"seeding"`
	Meta    bool `json:"meta"`            // the file list is known
	Files   int  `json:"files,omitempty"` // how many
}

// phonePlan is the free data hours as the phone needs them: the Add sheet suggests them outside the hours.
type phonePlan struct {
	On   bool      `json:"on"`
	In   bool      `json:"in"`
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// capture collects a window handler's reply.
type capture struct {
	h    http.Header
	code int
	buf  bytes.Buffer
}

func (c *capture) Header() http.Header         { return c.h }
func (c *capture) Write(b []byte) (int, error) { return c.buf.Write(b) }
func (c *capture) WriteHeader(code int)        { c.code = code }

// viaWindow runs one of the window's handlers with the given request body and returns its reply.
func (p *phoneServer) viaWindow(r *http.Request, h http.HandlerFunc, in any) map[string]any {
	b, _ := json.Marshal(in)
	rq, _ := http.NewRequestWithContext(r.Context(), "POST", "/", bytes.NewReader(b))
	c := &capture{h: http.Header{}, code: http.StatusOK}
	h(c, rq)
	var out map[string]any
	if c.code != http.StatusOK || json.Unmarshal(c.buf.Bytes(), &out) != nil || out == nil {
		return map[string]any{"error": strings.TrimSpace(c.buf.String())}
	}
	return out
}

// window is the window's side, for its handlers.
func (p *phoneServer) window() *Server { return &Server{m: p.m} }

// phoneLinkProblem says what's wrong with a link the phone sent ("" when it's fine).
func phoneLinkProblem(u string) string {
	if !(strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") || isFTP(u) || isMagnet(u)) {
		return "That isn't a web, FTP or magnet link"
	}
	if protectedURL(u) {
		return protectedMsg
	}
	return ""
}

// probe reads a link the way the Add dialog does.
func (p *phoneServer) probe(w http.ResponseWriter, r *http.Request, req *phoneReq) {
	u := strings.TrimSpace(req.URL)
	if msg := phoneLinkProblem(u); msg != "" {
		writeJSON(w, map[string]any{"error": msg, "protected": protectedURL(u)})
		return
	}
	res := p.viaWindow(r, p.window().probe, map[string]any{"url": u})
	for _, k := range []string{"dir", "finalUrl", "early", "mime", "service", "serverMirrors", "checksum"} { // (the window's details, not the phone's)
		delete(res, k)
	}
	writeJSON(w, res)
}

// add adds a link: through the window's add (a plain link, a stream, a metalink) or the torrent add
// (a magnet link, a .torrent), after the probe the page did (or, from a caller that didn't probe, one
// made here), so a .torrent or metalink link is never saved as a plain file.
func (p *phoneServer) add(w http.ResponseWriter, r *http.Request, req *phoneReq) {
	u := strings.TrimSpace(req.URL)
	_, isRef := parseTorrentRef(u)
	if !isRef {
		if msg := phoneLinkProblem(u); msg != "" {
			writeJSON(w, map[string]any{"error": msg})
			return
		}
	}
	queue := req.QueueID
	switch {
	case req.Start:
		queue = ""
	case queue == "":
		queue = "main"
	}
	srv := p.window()
	torrent := func(link string) {
		ta := map[string]any{"magnet": link, "start": req.Start, "queueId": queue}
		if req.Skip != nil {
			ta["skip"], ta["prio"] = req.Skip, req.Prio
		}
		if req.Seq != nil {
			ta["seq"] = *req.Seq
		}
		if req.Ends != nil {
			ta["ends"] = *req.Ends
		}
		p.reply(w, p.viaWindow(r, srv.torrentAdd, ta))
	}
	if isRef || isMagnet(u) {
		torrent(u)
		return
	}
	if !req.Probed && !req.Meta { // nobody has looked at the link: do it now, as the Add dialog would
		res := p.viaWindow(r, srv.probe, map[string]any{"url": u})
		switch {
		case res["protected"] == true:
			writeJSON(w, map[string]any{"error": res["error"]})
			return
		case res["kind"] == "torrent":
			if link, _ := res["url"].(string); link != "" {
				torrent(link)
				return
			}
		case res["metalink"] != nil || res["folder"] != nil: // (a shared folder: all its files, as the window's Add would)
			req.Meta, req.MetaFile = true, -1
		case res["kind"] != "hls":
			if n, _ := res["name"].(string); n != "" {
				req.Name = n
			}
			if sz, _ := res["size"].(float64); sz > 0 {
				req.Size = int64(sz)
			}
		}
	}
	in := map[string]any{"url": u, "name": req.Name, "start": req.Start, "queueId": queue, "size": req.Size}
	if req.Meta {
		in["meta"], in["metaFile"] = true, req.MetaFile
	}
	p.reply(w, p.viaWindow(r, srv.add, in))
}

// reply passes a window handler's answer on: an error as the phone's note, else the new download's id.
func (p *phoneServer) reply(w http.ResponseWriter, res map[string]any) {
	if e, _ := res["error"].(string); e != "" {
		writeJSON(w, map[string]any{"error": cap1(e)})
		return
	}
	out := map[string]any{"ok": true, "id": res["id"]}
	if ids, ok := res["ids"]; ok {
		out["ids"] = ids
	}
	writeJSON(w, out)
}

// renew gives a download whose link expired the link the phone just probed ("Carry on with this link").
func (p *phoneServer) renew(w http.ResponseWriter, r *http.Request, req *phoneReq) {
	u := strings.TrimSpace(req.URL)
	if msg := phoneLinkProblem(u); msg != "" || isMagnet(u) {
		writeJSON(w, map[string]any{"error": "That isn't a web link"})
		return
	}
	res := p.viaWindow(r, p.window().renew, map[string]any{"id": req.ID, "url": u})
	if e, _ := res["error"].(string); e != "" {
		writeJSON(w, map[string]any{"error": cap1(e)})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// torrentMeta fetches a magnet link's file list from peers while the Add sheet waits.
func (p *phoneServer) torrentMeta(w http.ResponseWriter, r *http.Request, req *phoneReq) {
	u := strings.TrimSpace(req.URL)
	if !isMagnet(u) {
		writeJSON(w, map[string]any{"error": "That isn't a magnet link"})
		return
	}
	res := p.viaWindow(r, p.window().torrentMeta, map[string]any{"magnet": u, "stop": req.Stop})
	for _, k := range []string{"dir", "trackers", "webSeeds", "pieces", "pieceLen", "comment"} {
		delete(res, k)
	}
	writeJSON(w, res)
}

// torrentFiles is a torrent in the list: its files and which are chosen, for "Choose files".
func (p *phoneServer) torrentFiles(w http.ResponseWriter, r *http.Request, req *phoneReq) {
	res := p.viaWindow(r, p.window().torrentDetail, map[string]any{"id": req.ID})
	out := map[string]any{}
	for _, k := range []string{"error", "files", "fileCount", "needMeta", "seq", "ends"} {
		if v, ok := res[k]; ok {
			out[k] = v
		}
	}
	writeJSON(w, out)
}

// torrentPick changes which files of a torrent in the list are downloaded, and their priority.
func (p *phoneServer) torrentPick(w http.ResponseWriter, r *http.Request, req *phoneReq) {
	in := map[string]any{"id": req.ID, "skip": req.Skip}
	if req.Prio != nil {
		in["prio"] = req.Prio
	}
	res := p.viaWindow(r, p.window().torrentFiles, in)
	if e, _ := res["error"].(string); e != "" {
		writeJSON(w, map[string]any{"error": cap1(e)})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// limitOne sets one download's own speed limit (KB/s, 0: none).
func (p *phoneServer) limitOne(w http.ResponseWriter, r *http.Request, req *phoneReq) {
	if req.KB < 0 || req.KB > 10_000_000 {
		http.Error(w, "bad request", 400)
		return
	}
	res := p.viaWindow(r, p.window().limit, map[string]any{"id": req.ID, "kb": req.KB})
	if e, _ := res["error"].(string); e != "" {
		writeJSON(w, map[string]any{"error": "That download isn't in the list any more"})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
