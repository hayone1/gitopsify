package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
)

// uiHandler serves the built web app: its files, and index.html for every
// other path (the app routes in the browser).
type uiHandler struct {
	files fs.FS
	index []byte
	csp   string
}

var inlineScript = regexp.MustCompile(`(?s)<script(?:\s[^>]*)?>(.*?)</script>`)

func newUIHandler(files fs.FS) *uiHandler {
	h := &uiHandler{files: files}
	if files != nil {
		h.index, _ = fs.ReadFile(files, "index.html")
	}
	// Allow exactly the inline scripts index.html has (Nuxt's boot
	// configuration), by hash, instead of 'unsafe-inline'.
	var hashes []string
	for _, m := range inlineScript.FindAllSubmatch(h.index, -1) {
		if len(bytes.TrimSpace(m[1])) == 0 || bytes.Contains(m[0][:bytes.IndexByte(m[0], '>')], []byte("src=")) {
			continue
		}
		if bytes.Contains(m[0][:bytes.IndexByte(m[0], '>')], []byte(`type="application/json"`)) {
			continue // data, not script
		}
		sum := sha256.Sum256(m[1])
		hashes = append(hashes, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
	}
	h.csp = "default-src 'self'; script-src 'self' " + strings.Join(hashes, " ") +
		"; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; connect-src 'self'; " +
		"frame-ancestors 'none'; base-uri 'self'; form-action 'self'; object-src 'none'"
	return h
}

const noUI = `<!doctype html><html><head><meta charset="utf-8"><title>gitopsify</title></head>
<body style="font-family:system-ui;max-width:40rem;margin:4rem auto;line-height:1.5">
<h1>gitopsify</h1><p>This build of gitopsify has no web UI: it was built without <code>make ui</code>
(for example with <code>go install</code>). The API works at <code>/api/v1</code>.</p>
<p>Use a release binary or the container image for the web UI.</p></body></html>`

func (h *uiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Security-Policy", h.csp)
	if h.index == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(noUI))
		return
	}
	p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if p != "" && p != "index.html" {
		if f, err := h.files.Open(p); err == nil {
			st, err := f.Stat()
			_ = f.Close()
			if err == nil && !st.IsDir() {
				if strings.HasPrefix(p, "_nuxt/") {
					// Content-hashed names: cache for good.
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				} else {
					w.Header().Set("Cache-Control", "no-cache")
				}
				http.ServeFileFS(w, r, h.files, p)
				return
			}
		}
		if strings.HasPrefix(p, "_nuxt/") {
			http.NotFound(w, r) // a missing asset isn't a page
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(h.index)
}
