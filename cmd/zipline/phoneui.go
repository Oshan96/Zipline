package main

import (
	"io"
	"net/http"

	"zipline/web"
)

// phoneui.js is the window's side of phone control: the question "A phone wants to connect" and the
// networks and waiting phones in Options → Phone.
var phoneUIJS = web.PhoneUIJS

// phoneUIRoutes adds the script to the window's routes.
func (s *Server) phoneUIRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/phoneui.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		io.WriteString(w, phoneUIJS)
	})
}
