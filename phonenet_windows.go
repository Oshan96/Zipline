//go:build windows

package main

import (
	"context"
	"os/exec"
	"time"
)

// systemNetworks: Windows marks each network Public, Private or Domain. The label is read with
// PowerShell's Get-NetConnectionProfile (hidden, with a timeout), which names the adapter each
// profile belongs to. The registry (NetworkList\Profiles) also holds the labels, but not which
// profile is the live one, so it can't say which adapter is on a Public network. If PowerShell can't
// answer (older Windows, a locked-down machine, a timeout), nothing is said and every network counts
// as not Public.
func systemNetworks(ifs []phoneIf) map[string]netVerdict {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	script := `Get-NetConnectionProfile | ForEach-Object { '{0}|{1}|{2}' -f $_.InterfaceIndex, $_.NetworkCategory, [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes([string]$_.Name)) }`
	out, err := hidden(exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)).Output()
	if err != nil {
		return nil
	}
	return winVerdicts(ifs, parseWinProfiles(string(out)))
}
