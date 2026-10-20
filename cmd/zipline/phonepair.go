package main

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// Allowing a new phone. The code in the QR is a phone's secret key, and the page it opens is plain
// HTTP, so seeing the code is not enough: a phone that uses a new code only asks to connect. The
// window shows "A phone wants to connect: <name>, <address>" and the person at the computer allows
// or denies it (the same question the browser extension is asked, extlink.go). Until it's allowed the
// code opens nothing but this question, which the phone repeats every couple of seconds to hear the
// answer. A phone that was allowed before just checks in.

// cleanPhoneName makes the name a phone gave itself fit to show: no control characters, none that turn
// the text around (right-to-left overrides), 60 letters.
func cleanPhoneName(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r):
			return ' '
		case unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
	return clip(strings.Join(strings.Fields(s), " "), 60)
}

// hello is the phone's first call and its check-in.
func (p *phoneServer) hello(w http.ResponseWriter, r *http.Request, dev *PhoneDevice, name string) {
	m := p.m
	if !dev.Pending {
		m.mu.Lock()
		m.dirty = true
		m.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "app": "Zipline", "version": appVersion})
		return
	}
	name = cleanPhoneName(name)
	if name == "" {
		name = "Phone"
	}
	addr, _, _ := net.SplitHostPort(r.RemoteAddr)
	now := time.Now()
	p.mu.Lock()
	for id, a := range p.asks { // (answers nobody came back for)
		if now.Sub(a.At) > 10*time.Minute {
			delete(p.asks, id)
		}
	}
	a := p.asks[dev.ID]
	if a != nil && a.State == askDenied {
		delete(p.asks, dev.ID)
		p.mu.Unlock()
		m.removePhone(dev.ID) // told once; the code is of no use now
		writeJSON(w, map[string]any{"ok": false, "denied": true})
		return
	}
	// a new ask, when none is waiting, when it ran out, or when another address now holds the code
	fresh := a == nil || now.Sub(a.At) > pairAskLife || a.Addr != addr
	if fresh {
		a = &phoneAsk{ID: dev.ID, Token: randHex(8), Name: name, Addr: addr, At: now, State: askPending}
		p.asks[dev.ID] = a
	}
	token, wake := a.Token, p.env.wake
	p.mu.Unlock()
	if fresh {
		m.AddPrompt(Prompt{Kind: "phone", JobID: token, Title: name, Name: addr, Source: "phone"})
		if wake != nil {
			wake()
		}
	}
	writeJSON(w, map[string]any{"ok": false, "wait": true, "left": int(pairAskLife.Seconds())})
}

// answerPhone is the window's answer to a phone that wants to connect (token: the ask's).
func (m *Manager) answerPhone(token string, allow bool) error {
	ph := m.phone
	if ph == nil || token == "" {
		return errPhoneAsk
	}
	ph.mu.Lock()
	var a *phoneAsk
	for _, x := range ph.asks {
		if x.Token == token && x.State == askPending && time.Since(x.At) <= pairAskLife {
			a = x
		}
	}
	if a == nil {
		ph.mu.Unlock()
		return errPhoneAsk
	}
	if !allow {
		a.State = askDenied // (the phone is told on its next call)
		ph.mu.Unlock()
		return nil
	}
	delete(ph.asks, a.ID)
	ph.mu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.settings.Phone.Devices {
		d := &m.settings.Phone.Devices[i]
		if d.ID == a.ID && d.Pending {
			d.Pending, d.Name = false, a.Name
			m.dirty = true
			m.notify("phonePaired", d.ID, "Paired with "+iso(d.Name))
			return nil
		}
	}
	return errPhoneAsk
}

var errPhoneAsk = errors.New("that request has expired; ask again from the phone")

// phoneAskVw is a phone waiting for the window's answer, as Options → Phone shows it.
type phoneAskVw struct {
	Token string `json:"token"`
	Name  string `json:"name"`
	Addr  string `json:"addr"`
}

// markAsks puts the waiting phones on the devices they ask for.
func (p *phoneServer) markAsks(devs []phoneDevVw) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range devs {
		if a := p.asks[devs[i].ID]; a != nil && a.State == askPending && time.Since(a.At) <= pairAskLife {
			devs[i].Ask = &phoneAskVw{a.Token, a.Name, a.Addr}
		}
	}
}
