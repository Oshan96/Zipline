//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// playSound uses the system's command-line player: afplay on macOS; PulseAudio, PipeWire
// or ALSA on Linux. It returns false if none is available.
func playSound(name string) bool {
	b := soundWAV(name)
	if b == nil {
		return false
	}
	players := [][]string{{"paplay"}, {"pw-play"}, {"aplay", "-q"}}
	if runtime.GOOS == "darwin" {
		players = [][]string{{"afplay"}}
	}
	for _, p := range players {
		bin, err := exec.LookPath(p[0])
		if err != nil {
			continue
		}
		f := filepath.Join(os.TempDir(), "zipline-"+name+".wav")
		if st, err := os.Stat(f); err != nil || st.Size() != int64(len(b)) {
			if os.WriteFile(f, b, 0o644) != nil {
				return false
			}
		}
		cmd := exec.Command(bin, append(p[1:], f)...)
		if cmd.Start() != nil {
			continue
		}
		go cmd.Wait()
		return true
	}
	return false
}
