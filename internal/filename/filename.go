// Package filename picks the name a download is saved under, the way browsers do.
package filename

import (
	"errors"
	"io"
	"mime"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// File names the way Chrome picks them (net/http/http_content_disposition.cc and
// net/base/filename_util): a lenient Content-Disposition parser, a URL fallback and a
// cleaner that makes any name safe to save on Windows, macOS and Linux.

// ParseContentDisposition returns the file name a Content-Disposition header suggests,
// without directories. ok is false when the header names no usable file, so the caller
// can fall back to the URL. Real servers send headers Go's strict mime parser rejects
// (unquoted names with spaces, raw UTF-8, repeated parameters), so this follows Chrome:
// filename* beats RFC 2231 continuations, which beat filename; the first valid copy of
// each wins; parameters that make no sense are skipped.
func ParseContentDisposition(v string) (name string, ok bool) {
	params := splitParams(v)
	// The disposition type is optional, and anything that isn't a token is a parameter.
	if len(params) > 0 && isToken(strings.Trim(params[0], " \t")) {
		params = params[1:]
	}
	var plain, ext string
	parts := map[int]cdPart{} // RFC 2231 continuations: filename*0, filename*1*, …
	for _, p := range params {
		k, val, found := strings.Cut(p, "=")
		k = strings.ToLower(strings.Trim(k, " \t"))
		val = strings.Trim(val, " \t")
		if !found || k == "" || strings.ContainsRune(k, '"') {
			continue
		}
		switch {
		case k == "filename":
			if plain == "" {
				plain = decodeFilenameValue(unquote(val))
			}
		case k == "filename*":
			if ext == "" {
				ext = decodeExtValue(val)
			}
		case strings.HasPrefix(k, "filename*"):
			idx, enc := strings.CutSuffix(k[len("filename*"):], "*")
			// no signs or leading zeros: filename*01 is not part 1
			n, err := strconv.Atoi(idx)
			if _, dup := parts[n]; err != nil || n < 0 || strconv.Itoa(n) != idx || dup {
				continue
			}
			parts[n] = cdPart{val, enc}
		}
	}
	for _, n := range []string{ext, joinContinuations(parts), plain} {
		if n = lastComponent(n); CleanOrEmpty(n) != "" {
			return n, true
		}
	}
	return "", false
}

// splitParams splits a header at the semicolons outside quoted strings.
func splitParams(s string) []string {
	var out []string
	start, quoted, escaped := 0, false, false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case escaped:
			escaped = false
		case quoted && c == '\\':
			escaped = true
		case c == '"':
			quoted = !quoted
		case c == ';' && !quoted:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func isToken(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c >= 0x7f || strings.IndexByte(`()<>@,;:\"/[]?={}`, c) >= 0 {
			return false
		}
	}
	return s != ""
}

// unquote undoes a quoted-string. A missing closing quote just drops the opening one,
// as Chrome does, so `filename="report.pdf` still names report.pdf.
func unquote(v string) string {
	if v == "" || v[0] != '"' {
		return v
	}
	if len(v) < 2 || v[len(v)-1] != '"' {
		return v[1:]
	}
	var b strings.Builder
	escaped := false
	for i := 1; i < len(v)-1; i++ {
		if v[i] == '\\' && !escaped {
			escaped = true
			continue
		}
		escaped = false
		b.WriteByte(v[i])
	}
	return b.String()
}

// decodeFilenameValue decodes a plain filename= value word by word, like Chrome:
// RFC 2047 encoded words (=?UTF-8?B?…?=), %XX escapes when they spell UTF-8 (what IE
// taught servers to send), and raw bytes as UTF-8 or else Windows-1252. "" if unreadable.
func decodeFilenameValue(v string) string {
	var b strings.Builder
	afterWord := true // Chrome drops the space after an encoded word, as RFC 2047 does between two
	for v != "" {
		i := strings.IndexAny(v, " \t\r\n")
		if i == 0 {
			if !afterWord {
				b.WriteByte(' ')
			}
			v = v[1:]
			continue
		}
		if i < 0 {
			i = len(v)
		}
		w := v[:i]
		v = v[i:]
		afterWord = false
		switch {
		case !IsASCII(w):
			b.WriteString(fromLegacy(w))
		case isEncodedWord(w):
			d, err := wordDecoder.Decode(w)
			if err != nil {
				return "" // broken, or a charset we can't read: better the URL than gibberish
			}
			b.WriteString(d)
			afterWord = true
		default:
			if d := pctDecode(w); utf8.ValidString(d) {
				w = d
			}
			b.WriteString(w)
		}
	}
	if !utf8.ValidString(b.String()) { // an encoded word that claimed UTF-8 but wasn't
		return ""
	}
	return b.String()
}

// isEncodedWord reports whether w starts like an RFC 2047 word: =?charset?B?… or Q.
func isEncodedWord(w string) bool {
	f := strings.SplitN(w, "?", 4)
	return len(f) == 4 && f[0] == "=" && f[1] != "" && len(f[2]) == 1 && strings.ContainsAny(f[2], "bBqQ")
}

// decodeExtValue decodes an RFC 8187 value, charset'language'%XX…, as Chrome does:
// quotes, raw non-ASCII bytes, a missing charset or an unknown one make it "".
func decodeExtValue(v string) string {
	f := strings.Split(v, "'")
	if len(f) != 3 || f[0] == "" || f[2] == "" || strings.ContainsRune(v, '"') || !IsASCII(f[2]) {
		return ""
	}
	s, _ := decodeCharset(pctDecode(f[2]), f[0])
	return s
}

// cdPart is one piece of an RFC 2231 name; enc marks filename*N* pieces, which are %-encoded.
type cdPart struct {
	v   string
	enc bool
}

// joinContinuations puts filename*0, filename*1, … back together. The first part, when
// encoded, carries the charset. A gap ends the name.
func joinContinuations(parts map[int]cdPart) string {
	var b strings.Builder
	charset := ""
	for i := 0; ; i++ {
		p, ok := parts[i]
		if !ok {
			break
		}
		encoded, v := p.enc, unquote(p.v)
		if encoded && i == 0 {
			f := strings.Split(v, "'")
			if len(f) != 3 || f[0] == "" {
				return ""
			}
			charset, v = f[0], f[2]
		}
		if encoded {
			v = pctDecode(v)
		}
		b.WriteString(v)
	}
	if charset == "" {
		return fromLegacy(b.String())
	}
	s, _ := decodeCharset(b.String(), charset)
	return s
}

// wordDecoder reads RFC 2047 words; the CharsetReader adds the Latin-1 and UTF-8
// spellings Go doesn't know (iso88591, latin1, windows-1252, utf8…).
var wordDecoder = &mime.WordDecoder{CharsetReader: func(charset string, r io.Reader) (io.Reader, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	s, ok := decodeCharset(string(b), charset)
	if !ok {
		return nil, errors.New("unsupported charset " + charset)
	}
	return strings.NewReader(s), nil
}}

// decodeCharset converts b to UTF-8. Only UTF-8 and ISO-8859-1 are supported; like
// browsers, ISO-8859-1 means its superset Windows-1252 (so 0x80 is €, not a control).
func decodeCharset(b, charset string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "utf-8", "utf8", "unicode-1-1-utf-8", "unicode11utf8", "unicode20utf8", "x-unicode20utf8":
		if !utf8.ValidString(b) {
			return "", false
		}
		return b, true
	case "iso-8859-1", "iso8859-1", "iso88591", "iso_8859-1", "iso_8859-1:1987", "iso-ir-100", "latin1", "l1",
		"csisolatin1", "ibm819", "cp819", "windows-1252", "cp1252", "x-cp1252", "us-ascii", "ascii", "ansi_x3.4-1968":
		return fromWindows1252(b), true
	}
	return "", false
}

// fromLegacy keeps valid UTF-8 and reads anything else as Windows-1252, the usual
// encoding of servers that send raw bytes (Chrome uses the page's charset here).
func fromLegacy(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return fromWindows1252(s)
}

// cp1252 holds what bytes 0x80–0x9f mean in Windows-1252 (the five unused ones stay C1).
var cp1252 = [32]rune{0x20ac, 0x81, 0x201a, 0x192, 0x201e, 0x2026, 0x2020, 0x2021, 0x2c6, 0x2030, 0x160, 0x2039,
	0x152, 0x8d, 0x17d, 0x8f, 0x90, 0x2018, 0x2019, 0x201c, 0x201d, 0x2022, 0x2013, 0x2014, 0x2dc, 0x2122, 0x161,
	0x203a, 0x153, 0x9d, 0x17e, 0x178}

func fromWindows1252(s string) string {
	r := make([]rune, len(s))
	for i := 0; i < len(s); i++ {
		r[i] = rune(s[i])
		if s[i] >= 0x80 && s[i] < 0xa0 {
			r[i] = cp1252[s[i]-0x80]
		}
	}
	return string(r)
}

func IsASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// pctDecode decodes every valid %XX and leaves broken ones ("50%", "%zz") as they are,
// where url.PathUnescape would give up on the whole string.
func pctDecode(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			n, _ := strconv.ParseUint(s[i+1:i+3], 16, 8)
			b = append(b, byte(n))
			i += 2
			continue
		}
		b = append(b, s[i])
	}
	return string(b)
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// lastComponent drops the directories a name came with (either slash), so a server
// can't aim a download at "../../.bashrc" or "C:\Windows\x.dll".
func lastComponent(n string) string {
	n = strings.TrimRight(n, `/\`)
	return n[strings.LastIndexAny(n, `/\`)+1:]
}

// FromURLPath picks a file name from an escaped URL path (u.EscapedPath()): the
// last segment with something usable in it, minus any ";jsessionid=…" or query,
// %-decoded when that gives UTF-8 and kept as sent otherwise. "" means none.
func FromURLPath(p string) string {
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	for p != "" {
		i := strings.LastIndexAny(p, `/\`)
		seg := p[i+1:]
		p = p[:max(i, 0)]
		seg, _, _ = strings.Cut(seg, ";")
		if d := pctDecode(seg); utf8.ValidString(d) {
			seg = d
		}
		if CleanOrEmpty(seg) != "" {
			return seg
		}
	}
	return ""
}

// Clean makes n safe to save under on any system (files move between them):
// no characters Windows forbids, no invisible tricks, no device names, no extensions the
// shell would act on, at most 200 bytes. It never returns "".
func Clean(n string) string {
	if n = CleanOrEmpty(n); n != "" {
		return n
	}
	return "download"
}

// windowsDevices are names Windows opens as devices whatever the extension ("con.txt" is
// the console). Chrome adds clock$ and, below, the shell's desktop.ini and thumbs.db.
var windowsDevices = func() map[string]bool {
	m := map[string]bool{"con": true, "prn": true, "aux": true, "nul": true, "clock$": true, "conin$": true, "conout$": true}
	for _, d := range "0123456789¹²³" {
		m["com"+string(d)], m["lpt"+string(d)] = true, true
	}
	return m
}()

// riskyExts run something when opened from a file manager rather than showing a
// file: shortcuts (.lnk, .url, .desktop), shell commands (.scf) and DLL redirection
// (.local). Chrome saves them as .download.
var riskyExts = map[string]bool{".lnk": true, ".local": true, ".scf": true, ".url": true, ".desktop": true}

// CleanOrEmpty is Clean without the "download" fallback, so callers can tell a
// name that cleans away to nothing.
func CleanOrEmpty(n string) string {
	n = strings.TrimSpace(n)
	n = strings.Map(func(r rune) rune {
		switch {
		case strings.ContainsRune(`<>:"/\|?*`, r) || r < 32 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			return '_'
		case unicode.Is(unicode.Cf, r) && r != 0x200c && r != 0x200d:
			// invisible format characters, such as the right-to-left override that makes
			// "fdp.exe" display as ".pdf" (zero-width joiners stay: emoji and some scripts need them)
			return -1
		}
		return r
	}, n)
	// Windows drops trailing dots and spaces, and leading dots hide files elsewhere.
	n = strings.TrimFunc(n, dotOrSpace)
	if n == "" {
		return ""
	}
	ext := filepath.Ext(n)
	lext := strings.ToLower(ext)
	if riskyExts[lext] || strings.HasPrefix(lext, ".{") && strings.HasSuffix(lext, "}") { // .{CLSID} hides the real type
		n, ext = n[:len(n)-len(ext)]+".download", ".download"
	}
	stem, _, _ := strings.Cut(n, ".")
	if stem = strings.TrimRight(stem, " "); windowsDevices[strings.ToLower(stem)] ||
		strings.EqualFold(n, "desktop.ini") || strings.EqualFold(n, "thumbs.db") {
		n = "_" + n
	}
	if len(n) > 200 {
		if len(ext) > 20 {
			ext = ""
		}
		cut := 200 - len(ext)
		for cut > 0 && !utf8.RuneStart(n[cut]) { // don't split a character
			cut--
		}
		stem := n[:cut]
		if ext == "" {
			stem = strings.TrimRightFunc(stem, dotOrSpace)
		}
		n = stem + ext
	}
	return n
}

func dotOrSpace(r rune) bool { return r == '.' || unicode.IsSpace(r) }
