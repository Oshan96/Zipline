//go:build windows

package main

import (
	"fmt"
	"net"
	"unsafe"
)

// Windows: the route Windows would use to reach the internet gives the gateway and the adapter,
// and the gateway's hardware address comes from the neighbour (ARP) cache. The adapter's name
// ("Wi-Fi", "Ethernet") is shown.

var (
	pGetBestRoute = iphlpapi.NewProc("GetBestRoute")
	pSendARP      = iphlpapi.NewProc("SendARP")
)

type mibIPForwardRow struct {
	Dest, Mask, Policy, NextHop, IfIndex, Type, Proto, Age, NextHopAS uint32
	Metric1, Metric2, Metric3, Metric4, Metric5                       uint32
}

func systemNetwork() netInfo {
	if pGetBestRoute.Find() != nil {
		return thisComputer
	}
	var row mibIPForwardRow
	dest := uint32(1) | 1<<8 | 1<<16 | 1<<24 // 1.1.1.1 (network byte order reads the same)
	if r, _, _ := pGetBestRoute.Call(uintptr(dest), 0, uintptr(unsafe.Pointer(&row))); r != 0 {
		return thisComputer
	}
	ifName := ""
	if i, err := net.InterfaceByIndex(int(row.IfIndex)); err == nil {
		ifName = i.Name
	}
	gw, mac := "", ""
	if v := row.NextHop; v != 0 {
		gw = net.IPv4(byte(v), byte(v>>8), byte(v>>16), byte(v>>24)).String()
		if pSendARP.Find() == nil {
			var hw [8]byte
			n := uint32(len(hw))
			if r, _, _ := pSendARP.Call(uintptr(v), 0, uintptr(unsafe.Pointer(&hw[0])), uintptr(unsafe.Pointer(&n))); r == 0 && n == 6 {
				mac = fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", hw[0], hw[1], hw[2], hw[3], hw[4], hw[5])
			}
		}
	}
	return gatewayNet(gw, mac, ifName, "")
}
