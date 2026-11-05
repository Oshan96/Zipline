package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// a stand-in for GitHub's latest-release answer, with the installer it lists
func updateFake(t *testing.T, tag string, setup []byte, digest string, edit func(map[string]any)) *svcFake {
	f := newSvcFake(t)
	swap(t, &updateAPI, f.URL+"/latest")
	swap(t, &updateFrom, f.URL+"/download/")
	swap(t, &updatePage, f.URL+"/releases/")
	f.file("/download/"+tag+"/Zipline-Windows-Setup.exe", setup)
	f.route = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/latest" {
			return false
		}
		rel := map[string]any{"tag_name": tag, "html_url": f.URL + "/releases/tag/" + tag,
			"body": "## Download\n\n| | Installer |\n\n## What's Changed\n### New features\n" +
				"* Unpack archives when they finish by @Oshan96 in https://github.com/Oshan96/Zipline/pull/6\n" +
				"* Download Pixeldrain links by @Oshan96 in https://github.com/Oshan96/Zipline/pull/7\n\n**Full Changelog**: https://x",
			"assets": []any{
				map[string]any{"name": "Zipline-Windows-Setup.exe", "browser_download_url": f.URL + "/download/" + tag + "/Zipline-Windows-Setup.exe", "size": len(setup), "digest": digest},
				map[string]any{"name": "Zipline-macOS.dmg", "browser_download_url": f.URL + "/download/" + tag + "/Zipline-macOS.dmg", "size": 9, "digest": "sha256:00"},
			}}
		if edit != nil {
			edit(rel)
		}
		writeJSONTest(w, 200, rel)
		return true
	}
	return f
}

type installerRun struct {
	sync.Mutex
	path string
	data []byte
}

func fakeInstaller(t *testing.T) *installerRun {
	ran := &installerRun{}
	swap(t, &runInstaller, func(p string) error {
		b, _ := os.ReadFile(p)
		ran.Lock()
		ran.path, ran.data = p, b
		ran.Unlock()
		return nil
	})
	return ran
}

// Zipline finds a newer release, says what changed (without the authors and links) and says so once;
// an installed Windows copy downloads the installer, checks it against GitHub's SHA-256, runs it, and quits.
func TestUpdateCheckAndInstall(t *testing.T) {
	setup := bytes.Repeat([]byte("MZ installer "), 5000)
	updateFake(t, "v9.9.9", setup, "sha256:"+sha256Hex(setup), nil)
	swap(t, &updateAsset, func() (string, bool) { return "Zipline-Windows-Setup.exe", true })
	ran := fakeInstaller(t)
	m := newTestManager(t)
	v := m.CheckUpdate(context.Background())
	if v.Version != "9.9.9" || !v.Install || v.Asset != "Zipline-Windows-Setup.exe" || v.Current != appVersion || !strings.HasSuffix(v.Page, "/releases/tag/v9.9.9") {
		t.Fatalf("check: %+v", v)
	}
	if strings.Join(v.Notes, "|") != "Unpack archives when they finish|Download Pixeldrain links" {
		t.Errorf("notes: %q", v.Notes)
	}
	m.CheckUpdate(context.Background()) // (again: no second notice)
	m.mu.Lock()
	told := 0
	for _, n := range m.notices {
		if n.Kind == "update" {
			told++
		}
	}
	m.mu.Unlock()
	if told != 1 {
		t.Errorf("%d notices for one new version", told)
	}
	if err := m.InstallUpdate(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-m.quit:
	case <-time.After(10 * time.Second):
		t.Fatal("Zipline didn't quit for the installer")
	}
	ran.Lock()
	defer ran.Unlock()
	if !bytes.Equal(ran.data, setup) || !strings.HasSuffix(ran.path, "Zipline-Windows-Setup.exe") {
		t.Fatalf("the installer that ran: %q (%d bytes)", ran.path, len(ran.data))
	}
}

// An installer that doesn't match GitHub's checksum is never run; a file from anywhere but the
// release's downloads isn't offered; drafts, pre-releases and this version itself aren't updates.
func TestUpdateRefusals(t *testing.T) {
	setup := []byte("the real installer")
	swap(t, &updateAsset, func() (string, bool) { return "Zipline-Windows-Setup.exe", true })
	ran := fakeInstaller(t)

	updateFake(t, "v9.9.9", setup, "sha256:"+strings.Repeat("ab", 32), nil)
	m := newTestManager(t)
	m.CheckUpdate(context.Background())
	if err := m.InstallUpdate(); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		m.mu.Lock()
		st, e := m.upd.view.State, m.upd.view.Error
		m.mu.Unlock()
		if st == "failed" {
			if !strings.Contains(e, "checksum") {
				t.Errorf("error: %q", e)
			}
			break
		}
		if time.Now().After(end) {
			t.Fatalf("state %q", st)
		}
	}
	ran.Lock()
	if ran.path != "" {
		t.Error("a mismatched installer was run")
	}
	ran.Unlock()
	if entries, _ := os.ReadDir(m.dataDir + "/update"); len(entries) != 0 {
		t.Errorf("the mismatched installer was kept: %v", entries)
	}

	for name, edit := range map[string]func(map[string]any){
		"elsewhere": func(r map[string]any) {
			r["assets"].([]any)[0].(map[string]any)["browser_download_url"] = "https://evil.example/Zipline-Windows-Setup.exe"
		},
		"no digest": func(r map[string]any) { r["assets"].([]any)[0].(map[string]any)["digest"] = "" },
	} {
		updateFake(t, "v9.9.9", setup, "", edit)
		v := (&Manager{}).checkOnly(t)
		if v.Version != "9.9.9" || v.Install {
			t.Errorf("%s: %+v", name, v)
		}
	}
	for name, edit := range map[string]func(map[string]any){
		"draft":       func(r map[string]any) { r["draft"] = true },
		"pre-release": func(r map[string]any) { r["prerelease"] = true },
		"this one":    func(r map[string]any) { r["tag_name"] = "v" + appVersion },
		"older":       func(r map[string]any) { r["tag_name"] = "v0.9.0" },
		"not a tag":   func(r map[string]any) { r["tag_name"] = "nightly" },
	} {
		updateFake(t, "v9.9.9", setup, "", edit)
		if v := (&Manager{}).checkOnly(t); v.Version != "" {
			t.Errorf("%s offered as an update: %+v", name, v)
		}
	}

	// installing runs a program: the window may ask, a program with the secret may not
	updateFake(t, "v9.9.9", setup, "sha256:"+sha256Hex(setup), nil)
	m2 := newTestManager(t)
	m2.CheckUpdate(context.Background())
	h := (&Server{m: m2, token: "t"}).routes()
	rec := winReq(h, "POST", "/api/update", map[string]any{"action": "install"}, nil, map[string]string{"X-Token": "t"})
	if rec.Code != 403 {
		t.Fatalf("install with the secret: %d", rec.Code)
	}
}

// checkOnly runs fetchUpdate, as CheckUpdate does, without a manager's notices.
func (*Manager) checkOnly(t *testing.T) UpdateView {
	t.Helper()
	v, err := fetchUpdate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestReleaseNotes(t *testing.T) {
	body := "## Download\n* not this\n## What's Changed\n### Fixes\n* Stop a batch from stacking up windows by @a-b in https://github.com/x/y/pull/3\n- Plain line\n\n**Full Changelog**: z\n## New Contributors\n* @x made their first contribution"
	if got := strings.Join(releaseNotes(body), "|"); got != "Stop a batch from stacking up windows|Plain line" {
		t.Errorf("%q", got)
	}
}
