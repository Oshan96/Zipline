package main

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"

	"zipline/internal/httpauth"
	"zipline/internal/psl"
)

// Requests for a download go through doReq. It gives each download its own cookie jar, so
// cookies a site sets on the way (a redirect to the file server, a session cookie) are sent back
// as a browser would, and it sends a saved login only to a server that asks for one: Digest when
// offered, Basic only over HTTPS or on the local network (a password sent as Basic over plain
// http on the internet can be read by anyone on the way).

// hostAuth remembers how a server asked to log in, so the download's other connections send the
// login straight away instead of being turned away first.
type hostAuth struct {
	scheme string // "basic" or "digest"
	digest *httpauth.Digest
}

// errInsecureLogin: the server wants the password sent in a way anyone on the way could read.
var errInsecureLogin = errors.New("the site asks for the password over an unencrypted (http) connection, so Zipline didn't send it — use an https:// link if the site has one")

type urlLoginKey struct{}

// client is the download's HTTP client: the shared connections, its own cookies.
func (d *Download) client() *http.Client {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.jar == nil {
		d.jar, _ = cookiejar.New(&cookiejar.Options{PublicSuffixList: psl.List{}})
	}
	return &http.Client{Transport: httpClient.Transport, Jar: d.jar}
}

// loginFor finds the login for a request to u: one typed into the link itself, the one from the
// Add dialog (for the download's own site only), or a saved one for the site (Options → Sites).
func loginFor(ctx context.Context, d *Download, u *url.URL) (user, pass string, ok bool) {
	own := true
	if pu, err := url.Parse(d.URL); err == nil && pu.Host != "" && !sameSite(pu.Hostname(), u.Hostname()) {
		own = false
	}
	if ui, _ := ctx.Value(urlLoginKey{}).(*url.Userinfo); ui != nil && own {
		p, _ := ui.Password()
		return ui.Username(), usableSecret(p), true
	}
	if a := d.Headers["Authorization"]; own && strings.HasPrefix(a, "Basic ") {
		if b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(a[6:])); err == nil {
			if us, pw, found := strings.Cut(string(b), ":"); found {
				return us, pw, true
			}
		}
	}
	return siteLogin(u)
}

// doReq sends a request built by buildRequest for d.
func doReq(d *Download, req *http.Request) (*http.Response, error) {
	if d == nil {
		resp, err := httpClient.Do(req)
		if err != nil {
			noteProxyFailure(err)
		}
		return resp, err
	}
	if cloudHandles(d, req) { // a share link's file: the service's own ways (cloud_hooks.go)
		return cloudDo(d, req)
	}
	c := d.client()
	host := strings.ToLower(req.URL.Host)
	if user, pass, ok := loginFor(req.Context(), d, req.URL); ok && req.Header.Get("Authorization") == "" {
		d.mu.Lock()
		ha := d.auth[host]
		d.mu.Unlock()
		if ha != nil { // this server asked before: answer up front
			setAuth(req, ha, user, pass)
		}
	}
	resp, err := c.Do(req)
	if err != nil {
		noteProxyFailure(err) // a dead proxy from the system/PAC list: later requests use the next one
		return nil, err
	}
	for try := 0; try < 2 && resp.StatusCode == http.StatusUnauthorized; try++ {
		at := resp.Request.URL // (after redirects: the server that asked)
		user, pass, ok := loginFor(req.Context(), d, at)
		if !ok {
			return resp, nil
		}
		chs := httpauth.ParseChallenges(resp.Header.Values("WWW-Authenticate"))
		ha := &hostAuth{}
		switch httpauth.Choose(chs, at) {
		case "Digest":
			dg, err := httpauth.BestDigest(chs)
			if err != nil {
				return resp, nil
			}
			if try > 0 && !dg.Stale { // a fresh challenge that isn't about an old nonce: the login is wrong
				return resp, nil
			}
			ha.scheme, ha.digest = "digest", dg
		case "Basic":
			if try > 0 {
				return resp, nil // refused
			}
			ha.scheme = "basic"
		default:
			for _, ch := range chs {
				if ch.Scheme == "basic" {
					drainClose(resp.Body)
					return nil, permanentErr{errInsecureLogin}
				}
			}
			return resp, nil
		}
		drainClose(resp.Body)
		h := strings.ToLower(at.Host)
		d.mu.Lock()
		if d.auth == nil {
			d.auth = map[string]*hostAuth{}
		}
		d.auth[h] = ha
		d.mu.Unlock()
		again := resend(req, req.Context())
		again.URL, again.Host = at, ""
		again.Header.Del("Authorization")
		setAuth(again, ha, user, pass)
		if resp, err = c.Do(again); err != nil {
			noteProxyFailure(err)
			return nil, err
		}
	}
	return resp, nil
}

func setAuth(req *http.Request, ha *hostAuth, user, pass string) {
	switch ha.scheme {
	case "basic":
		if httpauth.BasicAllowed(req.URL) {
			req.SetBasicAuth(user, pass)
		}
	case "digest":
		req.Header.Set("Authorization", ha.digest.Authorize(req.Method, req.URL.RequestURI(), user, pass))
	}
}
