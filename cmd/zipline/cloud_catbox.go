package main

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

// Catbox albums (catbox.moe/c/<id>): the album's page links each of its files at files.catbox.moe, so
// the Add dialog lists them like a shared folder's. A single Catbox file (files.catbox.moe/<name>) is an
// ordinary link and needs nothing.

var (
	catboxWeb   = "https://catbox.moe"                                              // replaced in tests
	catboxFiles = []string{"https://files.catbox.moe", "https://litter.catbox.moe"} // where an album's files are; replaced in tests
)

func catboxHost(h string) bool {
	return hostIn(h, "catbox.moe") && !hostIn(h, "files.catbox.moe", "litter.catbox.moe")
}

var catboxID = regexp.MustCompile(`^[A-Za-z0-9]{1,32}$`)

// catboxAlbum is an album link's id ("" for anything else).
func catboxAlbum(u *url.URL) string {
	if rest, ok := strings.CutPrefix(u.Path, "/c/"); ok && catboxID.MatchString(strings.TrimSuffix(rest, "/")) {
		return strings.TrimSuffix(rest, "/")
	}
	return ""
}

var catboxTitle = regexp.MustCompile(`(?s)<h1[^>]*>(.*?)</h1>`)
var htmlTag = regexp.MustCompile(`<[^>]*>`)

// catboxFolder lists an album's files from its page.
func catboxFolder(ctx context.Context, raw string) (*cloudFolder, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errNotFolder
	}
	id := catboxAlbum(u)
	if id == "" {
		return nil, errNotFolder
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	page, code, err := cloudPage(ctx, catboxWeb+"/c/"+id, nil)
	switch {
	case err != nil:
		return nil, err
	case code == 404:
		return nil, permanentErr{errors.New("Catbox: there's no album at this link")}
	case code >= 400:
		return nil, fmt.Errorf("Catbox answered %d", code)
	}
	cf := &cloudFolder{Service: "Catbox", Name: "Catbox album " + id}
	if m := catboxTitle.FindSubmatch(page); m != nil {
		if t := strings.TrimSpace(html.UnescapeString(htmlTag.ReplaceAllString(string(m[1]), ""))); t != "" {
			cf.Name = t
		}
	}
	seen := map[string]bool{}
	for _, base := range catboxFiles {
		re := regexp.MustCompile(regexp.QuoteMeta(base) + `/[A-Za-z0-9][A-Za-z0-9._-]*`)
		for _, link := range re.FindAllString(string(page), -1) {
			if seen[link] {
				continue
			}
			seen[link] = true
			if len(cf.Files) >= maxFolderFiles {
				cf.More = true
				break
			}
			cf.Files = append(cf.Files, cloudFile{Path: path.Base(link), Size: -1, URL: link})
		}
	}
	if len(cf.Files) == 0 {
		return nil, permanentErr{errors.New("Catbox: this album has no files")}
	}
	return cf, nil
}
