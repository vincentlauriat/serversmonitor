package server

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

func (s *server) handleInstallScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript")
	w.Write(s.InstallScript)
}

// staticHandler serves the embedded SvelteKit export; unknown paths fall back
// to index.html so client-side routes survive a reload.
func (s *server) staticHandler() http.Handler {
	fileServer := http.FileServerFS(s.Static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		if f, err := s.Static.Open(p); err == nil {
			st, err := f.Stat()
			f.Close()
			if err == nil && !st.IsDir() {
				if strings.HasPrefix(p, "_app/immutable/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		data, err := fs.ReadFile(s.Static, "index.html")
		if err != nil {
			http.Error(w, "front not built", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})
}
