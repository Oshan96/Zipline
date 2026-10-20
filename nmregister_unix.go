//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"runtime"
)

// nmDir is a browser's own folder (it exists once the browser has run) and the folder its
// per-user host manifests go in.
type nmDir struct {
	browser, hosts string
	firefox        bool
}

// nmDirs lists where each browser looks, on macOS (darwin) or Linux and the other Unixes.
func nmDirs(home, config string, darwin bool) []nmDir {
	if darwin {
		as := filepath.Join(home, "Library", "Application Support")
		var out []nmDir
		for _, b := range []string{"Google/Chrome", "Chromium", "Microsoft Edge", "BraveSoftware/Brave-Browser", "Vivaldi", "com.operasoftware.Opera"} {
			d := filepath.Join(as, filepath.FromSlash(b))
			out = append(out, nmDir{d, filepath.Join(d, "NativeMessagingHosts"), false})
		}
		return append(out, nmDir{filepath.Join(as, "Mozilla"), filepath.Join(as, "Mozilla", "NativeMessagingHosts"), true})
	}
	var out []nmDir
	for _, b := range []string{"google-chrome", "chromium", "microsoft-edge", "BraveSoftware/Brave-Browser", "vivaldi", "opera"} {
		d := filepath.Join(config, filepath.FromSlash(b))
		out = append(out, nmDir{d, filepath.Join(d, "NativeMessagingHosts"), false})
	}
	return append(out, nmDir{filepath.Join(home, ".mozilla"), filepath.Join(home, ".mozilla", "native-messaging-hosts"), true})
}

func registerNativeHostFor(exe, dataDir string) []error {
	home, _ := os.UserHomeDir()
	config, _ := os.UserConfigDir() // ($XDG_CONFIG_HOME or ~/.config on Linux)
	return writeHostManifests(exe, nmDirs(home, config, runtime.GOOS == "darwin"))
}

func writeHostManifests(exe string, dirs []nmDir) []error {
	var errs []error
	for _, d := range dirs {
		if st, err := os.Stat(d.browser); err != nil || !st.IsDir() {
			continue
		}
		if err := writeIfChanged(filepath.Join(d.hosts, nmHostName+".json"), nmManifestJSON(exe, d.firefox)); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}
