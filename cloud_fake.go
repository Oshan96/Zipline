//go:build zlfake

package main

import (
	"os"
	"regexp"
)

// A build for trying the cloud services against local stand-ins (go build -tags zlfake): the
// services' addresses come from ZL_CLOUD_FAKE (a base address, each service under its own path).
// Release builds don't have this file.

func init() {
	base := os.Getenv("ZL_CLOUD_FAKE")
	if base == "" {
		return
	}
	gdriveBase, gdocsBase, gdriveWeb = base+"/gdrive", base+"/gdocs", base+"/gweb"
	gdriveKeyHosts = append(gdriveKeyHosts, "127.0.0.1")
	onedriveAPI, badgerURL = base+"/onedrive/v1.0", base+"/badger/token"
	megaAPI = base + "/mega"
	yandexAPI = base + "/yandex/v1/disk/public/resources"
	pcloudAPIs, pcloudScheme = []string{base + "/pcloud-us", base + "/pcloud-eu"}, "http://"
	mediafireWeb = base + "/mediafire"
	mfDirectRe = regexp.MustCompile("^" + regexp.QuoteMeta(base) + "/")
	wetransferWeb = base + "/wetransfer"
}
