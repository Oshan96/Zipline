package main

import "strings"

// web/i18n.js holds the words and number formats for both pages: the message catalogue, t(), and the size,
// speed, time and date formats. The window page loads it from /i18n.js. The phone page can't: its
// Content-Security-Policy allows no external scripts (see phone.go), so the same file is pasted into its
// <script> at the page's i18n marker, where it stays one source of words for both.
const i18nMarker = "/*ZL_I18N*/"

func init() {
	phoneHTML = strings.Replace(phoneHTML, i18nMarker, i18nJS, 1)
}
