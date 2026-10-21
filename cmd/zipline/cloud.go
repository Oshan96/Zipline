package main

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Cloud share links: a "share" link from Google Drive, Dropbox or OneDrive opens a web page,
// not the file. Zipline turns it into the address that serves the file itself, every time it
// connects (those addresses don't last), so the download keeps the share link and can resume.

var (
	gdriveBase   = "https://drive.usercontent.google.com" // replaced in tests
	gdocsBase    = "https://docs.google.com"
	onedriveAPI  = "https://api.onedrive.com/v1.0"
	badgerURL    = "https://api-badgerp.svc.ms/v1.0/token"
	badgerAppID  = "5cbed6ac-a083-4e14-b191-b4ba07653de2" // OneDrive's own web viewer
	errCloudPage = errors.New("the share link opened a web page instead of the file")
)

// cloudService names the service a share link belongs to ("" for an ordinary link).
func cloudService(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	h := strings.ToLower(u.Hostname())
	switch {
	case (h == "drive.google.com" || h == "docs.google.com") && gdriveID(u) != "":
		return "Google Drive"
	case (h == "www.dropbox.com" || h == "dropbox.com") && (strings.HasPrefix(u.Path, "/s/") || strings.HasPrefix(u.Path, "/scl/") || strings.HasPrefix(u.Path, "/sh/")):
		return "Dropbox"
	case h == "1drv.ms" || h == "onedrive.live.com" && (u.Query().Get("resid") != "" || u.Query().Get("redeem") != "" || u.Query().Get("cid") != "" || strings.HasPrefix(u.Path, "/redir") || strings.HasPrefix(u.Path, "/download")):
		return "OneDrive"
	case strings.HasSuffix(h, ".sharepoint.com") && sharepointShare.MatchString(u.Path):
		return "OneDrive"
	}
	return moreCloudService(u, h)
}

var sharepointShare = regexp.MustCompile(`^/:[a-z]:/`)

var gdrivePath = regexp.MustCompile(`^/(?:file|document|spreadsheets|presentation)(?:/u/\d+)?/d/([A-Za-z0-9_-]{10,})`)

// gdriveID finds the file in a Google Drive or Docs link.
func gdriveID(u *url.URL) string {
	if m := gdrivePath.FindStringSubmatch(u.Path); m != nil {
		return m[1]
	}
	if id := u.Query().Get("id"); len(id) >= 10 && (u.Path == "/uc" || u.Path == "/open" || strings.HasSuffix(u.Path, "/download")) {
		return id
	}
	return ""
}

// resolveCloud turns a share link into the address of the file. Ordinary links come back
// unchanged.
func resolveCloud(ctx context.Context, d *Download, raw string) (string, error) {
	switch cloudService(raw) {
	case "Google Drive":
		return resolveGDrive(ctx, d, raw)
	case "Dropbox":
		return dropboxDirect(raw), nil
	case "OneDrive":
		return resolveOneDrive(ctx, d, raw)
	}
	return resolveMoreCloud(ctx, d, raw)
}

// ---- Google Drive

func resolveGDrive(ctx context.Context, d *Download, raw string) (string, error) {
	u, _ := url.Parse(raw)
	id, key := gdriveID(u), gdriveResourceKey(raw)
	if id == "" && gdriveFolderID(u) != "" {
		return "", permanentErr{errCloudFolder}
	}
	for _, kind := range []string{"document", "spreadsheets", "presentation"} {
		if strings.HasPrefix(u.Path, "/"+kind+"/") { // Docs, Sheets and Slides come as Office files
			format := map[string]string{"document": "docx", "spreadsheets": "xlsx", "presentation": "pptx"}[kind]
			return withResourceKey(gdocsBase+"/"+kind+"/d/"+id+"/export?format="+format, key), nil
		}
	}
	// Small files come straight away; big ones first show a "can't scan for viruses" page
	// with a form that leads to the file.
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for try := 0; ; try++ {
		direct := withResourceKey(gdriveBase+"/download?id="+url.QueryEscape(id)+"&export=download", key)
		req, err := buildRequest(ctx, d, direct, "bytes=0-0")
		if err != nil {
			return "", err
		}
		resp, err := doReq(d, req)
		if err != nil {
			return "", err
		}
		if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") && resp.StatusCode < 400 {
			resp.Body.Close()
			return direct, nil // the file itself
		}
		page, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if next := gdriveConfirm(page, resp.Request.URL); next != "" {
			return withResourceKey(next, key), nil
		}
		if k := pageResourceKey(page); key == "" && k != "" && try == 0 { // an older file: its page knows the key
			key = k
			continue
		}
		return "", permanentErr{gdriveProblem(page, resp.StatusCode, resp.Request.URL)}
	}
}

var (
	formRe    = regexp.MustCompile(`(?is)(<form\b[^>]*>)(.*?)</form>`)
	actionRe  = regexp.MustCompile(`(?i)\baction="([^"]+)"`)
	formTagRe = regexp.MustCompile(`(?is)<form[^>]*\bid="download-form"[^>]*>`)
	hiddenRe  = regexp.MustCompile(`(?is)<input[^>]*>`)
	gAttrRe   = regexp.MustCompile(`(?i)\b(name|value|type)="([^"]*)"`)
	ucHrefRe  = regexp.MustCompile(`href="(/uc\?export=download[^"]+)"`)
	dlURLRe   = regexp.MustCompile(`"downloadUrl":"([^"]+)"`)
	uuidRe    = regexp.MustCompile(`(?i)name="uuid"\s+value="([^"]+)"`)
	gerrRe    = regexp.MustCompile(`(?is)<p class="uc-error-subcaption">(.*?)</p>`)
	tagRe     = regexp.MustCompile(`<[^>]+>`)
)

// gdriveConfirm finds the way past Google's warning page: its download form (whatever hidden
// fields it carries: id, confirm, uuid, at …), or a link or address in the page.
func gdriveConfirm(page []byte, base *url.URL) string {
	for _, f := range formRe.FindAllSubmatch(page, -1) {
		act := ""
		if m := actionRe.FindSubmatch(f[1]); m != nil {
			act = html.UnescapeString(string(m[1]))
		}
		if !formTagRe.Match(f[1]) && !strings.Contains(act, "/download") && !strings.Contains(act, "/uc") {
			continue
		}
		au, err := base.Parse(act)
		if err == nil && act != "" {
			q := au.Query()
			for _, in := range hiddenRe.FindAll(f[2], -1) {
				var name, value, typ string
				for _, a := range gAttrRe.FindAllSubmatch(in, -1) {
					switch strings.ToLower(string(a[1])) {
					case "name":
						name = html.UnescapeString(string(a[2]))
					case "value":
						value = html.UnescapeString(string(a[2]))
					case "type":
						typ = strings.ToLower(string(a[2]))
					}
				}
				if name != "" && (typ == "hidden" || typ == "") {
					q.Set(name, value)
				}
			}
			if q.Get("id") == "" {
				q.Set("id", base.Query().Get("id"))
			}
			if q.Get("id") != "" {
				au.RawQuery = q.Encode()
				return au.String()
			}
		}
	}
	if m := ucHrefRe.FindSubmatch(page); m != nil {
		if au, err := url.Parse(gdocsBase + html.UnescapeString(string(m[1]))); err == nil {
			return au.String()
		}
	}
	if m := dlURLRe.FindSubmatch(page); m != nil {
		s := strings.NewReplacer(`=`, "=", `&`, "&", `\/`, "/").Replace(string(m[1]))
		return s
	}
	if m := uuidRe.FindSubmatch(page); m != nil {
		if id := base.Query().Get("id"); id != "" {
			return gdriveBase + "/download?id=" + url.QueryEscape(id) + "&export=download&confirm=t&uuid=" + url.QueryEscape(string(m[1]))
		}
	}
	return ""
}

// gdriveProblem is why Google didn't hand out the file, in Google's own words.
func gdriveProblem(page []byte, code int, at *url.URL) error {
	words := gdriveWords(page)
	low := strings.ToLower(words + " " + string(page[:min(len(page), 4096)]))
	signIn := at != nil && strings.HasPrefix(strings.ToLower(at.Hostname()), "accounts.")
	switch {
	case strings.Contains(low, "too many users") || strings.Contains(low, "quota"):
		return errors.New("Google Drive: " + cmp.Or(words, "Too many users have viewed or downloaded this file recently. Please try accessing the file again later."))
	case signIn || code == 401 || code == 403 || strings.Contains(low, "you need access") || strings.Contains(low, "request access"):
		if signIn || words == "" {
			words = "You need access."
		}
		return errors.New("Google Drive: " + strings.TrimSuffix(words, ".") + `. The file isn't shared with "Anyone with the link": open it in your browser while signed in and click Download, and the extension passes it to Zipline with your sign-in`)
	case code == 404 || strings.Contains(low, "does not exist") || strings.Contains(low, "not found"):
		return errors.New("Google Drive: " + cmp.Or(words, "Sorry, the file you have requested does not exist."))
	case words != "":
		return errors.New("Google Drive: " + words)
	}
	return errors.New("Google Drive didn't hand out the file. Open the link in your browser and click Download: the extension passes it to Zipline")
}

// ---- Dropbox

// dropboxDirect asks Dropbox for the file instead of its preview page (folders come as a zip).
func dropboxDirect(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	q.Del("raw")
	q.Set("dl", "1")
	u.RawQuery = q.Encode()
	return u.String()
}

// ---- OneDrive

// resolveOneDrive gets the download address for a OneDrive share link. Work and school
// (SharePoint) links take download=1. Personal links go through OneDrive's sharing API, which
// gives a short-lived address for the file.
func resolveOneDrive(ctx context.Context, d *Download, raw string) (string, error) {
	u, _ := url.Parse(raw)
	if strings.HasSuffix(strings.ToLower(u.Hostname()), ".sharepoint.com") {
		q := u.Query()
		q.Set("download", "1")
		u.RawQuery = q.Encode()
		return u.String(), nil
	}
	return onedrivePersonal(ctx, d, raw)
}

// shareID encodes a share link the way OneDrive's sharing API wants it.
func shareID(raw string) string {
	return "u!" + strings.TrimRight(base64.URLEncoding.EncodeToString([]byte(raw)), "=")
}

var oneDriveRedeem = onedriveRedeem // replaced in tests

// onedriveRedeem reads the "redeem" code a 1drv.ms link redirects with.
func onedriveRedeem(ctx context.Context, d *Download, raw string) string {
	req, err := buildRequest(ctx, d, raw, "")
	if err != nil {
		return ""
	}
	c := *httpClient
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		return ""
	}
	resp.Body.Close()
	if loc, err := resp.Location(); err == nil {
		return loc.Query().Get("redeem")
	}
	return ""
}
