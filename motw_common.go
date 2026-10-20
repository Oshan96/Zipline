package main

import (
	"bytes"
	"encoding/binary"
	"net/url"
	"strings"
	"unicode/utf16"
)

// Mark of the Web: finished downloads are labelled with where they came from, as browsers
// do, so Windows SmartScreen, Office's Protected View and macOS Gatekeeper treat them as
// coming from the internet, and file managers can show the source.

// originURL is a download's address made fit to store with the file: no user name,
// password or fragment. ok is false for anything but a web or FTP address (a magnet link,
// a data: URL, a local file).
func originURL(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "ftp", "ftps":
	default:
		return "", false
	}
	u.User = nil
	u.Fragment, u.RawFragment = "", ""
	return u.String(), true
}

// zoneIdentifier is the Zone.Identifier stream Chrome and Edge write: zone 3 (Internet),
// then where the file came from. Torrents and other non-web sources get about:internet, so
// they're still treated as coming from the internet.
func zoneIdentifier(srcURL, referrer string) string {
	s := "[ZoneTransfer]\r\nZoneId=3\r\n"
	if ref, ok := originURL(referrer); ok {
		s += "ReferrerUrl=" + ref + "\r\n"
	}
	host, ok := originURL(srcURL)
	if !ok {
		host = "about:internet"
	}
	return s + "HostUrl=" + host + "\r\n"
}

// whereFroms encodes urls as a binary property list holding an array of strings: what macOS
// keeps in com.apple.metadata:kMDItemWhereFroms (Finder's "Where from"). Object 0 is the
// array, 1… its strings.
func whereFroms(urls []string) []byte {
	b := bytes.NewBufferString("bplist00")
	offsets := []int{b.Len()}
	plistMarker(b, 0xa0, len(urls))
	for i := range urls {
		b.WriteByte(byte(i + 1)) // one-byte references: far fewer than 256 objects
	}
	for _, u := range urls {
		offsets = append(offsets, b.Len())
		if isASCII(u) {
			plistMarker(b, 0x50, len(u))
			b.WriteString(u)
			continue
		}
		units := utf16.Encode([]rune(u))
		plistMarker(b, 0x60, len(units))
		for _, c := range units {
			b.Write(binary.BigEndian.AppendUint16(nil, c))
		}
	}
	table := b.Len()
	size := 1 // bytes per offset: enough for the largest one
	for table>>(8*size) > 0 {
		size *= 2
	}
	for _, o := range offsets {
		b.Write(binary.BigEndian.AppendUint64(nil, uint64(o))[8-size:])
	}
	b.Write(make([]byte, 6)) // trailer: unused, sort version
	b.WriteByte(byte(size))
	b.WriteByte(1) // reference size
	b.Write(binary.BigEndian.AppendUint64(nil, uint64(len(offsets))))
	b.Write(binary.BigEndian.AppendUint64(nil, 0)) // the top object
	b.Write(binary.BigEndian.AppendUint64(nil, uint64(table)))
	return b.Bytes()
}

// plistMarker writes an object's type and length: in its low 4 bits if under 15, otherwise
// as an integer object after it.
func plistMarker(b *bytes.Buffer, kind byte, n int) {
	switch {
	case n < 15:
		b.WriteByte(kind | byte(n))
	case n <= 0xff:
		b.Write([]byte{kind | 0x0f, 0x10, byte(n)})
	case n <= 0xffff:
		b.Write(binary.BigEndian.AppendUint16([]byte{kind | 0x0f, 0x11}, uint16(n)))
	default:
		b.Write(binary.BigEndian.AppendUint32([]byte{kind | 0x0f, 0x12}, uint32(n)))
	}
}
