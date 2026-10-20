package main

import (
	"cmp"
	"context"
	"errors"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// Yandex Disk public links (disk.yandex.ru/d/…, yadi.sk/d/…, disk.360.yandex.ru/d/…) go through
// Yandex's public API: the resource's details (name, size, SHA-256, file or folder), then the
// address to download it from. A file in a shared folder is the folder's link with its path after
// it (…/d/<key>/Photos/a.jpg), as Yandex's own pages show it.

var yandexAPI = "https://cloud-api.yandex.net/v1/disk/public/resources" // replaced in tests

func yandexHost(h string) bool {
	h = strings.ToLower(h)
	if h == "yadi.sk" || h == "www.yadi.sk" {
		return true
	}
	for _, p := range []string{"disk.yandex.", "disk.360.yandex."} {
		if rest, ok := strings.CutPrefix(h, p); ok && rest != "" && !strings.Contains(rest, "..") {
			return true
		}
	}
	return false
}

// yandexLink splits a public link into the public key (the shared link itself) and the path of a
// file inside a shared folder ("" for the shared thing itself).
func yandexLink(u *url.URL) (key, inner string) {
	if h := u.Query().Get("hash"); h != "" && strings.HasPrefix(u.Path, "/public") {
		return h, u.Query().Get("path") // (the older ?hash= form: the key itself)
	}
	parts := strings.SplitN(strings.TrimPrefix(u.EscapedPath(), "/"), "/", 3)
	if len(parts) < 2 || (parts[0] != "d" && parts[0] != "i") || parts[1] == "" {
		return "", ""
	}
	key = "https://" + strings.ToLower(u.Host) + "/" + parts[0] + "/" + parts[1]
	if len(parts) == 3 && strings.Trim(parts[2], "/") != "" {
		if p, err := url.PathUnescape(strings.TrimSuffix(parts[2], "/")); err == nil {
			inner = "/" + p
		}
	}
	return key, inner
}

type ydItem struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	Href     string `json:"href"`
	Embedded *struct {
		Items []ydItem `json:"items"`
		Total int      `json:"total"`
	} `json:"_embedded"`
	Error   string `json:"error"`
	Message string `json:"message"`
	Desc    string `json:"description"`
}

// yandexCall asks the public API (what: "" for the details, "/download" for the address).
func yandexCall(ctx context.Context, what, key, inner string, extra url.Values) (*ydItem, error) {
	q := url.Values{"public_key": {key}}
	if inner != "" {
		q.Set("path", inner)
	}
	for k, v := range extra {
		q[k] = v
	}
	var it ydItem
	code, err := cloudGet(ctx, yandexAPI+what+"?"+q.Encode(), nil, &it)
	if it.Error != "" {
		said := strings.TrimSuffix(cmp.Or(it.Desc, it.Message), ".")
		switch {
		case code == 404 || it.Error == "DiskNotFoundError":
			return nil, permanentErr{errors.New("Yandex Disk: " + said + " (the file was deleted, or the link is wrong)")}
		case strings.Contains(it.Error, "Limit") || code == 429:
			return nil, permanentErr{errors.New("Yandex Disk: " + said + ". Try again later")}
		case code == 403 || code == 401:
			return nil, permanentErr{errors.New("Yandex Disk: " + said)}
		}
		return nil, errors.New("Yandex Disk: " + said)
	}
	if err == nil && code >= 400 {
		err = statusErr{code, strconv.Itoa(code), 0}
	}
	if err != nil {
		return nil, err
	}
	return &it, nil
}

func resolveYandex(ctx context.Context, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", permanentErr{err}
	}
	key, inner := yandexLink(u)
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	it, err := yandexCall(ctx, "", key, inner, url.Values{"limit": {"0"}})
	if err != nil {
		return "", err
	}
	if it.Type == "dir" {
		return "", permanentErr{errCloudFolder}
	}
	dl, err := yandexCall(ctx, "/download", key, inner, nil)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(dl.Href, "https://") && !strings.HasPrefix(dl.Href, "http://") {
		return "", errors.New("Yandex Disk didn't hand out the file's address. Try again later")
	}
	noteCloudFact(dl.Href, &cloudFact{name: it.Name, size: it.Size, sha256: strings.ToLower(it.SHA256)})
	return dl.Href, nil
}

// yandexFolder lists a shared Yandex Disk folder.
func yandexFolder(ctx context.Context, raw string) (*cloudFolder, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errNotFolder
	}
	key, inner := yandexLink(u)
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	const page = 200
	cf := &cloudFolder{Service: "Yandex Disk"}
	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		for off := 0; ; off += page {
			it, err := yandexCall(ctx, "", key, cmp.Or(dir, inner), url.Values{"limit": {strconv.Itoa(page)}, "offset": {strconv.Itoa(off)}})
			if err != nil {
				return err
			}
			if it.Type != "dir" {
				return errNotFolder
			}
			if cf.Name == "" {
				cf.Name = it.Name
			}
			if it.Embedded == nil {
				return nil
			}
			for _, c := range it.Embedded.Items {
				p := c.Path
				if p == "" {
					p = path.Join(cmp.Or(dir, inner, "/"), c.Name)
				}
				switch {
				case len(cf.Files) >= maxFolderFiles:
					cf.More = true
					return nil
				case c.Type == "dir" && depth >= maxFolderDepth:
					cf.More = true
				case c.Type == "dir":
					if err := walk(p, depth+1); err != nil {
						return err
					}
				default:
					rel := strings.TrimPrefix(strings.TrimPrefix(p, cmp.Or(inner, "/")), "/")
					link := key + escapePath(p)
					if !strings.HasPrefix(key, "https://") {
						link = "https://disk.yandex.ru/public?hash=" + url.QueryEscape(key) + "&path=" + url.QueryEscape(p)
					}
					cf.Files = append(cf.Files, cloudFile{Path: rel, Size: c.Size, URL: link})
				}
			}
			if len(it.Embedded.Items) < page || off+page >= it.Embedded.Total {
				return nil
			}
		}
	}
	if err := walk("", 0); err != nil {
		return nil, err
	}
	return cf, nil
}

// escapePath writes a path for a link, each part escaped.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}
