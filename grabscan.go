package main

import (
	"encoding/json"
	"html"
	"mime"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
)

// What the site grabber makes of an address and of a page, apart from fetching: tidying
// addresses so one file isn't listed under five spellings, finding the links a page keeps in
// places other than href and src, and deciding what kind of thing a link points to.

// ---------------------------------------------------------------- tidying addresses

// trackingParams are added by advertising and mail tools and never change what a page or file is.
var trackingParams = map[string]bool{"fbclid": true, "gclid": true, "gclsrc": true, "dclid": true, "msclkid": true,
	"yclid": true, "igshid": true, "mc_cid": true, "mc_eid": true}

var jsessionRe = regexp.MustCompile(`(?i);jsessionid=[^/;?#]*`)

// looksSessionID: the value of a "sid" parameter is only taken for a session when it looks like
// one (a long hex or mixed letters-and-digits token); ?sid=12 is a section or a song.
func looksSessionID(v string) bool {
	if len(v) < 16 {
		return false
	}
	hex, letters, digits := true, false, false
	for _, c := range v {
		switch {
		case c >= '0' && c <= '9':
			digits = true
		case c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F':
			letters = true
		case c >= 'g' && c <= 'z' || c >= 'G' && c <= 'Z':
			letters, hex = true, false
		default:
			return false
		}
	}
	return hex || len(v) >= 20 && letters && digits
}

// dropParam: is this query parameter noise (tracking, a session) rather than part of the address?
func dropParam(name, value string) bool {
	n := strings.ToLower(name)
	switch {
	case strings.HasPrefix(n, "utm_"), trackingParams[n], n == "phpsessid", n == "jsessionid":
		return true
	case n == "sid":
		return looksSessionID(value)
	}
	return false
}

// cleanURL returns the address with its fragment, tracking and session parameters taken off,
// the scheme and host in lower case and a default port left out. The order of the other
// parameters stays as the page had it, so the server gets what the page asked for.
func cleanURL(u *url.URL) *url.URL {
	c := *u
	c.Fragment, c.RawFragment = "", ""
	c.Scheme = strings.ToLower(c.Scheme)
	host, port := strings.ToLower(c.Hostname()), c.Port()
	switch {
	case c.Scheme == "http" && port == "80", c.Scheme == "https" && port == "443", c.Scheme == "ftp" && port == "21", c.Scheme == "ftps" && port == "990":
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	c.Host = host
	if p := c.EscapedPath(); jsessionRe.MatchString(p) {
		p = jsessionRe.ReplaceAllString(p, "")
		c.RawPath = p
		c.Path, _ = url.PathUnescape(p)
	}
	if c.Path == "" && (c.Scheme == "http" || c.Scheme == "https") {
		c.Path = "/"
	}
	if c.RawQuery != "" {
		var keep []string
		for _, piece := range strings.Split(c.RawQuery, "&") {
			k, v, _ := strings.Cut(piece, "=")
			if k, _ = url.QueryUnescape(k); k == "" || dropParam(k, v) {
				if k == "" && piece != "" {
					keep = append(keep, piece)
				}
				continue
			}
			keep = append(keep, piece)
		}
		c.RawQuery = strings.Join(keep, "&")
		c.ForceQuery = false
	}
	return &c
}

// dedupKey is what two spellings of the same address have in common: the cleaned address with
// its parameters in order.
func dedupKey(u *url.URL) string {
	c := cleanURL(u)
	if c.RawQuery != "" {
		parts := strings.Split(c.RawQuery, "&")
		sort.Strings(parts)
		c.RawQuery = strings.Join(parts, "&")
	}
	return c.String()
}

func dedupKeyOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return dedupKey(u)
	}
	return raw
}

// ---------------------------------------------------------------- finding links

var (
	metaTagRe  = regexp.MustCompile(`(?is)<meta\s[^>]*>`)
	linkTagRe  = regexp.MustCompile(`(?is)<link\s[^>]*>`)
	tagAttrRe  = regexp.MustCompile(`(?is)([a-zA-Z_:][-a-zA-Z0-9_:.]*)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+))`)
	ldJSONRe   = regexp.MustCompile(`(?is)<script\b[^>]*\btype\s*=\s*["']?application/ld\+json["']?[^>]*>(.*?)</script>`)
	importRe   = regexp.MustCompile(`(?i)@import\s*(?:url\(\s*)?['"]?([^'")\s;]+)['"]?`)
	styleTagRe = regexp.MustCompile(`(?is)<style\b[^>]*>(.*?)</style>`)
)

// metaLinkNames are the <meta> properties that hold the address of a video, a sound or a picture.
var metaLinkNames = map[string]bool{
	"og:video": true, "og:video:url": true, "og:video:secure_url": true,
	"og:audio": true, "og:audio:url": true, "og:audio:secure_url": true,
	"og:image": true, "og:image:url": true, "og:image:secure_url": true,
	"twitter:player:stream": true, "twitter:image": true, "twitter:image:src": true,
}

func tagAttrs(tag string) map[string]string {
	out := map[string]string{}
	for _, m := range tagAttrRe.FindAllStringSubmatch(tag, -1) {
		k := strings.ToLower(m[1])
		if _, dup := out[k]; !dup {
			out[k] = html.UnescapeString(m[2] + m[3] + m[4])
		}
	}
	return out
}

// pageBase is the address relative links on the page count from (its <base href>, else itself).
func pageBase(page *url.URL, body string) *url.URL {
	if m := baseRe.FindStringSubmatch(body); m != nil {
		if b, err := page.Parse(html.UnescapeString(m[1])); err == nil {
			return b
		}
	}
	return page
}

func webURL(base *url.URL, ref string) *url.URL {
	ref = strings.TrimSpace(html.UnescapeString(ref))
	low := strings.ToLower(ref)
	if ref == "" || strings.HasPrefix(low, "javascript:") || strings.HasPrefix(low, "data:") || strings.HasPrefix(low, "blob:") ||
		strings.HasPrefix(low, "mailto:") || strings.HasPrefix(low, "tel:") {
		return nil
	}
	u, err := base.Parse(ref)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "ftp" && u.Scheme != "ftps") {
		return nil
	}
	return u
}

// extractExtra finds what extractLinks doesn't look at: the addresses in <meta> tags
// (og:video, og:image, twitter:player:stream…) and in JSON-LD (contentUrl and embedUrl of
// videos, pictures and other media).
func extractExtra(page *url.URL, body string) []*url.URL {
	base := pageBase(page, body)
	var out []*url.URL
	add := func(ref string) {
		if u := webURL(base, ref); u != nil {
			out = append(out, u)
		}
	}
	for _, tag := range metaTagRe.FindAllString(body, -1) {
		a := tagAttrs(tag)
		if name := strings.ToLower(a["property"]); metaLinkNames[name] || metaLinkNames[strings.ToLower(a["name"])] {
			add(a["content"])
		}
	}
	for _, m := range ldJSONRe.FindAllStringSubmatch(body, -1) {
		var v any
		if json.Unmarshal([]byte(strings.TrimSpace(m[1])), &v) == nil {
			walkLD(v, add, 0)
		}
	}
	return out
}

// walkLD collects contentUrl and embedUrl from the media objects of a JSON-LD document.
func walkLD(v any, add func(string), depth int) {
	if depth > 12 {
		return
	}
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			walkLD(e, add, depth+1)
		}
	case map[string]any:
		if isMediaLD(x["@type"]) {
			for _, k := range []string{"contentUrl", "embedUrl"} {
				switch s := x[k].(type) {
				case string:
					add(s)
				case []any:
					for _, e := range s {
						if str, ok := e.(string); ok {
							add(str)
						}
					}
				}
			}
		}
		for _, e := range x {
			walkLD(e, add, depth+1)
		}
	}
}

func isMediaLD(t any) bool {
	has := func(s string) bool {
		s = strings.ToLower(s)
		return strings.HasSuffix(s, "videoobject") || strings.HasSuffix(s, "imageobject") || strings.HasSuffix(s, "mediaobject") || strings.HasSuffix(s, "audioobject")
	}
	switch x := t.(type) {
	case string:
		return has(x)
	case []any:
		for _, e := range x {
			if s, ok := e.(string); ok && has(s) {
				return true
			}
		}
	}
	return false
}

// extractSheets finds the style sheets a page links to, and the ones its inline styles @import.
func extractSheets(page *url.URL, body string) []*url.URL {
	base := pageBase(page, body)
	var out []*url.URL
	for _, tag := range linkTagRe.FindAllString(body, -1) {
		a := tagAttrs(tag)
		rel := strings.ToLower(a["rel"])
		if strings.Contains(rel, "stylesheet") || strings.ToLower(a["as"]) == "style" && strings.Contains(rel, "preload") {
			if u := webURL(base, a["href"]); u != nil {
				out = append(out, u)
			}
		}
	}
	for _, m := range styleTagRe.FindAllStringSubmatch(body, -1) {
		out = append(out, cssImports(base, m[1])...)
	}
	return out
}

// cssImports are the style sheets a style sheet @imports.
func cssImports(base *url.URL, css string) []*url.URL {
	var out []*url.URL
	for _, m := range importRe.FindAllStringSubmatch(css, -1) {
		if u := webURL(base, m[1]); u != nil {
			out = append(out, u)
		}
	}
	return out
}

// cssAssets are the addresses in a style sheet's url(…) values (pictures, fonts).
func cssAssets(base *url.URL, css string) []*url.URL {
	var out []*url.URL
	for _, m := range cssURLRe.FindAllStringSubmatch(css, -1) {
		if u := webURL(base, m[1]); u != nil {
			out = append(out, u)
		}
	}
	return out
}

// cssAssetExts are the file types a style sheet points to; its sheets are only read when one of them is wanted.
var cssAssetExts = []string{"png", "jpg", "jpeg", "gif", "webp", "svg", "avif", "bmp", "ico", "cur", "tif", "tiff", "woff", "woff2", "ttf", "otf", "eot"}

// ---------------------------------------------------------------- what a link is

// htmlExts are certainly web pages; scriptExts could be a page or a file the script hands out.
var htmlExts = map[string]bool{"html": true, "htm": true, "xhtml": true, "shtml": true}
var scriptExts = map[string]bool{"php": true, "php3": true, "php4": true, "php5": true, "asp": true, "aspx": true, "ashx": true,
	"jsp": true, "jspx": true, "cgi": true, "cfm": true, "pl": true, "do": true, "action": true, "py": true}

// webExts are what a page is made of: nobody asks for them, and they are never pages.
var webExts = []string{"css", "js", "mjs", "json", "xml", "map", "txt", "rss", "atom", "webmanifest", "ico", "woff", "woff2", "ttf", "otf", "eot",
	"png", "jpg", "jpeg", "gif", "webp", "svg", "avif", "bmp"}

// downloadHints in an address suggest a script that hands out files.
var downloadHintRe = regexp.MustCompile(`(?i)download|attachment|[/_?&=.-](dl|file|get|export|media|asset)s?([/_?&=.-]|$)`)

func looksLikeDownload(u *url.URL) bool {
	return downloadHintRe.MatchString(u.Path + "?" + u.RawQuery)
}

// mimeExts maps the types servers send for files to the extensions the grabber knows them by.
var mimeExts = map[string][]string{
	"image/jpeg": {"jpg", "jpeg"}, "image/png": {"png"}, "image/gif": {"gif"}, "image/webp": {"webp"}, "image/svg+xml": {"svg"},
	"image/avif": {"avif"}, "image/bmp": {"bmp"}, "image/tiff": {"tif", "tiff"}, "image/x-icon": {"ico"}, "image/vnd.microsoft.icon": {"ico"},
	"video/mp4": {"mp4", "m4v"}, "video/webm": {"webm"}, "video/x-matroska": {"mkv"}, "video/quicktime": {"mov"}, "video/x-msvideo": {"avi"},
	"video/x-flv": {"flv"}, "video/mpeg": {"mpg", "mpeg"}, "video/ogg": {"ogv"}, "video/3gpp": {"3gp"}, "video/x-ms-wmv": {"wmv"},
	"audio/mpeg": {"mp3"}, "audio/mp4": {"m4a"}, "audio/x-m4a": {"m4a"}, "audio/aac": {"aac"}, "audio/ogg": {"ogg", "oga"}, "audio/opus": {"opus"},
	"audio/flac": {"flac"}, "audio/x-flac": {"flac"}, "audio/wav": {"wav"}, "audio/x-wav": {"wav"}, "audio/webm": {"weba"},
	"application/pdf": {"pdf"}, "application/msword": {"doc"}, "application/vnd.ms-excel": {"xls"}, "application/vnd.ms-powerpoint": {"ppt"},
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   {"docx"},
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         {"xlsx"},
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": {"pptx"},
	"application/epub+zip": {"epub"}, "application/rtf": {"rtf"}, "text/csv": {"csv"}, "application/vnd.oasis.opendocument.text": {"odt"},
	"application/zip": {"zip"}, "application/x-zip-compressed": {"zip"}, "application/x-rar-compressed": {"rar"}, "application/vnd.rar": {"rar"},
	"application/x-7z-compressed": {"7z"}, "application/gzip": {"gz"}, "application/x-gzip": {"gz"}, "application/x-tar": {"tar"},
	"application/x-bzip2": {"bz2"}, "application/x-xz": {"xz"},
	"application/x-msdownload": {"exe"}, "application/vnd.microsoft.portable-executable": {"exe"}, "application/x-msi": {"msi"},
	"application/vnd.android.package-archive": {"apk"}, "application/x-apple-diskimage": {"dmg"}, "application/x-iso9660-image": {"iso"},
	"application/x-bittorrent": {"torrent"}, "application/vnd.debian.binary-package": {"deb"}, "application/x-rpm": {"rpm"},
}

// extsForType lists the extensions a Content-Type stands for.
func extsForType(ct string) []string {
	ct = strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
	if x, ok := mimeExts[ct]; ok {
		return x
	}
	if ct == "" || ct == "application/octet-stream" || ct == "binary/octet-stream" || strings.HasPrefix(ct, "text/html") {
		return nil
	}
	var out []string
	if exts, _ := mime.ExtensionsByType(ct); len(exts) > 0 {
		for _, e := range exts {
			out = append(out, strings.TrimPrefix(e, "."))
		}
	}
	return out
}

func isHTMLType(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.Contains(ct, "text/html") || strings.Contains(ct, "application/xhtml")
}

// matchFile decides whether a file with this name and Content-Type is one the user asked for,
// and returns the extension that matched and the name to show (given an extension when it had none).
func matchFile(want map[string]bool, name, ct string) (ext, shown string, ok bool) {
	shown = name
	ext = strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))
	if want[ext] {
		return ext, shown, true
	}
	for _, x := range extsForType(ct) {
		if want[x] {
			if cur := path.Ext(name); cur == "" || scriptExts[ext] {
				shown = strings.TrimSuffix(name, cur) + "." + x
			}
			return x, shown, true
		}
	}
	return "", name, false
}

// ---------------------------------------------------------------- pages built by script

var (
	scriptBlockRe = regexp.MustCompile(`(?is)<script\b[^>]*>(.*?)</script>`)
	noscriptRe    = regexp.MustCompile(`(?is)<noscript\b[^>]*>(.*?)</noscript>`)
	styleBlockRe  = regexp.MustCompile(`(?is)<style\b[^>]*>.*?</style>`)
	htmlTagRe     = regexp.MustCompile(`(?s)<[^>]*>`)
	emptyRootRe   = regexp.MustCompile(`(?is)<div\b[^>]*\bid\s*=\s*["']?(?:root|app|__next|__nuxt|svelte|q-app|ember-application)["']?[^>]*>\s*(?:<!--.*?-->\s*)*</div>|<app-root[^>]*>\s*</app-root>`)
	bundleRe      = regexp.MustCompile(`(?i)<script\b[^>]*\bsrc\s*=\s*["']?[^"'>\s]*(?:bundle|main|app|chunk|vendor|runtime|index|polyfills)[^"'>\s]*\.m?js`)
	needJSRe      = regexp.MustCompile(`(?i)javascript|enable (?:it|scripts)|browser`)
)

// looksScriptBuilt guesses from the page itself whether the links are made by script: an empty
// container for the app, a "turn on JavaScript" note, a bundle, and next to no text of its own.
func looksScriptBuilt(body string) bool {
	score := 0
	for _, m := range noscriptRe.FindAllStringSubmatch(body, -1) {
		if needJSRe.MatchString(htmlTagRe.ReplaceAllString(m[1], " ")) {
			score += 2
			break
		}
	}
	if emptyRootRe.MatchString(body) {
		score += 2
	}
	big := 0
	for _, m := range scriptBlockRe.FindAllStringSubmatch(body, -1) {
		big += len(m[1])
	}
	if big > 30<<10 || bundleRe.MatchString(body) {
		score++
	}
	text := scriptBlockRe.ReplaceAllString(body, " ")
	text = noscriptRe.ReplaceAllString(text, " ")
	text = styleBlockRe.ReplaceAllString(text, " ")
	text = strings.Join(strings.Fields(html.UnescapeString(htmlTagRe.ReplaceAllString(text, " "))), " ")
	if len(text) < 300 {
		score++
	}
	return score >= 3
}

// pageParts are the files a page is made of: following them never leads to content.
var pageParts = map[string]bool{"css": true, "js": true, "mjs": true, "json": true, "xml": true, "map": true, "webmanifest": true, "ico": true,
	"woff": true, "woff2": true, "ttf": true, "otf": true, "eot": true}

// contentLinks counts the links a visitor could follow (not the scripts, styles and icons a page is made of).
func contentLinks(links []*url.URL, self *url.URL) int {
	me := dedupKey(self)
	seen := map[string]bool{}
	for _, l := range links {
		if k := dedupKey(l); !pageParts[extOf(l)] && k != me {
			seen[k] = true
		}
	}
	return len(seen)
}
