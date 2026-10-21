package sitemap

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
)

func gz(t *testing.T, s string) []byte {
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	zw.Write([]byte(s))
	zw.Close()
	return b.Bytes()
}

func TestReadSitemap(t *testing.T) {
	read := func(in []byte) (urls, subs []string) {
		t.Helper()
		urls, subs, err := Read(bytes.NewReader(in))
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	urlset := `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9" xmlns:image="http://www.google.com/schemas/sitemap-image/1.1" xmlns:video="http://www.google.com/schemas/sitemap-video/1.1">
 <url><loc>http://h/a.html</loc><lastmod>2024-01-01</lastmod>
  <image:image><image:loc>http://h/img/p.jpg</image:loc></image:image>
  <video:video><video:thumbnail_loc>http://h/t.jpg</video:thumbnail_loc><video:content_loc>http://h/v.mp4</video:content_loc></video:video></url>
 <url><loc>
   http://h/b.html
 </loc></url>
</urlset>`
	urls, subs := read([]byte(urlset))
	if strings.Join(urls, " ") != "http://h/a.html http://h/img/p.jpg http://h/v.mp4 http://h/b.html" || len(subs) != 0 {
		t.Fatalf("urlset: %v %v", urls, subs)
	}
	index := `<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"><sitemap><loc>http://h/s1.xml</loc></sitemap><sitemap><loc>http://h/s2.xml.gz</loc></sitemap></sitemapindex>`
	urls, subs = read([]byte(index))
	if len(urls) != 0 || strings.Join(subs, " ") != "http://h/s1.xml http://h/s2.xml.gz" {
		t.Fatalf("index: %v %v", urls, subs)
	}
	urls, subs = read(gz(t, urlset)) // .xml.gz
	if len(urls) != 4 || len(subs) != 0 {
		t.Fatalf("gzip: %v %v", urls, subs)
	}
	urls, _ = read([]byte("# my pages\nhttp://h/x.html\nhttps://h/y.html\nnot a link\n")) // the plain-text form
	if strings.Join(urls, " ") != "http://h/x.html https://h/y.html" {
		t.Fatalf("text: %v", urls)
	}
	urls, _ = read([]byte(`<urlset><url><loc>http://h/cut.html</loc></url><url><loc>http://h/cu`)) // cut short
	if len(urls) == 0 || urls[0] != "http://h/cut.html" {
		t.Fatalf("a cut file still gives what came before: %v", urls)
	}
}
