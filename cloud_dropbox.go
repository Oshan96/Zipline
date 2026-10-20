package main

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
)

// Dropbox answers a shared link it won't serve with a web page: an account whose links were
// switched off for a while because of too much traffic (429), a password or sign-in, a deleted
// file. Such a page fails the download in Dropbox's words, instead of being saved as the file.

var trafficRe = regexp.MustCompile(`(?i)>\s*([^<>]*too much traffic[^<>]*)<`)

// dropboxRefusal reads the page Dropbox sent instead of the file (nil when it sent the file).
func dropboxRefusal(resp *http.Response) error {
	code := resp.StatusCode
	shareLink := resp.Request != nil && hostIn(firstURL(resp).Hostname(), "dropbox.com") // (not its file servers)
	page := isHTML(resp.Header) && !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Disposition")), "attachment")
	if !(code == 429 || code == 460 || code == 509 || page && shareLink && code != 304) {
		return nil
	}
	text := pageText(resp, 256<<10)
	low, title := strings.ToLower(text), strings.ToLower(htmlTitle(text))
	var msg string
	switch {
	case code == 429 || code == 460 || code == 509 || strings.Contains(low, "too much traffic") || strings.Contains(low, "temporarily disabled"):
		said := "This account's links are generating too much traffic and have been temporarily disabled!"
		if m := trafficRe.FindStringSubmatch(text); m != nil {
			said = tagText(m[1])
		}
		msg = "Dropbox: " + said + " Try again later: Dropbox lifts it after a while (usually within a day)"
	case strings.Contains(title, "password"):
		msg = "Dropbox: this link is protected with a password. Open it in your browser, enter the password and click Download: the extension passes it to Zipline"
	case strings.Contains(title, "log in") || strings.Contains(title, "sign in"):
		msg = "Dropbox: this file isn't shared publicly (Dropbox asks to sign in). Open the link in your browser and click Download: the extension passes it to Zipline with your sign-in"
	case code == 404 || code == 410 || strings.Contains(title, "invalid link") || strings.Contains(title, "error") || strings.Contains(title, "not found"):
		msg = "Dropbox: the link isn't valid any more" + inParens(htmlTitle(text)) + ": the file was deleted, or the link was switched off"
	case code < 300:
		msg = "Dropbox sent a web page" + inParens(htmlTitle(text)) + " instead of the file. Open the link in your browser and click Download: the extension passes it to Zipline"
	default:
		return nil
	}
	resp.Body.Close()
	return permanentErr{errors.New(msg)}
}

// inParens is " (s)", or nothing for an empty s.
func inParens(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}
