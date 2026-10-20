package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// Both pages get the one set of words: the window loads /i18n.js before its own script, and the phone
// page (which may load no scripts) has the same file pasted in, with nothing left of the marker.
func TestPagesShareTheWords(t *testing.T) {
	i, j := strings.Index(indexHTML, `<script src="/i18n.js"></script>`), strings.Index(indexHTML, `<script src="/app.js"></script>`)
	if i < 0 || j < i {
		t.Error("the window page should load /i18n.js before /app.js")
	}
	if strings.Contains(phoneHTML, i18nMarker) || !strings.Contains(phoneHTML, "const MESSAGES = {") {
		t.Error("the phone page should have i18n.js pasted in, and no marker left")
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/i18n.js", nil)
	req.Host = "127.0.0.1:17980"
	(&Server{token: "tok"}).routes().ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.String() != i18nJS || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") {
		t.Errorf("/i18n.js: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	m, err := os.ReadFile("web/m.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, own := range []string{"function fmtSize(", "const fmtSpeed =", "const esc ="} {
		if strings.Contains(appJS, own) || strings.Contains(string(m), own) {
			t.Errorf("%q belongs in web/i18n.js only", own)
		}
	}
}

var messageUse = regexp.MustCompile(`\b(?:t|th|ht|te)\("([A-Za-z0-9_.]+)"`)

// Every message the pages ask for is in the catalogue, every catalogue message reads (the ICU-style
// text parses), and the plurals give "1 file" and "2 files".
func TestMessageCatalogue(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node isn't installed")
	}
	script := i18nJS + `
const bad = [];
for (const id of Object.keys(MESSAGES.en)) { try { th(id, { n: 2, count: 2, pct: "5%" }); } catch (e) { bad.push(id + ": " + e.message); } }
console.log(JSON.stringify({ ids: Object.keys(MESSAGES.en), bad,
  plural: [t("qm.count", { n: 1 }), t("qm.count", { n: 2 }), t("dl.count", { n: 1 }), t("dl.count", { n: 5 })],
  wait: fmtWait(25), size: fmtSize(1536), speed: fmtSpeed(2048) }));`
	cmd := exec.Command(node, "-e", script)
	cmd.Env = append(cmd.Environ(), "LC_ALL=en_US.UTF-8", "LANG=en_US.UTF-8", "LANGUAGE=en")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v %s", err, out)
	}
	var got struct {
		IDs    []string
		Bad    []string
		Plural []string
		Wait   string
		Size   string
		Speed  string
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node said %q (%v)", out, err)
	}
	for _, b := range got.Bad {
		t.Errorf("message doesn't read: %s", b)
	}
	have := map[string]bool{}
	for _, id := range got.IDs {
		have[id] = true
	}
	for name, src := range map[string]string{"app.js": appJS, "m.html": phoneHTML} {
		for _, m := range messageUse.FindAllStringSubmatch(src, -1) {
			id := m[1]
			if strings.HasSuffix(id, ".") { // a prefix: the rest is added by the code (col.size, seg.done…)
				ok := false
				for have := range have {
					ok = ok || strings.HasPrefix(have, id)
				}
				if !ok {
					t.Errorf("%s asks for %s… and the catalogue has none", name, id)
				}
			} else if !have[id] {
				t.Errorf("%s asks for the message %q, which the catalogue doesn't have", name, id)
			}
		}
	}
	want := []string{"1 file", "2 files", "1 download", "5 downloads"}
	for i, w := range want {
		if got.Plural[i] != w {
			t.Errorf("plural %d: %q, want %q", i, got.Plural[i], w)
		}
	}
	if got.Wait != "25\u00a0sec" || got.Size != "1.50 KB" || got.Speed != "2.00 KB/s" {
		t.Errorf("formats: %q %q %q", got.Wait, got.Size, got.Speed)
	}
}
