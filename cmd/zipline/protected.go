package main

import (
	"net/url"
	"strings"
)

// Sites Zipline won't download from: YouTube, and streaming services whose video is DRM-protected
// or whose terms don't allow saving it. This is the one list: the app checks it on every way a
// link comes in, and the browser extension gets it in the ping reply (config.protected), so the
// two can't disagree. (The extension keeps a copy of its own for an older or stopped app; a test
// keeps that copy equal to this list.)
var protectedSites = []string{"youtube.com", "youtu.be", "youtube-nocookie.com", "googlevideo.com", "netflix.com", "nflxvideo.net",
	"disneyplus.com", "primevideo.com", "amazonvideo.com", "hulu.com", "max.com", "hbomax.com", "spotify.com",
	"tv.apple.com", "peacocktv.com", "paramountplus.com", "crunchyroll.com"}

const protectedMsg = "Zipline doesn't download from YouTube or DRM-protected streaming services"

// protectedHost: is this host (or a subdomain of it) on the list?
func protectedHost(h string) bool {
	h = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
	if h == "" {
		return false
	}
	for _, s := range protectedSites {
		if h == s || strings.HasSuffix(h, "."+s) {
			return true
		}
	}
	return false
}

// hostFromLink reads the host of a link, also when it was typed without http:// (a command line).
func hostFromLink(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "magnet:") {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// protectedURL: does the link point at a protected site?
func protectedURL(raw string) bool { return protectedHost(hostFromLink(raw)) }

// protectedLink is protectedURL plus one more case: a stream manifest (.m3u8 / .mpd) whose
// Referer is a protected site, because those services serve their streams from CDN hosts that
// aren't on the list. A plain file linked from such a page is still fine to download.
func protectedLink(raw string, hdr map[string]string) bool {
	if protectedURL(raw) {
		return true
	}
	if !looksLikeHLS(raw) {
		return false
	}
	for k, v := range hdr {
		if strings.EqualFold(k, "Referer") && protectedURL(v) {
			return true
		}
	}
	return false
}
