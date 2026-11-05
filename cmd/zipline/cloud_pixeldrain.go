package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// Pixeldrain links go through its public API: a file (pixeldrain.com/u/<id>) is /api/file/<id>/info
// for its name, size and SHA-256, then /api/file/<id>?download; a list (/l/<id>, #item=<n> for one of
// its files) is /api/list/<id>; a folder of its filesystem (/d/<path>) is /api/filesystem/<path>?stat,
// and a file in it /api/filesystem/<path>?attach. A file Pixeldrain won't hand out just now (over the
// free download limit, or it wants a person to solve a captcha first) says why in "availability".

var pixeldrainAPI = "https://pixeldrain.com" // replaced in tests

const pixeldrainWeb = "https://pixeldrain.com" // (share links keep the real address)

func pixeldrainHost(h string) bool { return hostIn(h, "pixeldrain.com") }

func pixeldrainID(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// pixeldrainLink reads a link: kind "file", "list" or "fs", the file's or list's id or the filesystem
// path (with its leading "/"), and for a list the one file in it (-1 for the whole list).
func pixeldrainLink(u *url.URL) (kind, id string, item int) {
	p := u.EscapedPath()
	item = -1
	cut := func(prefixes ...string) (string, bool) {
		for _, pre := range prefixes {
			if rest, ok := strings.CutPrefix(p, pre); ok {
				return strings.Trim(rest, "/"), true
			}
		}
		return "", false
	}
	if rest, ok := cut("/u/", "/api/file/"); ok && pixeldrainID(rest) {
		return "file", rest, -1
	}
	if rest, ok := cut("/l/", "/api/list/"); ok && pixeldrainID(rest) {
		if v, ok := strings.CutPrefix(u.Fragment, "item="); ok {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				item = n
			}
		}
		return "list", rest, item
	}
	if rest, ok := cut("/d/", "/api/filesystem/"); ok && rest != "" {
		if dec, err := url.PathUnescape(rest); err == nil && !strings.Contains("/"+dec+"/", "/../") {
			return "fs", "/" + dec, -1
		}
	}
	return "", "", -1
}

type pdFile struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Size         int64  `json:"size"`
	SHA256       string `json:"hash_sha256"`
	Availability string `json:"availability"`
	AvailMsg     string `json:"availability_message"`
}

type pdList struct {
	Title string   `json:"title"`
	Files []pdFile `json:"files"`
}

type pdNode struct {
	Type   string `json:"type"` // "file" or "dir"
	Path   string `json:"path"`
	Name   string `json:"name"`
	Size   int64  `json:"file_size"`
	SHA256 string `json:"sha256_sum"`
}

type pdStat struct {
	Path      []pdNode `json:"path"`
	BaseIndex int      `json:"base_index"`
	Children  []pdNode `json:"children"`
}

// pixeldrainGet asks the API, and turns its refusals into errors in its words.
func pixeldrainGet(ctx context.Context, endpoint string, out any) error {
	var raw json.RawMessage
	code, err := cloudGet(ctx, pixeldrainAPI+endpoint, nil, &raw)
	switch {
	case code == 404:
		return permanentErr{errors.New("Pixeldrain: it isn't there (deleted, or the link is wrong)")}
	case code >= 400:
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Message != "" {
			return fmt.Errorf("Pixeldrain: %s", strings.TrimSuffix(e.Message, "."))
		}
		return fmt.Errorf("Pixeldrain answered %d", code)
	case err != nil:
		return err
	}
	return json.Unmarshal(raw, out)
}

// pixeldrainUnavailable is a file's "availability" as an error (nil when it can be downloaded).
func pixeldrainUnavailable(f pdFile) error {
	if f.Availability == "" {
		return nil
	}
	msg := strings.TrimSuffix(strings.TrimSpace(f.AvailMsg), ".")
	if msg == "" {
		msg = "the file can't be downloaded right now (" + f.Availability + ")"
	}
	return fmt.Errorf("Pixeldrain: %s. Opening the link in your browser may get it", msg)
}

func resolvePixeldrain(ctx context.Context, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", permanentErr{err}
	}
	kind, id, item := pixeldrainLink(u)
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	switch kind {
	case "list":
		if item < 0 {
			return "", permanentErr{errCloudFolder}
		}
		var l pdList
		if err := pixeldrainGet(ctx, "/api/list/"+id, &l); err != nil {
			return "", err
		}
		if item >= len(l.Files) {
			return "", permanentErr{errors.New("Pixeldrain: that file is no longer in the list")}
		}
		id = l.Files[item].ID
		fallthrough
	case "file":
		var f pdFile
		if err := pixeldrainGet(ctx, "/api/file/"+id+"/info", &f); err != nil {
			return "", err
		}
		if err := pixeldrainUnavailable(f); err != nil {
			return "", err
		}
		addr := pixeldrainAPI + "/api/file/" + id + "?download"
		noteCloudFact(addr, &cloudFact{name: f.Name, size: f.Size, sha256: f.SHA256})
		return addr, nil
	case "fs":
		var st pdStat
		if err := pixeldrainGet(ctx, "/api/filesystem"+pdEscape(id)+"?stat", &st); err != nil {
			return "", err
		}
		if st.BaseIndex < 0 || st.BaseIndex >= len(st.Path) || st.Path[st.BaseIndex].Type != "file" {
			return "", permanentErr{errCloudFolder}
		}
		n := st.Path[st.BaseIndex]
		addr := pixeldrainAPI + "/api/filesystem" + pdEscape(n.Path) + "?attach"
		noteCloudFact(addr, &cloudFact{name: n.Name, size: n.Size, sha256: n.SHA256})
		return addr, nil
	}
	return "", permanentErr{errors.New("Zipline doesn't know this kind of Pixeldrain link")}
}

// pdEscape writes a filesystem path for an address, each part escaped.
func pdEscape(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// pixeldrainFolder lists a list's files, or a filesystem folder's (and its folders').
func pixeldrainFolder(ctx context.Context, raw string) (*cloudFolder, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errNotFolder
	}
	kind, id, item := pixeldrainLink(u)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	switch {
	case kind == "list" && item < 0:
		var l pdList
		if err := pixeldrainGet(ctx, "/api/list/"+id, &l); err != nil {
			return nil, err
		}
		cf := &cloudFolder{Service: "Pixeldrain", Name: l.Title}
		for _, f := range l.Files {
			if len(cf.Files) >= maxFolderFiles {
				cf.More = true
				break
			}
			cf.Files = append(cf.Files, cloudFile{Path: f.Name, Size: f.Size, URL: pixeldrainWeb + "/u/" + f.ID})
		}
		return cf, nil
	case kind == "fs":
		var st pdStat
		if err := pixeldrainGet(ctx, "/api/filesystem"+pdEscape(id)+"?stat", &st); err != nil {
			return nil, err
		}
		if st.BaseIndex < 0 || st.BaseIndex >= len(st.Path) || st.Path[st.BaseIndex].Type != "dir" {
			return nil, errNotFolder
		}
		cf := &cloudFolder{Service: "Pixeldrain", Name: st.Path[st.BaseIndex].Name}
		var walk func(dir string, children []pdNode, depth int) error
		walk = func(dir string, children []pdNode, depth int) error {
			for _, c := range children {
				rel := path.Join(dir, c.Name)
				switch {
				case len(cf.Files) >= maxFolderFiles:
					cf.More = true
					return nil
				case c.Name == ".search_index.gz": // (Pixeldrain's own)
				case c.Type == "dir" && depth >= maxFolderDepth:
					cf.More = true
				case c.Type == "dir":
					var sub pdStat
					if err := pixeldrainGet(ctx, "/api/filesystem"+pdEscape(c.Path)+"?stat", &sub); err != nil {
						return err
					}
					if err := walk(rel, sub.Children, depth+1); err != nil {
						return err
					}
				case c.Type == "file":
					cf.Files = append(cf.Files, cloudFile{Path: rel, Size: c.Size, URL: pixeldrainWeb + "/d" + pdEscape(c.Path)})
				}
			}
			return nil
		}
		return cf, walk("", st.Children, 0)
	}
	return nil, errNotFolder
}
