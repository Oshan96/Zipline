package main

import (
	"bytes"
	"encoding/binary"
	"math"
	"sync"
	"time"
)

// Sounds for download events. The chimes are generated here (no sound files to ship) and
// played by the operating system, so they work even when the window is closed.

type SoundSettings struct {
	On     bool `json:"on"`
	Done   bool `json:"done"`   // a download finished
	Failed bool `json:"failed"` // a download stopped with an error
	Queue  bool `json:"queue"`  // a queue finished
}

func (s SoundSettings) wants(kind string) bool {
	switch kind {
	case "done":
		return s.On && s.Done
	case "failed":
		return s.On && s.Failed
	case "queue":
		return s.On && s.Queue
	}
	return false
}

type tone struct{ freq, at, dur float64 }

var chimes = map[string][]tone{
	"done":   {{880, 0, 0.35}, {1318.5, 0.11, 0.55}},                   // rising: done
	"failed": {{493.9, 0, 0.3}, {370, 0.14, 0.5}},                      // falling: something went wrong
	"queue":  {{659.3, 0, 0.3}, {830.6, 0.1, 0.3}, {987.8, 0.2, 0.65}}, // three steps: all done
}

var (
	wavMu    sync.Mutex
	wavCache = map[string][]byte{}
)

// soundWAV returns a short chime as a 16-bit mono WAV file (nil for an unknown name).
func soundWAV(name string) []byte {
	wavMu.Lock()
	defer wavMu.Unlock()
	if b, ok := wavCache[name]; ok {
		return b
	}
	notes, ok := chimes[name]
	if !ok {
		return nil
	}
	const rate = 44100
	end := 0.0
	for _, n := range notes {
		end = max(end, n.at+n.dur)
	}
	samples := make([]float64, int((end+0.03)*rate))
	for _, n := range notes {
		start := int(n.at * rate)
		for i := 0; i < int(n.dur*rate) && start+i < len(samples); i++ {
			t := float64(i) / rate
			env := math.Exp(-t / (n.dur / 4))
			if t < 0.006 {
				env *= t / 0.006 // soft attack, no click
			}
			v := math.Sin(2*math.Pi*n.freq*t) + 0.25*math.Sin(4*math.Pi*n.freq*t)
			samples[start+i] += v * env
		}
	}
	peak := 0.0
	for _, s := range samples {
		peak = max(peak, math.Abs(s))
	}
	var b bytes.Buffer
	data := uint32(len(samples) * 2)
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, 36+data)
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(rate), uint32(rate * 2), uint16(2), uint16(16)} {
		binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, data)
	for _, s := range samples {
		binary.Write(&b, binary.LittleEndian, int16(s/peak*0.45*32767))
	}
	wavCache[name] = b.Bytes()
	return wavCache[name]
}

var lastSound time.Time

// soundPlayer plays a chime on this computer (replaced in tests).
var soundPlayer = playSound

// playEvent plays the sound for an event if it's switched on. Caller holds m.mu.
func (m *Manager) playEvent(kind string) {
	if !m.settings.Sounds.wants(kind) || time.Since(lastSound) < 1200*time.Millisecond {
		return // a burst of events (a batch finishing) plays once
	}
	lastSound = time.Now()
	go func() {
		if !soundPlayer(kind) { // no system player: let an open window play it
			m.mu.Lock()
			m.notify("sound", "", kind)
			m.mu.Unlock()
		}
	}()
}
