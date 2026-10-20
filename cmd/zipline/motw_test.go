package main

import (
	"encoding/hex"
	"testing"
)

func TestOriginURL(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"https://user:secret@example.com/a/file.iso?x=1#part2", "https://example.com/a/file.iso?x=1"},
		{"ftp://anon:me%40mail@ftp.example.org/pub/f.zip", "ftp://ftp.example.org/pub/f.zip"},
		{"HTTP://Example.com/x", "http://Example.com/x"},
		{"magnet:?xt=urn:btih:abc", ""},
		{"data:text/plain,hi", ""},
		{"file:///home/me/x", ""},
		{"", ""},
		{"https://bad\nurl/", ""},
	} {
		got, ok := originURL(c.in)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("originURL(%q) = %q %v, want %q", c.in, got, ok, c.want)
		}
	}
}

// J2: the stream is byte for byte what Chrome and Edge write; credentials never go in it.
func TestZoneIdentifier(t *testing.T) {
	for _, c := range []struct{ src, ref, want string }{
		{"https://u:p@dl.example.com/setup.exe", "https://example.com/download",
			"[ZoneTransfer]\r\nZoneId=3\r\nReferrerUrl=https://example.com/download\r\nHostUrl=https://dl.example.com/setup.exe\r\n"},
		{"https://dl.example.com/setup.exe", "",
			"[ZoneTransfer]\r\nZoneId=3\r\nHostUrl=https://dl.example.com/setup.exe\r\n"},
		{"magnet:?xt=urn:btih:abc&dn=x", "",
			"[ZoneTransfer]\r\nZoneId=3\r\nHostUrl=about:internet\r\n"},
		{"magnet:?xt=urn:btih:abc", "https://tracker:pw@site.example/t/1",
			"[ZoneTransfer]\r\nZoneId=3\r\nReferrerUrl=https://site.example/t/1\r\nHostUrl=about:internet\r\n"},
	} {
		if got := zoneIdentifier(c.src, c.ref); got != c.want {
			t.Errorf("zoneIdentifier(%q, %q)\n got %q\nwant %q", c.src, c.ref, got, c.want)
		}
	}
}

// The "Where from" property list matches what Python's plistlib writes for the same list.
func TestWhereFromsPlist(t *testing.T) {
	for _, c := range []struct {
		urls []string
		want string
	}{
		{[]string{"https://example.com/files/ubuntu-24.04-desktop-amd64.iso", "https://example.com/downloads"},
			"62706c6973743030a201025f103868747470733a2f2f6578616d706c652e636f6d2f66696c65732f7562756e74752d32342e30342d6465736b746f702d616d6436342e69736f5f101d68747470733a2f2f6578616d706c652e636f6d2f646f776e6c6f616473080b460000000000000101000000000000000300000000000000000000000000000066"},
		{[]string{"http://a/b"},
			"62706c6973743030a1015a687474703a2f2f612f62080a0000000000000101000000000000000200000000000000000000000000000015"},
		{[]string{"https://例え.jp/ファイル.zip", "https://ex.com/"},
			"62706c6973743030a201026f101600680074007400700073003a002f002f4f8b3048002e006a0070002f30d530a130a430eb002e007a006900705f100f68747470733a2f2f65782e636f6d2f080b3a000000000000010100000000000000030000000000000000000000000000004c"},
	} {
		if got := hex.EncodeToString(whereFroms(c.urls)); got != c.want {
			t.Errorf("whereFroms(%q)\n got %s\nwant %s", c.urls, got, c.want)
		}
	}
	// long enough for two-byte lengths and offsets
	long := "https://example.com/" + string(make([]byte, 300))
	b := whereFroms([]string{long})
	if b[8] != 0xa1 || b[10] != 0x5f || b[11] != 0x11 || b[len(b)-26] != 2 {
		t.Fatalf("long string: % x", b[:14])
	}
}
