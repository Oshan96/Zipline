//go:build !windows

package main

import "os"

// systemCountry guesses where the computer is: the time zone's country (TZ, /etc/timezone or the
// zone /etc/localtime points to, looked up in zone.tab) — many people keep an en_US locale wherever
// they live, but set their clock to the local time — else the territory of LC_ALL or LANG.
func systemCountry() string {
	zones := []string{os.Getenv("TZ")}
	if b, err := os.ReadFile("/etc/timezone"); err == nil {
		zones = append(zones, string(b))
	}
	if l, err := os.Readlink("/etc/localtime"); err == nil {
		zones = append(zones, l)
	}
	for _, z := range zones {
		if c := zoneCountry(z); c != "" {
			return c
		}
	}
	for _, k := range []string{"LC_ALL", "LANG"} {
		if c := localeCountry(os.Getenv(k)); c != "" {
			return c
		}
	}
	return ""
}
