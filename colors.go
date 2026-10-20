package main

import (
	_ "embed"
	"strings"
)

// colors.css holds the colour, focus-ring and motion tokens for both pages: the window (index.html)
// and the phone page (m.html). It is pasted into each page's <style>, at the page's colours marker,
// rather than linked: the phone page's Content-Security-Policy allows no external styles, and a
// pasted block can't arrive late, so there's no flash of the wrong colours either.
//
//go:embed web/colors.css
var colorsCSS string

const colorsMarker = "/*ZL_COLORS*/"

func withColors(page string) string { return strings.Replace(page, colorsMarker, colorsCSS, 1) }

func init() {
	indexHTML = withColors(indexHTML)
	phoneHTML = withColors(phoneHTML)
}
