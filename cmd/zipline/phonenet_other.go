//go:build !windows

package main

import "runtime"

// systemNetworks: Linux asks NetworkManager which firewall zone each connection is in (nmcli, when
// it's installed). macOS has no such label, so it says nothing and every network counts as not Public.
func systemNetworks(ifs []phoneIf) map[string]netVerdict {
	if runtime.GOOS != "linux" {
		return nil
	}
	return nmVerdicts(ifs, cmdOut)
}
