package main

import (
	"strings"
	"testing"
	"time"
)

func askOf(m *Manager) *phoneAsk {
	m.phone.mu.Lock()
	defer m.phone.mu.Unlock()
	for _, a := range m.phone.asks {
		c := *a
		return &c
	}
	return nil
}

func phonePrompts(m *Manager) []Prompt {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Prompt
	for _, p := range m.prompts {
		if p.Kind == "phone" {
			out = append(out, p)
		}
	}
	return out
}

// A phone that uses a new code only asks. The window gets "A phone wants to connect" with the phone's
// name and address, and until it answers the code opens nothing else.
func TestNewPhoneOnlyAsks(t *testing.T) {
	m, c, _ := phoneManagerUnpaired(t)
	woken := 0
	m.phone.mu.Lock()
	m.phone.env.wake = func() { woken++ }
	m.phone.mu.Unlock()

	st, body := c.do("POST", "hello", map[string]string{"name": "Pixel 8 · Chrome"}, nil)
	if st != 200 || !strings.Contains(body, `"wait":true`) || strings.Contains(body, `"ok":true`) {
		t.Fatalf("first visit: %d %s", st, body)
	}
	ps := phonePrompts(m)
	if len(ps) != 1 || ps[0].Title != "Pixel 8 · Chrome" || ps[0].Name != "127.0.0.1" || ps[0].JobID == "" || woken != 1 {
		t.Fatalf("prompts %+v, woken %d", ps, woken)
	}
	// the key opens nothing else while it waits
	for _, path := range []string{"state", "add", "action", "limit", "probe", "queue", "profile"} {
		time.Sleep(3 * time.Millisecond)
		if st, _ := c.do("POST", path, map[string]any{"url": "http://example.com/x.zip"}, nil); st != 403 {
			t.Errorf("%s before the yes: %d", path, st)
		}
	}
	// asking again is the same question
	time.Sleep(3 * time.Millisecond)
	if st, body := c.do("POST", "hello", map[string]string{"name": "Pixel 8 · Chrome"}, nil); st != 200 || !strings.Contains(body, `"wait":true`) {
		t.Fatalf("second visit: %d %s", st, body)
	}
	if len(phonePrompts(m)) != 1 || woken != 1 {
		t.Fatalf("a second question was asked: %d prompts, woken %d", len(phonePrompts(m)), woken)
	}
	// the window's list shows who's asking
	v := m.phoneView()
	if len(v.Devices) != 1 || v.Devices[0].Ask == nil || v.Devices[0].Ask.Name != "Pixel 8 · Chrome" || v.Devices[0].Ask.Addr != "127.0.0.1" || v.Devices[0].Ask.Token != ps[0].JobID {
		t.Fatalf("devices %+v", v.Devices)
	}
	if err := m.answerPhone(ps[0].JobID, true); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Millisecond)
	if st, body := c.do("POST", "hello", map[string]string{"name": "Pixel 8 · Chrome"}, nil); st != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("after the yes: %d %s", st, body)
	}
	time.Sleep(3 * time.Millisecond)
	if st, _ := c.do("GET", "state", nil, nil); st != 200 {
		t.Fatalf("state after the yes: %d", st)
	}
	v = m.phoneView()
	if len(v.Devices) != 1 || v.Devices[0].Pending || v.Devices[0].Ask != nil || v.Devices[0].Name != "Pixel 8 · Chrome" {
		t.Fatalf("devices after %+v", v.Devices)
	}
	if err := m.answerPhone(ps[0].JobID, true); err == nil {
		t.Fatal("an answer was taken twice")
	}
}

func TestDeniedPhoneIsToldOnceAndForgotten(t *testing.T) {
	m, c, _ := phoneManagerUnpaired(t)
	c.do("POST", "hello", map[string]string{"name": "Mystery"}, nil)
	if err := m.answerPhone(askOf(m).Token, false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Millisecond)
	if st, body := c.do("POST", "hello", map[string]string{"name": "Mystery"}, nil); st != 200 || !strings.Contains(body, `"denied":true`) {
		t.Fatalf("after the no: %d %s", st, body)
	}
	if n := len(m.phoneView().Devices); n != 0 {
		t.Fatalf("the code stayed: %d devices", n)
	}
	time.Sleep(3 * time.Millisecond)
	if st, _ := c.do("POST", "hello", map[string]string{"name": "Mystery"}, nil); st != 401 {
		t.Fatalf("a refused phone came back in: %d", st)
	}
}

func TestPhoneAskRunsOutAndMovesWithTheAddress(t *testing.T) {
	m, c, _ := phoneManagerUnpaired(t)
	c.do("POST", "hello", map[string]string{"name": "A"}, nil)
	first := askOf(m)
	// nobody answered for two minutes
	m.phone.mu.Lock()
	m.phone.asks[first.ID].At = time.Now().Add(-pairAskLife - time.Second)
	m.phone.mu.Unlock()
	if err := m.answerPhone(first.Token, true); err == nil {
		t.Fatal("an old question was answered")
	}
	time.Sleep(3 * time.Millisecond)
	c.do("POST", "hello", map[string]string{"name": "A"}, nil)
	second := askOf(m)
	if second.Token == first.Token || len(phonePrompts(m)) != 2 {
		t.Fatalf("no new question: %+v", second)
	}
	// another address now holds the code: the window's question changes, so a yes can't be for the old one
	m.phone.mu.Lock()
	m.phone.asks[second.ID].Addr = "192.168.1.99"
	m.phone.mu.Unlock()
	time.Sleep(3 * time.Millisecond)
	c.do("POST", "hello", map[string]string{"name": "B"}, nil)
	third := askOf(m)
	if third.Token == second.Token || third.Name != "B" || third.Addr != "127.0.0.1" {
		t.Fatalf("third %+v", third)
	}
	if err := m.answerPhone(second.Token, true); err == nil {
		t.Fatal("the old question was answered after it changed")
	}
	if err := m.answerPhone(third.Token, true); err != nil {
		t.Fatal(err)
	}
}

// Only the window can allow a phone: not a program with the secret.
func TestOnlyTheWindowAllowsAPhone(t *testing.T) {
	m, c, _ := phoneManagerUnpaired(t)
	c.do("POST", "hello", map[string]string{"name": "A"}, nil)
	h := (&Server{m: m, token: "t"}).routes()
	if code := postCode(h, "phone", map[string]any{"action": "answer", "token": askOf(m).Token, "allow": true}); code != 403 {
		t.Fatalf("a program allowed a phone: %d", code)
	}
	if a := askOf(m); a == nil || a.State != askPending {
		t.Fatal("the question was answered")
	}
}

func TestCleanPhoneName(t *testing.T) {
	for in, want := range map[string]string{
		"Pixel 8":                "Pixel 8",
		"  Pixel\t8\n · Chrome ": "Pixel 8 · Chrome",
		"":                       "",
		strings.Repeat("é", 100): strings.Repeat("é", 60),
		"<b>x</b>\u0007‮":        "<b>x</b>",
	} {
		if got := cleanPhoneName(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}
