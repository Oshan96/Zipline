package main

import (
	"context"
	"encoding/base64"
	"errors"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MediaFire: its public API (file/get_info) gives the name, size, SHA-256 and whether a password
// is needed; the file's address is the download button on the file's page, either as the link
// itself or, on newer pages, base64 in data-scrambled-url. A removed file's page leads to
// error.php?errno=…. The address works for a while and with several connections.

var mediafireWeb = "https://www.mediafire.com" // replaced in tests

func mediafireHost(h string) bool { return hostIn(h, "mediafire.com") }

var mfKey = regexp.MustCompile(`^[A-Za-z0-9]{6,}$`)

// mediafireLink reads a link's file key (mediafire.com/file/<key>/…, /download/<key>, /view/<key>, /?<key>), or
// folder for a folder's link.
func mediafireLink(u *url.URL) (key string, folder bool) {
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch {
	case parts[0] == "folder":
		return "", true
	case (parts[0] == "file" || parts[0] == "download" || parts[0] == "file_premium" || parts[0] == "view") && len(parts) > 1:
		key = parts[1]
	case u.Path == "/" || u.Path == "":
		key, _, _ = strings.Cut(u.RawQuery, "&")
	}
	if !mfKey.MatchString(key) {
		return "", false
	}
	return key, false
}

var (
	mfButtonRe    = regexp.MustCompile(`(?is)<a\b[^>]*\bid="downloadButton"[^>]*>`)
	mfAriaRe      = regexp.MustCompile(`(?is)<a\b[^>]*\baria-label="Download file"[^>]*>`)
	mfHrefRe      = regexp.MustCompile(`(?i)\bhref="([^"]*)"`)
	mfScrambledRe = regexp.MustCompile(`(?i)\bdata-scrambled-url="([^"]*)"`)
	mfH3Re        = regexp.MustCompile(`(?is)<h3[^>]*>(.*?)</h3>`)
	mfDirectRe    = regexp.MustCompile(`^https?://download\d*\.mediafire\.com/`) // replaced in tests
)

// mediafireButton finds the file's address in its page's download button.
func mediafireButton(page string) string {
	for _, re := range []*regexp.Regexp{mfButtonRe, mfAriaRe} {
		for _, tag := range re.FindAllString(page, -1) {
			var cands []string
			if m := mfHrefRe.FindStringSubmatch(tag); m != nil {
				cands = append(cands, html.UnescapeString(m[1]))
			}
			if m := mfScrambledRe.FindStringSubmatch(tag); m != nil {
				if b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(m[1])); err == nil {
					cands = append(cands, string(b))
				}
			}
			for _, c := range cands {
				if mfDirectRe.MatchString(c) {
					return c
				}
			}
		}
	}
	return ""
}

func resolveMediaFire(ctx context.Context, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", permanentErr{err}
	}
	key, folder := mediafireLink(u)
	if folder {
		return "", permanentErr{errors.New("MediaFire folder links aren't supported: open the folder in your browser and copy the link of a file in it")}
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	var info struct {
		Response struct {
			Result  string `json:"result"`
			Message string `json:"message"`
			Error   any    `json:"error"`
			File    struct {
				Name     string `json:"filename"`
				Size     string `json:"size"`
				Hash     string `json:"hash"`
				Password string `json:"password_protected"`
				Privacy  string `json:"privacy"`
			} `json:"file_info"`
		} `json:"response"`
	}
	f := &cloudFact{size: -1}
	if _, err := cloudGet(ctx, mediafireWeb+"/api/1.5/file/get_info.php?quick_key="+url.QueryEscape(key)+"&response_format=json", nil, &info); err == nil {
		r := info.Response
		switch {
		case r.Result == "Error":
			return "", permanentErr{errors.New("MediaFire: " + strings.TrimSuffix(r.Message, ".") + " (the file was removed, or the link is wrong)")}
		case r.File.Password == "yes":
			return "", permanentErr{errors.New("MediaFire: this file is protected with a password. Open it in your browser, enter the password and click Download: the extension passes it to Zipline")}
		case r.File.Privacy == "private":
			return "", permanentErr{errors.New("MediaFire: this file is private: only its owner can download it")}
		}
		f.name, f.sha256 = r.File.Name, strings.ToLower(r.File.Hash)
		if n, err := strconv.ParseInt(r.File.Size, 10, 64); err == nil {
			f.size = n
		}
		if len(f.sha256) != 64 {
			f.sha256 = ""
		}
	}
	req, err := http.NewRequestWithContext(ctx, "GET", mediafireWeb+"/file/"+url.PathEscape(key), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept-Language", "en")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	page := pageText(resp, 2<<20)
	resp.Body.Close()
	if fin := resp.Request.URL; strings.HasSuffix(fin.Path, "/error.php") {
		return "", permanentErr{mediafireGone(fin.Query().Get("errno"), page)}
	}
	if resp.StatusCode == 429 {
		return "", errors.New("MediaFire is turning away too many requests: try again in a while")
	}
	if dl := mediafireButton(page); dl != "" {
		noteCloudFact(dl, f)
		return dl, nil
	}
	low := strings.ToLower(page)
	switch {
	case strings.Contains(low, `name="downloadp"`):
		return "", permanentErr{errors.New("MediaFire: this file is protected with a password. Open it in your browser, enter the password and click Download: the extension passes it to Zipline")}
	case strings.Contains(low, "recaptcha") || strings.Contains(low, "hcaptcha") || strings.Contains(low, "cf-turnstile"):
		return "", permanentErr{errChallenge}
	}
	return "", errors.New("MediaFire's page had no download button. Open the link in your browser and click Download: the extension passes it to Zipline")
}

// mediafireGone says why MediaFire's error page came up: its own heading when it has one.
func mediafireGone(errno, page string) error {
	said := ""
	for _, re := range []*regexp.Regexp{gHeadRe, mfH3Re} {
		if m := re.FindStringSubmatch(page); m != nil && tagText(m[1]) != "" {
			said = tagText(m[1])
			break
		}
	}
	if said == "" {
		switch errno {
		case "378", "380", "388":
			said = "File Removed for Violation"
		default:
			said = "Invalid or Deleted File"
		}
	}
	said = strings.TrimSuffix(said, ".")
	if errno != "" {
		said += " (error " + errno + ")"
	}
	return errors.New("MediaFire: " + said)
}
