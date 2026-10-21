package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// UPnP IGD port mapping: an SSDP search finds the router, its device description names the
// WANIPConnection (or WANPPPConnection) service, and SOAP calls on that add and remove mappings.

// lanClient talks to the router: never through a proxy.
var lanClient = &http.Client{Timeout: 6 * time.Second, Transport: &http.Transport{
	Proxy: nil, DialContext: (&net.Dialer{Timeout: 4 * time.Second}).DialContext, DisableKeepAlives: true}}

type upnpIGD struct {
	ctrl     []upnpService // the connection services, best first
	using    int           // the one that worked
	local    netip.Addr    // this computer, as the router sees it
	location string
}

type upnpService struct{ typ, url string }

// upnpErr is a SOAP fault (UPnP's error codes: 718 the port is taken, 725 only permanent leases).
type upnpErr struct {
	code int
	desc string
}

func (e *upnpErr) Error() string { return fmt.Sprintf("UPnP error %d %s", e.code, e.desc) }

// discoverIGD searches for the router and reads its description. src is the address that
// answered the search, even when its description was no use.
func discoverIGD(ctx context.Context, ssdp string, wait time.Duration) (igd *upnpIGD, src netip.Addr, err error) {
	c, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return nil, src, err
	}
	defer c.Close()
	to, err := net.ResolveUDPAddr("udp4", ssdp)
	if err != nil {
		return nil, src, err
	}
	search := func() {
		for _, st := range []string{"urn:schemas-upnp-org:device:InternetGatewayDevice:1", "urn:schemas-upnp-org:device:InternetGatewayDevice:2"} {
			c.WriteToUDP([]byte("M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 2\r\nST: "+st+"\r\n\r\n"), to)
		}
	}
	search()
	start := time.Now()
	end := start.Add(wait)
	if d, ok := ctx.Deadline(); ok && d.Before(end) {
		end = d
	}
	resent := false
	seen := map[string]bool{}
	err = errPMNoAnswer
	buf := make([]byte, 4096)
	for {
		dl := end
		if !resent { // UDP: ask a second time after a second
			if dl = start.Add(time.Second); dl.After(end) {
				dl = end
			}
		}
		c.SetReadDeadline(dl)
		n, from, rerr := c.ReadFromUDPAddrPort(buf)
		if rerr != nil {
			if !resent && time.Now().Before(end) && ctx.Err() == nil {
				resent = true
				search()
				continue
			}
			return nil, src, err
		}
		resp, perr := http.ReadResponse(bufio.NewReader(bytes.NewReader(buf[:n])), nil)
		if perr != nil || resp.StatusCode != 200 {
			continue
		}
		loc := resp.Header.Get("Location")
		lu, perr := url.Parse(loc)
		if perr != nil || seen[loc] || lu.Scheme != "http" {
			continue
		}
		seen[loc] = true
		from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
		if !src.IsValid() {
			src = from.Addr()
		}
		// only a description on the device that answered, or on this network (never one elsewhere)
		if host, perr := netip.ParseAddr(lu.Hostname()); perr != nil || host.Unmap() != from.Addr() && !host.IsPrivate() && !host.IsLoopback() && !host.IsLinkLocalUnicast() {
			continue
		}
		if igd, err = fetchIGD(ctx, loc); err == nil {
			return igd, src, nil
		}
	}
}

type upnpDevice struct {
	Services []struct {
		Type string `xml:"serviceType"`
		URL  string `xml:"controlURL"`
	} `xml:"serviceList>service"`
	Devices []upnpDevice `xml:"deviceList>device"`
}

// fetchIGD reads a router's description for its connection services.
func fetchIGD(ctx context.Context, loc string) (*upnpIGD, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", loc, nil)
	if err != nil {
		return nil, err
	}
	resp, err := lanClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("the router's description: %s", resp.Status)
	}
	var root struct {
		URLBase string     `xml:"URLBase"`
		Device  upnpDevice `xml:"device"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 512<<10)).Decode(&root); err != nil {
		return nil, fmt.Errorf("the router's description: %w", err)
	}
	base, _ := url.Parse(loc)
	if root.URLBase != "" {
		if b, err := url.Parse(root.URLBase); err == nil && b.Scheme == "http" && b.Host == base.Host {
			base = b
		}
	}
	igd := &upnpIGD{location: loc}
	rank := map[string]int{"WANIPConnection:2": 1, "WANIPConnection:1": 0, "WANPPPConnection:1": 2}
	var walk func(d upnpDevice)
	walk = func(d upnpDevice) {
		for _, s := range d.Services {
			k, ok := strings.CutPrefix(s.Type, "urn:schemas-upnp-org:service:")
			if _, known := rank[k]; !ok || !known {
				continue
			}
			if u, err := base.Parse(strings.TrimSpace(s.URL)); err == nil && u.Host == base.Host {
				igd.ctrl = append(igd.ctrl, upnpService{s.Type, u.String()})
			}
		}
		for _, x := range d.Devices {
			walk(x)
		}
	}
	walk(root.Device)
	if len(igd.ctrl) == 0 {
		return nil, errors.New("the router has no port mapping service")
	}
	for i := 1; i < len(igd.ctrl); i++ { // (a stable sort: a few entries)
		for j := i; j > 0 && rank[strings.TrimPrefix(igd.ctrl[j].typ, "urn:schemas-upnp-org:service:")] < rank[strings.TrimPrefix(igd.ctrl[j-1].typ, "urn:schemas-upnp-org:service:")]; j-- {
			igd.ctrl[j], igd.ctrl[j-1] = igd.ctrl[j-1], igd.ctrl[j]
		}
	}
	host := base.Host
	if base.Port() == "" {
		host = net.JoinHostPort(base.Hostname(), "80")
	}
	ap, err := netip.ParseAddrPort(host)
	if err != nil {
		return nil, err
	}
	if igd.local, err = localAddrTowards(ap); err != nil {
		return nil, err
	}
	return igd, nil
}

// soap calls an action on a service and returns the reply's values by name.
func (g *upnpIGD) soap(ctx context.Context, svc upnpService, action string, args [][2]string) (map[string]string, error) {
	var body strings.Builder
	body.WriteString(`<?xml version="1.0"?>` + "\r\n" + `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	fmt.Fprintf(&body, `<u:%s xmlns:u="%s">`, action, svc.typ)
	for _, a := range args {
		body.WriteString("<" + a[0] + ">")
		xml.EscapeText(&body, []byte(a[1]))
		body.WriteString("</" + a[0] + ">")
	}
	fmt.Fprintf(&body, `</u:%s></s:Body></s:Envelope>`, action)
	req, err := http.NewRequestWithContext(ctx, "POST", svc.url, strings.NewReader(body.String()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"`+svc.typ+"#"+action+`"`)
	resp, err := lanClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	vals := map[string]string{}
	dec := xml.NewDecoder(io.LimitReader(resp.Body, 64<<10))
	var name string
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch x := tok.(type) {
		case xml.StartElement:
			name = x.Name.Local
		case xml.CharData:
			if name != "" {
				vals[name] += string(x)
			}
		case xml.EndElement:
			name = ""
		}
	}
	if resp.StatusCode != 200 {
		if code, err := strconv.Atoi(strings.TrimSpace(vals["errorCode"])); err == nil {
			return nil, &upnpErr{code, strings.TrimSpace(vals["errorDescription"])}
		}
		return nil, fmt.Errorf("the router replied %s", resp.Status)
	}
	return vals, nil
}

// addOne asks for one mapping and returns the lease it got.
func (g *upnpIGD) addOne(ctx context.Context, svc upnpService, port int, proto string, lease int) (int, error) {
	_, err := g.soap(ctx, svc, "AddPortMapping", [][2]string{{"NewRemoteHost", ""}, {"NewExternalPort", strconv.Itoa(port)}, {"NewProtocol", proto},
		{"NewInternalPort", strconv.Itoa(port)}, {"NewInternalClient", g.local.String()}, {"NewEnabled", "1"},
		{"NewPortMappingDescription", "Zipline"}, {"NewLeaseDuration", strconv.Itoa(lease)}})
	var ue *upnpErr
	if errors.As(err, &ue) && ue.code == 725 && lease != 0 { // OnlyPermanentLeasesSupported: as libtorrent, ask for one
		return g.addOne(ctx, svc, port, proto, 0)
	}
	return lease, err
}

// add maps the port for TCP (and UDP) with an hour's lease, or a permanent one where the router
// only has those, and reads the router's address outside.
func (g *upnpIGD) add(ctx context.Context, port int, udp bool) (pmResult, error) {
	protos := []string{"TCP"}
	if udp {
		protos = append(protos, "UDP")
	}
	var err error
	var ue *upnpErr
	for i := g.using; i < len(g.ctrl); i++ {
		svc := g.ctrl[i]
		lease := int(pmLease / time.Second)
		for _, proto := range protos {
			if lease, err = g.addOne(ctx, svc, port, proto, lease); err != nil {
				break
			}
		}
		if err == nil {
			g.using = i
			res := pmResult{via: "UPnP", ext: port, lease: time.Duration(lease) * time.Second}
			if v, err := g.soap(ctx, svc, "GetExternalIPAddress", nil); err == nil {
				res.external, _ = netip.ParseAddr(strings.TrimSpace(v["NewExternalIPAddress"]))
			}
			if lease != 0 {
				res.lease = g.leaseOf(ctx, svc, port, res.lease)
			}
			return res, nil
		}
		if !errors.As(err, &ue) || ue.code != 401 && ue.code != 602 { // a real no (not "no such action here")
			break
		}
	}
	if errors.As(err, &ue) {
		return pmResult{}, &pmRefused{msg: strings.TrimSpace(strconv.Itoa(ue.code) + " " + ue.desc), conflict: ue.code == 718}
	}
	return pmResult{}, err
}

// leaseOf is the lease the router really gave (some shorten it, some make it permanent).
func (g *upnpIGD) leaseOf(ctx context.Context, svc upnpService, port int, asked time.Duration) time.Duration {
	v, err := g.soap(ctx, svc, "GetSpecificPortMappingEntry", [][2]string{{"NewRemoteHost", ""}, {"NewExternalPort", strconv.Itoa(port)}, {"NewProtocol", "TCP"}})
	if err != nil {
		return asked
	}
	if n, err := strconv.Atoi(strings.TrimSpace(v["NewLeaseDuration"])); err == nil && n >= 0 && time.Duration(n)*time.Second <= asked {
		return time.Duration(n) * time.Second
	}
	return asked
}

func (g *upnpIGD) del(ctx context.Context, port int, udp bool) {
	protos := []string{"TCP"}
	if udp {
		protos = append(protos, "UDP")
	}
	for _, proto := range protos {
		g.soap(ctx, g.ctrl[g.using], "DeletePortMapping", [][2]string{{"NewRemoteHost", ""}, {"NewExternalPort", strconv.Itoa(port)}, {"NewProtocol", proto}})
	}
}
