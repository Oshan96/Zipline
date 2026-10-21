// Package web holds the pages Zipline serves: the window and the phone page, with the colours and
// words they share. They are built into the program, so it needs no files beside it.
package web

import _ "embed"

// IndexHTML is the window's page.
//
//go:embed index.html
var IndexHTML string

// AppJS is the window's script.
//
//go:embed app.js
var AppJS string

// I18nJS is the words and number formats both pages share.
//
//go:embed i18n.js
var I18nJS string

// ColorsCSS is the colour, focus-ring and motion tokens both pages share.
//
//go:embed colors.css
var ColorsCSS string

// PhoneHTML is the phone page.
//
//go:embed m.html
var PhoneHTML string

// PhoneUIJS is the window's side of phone control.
//
//go:embed phoneui.js
var PhoneUIJS string

// LockedHTML is the page the window's address shows to a browser that hasn't signed in.
//
//go:embed locked.html
var LockedHTML string

// ExtLinkJS is the window's side of the browser extension's link (Options → Browser).
//
//go:embed extlink.js
var ExtLinkJS string
