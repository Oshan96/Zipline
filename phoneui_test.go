package main

import (
	"regexp"
	"strings"
	"testing"
)

// phoneWords finds the catalogue ids a page uses, wherever they are in the code (not only inside t(…)):
// a quoted word with a dot whose first part is one the catalogue has (ph, fi, tor…).
func phoneWords(src string) []string {
	lit := regexp.MustCompile(`"([a-z]{1,6}\.[A-Za-z0-9_.]*)"`)
	have := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)"([a-z]{1,6})\.[A-Za-z0-9_.]*":`).FindAllStringSubmatch(i18nJS, -1) {
		have[m[1]] = true
	}
	var out []string
	for _, m := range lit.FindAllStringSubmatch(src, -1) {
		if have[strings.SplitN(m[1], ".", 2)[0]] {
			out = append(out, m[1])
		}
	}
	return out
}

func phoneInCatalogue(id string) bool {
	if strings.HasSuffix(id, ".") { // a prefix: the code adds the rest (o.phCat.public…)
		return strings.Contains(i18nJS, `"`+id)
	}
	return strings.Contains(i18nJS, `"`+id+`":`)
}

// The words the window's phone parts and the phone page ask for are in the catalogue.
func TestPhoneMessages(t *testing.T) {
	for name, src := range map[string]string{"phoneui.js": phoneUIJS, "m.html": phoneHTML} {
		words := phoneWords(src)
		if len(words) < 5 {
			t.Errorf("%s: found only %d words; the check is looking in the wrong place", name, len(words))
		}
		for _, id := range words {
			if !phoneInCatalogue(id) {
				t.Errorf("%s asks for %q, which the catalogue doesn't have", name, id)
			}
		}
	}
	for _, id := range []string{"o.phCat.public", "o.phCat.private", "o.phCat.domain", "o.phSt.on", "o.phSt.off", "o.phSt.allowed", "o.phSt.away", "o.phErr.public", "o.phErr.nonet"} {
		if !phoneInCatalogue(id) { // (phoneui.js and app.js build these)
			t.Errorf("%s is missing", id)
		}
	}
}

// The window loads the phone script, and the page it adds to the window's routes is served.
func TestPhoneUIServed(t *testing.T) {
	if !strings.Contains(indexHTML, `<script src="/phoneui.js">`) {
		t.Error("index.html doesn't load phoneui.js")
	}
	for _, fn := range []string{"phoneDialog", "phoneState", "phoneNets", "phoneAskRows", "wirePhoneAsks", "phoneAsked"} {
		if !strings.Contains(phoneUIJS, "function "+fn+"(") {
			t.Errorf("phoneui.js has no %s", fn)
		}
		if !strings.Contains(appJS, fn+"(") && fn != "phoneDialog" && fn != "phoneAsked" {
			t.Errorf("app.js doesn't use %s", fn)
		}
	}
}
