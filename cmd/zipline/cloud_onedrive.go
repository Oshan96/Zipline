package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

// OneDrive personal share links go through OneDrive's sharing API (api.onedrive.com/v1.0/shares/
// u!<the link in base64url>) with the anonymous "badger" token OneDrive's own web viewer uses. It
// isn't documented, so Microsoft can change it; the token is kept until shortly before it runs out
// (about a week). The item's details give the name, the size, the address of the file and its
// quickXorHash, which the finished file is checked against. A link to a folder lists its files,
// and a file in a shared folder is the folder's link with #item=<its id>.

// onedriveHost: OneDrive's personal hosts (onedrive.cloud.microsoft is its newer address).
func onedriveHost(h string) bool {
	return h == "onedrive.live.com" || h == "onedrive.cloud.microsoft" || strings.HasSuffix(h, ".onedrive.cloud.microsoft")
}

// onedriveLink: a link that names an item on a OneDrive host (not its home page).
func onedriveLink(u *url.URL) bool {
	q := u.Query()
	return q.Get("resid") != "" || q.Get("redeem") != "" || q.Get("cid") != "" || q.Get("id") != "" ||
		strings.HasPrefix(u.Path, "/redir") || strings.HasPrefix(u.Path, "/download") || strings.HasPrefix(u.Path, "/embed") || sharepointShare.MatchString(u.Path)
}

// onedriveRef splits a link into the share link and the item in a shared folder ("" for the link's own).
func onedriveRef(raw string) (share, item string) {
	share, frag, _ := strings.Cut(raw, "#")
	if v, ok := strings.CutPrefix(frag, "item="); ok {
		item, _ = url.QueryUnescape(v)
	}
	return share, item
}

var badger = struct {
	sync.Mutex
	url, token string
	until      time.Time
}{}

// onedriveToken is the anonymous token, fetched when there's none (or it ran out, or was refused).
func onedriveToken(ctx context.Context, fresh bool) string {
	badger.Lock()
	defer badger.Unlock()
	if !fresh && badger.token != "" && badger.url == badgerURL && time.Now().Before(badger.until) {
		return badger.token
	}
	body, _ := json.Marshal(map[string]string{"appId": badgerAppID})
	req, err := http.NewRequestWithContext(ctx, "POST", badgerURL, bytes.NewReader(body))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	var out struct {
		Token  string `json:"token"`
		Expiry string `json:"expiryTimeUtc"`
	}
	if code, err := cloudJSON(req, nil, &out); err != nil || code != 200 || out.Token == "" {
		return ""
	}
	until := time.Now().Add(time.Hour)
	if t, err := time.Parse(time.RFC3339, out.Expiry); err == nil {
		until = t.Add(-10 * time.Minute)
	}
	badger.url, badger.token, badger.until = badgerURL, out.Token, until
	return out.Token
}

// odItem is a OneDrive item's details.
type odItem struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	File *struct {
		Hashes struct {
			QuickXor string `json:"quickXorHash"`
		} `json:"hashes"`
	} `json:"file"`
	Folder    *struct{} `json:"folder"`
	DL        string    `json:"@content.downloadUrl"`
	DLGraph   string    `json:"@microsoft.graph.downloadUrl"`
	Children  []odItem  `json:"children"`
	ChildNext string    `json:"children@odata.nextLink"`
	Value     []odItem  `json:"value"`
	Next      string    `json:"@odata.nextLink"`
	Error     *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (it *odItem) downloadURL() string {
	for _, s := range []string{it.DL, it.DLGraph} {
		if strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://") {
			return s
		}
	}
	return ""
}

// onedriveCall asks the API for p (a path under it, or a next-page address on it).
func onedriveCall(ctx context.Context, p string) (*odItem, error) {
	u := p
	if !strings.HasPrefix(p, onedriveAPI+"/") {
		u = onedriveAPI + p
	}
	for try := 0; try < 2; try++ {
		token := onedriveToken(ctx, try > 0)
		hdr := map[string]string{"Prefer": "autoredeem"}
		if token != "" {
			hdr["Authorization"] = "Badger " + token
		}
		var it odItem
		code, err := cloudGet(ctx, u, hdr, &it)
		if code == 401 && try == 0 { // the token ran out early: a fresh one
			continue
		}
		if err == nil && it.Error != nil {
			err = onedriveWords(it.Error.Code, it.Error.Message)
		}
		return &it, err
	}
	return nil, errors.New("OneDrive turned the request away")
}

// onedriveWords is OneDrive's refusal in its own words.
func onedriveWords(code, msg string) error {
	switch {
	case code == "itemNotFound" || code == "notFound":
		return permanentErr{errors.New("OneDrive: " + strings.TrimSuffix(cmp.Or(msg, "The item doesn't exist"), ".") + " (the file was deleted or the link is wrong)")}
	case code == "accessDenied" || code == "unauthenticated" || code == "forbidden":
		return permanentErr{errors.New("OneDrive: " + strings.TrimSuffix(cmp.Or(msg, "Access denied"), ".") + ". Open the link in your browser and click Download: the extension passes it to Zipline")}
	}
	return errors.New("OneDrive: " + cmp.Or(msg, code))
}

// onedriveShareIDs are the ways to name a share link to the API: the "redeem" code it leads to
// first (its own, or the one a 1drv.ms link redirects with), then the link itself.
func onedriveShareIDs(ctx context.Context, d *Download, share string) []string {
	ids := []string{shareID(share)}
	redeem := ""
	if u, err := url.Parse(share); err == nil {
		redeem = u.Query().Get("redeem")
		if redeem == "" && strings.EqualFold(u.Hostname(), "1drv.ms") {
			redeem = oneDriveRedeem(ctx, d, share)
		}
	}
	if redeem != "" {
		ids = append([]string{"u!" + strings.TrimRight(redeem, "=")}, ids...)
	}
	return ids
}

// onedriveItemPaths are where the API answers for a link's item: through the sharing API, and for
// an older link with an authkey (onedrive.live.com/redir?resid=…&authkey=…) by the item itself.
func onedriveItemPaths(share, item string, ids []string) []string {
	var out []string
	for _, id := range ids {
		if item != "" {
			out = append(out, "/shares/"+id+"/items/"+url.PathEscape(item))
		} else {
			out = append(out, "/shares/"+id+"/driveItem")
		}
	}
	if u, err := url.Parse(share); err == nil && item == "" {
		q := u.Query()
		resid, key := cmp.Or(q.Get("resid"), q.Get("id")), q.Get("authkey")
		if cid, _, ok := strings.Cut(resid, "!"); ok && key != "" {
			out = append(out, "/drives/"+url.PathEscape(strings.ToLower(cid))+"/items/"+url.PathEscape(resid)+"?authkey="+url.QueryEscape(key))
		}
	}
	return out
}

// onedrivePersonal resolves a OneDrive personal share link to its file's address.
func onedrivePersonal(ctx context.Context, d *Download, raw string) (string, error) {
	share, item := onedriveRef(raw)
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	ids := onedriveShareIDs(ctx, d, share)
	var said error // OneDrive's own refusal, if it gave one
	for _, p := range onedriveItemPaths(share, item, ids) {
		it, err := onedriveCall(ctx, p)
		var pe permanentErr
		if err != nil && said == nil && errors.As(err, &pe) {
			said = err
		}
		if err != nil {
			continue
		}
		if it.Folder != nil {
			return "", permanentErr{errCloudFolder}
		}
		if dl := it.downloadURL(); dl != "" {
			f := &cloudFact{name: it.Name, size: -1}
			if it.Size > 0 {
				f.size = it.Size
			}
			if it.File != nil {
				f.quickXor = it.File.Hashes.QuickXor
			}
			noteCloudFact(dl, f)
			return dl, nil
		}
	}
	if item == "" { // the older way, still answered for some links
		for _, id := range ids {
			if req, err := http.NewRequestWithContext(ctx, "GET", onedriveAPI+"/shares/"+id+"/root/content", nil); err == nil {
				req.Header.Set("Range", "bytes=0-0")
				if resp, err := httpClient.Do(req); err == nil {
					drainClose(resp.Body)
					if resp.StatusCode < 300 && !isHTML(resp.Header) {
						return resp.Request.URL.String(), nil
					}
				}
			}
		}
	}
	if said != nil {
		return "", said
	}
	return "", permanentErr{errors.New("OneDrive didn't hand out the file for this link. Open it in your browser and click Download: the extension passes it to Zipline")}
}

// onedriveFolder lists a shared OneDrive folder.
func onedriveFolder(ctx context.Context, raw string) (*cloudFolder, error) {
	share, item := onedriveRef(raw)
	if u, err := url.Parse(share); err != nil || item != "" || strings.HasSuffix(strings.ToLower(u.Hostname()), ".sharepoint.com") {
		return nil, errNotFolder // (work and school links need a sign-in to be listed)
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	var root *odItem
	var sid string
	var lastErr error
	for _, id := range onedriveShareIDs(ctx, &Download{URL: share}, share) {
		it, err := onedriveCall(ctx, "/shares/"+id+"/driveItem?$expand=children")
		if err == nil {
			root, sid = it, id
			break
		}
		lastErr = err
	}
	if root == nil {
		return nil, lastErr
	}
	if root.Folder == nil {
		return nil, errNotFolder
	}
	cf := &cloudFolder{Service: "OneDrive", Name: root.Name}
	var walk func(dir, id string, list []odItem, next string, depth int) error
	walk = func(dir, id string, list []odItem, next string, depth int) error {
		if list == nil && next == "" {
			next = "/shares/" + sid + "/items/" + url.PathEscape(id) + "/children"
		}
		for {
			for _, c := range list {
				rel := path.Join(dir, c.Name)
				switch {
				case len(cf.Files) >= maxFolderFiles:
					cf.More = true
					return nil
				case c.Folder != nil && depth >= maxFolderDepth:
					cf.More = true
				case c.Folder != nil:
					if err := walk(rel, c.ID, nil, "", depth+1); err != nil {
						return err
					}
				case c.ID != "":
					cf.Files = append(cf.Files, cloudFile{Path: rel, Size: c.Size, URL: share + "#item=" + url.QueryEscape(c.ID)})
				}
			}
			if next == "" {
				return nil
			}
			if strings.HasPrefix(next, "http") && !strings.HasPrefix(next, onedriveAPI+"/") {
				return nil // (the token only goes to the API)
			}
			page, err := onedriveCall(ctx, next)
			if err != nil {
				return err
			}
			list, next = page.Value, page.Next
		}
	}
	return cf, walk("", root.ID, root.Children, root.ChildNext, 0)
}
