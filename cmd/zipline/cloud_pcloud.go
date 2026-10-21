package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// pCloud public links (u.pcloud.link/publink/show?code=…, e.pcloud.link for its EU region) go
// through pCloud's API: showpublink for the name, size and what's in a folder, getpublinkdownload
// for the file's address (a host and a path). A link from the EU region is only known to the EU
// API, so the other region is asked when the first doesn't know the code. A file in a shared
// folder is the folder's link with #fileid=<its id>.

var (
	pcloudAPIs   = []string{"https://api.pcloud.com", "https://eapi.pcloud.com"} // US, EU; replaced in tests
	pcloudScheme = "https://"                                                    // (the file servers'; http in tests)
)

func pcloudHost(h string) bool { return hostIn(h, "pcloud.link", "pcloud.com") }

// pcloudLink reads a public link's code, and the file in a shared folder ("" for the link's own).
func pcloudLink(u *url.URL) (code, fileID string) {
	code = u.Query().Get("code")
	if code == "" { // the older my.pcloud.com/#page=publink&code=…
		if q, err := url.ParseQuery(u.Fragment); err == nil && q.Get("page") == "publink" {
			code = q.Get("code")
		}
	}
	if v, ok := strings.CutPrefix(u.Fragment, "fileid="); ok {
		fileID = v
	}
	return code, fileID
}

type pcMeta struct {
	Name     string   `json:"name"`
	Size     int64    `json:"size"`
	IsFolder bool     `json:"isfolder"`
	FileID   int64    `json:"fileid"`
	Contents []pcMeta `json:"contents"`
}

type pcReply struct {
	Result int      `json:"result"`
	Error  string   `json:"error"`
	Meta   *pcMeta  `json:"metadata"`
	Hosts  []string `json:"hosts"`
	Path   string   `json:"path"`
}

// pcloudCall asks pCloud's API (in the link's region first) for method.
func pcloudCall(ctx context.Context, link *url.URL, method string, q url.Values) (*pcReply, error) {
	apis := pcloudAPIs
	if strings.HasPrefix(strings.ToLower(link.Hostname()), "e.") && len(apis) > 1 {
		apis = []string{apis[1], apis[0]}
	}
	var last *pcReply
	for _, api := range apis {
		var r pcReply
		if _, err := cloudGet(ctx, api+"/"+method+"?"+q.Encode(), nil, &r); err != nil {
			return nil, err
		}
		if r.Result == 0 {
			return &r, nil
		}
		last = &r
		if r.Result != 7001 { // (7001: the code isn't known in this region)
			break
		}
	}
	msg := "pCloud: " + strings.TrimSuffix(last.Error, ".")
	switch last.Result {
	case 7001, 7002, 7004, 7005, 7006: // unknown, deleted, expired, over its traffic or downloads limit
		return nil, permanentErr{errors.New(msg)}
	}
	return nil, fmt.Errorf("%s (error %d)", msg, last.Result)
}

func resolvePCloud(ctx context.Context, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", permanentErr{err}
	}
	code, fileID := pcloudLink(u)
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	q := url.Values{"code": {code}}
	f := &cloudFact{size: -1}
	if fileID == "" {
		r, err := pcloudCall(ctx, u, "showpublink", q)
		if err != nil {
			return "", err
		}
		if r.Meta != nil && r.Meta.IsFolder {
			return "", permanentErr{errCloudFolder}
		}
		if r.Meta != nil {
			f.name, f.size = r.Meta.Name, r.Meta.Size
		}
	} else {
		q.Set("fileid", fileID)
	}
	r, err := pcloudCall(ctx, u, "getpublinkdownload", q)
	if err != nil {
		return "", err
	}
	if len(r.Hosts) == 0 || !strings.HasPrefix(r.Path, "/") {
		return "", errors.New("pCloud didn't hand out the file's address. Try again later")
	}
	addr := pcloudScheme + r.Hosts[0] + r.Path
	noteCloudFact(addr, f)
	return addr, nil
}

// pcloudFolder lists a shared pCloud folder (showpublink gives the whole tree).
func pcloudFolder(ctx context.Context, raw string) (*cloudFolder, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errNotFolder
	}
	code, fileID := pcloudLink(u)
	if fileID != "" {
		return nil, errNotFolder
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	r, err := pcloudCall(ctx, u, "showpublink", url.Values{"code": {code}})
	if err != nil {
		return nil, err
	}
	if r.Meta == nil || !r.Meta.IsFolder {
		return nil, errNotFolder
	}
	share, _, _ := strings.Cut(raw, "#")
	cf := &cloudFolder{Service: "pCloud", Name: r.Meta.Name}
	var walk func(dir string, list []pcMeta, depth int)
	walk = func(dir string, list []pcMeta, depth int) {
		for _, c := range list {
			rel := path.Join(dir, c.Name)
			switch {
			case len(cf.Files) >= maxFolderFiles:
				cf.More = true
				return
			case c.IsFolder && depth >= maxFolderDepth:
				cf.More = true
			case c.IsFolder:
				walk(rel, c.Contents, depth+1)
			case c.FileID != 0:
				cf.Files = append(cf.Files, cloudFile{Path: rel, Size: c.Size, URL: share + "#fileid=" + strconv.FormatInt(c.FileID, 10)})
			}
		}
	}
	walk("", r.Meta.Contents, 0)
	return cf, nil
}
