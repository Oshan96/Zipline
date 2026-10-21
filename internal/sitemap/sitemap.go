// Package sitemap reads the sitemaps sites publish for crawlers.
package sitemap

import (
	"bufio"
	"compress/gzip"
	"encoding/xml"
	"io"
	"strings"
)

// Sitemaps: the list of pages (and sometimes pictures and videos) a site offers to crawlers.

const sitemapMax = 50 << 20 // (what the sitemap protocol allows a file to be, uncompressed)

// Read reads a sitemap file: an XML <urlset> (urls) or <sitemapindex> (subs, the sitemaps
// it points to), gzip-compressed or not, or a plain list of addresses. For the XML kinds the
// <loc> of a page and the <video:content_loc> of a video count as addresses.
func Read(r io.Reader) (urls, subs []string, err error) {
	br := bufio.NewReader(io.LimitReader(r, sitemapMax))
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return nil, nil, err
		}
		defer zr.Close()
		br = bufio.NewReader(io.LimitReader(zr, sitemapMax))
	}
	if head, _ := br.Peek(512); !strings.HasPrefix(strings.TrimSpace(string(head)), "<") {
		sc := bufio.NewScanner(br)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			if l := strings.TrimSpace(sc.Text()); strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://") {
				urls = append(urls, l)
			}
		}
		return urls, nil, nil
	}
	dec := xml.NewDecoder(br)
	dec.Strict = false
	dec.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }
	var stack []string
	for {
		tok, err := dec.Token()
		if err != nil {
			if err == io.EOF || len(urls)+len(subs) > 0 {
				return urls, subs, nil // (a file cut short still gives what came before)
			}
			return nil, nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			stack = append(stack, t.Name.Local)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			n := len(stack)
			if n == 0 {
				continue
			}
			s := strings.TrimSpace(string(t))
			if s == "" || len(urls)+len(subs) >= 100000 {
				continue
			}
			switch {
			case stack[n-1] == "loc" && n >= 2 && stack[n-2] == "sitemap":
				subs = append(subs, s)
			case stack[n-1] == "loc", stack[n-1] == "content_loc":
				urls = append(urls, s)
			}
		}
	}
}
