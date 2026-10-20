package main

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// Both pages get the one colour file, and nothing is left of the marker it replaces.
func TestPagesShareTheColourFile(t *testing.T) {
	for _, f := range []string{"web/index.html", "web/m.html"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(src), colorsMarker) != 1 {
			t.Errorf("%s should have the colours marker exactly once", f)
		}
		if strings.Contains(string(src), "--accentText:#") {
			t.Errorf("%s has colours of its own: they belong in web/colors.css", f)
		}
	}
	pages := map[string]string{}
	rec := httptest.NewRecorder()
	(&Server{token: "tok"}).index(rec, httptest.NewRequest("GET", "/", nil))
	pages["window"] = rec.Body.String()
	rec = httptest.NewRecorder()
	(&phoneServer{}).routes().ServeHTTP(rec, httptest.NewRequest("GET", "/m", nil))
	pages["phone"] = rec.Body.String()
	if csp := rec.Header().Get("Content-Security-Policy"); strings.Contains(csp, "style-src") && !strings.Contains(csp, "'unsafe-inline'") {
		t.Errorf("the phone page's styles are inline, but its policy is %q", csp)
	}
	for name, page := range pages {
		if strings.Contains(page, colorsMarker) {
			t.Errorf("%s page still has the colours marker", name)
		}
		for _, want := range []string{"--accentText:", "--ctl:", "--focus:", ":focus-visible", "prefers-reduced-motion", "prefers-contrast: more", "forced-colors"} {
			if !strings.Contains(page, want) {
				t.Errorf("%s page is missing %q", name, want)
			}
		}
	}
}
