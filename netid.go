package main

import (
	"net"
	"strconv"
	"strings"
)

// Which network is this? Quotas belong to a network (the home broadband, a phone's hotspot), so
// Zipline tells them apart by something stable that's cheap to read: the default gateway's
// address together with its hardware (MAC) address, which stay the same for a router and differ
// from one network to the next. The name shown is the Wi-Fi name where the system gives it
// cheaply, otherwise the adapter's name. When none of this can be read (no gateway, a tunnel
// without one), the adapter is the network; failing that, everything counts as "this computer".

type netInfo struct {
	key  string // "gw:192.168.1.1/a4:2b:b0:12:34:56", "if:wg0" or "pc"
	name string // "Home Wi-Fi", "Ethernet · 192.168.1.1", "This computer"
}

var thisComputer = netInfo{key: "pc", name: "This computer"}

// netIdentify reads the current network (replaced in tests). It may run a small system command,
// so it's called every half a minute or when the network changes, never per byte.
var netIdentify = systemNetwork

// gatewayNet builds the network's identity from what the system told: the gateway's address and
// MAC, the adapter it's reached through and the Wi-Fi name, any of which may be empty.
func gatewayNet(gw, mac, ifName, ssid string) netInfo {
	gw, mac, ifName, ssid = strings.TrimSpace(gw), strings.ToLower(strings.TrimSpace(mac)), strings.TrimSpace(ifName), strings.TrimSpace(ssid)
	if mac == "00:00:00:00:00:00" || mac == "ff:ff:ff:ff:ff:ff" || strings.Contains(mac, "incomplete") {
		mac = ""
	}
	if ip := net.ParseIP(gw); ip == nil || ip.IsUnspecified() {
		gw = ""
	}
	var n netInfo
	switch {
	case gw != "" && mac != "":
		n.key = "gw:" + gw + "/" + mac
	case gw != "":
		n.key = "gw:" + gw
	case ifName != "":
		n.key = "if:" + ifName
	default:
		return thisComputer
	}
	switch {
	case ssid != "":
		n.name = ssid
	case ifName != "" && gw != "":
		n.name = ifName + " · " + gw
	case ifName != "":
		n.name = ifName
	default:
		n.name = "Network " + gw
	}
	return n
}

// parseProcRoute finds the default route's gateway and adapter in Linux's /proc/net/route
// (addresses there are hexadecimal, in the machine's byte order: little-endian).
func parseProcRoute(s string) (gw, ifName string) {
	best := -1
	lines := strings.Split(s, "\n")
	for _, line := range lines[min(1, len(lines)):] {
		f := strings.Fields(line)
		if len(f) < 8 || f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		flags, err1 := strconv.ParseUint(f[3], 16, 32)
		v, err2 := strconv.ParseUint(f[2], 16, 32)
		metric, err3 := strconv.Atoi(f[6])
		if err1 != nil || err2 != nil || err3 != nil || flags&0x3 != 0x3 || v == 0 { // up, via a gateway
			continue
		}
		if best >= 0 && metric >= best {
			continue
		}
		best = metric
		gw = net.IPv4(byte(v), byte(v>>8), byte(v>>16), byte(v>>24)).String()
		ifName = f[0]
	}
	return gw, ifName
}

// parseProcARP finds an address's hardware address in Linux's /proc/net/arp.
func parseProcARP(s, ip string) string {
	for _, line := range strings.Split(s, "\n") {
		if f := strings.Fields(line); len(f) >= 4 && f[0] == ip {
			return f[3]
		}
	}
	return ""
}
