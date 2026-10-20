//go:build !windows

package main

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// systemNetwork: on Linux the kernel's route and neighbour tables (no commands needed), with the
// Wi-Fi or NetworkManager connection name if a tool for it is installed; on macOS the route, arp
// and networksetup commands.
func systemNetwork() netInfo {
	switch runtime.GOOS {
	case "linux":
		route, err := os.ReadFile("/proc/net/route")
		if err != nil {
			return thisComputer
		}
		gw, ifName := parseProcRoute(string(route))
		mac := ""
		if arp, err := os.ReadFile("/proc/net/arp"); err == nil && gw != "" {
			mac = parseProcARP(string(arp), gw)
		}
		ssid := ""
		if ifName != "" {
			if ssid = cmdOut("iwgetid", "-r", ifName); ssid == "" {
				ssid = cmdOut("nmcli", "-t", "-g", "GENERAL.CONNECTION", "device", "show", ifName)
			}
		}
		return gatewayNet(gw, mac, ifName, ssid)
	case "darwin":
		var gw, ifName string
		for _, line := range strings.Split(cmdOut("route", "-n", "get", "default"), "\n") {
			k, v, _ := strings.Cut(strings.TrimSpace(line), ":")
			switch k {
			case "gateway":
				gw = strings.TrimSpace(v)
			case "interface":
				ifName = strings.TrimSpace(v)
			}
		}
		mac := ""
		if gw != "" { // "? (192.168.1.1) at a4:2b:b0:12:34:56 on en0 ifscope [ethernet]"
			if _, rest, ok := strings.Cut(cmdOut("arp", "-n", gw), " at "); ok {
				mac, _, _ = strings.Cut(rest, " ")
			}
		}
		ssid := ""
		if ifName != "" {
			if _, v, ok := strings.Cut(cmdOut("networksetup", "-getairportnetwork", ifName), "Network: "); ok {
				ssid = v
			}
		}
		return gatewayNet(gw, mac, ifName, ssid)
	}
	return thisComputer
}

// cmdOut runs a small system command and returns its trimmed output ("" if it isn't there or fails).
func cmdOut(name string, args ...string) string {
	bin, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
