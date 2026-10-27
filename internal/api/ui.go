package api

import (
	"embed"
	"io/fs"
	"net/http"
)

// Painel web estático, embutido no binário. Ele consome a própria API
// (/health e /events) e não carrega nada de origens externas.
//
//go:embed ui
var uiFiles embed.FS

func uiHandler() http.Handler {
	sub, err := fs.Sub(uiFiles, "ui")
	if err != nil {
		panic(err)
	}
	files := http.StripPrefix("/ui/", http.FileServerFS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy",
			"default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; "+
				"base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}
