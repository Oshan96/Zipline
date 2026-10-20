package main

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// Google Drive: resource keys, Google's own words when it refuses, and shared folders.
//
// Files shared before Google's 2021 security update can need the link's resource key: it's sent
// both in the address (&resourcekey=) and as the X-Goog-Drive-Resource-Keys header, as Drive's own
// pages do.

var (
	gdriveWeb      = "https://drive.google.com"                      // replaced in tests
	gdriveKeyHosts = []string{"google.com", "googleusercontent.com"} // where the resource key header goes
)

// gdriveResourceKey is the resource key in a Drive link ("" when it has none).
func gdriveResourceKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Query().Get("resourcekey")
}

// gdriveKeyHeader adds the link's resource key to a request for its file.
func gdriveKeyHeader(link string, req *http.Request) {
	u, err := url.Parse(link)
	if err != nil {
		return
	}
	key, id := u.Query().Get("resourcekey"), gdriveID(u)
	if id == "" {
		id = gdriveFolderID(u)
	}
	if key != "" && id != "" && hostIn(req.URL.Hostname(), gdriveKeyHosts...) {
		req.Header.Set("X-Goog-Drive-Resource-Keys", id+"/"+key)
	}
}

// withResourceKey adds a resource key to a Drive address.
func withResourceKey(u, key string) string {
	if key == "" || strings.Contains(u, "resourcekey=") {
		return u
	}
	return u + "&resourcekey=" + url.QueryEscape(key)
}

var pageKeyRe = regexp.MustCompile(`(?:resourcekey=|"resourceKey":")([A-Za-z0-9_-]{10,})`)

// pageResourceKey finds a resource key a Drive page mentions.
func pageResourceKey(page []byte) string {
	if m := pageKeyRe.FindSubmatch(page); m != nil {
		return string(m[1])
	}
	return ""
}

var (
	gCaptionRe = regexp.MustCompile(`(?is)<p class="uc-error-caption">(.*?)</p>`)
	gErrMsgRe  = regexp.MustCompile(`(?is)<p class="errorMessage"[^>]*>(.*?)</p>`)
	gHeadRe    = regexp.MustCompile(`(?is)<h1[^>]*>(.*?)</h1>`)
	titleRe    = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
)

// tagText is an HTML fragment as plain text.
func tagText(s string) string {
	return strings.Join(strings.Fields(html.UnescapeString(tagRe.ReplaceAllString(s, " "))), " ")
}

// htmlTitle is a page's title as plain text.
func htmlTitle(page string) string {
	if m := titleRe.FindStringSubmatch(page); m != nil {
		return tagText(m[1])
	}
	return ""
}

// sentences keeps the first n sentences of a service's message (some go on for a paragraph).
func sentences(s string, n int) string {
	for i, c := range s {
		if c == '.' && (i+1 == len(s) || s[i+1] == ' ') {
			if n--; n == 0 {
				return s[:i+1]
			}
		}
	}
	return s
}

// gdriveWords is what Google's page says went wrong, in its own words ("" if it says nothing).
func gdriveWords(page []byte) string {
	p := string(page)
	for _, re := range []*regexp.Regexp{gerrRe, gErrMsgRe, gCaptionRe} {
		if m := re.FindStringSubmatch(p); m != nil {
			if t := tagText(m[1]); t != "" {
				return sentences(t, 2)
			}
		}
	}
	if m := gHeadRe.FindStringSubmatch(p); m != nil && strings.Contains(strings.ToLower(m[1]), "access") {
		return tagText(m[1]) // "You need access"
	}
	t := htmlTitle(p)
	t = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(t, "Google Drive - "), " - Google Drive"))
	if t == "Google Drive" || strings.Contains(strings.ToLower(t), "virus scan") {
		return ""
	}
	return t
}

// ---- shared folders

var gdriveFolderPath = regexp.MustCompile(`^/drive/(?:u/\d+/)?folders/([A-Za-z0-9_-]{10,})`)

// gdriveFolderID finds the folder in a Drive folder link ("" for other links).
func gdriveFolderID(u *url.URL) string {
	h := strings.ToLower(u.Hostname())
	if h != "drive.google.com" {
		return ""
	}
	if m := gdriveFolderPath.FindStringSubmatch(u.Path); m != nil {
		return m[1]
	}
	if id := u.Query().Get("id"); len(id) >= 10 && (u.Path == "/folderview" || u.Path == "/embeddedfolderview") {
		return id
	}
	return ""
}

var (
	ivdRe      = regexp.MustCompile(`window\['_DRIVE_ivd'\]\s*=\s*'((?:[^'\\]|\\.)*)'`)
	entryRe    = regexp.MustCompile(`(?is)<a [^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	entryTitle = regexp.MustCompile(`(?is)<div class="flip-entry-title">(.*?)</div>`)
	fileHref   = regexp.MustCompile(`^https://drive\.google\.com/file/d/([A-Za-z0-9_-]{10,})`)
	docsHref   = regexp.MustCompile(`^https://docs\.google\.com/(document|spreadsheets|presentation)/d/([A-Za-z0-9_-]{10,})`)
)

const gFolderMime = "application/vnd.google-apps.folder"

// gdriveEntry is one thing in a Drive folder.
type gdriveEntry struct {
	id, name, mime, key string
	size                int64
}

// gdriveFolderPage reads one Drive folder: its name and what's in it. Like gdown, it reads the
// list the folder's page carries for its scripts, and falls back on the plain "embedded" view of
// the folder (which has no sizes).
func gdriveFolderPage(ctx context.Context, id, key string) (string, []gdriveEntry, error) {
	q := "?hl=en"
	if key != "" {
		q += "&resourcekey=" + url.QueryEscape(key)
	}
	page, code, err := cloudPage(ctx, gdriveWeb+"/drive/folders/"+url.PathEscape(id)+q, map[string]string{"X-Goog-Drive-Resource-Keys": keyPair(id, key)})
	if err != nil {
		return "", nil, err
	}
	if name, list, ok := parseDriveIVD(page); ok {
		return name, list, nil
	}
	page2, code2, err := cloudPage(ctx, gdriveWeb+"/embeddedfolderview?id="+url.QueryEscape(id)+strings.Replace(q, "?hl=en", "", 1), nil)
	if err == nil && code2 == 200 {
		if name, list, ok := parseEmbeddedFolder(page2); ok {
			return name, list, nil
		}
	}
	if w := gdriveWords(page); w != "" && (code >= 400 || strings.Contains(strings.ToLower(w), "access")) {
		return "", nil, permanentErr{errors.New("Google Drive: " + w)}
	}
	if code == 404 {
		return "", nil, permanentErr{errors.New("Google Drive: Sorry, the folder you have requested does not exist.")}
	}
	return "", nil, permanentErr{errors.New(`Google Drive didn't list this folder: it may not be shared with "Anyone with the link"`)}
}

func keyPair(id, key string) string {
	if key == "" {
		return ""
	}
	return id + "/" + key
}

// parseDriveIVD reads the folder list a Drive folder page keeps in window['_DRIVE_ivd']: a
// JavaScript string holding JSON, whose first item lists the entries as [id, [parent], name, type …].
func parseDriveIVD(page []byte) (string, []gdriveEntry, bool) {
	m := ivdRe.FindSubmatch(page)
	if m == nil {
		return "", nil, false
	}
	var arr []json.RawMessage
	if json.Unmarshal([]byte(jsString(string(m[1]))), &arr) != nil || len(arr) == 0 {
		return "", nil, false
	}
	var rows [][]json.RawMessage
	json.Unmarshal(arr[0], &rows) // (null for an empty folder)
	var out []gdriveEntry
	for _, r := range rows {
		if len(r) < 4 {
			continue
		}
		var e gdriveEntry
		json.Unmarshal(r[0], &e.id)
		json.Unmarshal(r[2], &e.name)
		json.Unmarshal(r[3], &e.mime)
		e.size = -1
		if e.id != "" && e.name != "" {
			out = append(out, e)
		}
	}
	name := htmlTitle(string(page))
	if i := strings.LastIndex(name, " - "); i > 0 {
		name = name[:i] // "Holiday - Google Drive"
	}
	return name, out, true
}

// jsString is a JavaScript string literal's value (its \xNN, \uNNNN, \n, \' … escapes undone).
func jsString(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 == len(s) {
			b.WriteByte(c)
			continue
		}
		i++
		switch e := s[i]; e {
		case 'x', 'u':
			n := 2
			if e == 'u' {
				n = 4
			}
			if i+n < len(s) {
				if v, err := strconv.ParseUint(s[i+1:i+1+n], 16, 32); err == nil {
					r := rune(v)
					i += n
					if utf16.IsSurrogate(r) && i+6 < len(s) && s[i+1] == '\\' && s[i+2] == 'u' {
						if v2, err := strconv.ParseUint(s[i+3:i+7], 16, 32); err == nil {
							r = utf16.DecodeRune(r, rune(v2))
							i += 6
						}
					}
					b.WriteRune(r)
					continue
				}
			}
			b.WriteByte(e)
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '0':
			b.WriteByte(0)
		default:
			b.WriteByte(e) // \' \" \\ \/
		}
	}
	return b.String()
}

// parseEmbeddedFolder reads drive.google.com/embeddedfolderview: the folder's name as the page
// title and a link per entry.
func parseEmbeddedFolder(page []byte) (string, []gdriveEntry, bool) {
	p := string(page)
	name := htmlTitle(p)
	if name == "" {
		return "", nil, false
	}
	var out []gdriveEntry
	for _, m := range entryRe.FindAllStringSubmatch(p, -1) {
		href := html.UnescapeString(m[1])
		label := m[2]
		if t := entryTitle.FindStringSubmatch(label); t != nil {
			label = t[1]
		}
		e := gdriveEntry{name: tagText(label), size: -1}
		if hu, err := url.Parse(href); err == nil {
			e.key = hu.Query().Get("resourcekey")
			if id := gdriveFolderID(hu); id != "" {
				e.id, e.mime = id, gFolderMime
			}
		}
		if f := fileHref.FindStringSubmatch(href); f != nil {
			e.id, e.mime = f[1], "application/octet-stream"
		} else if f := docsHref.FindStringSubmatch(href); f != nil {
			e.id, e.mime = f[2], "application/vnd.google-apps."+strings.TrimSuffix(f[1], "s")
		}
		if e.id != "" && e.name != "" {
			out = append(out, e)
		}
	}
	return name, out, true
}

// gdriveFolder lists a shared Drive folder and the folders in it.
func gdriveFolder(ctx context.Context, u *url.URL) (*cloudFolder, error) {
	id, key := gdriveFolderID(u), u.Query().Get("resourcekey")
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	name, list, err := gdriveFolderPage(ctx, id, key)
	if err != nil {
		return nil, err
	}
	cf := &cloudFolder{Service: "Google Drive", Name: name}
	var walk func(dir string, list []gdriveEntry, depth int) error
	walk = func(dir string, list []gdriveEntry, depth int) error {
		for _, e := range list {
			if len(cf.Files) >= maxFolderFiles {
				cf.More = true
				return nil
			}
			rel := path.Join(dir, e.name)
			ext, kind := "", ""
			switch e.mime {
			case gFolderMime:
				if depth >= maxFolderDepth {
					cf.More = true
					continue
				}
				_, sub, err := gdriveFolderPage(ctx, e.id, e.key)
				if err != nil {
					return err
				}
				if err := walk(rel, sub, depth+1); err != nil {
					return err
				}
				continue
			case "application/vnd.google-apps.document":
				ext, kind = ".docx", "document"
			case "application/vnd.google-apps.spreadsheet":
				ext, kind = ".xlsx", "spreadsheets"
			case "application/vnd.google-apps.presentation":
				ext, kind = ".pptx", "presentation"
			}
			link := "https://drive.google.com/file/d/" + e.id + "/view"
			if kind != "" {
				link = "https://docs.google.com/" + kind + "/d/" + e.id + "/edit"
				rel += ext
			} else if strings.HasPrefix(e.mime, "application/vnd.google-apps.") {
				continue // forms, drawings, shortcuts: nothing to download
			}
			if e.key != "" {
				link += "?resourcekey=" + url.QueryEscape(e.key)
			}
			cf.Files = append(cf.Files, cloudFile{Path: rel, Size: e.size, URL: link})
		}
		return nil
	}
	return cf, walk("", list, 0)
}

// cloudPage fetches a service's web page (as a browser would ask for it).
func cloudPage(ctx context.Context, u string, hdr map[string]string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en")
	for k, v := range hdr {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return b, resp.StatusCode, err
}
