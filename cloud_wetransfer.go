package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// WeTransfer: a transfer's link (we.tl/t-…, which redirects to wetransfer.com/downloads/<id>/
// [<recipient>/]<security hash>) goes through the internal API WeTransfer's own page uses: POST
// /api/v4/transfers/<id>/download hands out a short-lived direct_link (the file, or a zip of all
// of a transfer's files). That API isn't published and changes now and then, so this is
// best-effort: when it stops answering, open the link in the browser and the extension passes the
// download on.

var wetransferWeb = "https://wetransfer.com" // replaced in tests

func wetransferHost(h string) bool { return hostIn(h, "wetransfer.com", "we.tl") }

// wetransferLink: a short link, or a transfer's download page.
func wetransferLink(u *url.URL) bool {
	if hostIn(u.Hostname(), "we.tl") {
		return len(strings.Trim(u.Path, "/")) > 2
	}
	_, _, _, ok := wetransferParts(u)
	return ok
}

// wetransferParts reads a transfer's download page address.
func wetransferParts(u *url.URL) (id, recipient, hash string, ok bool) {
	p := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(p) < 3 || p[0] != "downloads" {
		return "", "", "", false
	}
	switch len(p) {
	case 3:
		return p[1], "", p[2], true
	case 4:
		return p[1], p[2], p[3], true
	}
	return "", "", "", false
}

var csrfRe = regexp.MustCompile(`name="csrf-token" content="([^"]+)"`)

func resolveWeTransfer(ctx context.Context, raw string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Transport: httpClient.Transport, Jar: jar}
	u, err := url.Parse(raw)
	if err != nil {
		return "", permanentErr{err}
	}
	if hostIn(u.Hostname(), "we.tl") { // a short link: where it leads
		req, _ := http.NewRequestWithContext(ctx, "GET", raw, nil)
		req.Header.Set("User-Agent", browserUA)
		resp, err := c.Do(req)
		if err != nil {
			return "", err
		}
		drainClose(resp.Body)
		u = resp.Request.URL
	}
	id, recipient, hash, ok := wetransferParts(u)
	if !ok {
		return "", permanentErr{errors.New("WeTransfer: this transfer link doesn't lead to a download page (it may have expired)")}
	}
	call := func(method string, body map[string]any, csrf string, out any) (int, error) {
		b, _ := json.Marshal(body)
		req, err := http.NewRequestWithContext(ctx, "POST", wetransferWeb+"/api/v4/transfers/"+url.PathEscape(id)+"/"+method, bytes.NewReader(b))
		if err != nil {
			return 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("User-Agent", browserUA)
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		resp, err := c.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(out)
		return resp.StatusCode, nil
	}
	ask := map[string]any{"security_hash": hash}
	if recipient != "" {
		ask["recipient_id"] = recipient
	}
	// what's in the transfer (best-effort: the name of a single file, or why it can't be had)
	var prep struct {
		State string `json:"state"`
		Files []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"files"`
		Message string `json:"message"`
	}
	call("prepare-download", ask, "", &prep)
	switch prep.State {
	case "expired", "deleted", "blocked":
		return "", permanentErr{errors.New("WeTransfer: this transfer has " + map[string]string{"expired": "expired", "deleted": "been deleted", "blocked": "been blocked"}[prep.State])}
	}
	ask["intent"] = "entire_transfer"
	var out struct {
		Link    string `json:"direct_link"`
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	code, err := call("download", ask, "", &out)
	if err != nil {
		return "", err
	}
	if out.Link == "" && (code == 401 || code == 403 || code == 422) { // it wants the page's session first
		page, _, perr := cloudPageWith(ctx, c, wetransferWeb+"/")
		if perr == nil {
			if m := csrfRe.FindSubmatch(page); m != nil {
				code, err = call("download", ask, string(m[1]), &out)
				if err != nil {
					return "", err
				}
			}
		}
	}
	if !strings.HasPrefix(out.Link, "https://") && !strings.HasPrefix(out.Link, "http://") {
		said := strings.TrimSuffix(cmp.Or(out.Message, out.Error, prep.Message), ".")
		if said == "" {
			return "", errors.New("WeTransfer didn't hand out the download (its service changes now and then). Open the link in your browser and click Download: the extension passes it to Zipline")
		}
		if code == 404 || code == 410 || code == 403 {
			return "", permanentErr{errors.New("WeTransfer: " + said)}
		}
		return "", errors.New("WeTransfer: " + said)
	}
	f := &cloudFact{size: -1}
	if len(prep.Files) == 1 { // (several files come as one zip, named by WeTransfer)
		f.name, f.size = prep.Files[0].Name, prep.Files[0].Size
	}
	noteCloudFact(out.Link, f)
	return out.Link, nil
}

// cloudPageWith fetches a page with a client of its own (its cookies).
func cloudPageWith(ctx context.Context, c *http.Client, u string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", browserUA)
	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	return []byte(pageText(resp, 2<<20)), resp.StatusCode, nil
}
