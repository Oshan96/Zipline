package main

// sealedProjects is the saved scan projects as written to grabber.json: copies whose logins and
// cookies are sealed. Caller holds g.mu.
func (g *Grabber) sealedProjects() []*GrabProject {
	out := make([]*GrabProject, len(g.projects))
	for i, p := range g.projects {
		c := *p
		c.Opts.Headers = g.m.vault.protectHeaders(c.Opts.Headers)
		out[i] = &c
	}
	return out
}
