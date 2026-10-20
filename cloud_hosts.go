package main

import (
	"context"
	"net/url"
	"strings"
)

// The file hosts besides Google Drive, Dropbox and OneDrive (cloud.go): which links are theirs,
// and the resolver that turns each into its file's address.

// moreCloudService names the service of a link cloud.go's own list doesn't know ("" for none).
func moreCloudService(u *url.URL, h string) string {
	switch {
	case h == "drive.google.com" && gdriveFolderID(u) != "":
		return "Google Drive"
	case onedriveHost(h) && onedriveLink(u):
		return "OneDrive"
	case megaHost(h):
		if id, _, folder := megaLink(u); id != "" || folder {
			return "MEGA"
		}
	case yandexHost(h):
		if k, _ := yandexLink(u); k != "" {
			return "Yandex Disk"
		}
	case pcloudHost(h):
		if c, _ := pcloudLink(u); c != "" {
			return "pCloud"
		}
	case mediafireHost(h):
		if k, folder := mediafireLink(u); k != "" || folder {
			return "MediaFire"
		}
	case wetransferHost(h):
		if wetransferLink(u) {
			return "WeTransfer"
		}
	}
	return ""
}

// resolveMoreCloud turns a link to one of these hosts into its file's address.
func resolveMoreCloud(ctx context.Context, d *Download, raw string) (string, error) {
	switch cloudService(raw) {
	case "MEGA":
		return resolveMega(ctx, raw)
	case "Yandex Disk":
		return resolveYandex(ctx, raw)
	case "pCloud":
		return resolvePCloud(ctx, raw)
	case "MediaFire":
		return resolveMediaFire(ctx, raw)
	case "WeTransfer":
		return resolveWeTransfer(ctx, raw)
	}
	return raw, nil
}

// hostIn: h is one of the names, or under one of them.
func hostIn(h string, names ...string) bool {
	h = strings.ToLower(h)
	for _, n := range names {
		if h == n || strings.HasSuffix(h, "."+n) {
			return true
		}
	}
	return false
}
