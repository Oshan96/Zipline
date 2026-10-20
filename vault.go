package main

import (
	"encoding/base64"
	"errors"
	"strings"
	"sync"
)

// The vault keeps secrets secret in the files Zipline writes: passwords (the proxy's, a site's,
// a link's), logins typed in the Add dialog, cookies and other sign-in headers. In the JSON
// files each one is "enc:v1:" + base64 of bytes that only this user's account can turn back into
// the text. Windows does that by itself (DPAPI); on the other systems a random key seals them
// with AES-GCM, and the key lives in the Keychain, the Secret Service or a private file (see
// vault_gcm.go). In memory they are plain text, which is what the engine needs. Old files with
// plain values still load, and are written sealed from then on.
//
// A secret that can't be opened (the Keychain is locked, the key is gone) is never blanked: it
// stays in memory as the sealed text, is written back as it was, and the window says so.

const sealPrefix = "enc:v1:"

func isSealed(s string) bool { return strings.HasPrefix(s, sealPrefix) }

// usableSecret is a stored secret as the engine may send it: one that couldn't be opened is empty.
func usableSecret(s string) string {
	if isSealed(s) {
		return ""
	}
	return s
}

var (
	errNoKey    = errors.New("its key isn't on this computer")
	errWrongKey = errors.New("the key doesn't fit")
	errForeign  = errors.New("it was saved on another kind of computer")
	errDamaged  = errors.New("it is damaged")
)

// A sealer turns secrets into bytes that only this user's account can turn back.
type sealer interface {
	seal(plain []byte) ([]byte, error)
	open(sealed []byte) ([]byte, error)
	how() string // where the protection comes from: "dpapi", "keychain", "secret-service" or "file"
}

// legacyOpener is a sealer that can also read what an older version wrote ("dpapi:…" on Windows).
type legacyOpener interface {
	openLegacy(s string) ([]byte, error)
}

// vaultSealer makes the platform's sealer for a data folder. Tests replace it, so they never
// touch the real Keychain or Secret Service.
var vaultSealer = platformSealer

type vault struct {
	c sealer

	mu      sync.Mutex
	openErr error // the first secret that couldn't be opened, and why
	sealErr error // the first secret that had to be saved as it was, and why
	told    bool  // sealNote was handed out
}

func newVault(dir string) *vault { return &vault{c: vaultSealer(dir)} }

// protect seals a secret for a file. Empty and already sealed text stays as it is. If it can't be
// sealed it stays readable, rather than being lost.
func (v *vault) protect(s string) string {
	if v == nil || s == "" || isSealed(s) || strings.HasPrefix(s, "dpapi:") {
		return s
	}
	blob, err := v.c.seal([]byte(s))
	v.mu.Lock()
	if err != nil && v.sealErr == nil {
		v.sealErr = err
	} else if err == nil {
		v.sealErr, v.told = nil, false // (what was saved plain is sealed now)
	}
	v.mu.Unlock()
	if err != nil {
		return s
	}
	return sealPrefix + base64.StdEncoding.EncodeToString(blob)
}

// reveal opens a secret read from a file. Plain text is returned as it is; sealed text that
// can't be opened is returned as it is too, so the next save writes it back unchanged.
func (v *vault) reveal(s string) string {
	if v == nil {
		return s
	}
	var plain []byte
	var err error
	switch {
	case isSealed(s):
		var blob []byte
		if blob, err = decodeSealed(s[len(sealPrefix):]); err != nil {
			err = errDamaged
		} else {
			plain, err = v.c.open(blob)
		}
	case strings.HasPrefix(s, "dpapi:"):
		if lo, ok := v.c.(legacyOpener); ok {
			plain, err = lo.openLegacy(s)
		} else {
			err = errForeign
		}
	default:
		return s
	}
	if err != nil {
		v.mu.Lock()
		if v.openErr == nil {
			v.openErr = err
		}
		v.mu.Unlock()
		return s
	}
	return string(plain)
}

// decodeSealed reads the base64 of a sealed secret: the padded form of the files, or the
// URL-safe unpadded one a link carries (a "/" would end its host name).
func decodeSealed(b64 string) ([]byte, error) {
	b64 = strings.NewReplacer("-", "+", "_", "/").Replace(strings.TrimRight(b64, "="))
	return base64.RawStdEncoding.DecodeString(b64)
}

// urlSafe rewrites a sealed secret for the password part of a link.
func urlSafe(sealed string) string {
	return sealPrefix + strings.NewReplacer("+", "-", "/", "_", "=", "").Replace(sealed[len(sealPrefix):])
}

// credentialHeader says whether a request header carries a login or a sign-in.
func credentialHeader(name string) bool {
	return name != "" && (secretHeaders[strings.ToLower(name)])
}

// protectHeaders returns h with its logins and cookies sealed (h itself is left alone).
func (v *vault) protectHeaders(h map[string]string) map[string]string {
	if len(h) == 0 {
		return h
	}
	out := make(map[string]string, len(h))
	for k, x := range h {
		if credentialHeader(k) {
			x = v.protect(x)
		}
		out[k] = x
	}
	return out
}

// revealHeaders opens the logins and cookies in h.
func (v *vault) revealHeaders(h map[string]string) {
	for k, x := range h {
		if credentialHeader(k) {
			h[k] = v.reveal(x)
		}
	}
}

// protectAll returns m (host → Cookie header, say) with every value sealed.
func (v *vault) protectAll(m map[string]string) map[string]string {
	if len(m) == 0 {
		return m
	}
	out := make(map[string]string, len(m))
	for k, x := range m {
		out[k] = v.protect(x)
	}
	return out
}

func (v *vault) revealAll(m map[string]string) {
	for k, x := range m {
		m[k] = v.reveal(x)
	}
}

// ---------------------------------------------------------------- passwords inside links

// userinfoSpan finds the "user:password" part of a link written as text, raw[start:at] (the "@"
// follows). It works on the text, not a parsed URL, so nothing else in the link changes.
func userinfoSpan(raw string) (start, at, end int) {
	k := strings.Index(raw, "://")
	if k <= 0 || !isSchemeName(raw[:k]) {
		return -1, -1, -1
	}
	start, end = k+3, len(raw)
	if e := strings.IndexAny(raw[start:], "/?#"); e >= 0 {
		end = start + e
	}
	i := strings.LastIndexByte(raw[start:end], '@')
	if i < 0 {
		return -1, -1, -1
	}
	return start, start + i, end
}

func isSchemeName(s string) bool {
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case i > 0 && (c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'):
		default:
			return false
		}
	}
	return s != ""
}

// mapURLPass rewrites the password in a link's user info with f.
func mapURLPass(raw string, f func(pass string) string) string {
	start, at, _ := userinfoSpan(raw)
	if start < 0 {
		return raw
	}
	user, pass, ok := strings.Cut(raw[start:at], ":")
	if !ok || pass == "" {
		return raw
	}
	return raw[:start] + user + ":" + f(pass) + raw[at:]
}

// stripURLPass takes the password out of a link ("ftp://me:pw@host/f" → "ftp://me@host/f"), for
// anything shown or sent to the window.
func stripURLPass(raw string) string {
	start, at, _ := userinfoSpan(raw)
	if start < 0 {
		return raw
	}
	user, _, ok := strings.Cut(raw[start:at], ":")
	if !ok {
		return raw
	}
	return raw[:start] + user + raw[at:]
}

func stripURLPasses(list []string) []string {
	var out []string
	for i, u := range list {
		if s := stripURLPass(u); s != u {
			if out == nil {
				out = append([]string(nil), list...)
			}
			out[i] = s
		}
	}
	if out == nil {
		return list
	}
	return out
}

// restoreURLPass gives a link back the password stripURLPass took out, when it still names the
// same user and server as one of olds (the window sends back the link it was shown).
func restoreURLPass(raw string, olds ...string) string {
	start, at, end := userinfoSpan(raw)
	if start < 0 || strings.Contains(raw[start:at], ":") {
		return raw
	}
	for _, old := range olds {
		ostart, oat, oend := userinfoSpan(old)
		if ostart < 0 {
			continue
		}
		user, pass, ok := strings.Cut(old[ostart:oat], ":")
		if ok && pass != "" && user == raw[start:at] && old[oat+1:oend] == raw[at+1:end] {
			return raw[:at] + ":" + pass + raw[at:]
		}
	}
	return raw
}

func restoreURLPasses(list, olds []string) []string {
	out := make([]string, len(list))
	for i, u := range list {
		out[i] = restoreURLPass(u, olds...)
	}
	return out
}

// exportLink is a link for an export: without its password, unless the export is to carry the secrets.
func exportLink(raw string, secrets bool) string {
	if secrets {
		return raw
	}
	return stripURLPass(raw)
}

func exportLinks(list []string, secrets bool) []string {
	if secrets {
		return list
	}
	return stripURLPasses(list)
}

func (v *vault) protectURL(raw string) string {
	return mapURLPass(raw, func(p string) string {
		if s := v.protect(p); s != p {
			return urlSafe(s)
		}
		return p
	})
}

func (v *vault) revealURL(raw string) string {
	return mapURLPass(raw, v.reveal)
}

// ---------------------------------------------------------------- what the files hold

// dlWire is a download as it is written to downloads.json: the very same JSON as the Download
// itself, except that its link's password, its logins and its cookies are sealed. (The outer
// fields hide the embedded download's own.)
type dlWire struct {
	*Download
	URL      string            `json:"url"`
	FinalURL string            `json:"finalUrl,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Mirrors  []string          `json:"mirrors,omitempty"`
	Body     []byte            `json:"body,omitempty"` // (always empty: the form goes in bodySealed)
	BodySeal string            `json:"bodySealed,omitempty"`
}

func (v *vault) wire(d *Download) dlWire {
	w := dlWire{Download: d, URL: v.protectURL(d.URL), FinalURL: v.protectURL(d.FinalURL), Headers: v.protectHeaders(d.Headers), BodySeal: d.BodySeal}
	if len(d.Body) > 0 {
		w.BodySeal = v.protect(base64.StdEncoding.EncodeToString(d.Body))
	}
	if len(d.Mirrors) > 0 {
		w.Mirrors = make([]string, len(d.Mirrors))
		for i, u := range d.Mirrors {
			w.Mirrors[i] = v.protectURL(u)
		}
	}
	return w
}

// openDownload opens what wire sealed, in a download just read from the file.
func (v *vault) openDownload(d *Download) {
	d.URL, d.FinalURL = v.revealURL(d.URL), v.revealURL(d.FinalURL)
	v.revealHeaders(d.Headers)
	for i, u := range d.Mirrors {
		d.Mirrors[i] = v.revealURL(u)
	}
	if d.BodySeal != "" { // (a form body that can't be opened stays sealed, and is saved back as it was)
		if b64 := v.reveal(d.BodySeal); !isSealed(b64) {
			if b, err := base64.StdEncoding.DecodeString(b64); err == nil {
				d.Body, d.BodySeal = b, ""
			}
		}
	}
}

// sealSettings seals the passwords in st, a copy of the settings about to be written (the lists
// it changes are copied first).
func (v *vault) sealSettings(st *Settings) {
	st.Proxy.Pass = v.protect(st.Proxy.Pass)
	st.Proxy.URL = v.protectURL(st.Proxy.URL)
	st.Proxy.PassSet = false
	st.Sites = append([]SiteRule(nil), st.Sites...)
	for i := range st.Sites {
		st.Sites[i].Pass = v.protect(st.Sites[i].Pass)
		st.Sites[i].PassSet = false
	}
	st.Phone.Devices = append([]PhoneDevice(nil), st.Phone.Devices...)
	for i := range st.Phone.Devices {
		st.Phone.Devices[i].Key = v.protect(st.Phone.Devices[i].Key)
	}
}

// openSettings opens what sealSettings sealed, in settings just read from the file.
func (v *vault) openSettings(st *Settings) {
	st.Proxy.Pass = v.reveal(st.Proxy.Pass)
	st.Proxy.URL = v.revealURL(st.Proxy.URL)
	for i := range st.Sites {
		st.Sites[i].Pass = v.reveal(st.Sites[i].Pass)
	}
	for i := range st.Phone.Devices {
		st.Phone.Devices[i].Key = v.reveal(st.Phone.Devices[i].Key)
	}
}

// ---------------------------------------------------------------- telling the user

// vaultInfo is what Options shows about how saved passwords are kept.
type vaultInfo struct {
	How    string `json:"how"`              // dpapi, keychain, secret-service, file
	Locked bool   `json:"locked,omitempty"` // some saved secrets couldn't be opened (they're kept as they are)
	Plain  bool   `json:"plain,omitempty"`  // some new secrets had to be saved without protection
}

func (v *vault) info() vaultInfo {
	if v == nil {
		return vaultInfo{How: "none"}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return vaultInfo{How: v.c.how(), Locked: v.openErr != nil, Plain: v.sealErr != nil}
}

// openNote is the sentence for a start-up that couldn't open some saved secrets ("" if all opened).
func (v *vault) openNote() string {
	if v == nil {
		return ""
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.openErr == nil {
		return ""
	}
	return "Zipline couldn't open some saved passwords or cookies (" + v.openErr.Error() + "). They are kept as they are, and work again once the key is available, for example after unlocking the Keychain: quit and start Zipline again then."
}

// sealNote is the sentence for secrets that had to be saved without protection; it comes once.
func (v *vault) sealNote() string {
	if v == nil {
		return ""
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.sealErr == nil || v.told {
		return ""
	}
	v.told = true
	return "Zipline couldn't protect a newly saved password or cookie (" + v.sealErr.Error() + "), so it was saved without protection."
}
