//go:build !windows

package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func vtModes(t *testing.T, dir string) map[string]fs.FileMode {
	t.Helper()
	out := map[string]fs.FileMode{}
	filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err == nil {
			rel, _ := filepath.Rel(dir, p)
			if info, err := e.Info(); err == nil {
				out[rel] = info.Mode().Perm()
			}
		}
		return nil
	})
	return out
}

func TestDataFolderIsPrivate(t *testing.T) {
	vtUse(t, &vtKeys{keys: vtKey(1)})
	dir := vtDir(t)
	// what an older version left: everyone can read it
	os.Chmod(dir, 0o755)
	for _, d := range []string{"torrents", "scans", "window/Default"} {
		os.MkdirAll(filepath.Join(dir, d), 0o755)
	}
	old := []string{"settings.json", "downloads.json", "downloads.json.bak", "grabber.json", "usage.json", "dht.json", "zipline.log", "crash.log", "running", "torrents/ab.torrent", "scans/s0.json"}
	for _, f := range old {
		os.WriteFile(filepath.Join(dir, f), []byte("{}"), 0o644)
	}
	os.WriteFile(filepath.Join(dir, "window/Default/Prefs"), []byte("{}"), 0o644)
	os.Symlink("/etc/hostname", filepath.Join(dir, "link"))

	m := NewManager(dir)
	for rel, mode := range vtModes(t, dir) {
		switch {
		case rel == "link":
		case rel == "window" || filepath.Dir(rel) == "window" || filepath.Base(rel) == "Prefs":
			if mode != 0o755 && mode != 0o644 {
				t.Errorf("%s (the browser's own) was changed to %v", rel, mode)
			}
		case mode&0o077 != 0:
			t.Errorf("after start-up %s is %v", rel, mode)
		}
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Errorf("data folder %v", st.Mode().Perm())
	}

	// and everything written from now on is private too
	vtSetSecrets(m)
	m.mu.Lock()
	m.settings.DownloadDir = dir
	m.mu.Unlock()
	m.save()
	m.saveTorrentFile(&Metainfo{InfoHash: [20]byte{1, 2, 3}}, []byte("d4:infod4:name1:aee"))
	m.grab.SaveProject(GrabProject{Name: "p", Opts: GrabOptions{URL: "https://x.example/", Types: []string{"Images"}, Headers: map[string]string{"Cookie": "a=b"}}}, "")
	m.grab.persist(&GrabJob{ID: "s1", Opts: GrabOptions{URL: "https://x.example/"}, State: "done", seen: map[string]bool{}})
	os.RemoveAll(filepath.Join(dir, "torrents"))
	m.saveTorrentFile(&Metainfo{InfoHash: [20]byte{1, 2, 3}}, []byte("d4:infod4:name1:aee"))
	m.save() // (a second save makes the .bak files)
	files := 0
	for rel, mode := range vtModes(t, dir) {
		if rel == "." || rel == "link" || rel == "window" || filepath.Dir(rel) == "window" || filepath.Base(rel) == "Prefs" {
			continue
		}
		files++
		if mode&0o077 != 0 {
			t.Errorf("%s is %v: other users could read it", rel, mode)
		}
	}
	if files < 10 {
		t.Errorf("only %d entries checked", files)
	}
	if modes := vtModes(t, dir); modes["torrents"] != 0o700 || modes["scans"] != 0o700 || modes["settings.json"] != 0o600 {
		t.Errorf("modes %v", modes)
	}
}

func TestPrivateDirIsMadeOrFixed(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "a", "Zipline")
	if err := makePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Errorf("new folder %v", st.Mode().Perm())
	}
	os.Chmod(dir, 0o755)
	makePrivateDir(dir)
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Errorf("existing folder %v", st.Mode().Perm())
	}
}
