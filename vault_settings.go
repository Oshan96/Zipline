package main

import "strings"

// The window never gets a password back. GET /api/settings sends every password field empty with
// "passSet": true beside it when one is saved; a save that sends a field empty with passSet still
// true keeps the stored password, empty with passSet false clears it, and typed text replaces it.
// A link's or the proxy address's "user:password@" loses its password in what the window sees,
// and gets it back when the window sends the same address back.

type settingsView struct {
	Settings
	Vault vaultInfo `json:"vault"` // how saved passwords are kept (Options says so)
}

// settingsView is the settings as the window may see them.
func (m *Manager) settingsView() settingsView {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.settings
	st.Phone.Devices = nil // (their keys stay here; Options → Phone asks /api/phone)
	st.Proxy.PassSet, st.Proxy.Pass = st.Proxy.Pass != "", ""
	st.Proxy.URL = stripURLPass(st.Proxy.URL)
	st.Sites = append([]SiteRule(nil), st.Sites...)
	for i := range st.Sites {
		st.Sites[i].PassSet, st.Sites[i].Pass = st.Sites[i].Pass != "", ""
	}
	return settingsView{st, m.vault.info()}
}

// mergeSecrets puts the stored passwords back into settings the window sent, wherever it left them
// untouched. Caller holds m.mu.
func (m *Manager) mergeSecrets(st *Settings) {
	old := m.settings
	if st.Proxy.Pass == "" && st.Proxy.PassSet {
		st.Proxy.Pass = old.Proxy.Pass
	}
	st.Proxy.PassSet = false
	st.Proxy.URL = restoreURLPass(st.Proxy.URL, old.Proxy.URL)
	for i := range st.Sites {
		r := &st.Sites[i]
		if r.Pass == "" && r.PassSet {
			r.Pass = savedSitePass(old.Sites, *r)
		}
		r.PassSet = false
	}
	ensureSiteIDs(st.Sites)
}

// savedSitePass finds the password a rule had before: by its id, or for a rule from a window that
// doesn't send ids, by site and user.
func savedSitePass(old []SiteRule, r SiteRule) string {
	for _, o := range old {
		if r.ID != "" && o.ID == r.ID || r.ID == "" && strings.EqualFold(o.Site, r.Site) && o.User == r.User {
			return o.Pass
		}
	}
	return ""
}

// ensureSiteIDs gives every rule that has none an id of its own.
func ensureSiteIDs(rules []SiteRule) {
	for i := range rules {
		if rules[i].ID == "" {
			rules[i].ID = newID()
		}
	}
}

// fillProxyPass gives a proxy the window wants to test the password it left untouched.
func (m *Manager) fillProxyPass(p *ProxySettings) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.Pass == "" && p.PassSet {
		p.Pass = m.settings.Proxy.Pass
	}
	p.PassSet = false
	p.URL = restoreURLPass(p.URL, m.settings.Proxy.URL)
}

// withoutLogins is a scan's options as the window may see them: no cookies, no logins.
func (o GrabOptions) withoutLogins() GrabOptions {
	if len(o.Headers) == 0 {
		return o
	}
	h := map[string]string{}
	for k, v := range o.Headers {
		if !credentialHeader(k) {
			h[k] = v
		}
	}
	if len(h) == 0 {
		h = nil
	}
	o.Headers = h
	return o
}

// noteVaultSave tells the window, once, that a secret had to be saved without protection.
func (m *Manager) noteVaultSave() {
	if n := m.vault.sealNote(); n != "" {
		logf("%s", n)
		m.mu.Lock()
		m.notify("refused", "", n)
		m.mu.Unlock()
	}
}
