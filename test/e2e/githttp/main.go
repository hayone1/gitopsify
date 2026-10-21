// githttp serves the bare repositories under a directory over Git's smart
// HTTP protocol (clone, fetch and push) for end-to-end tests.
package main

import (
	"flag"
	"log"
	"net/http"
	"net/http/cgi"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	root := flag.String("root", ".", "directory holding bare repositories")
	addr := flag.String("addr", "0.0.0.0:8099", "listen address")
	flag.Parse()
	h := &cgi.Handler{
		Path: backend(),
		Env:  []string{"GIT_PROJECT_ROOT=" + *root, "GIT_HTTP_EXPORT_ALL=1"},
	}
	log.Printf("serving %s on %s", *root, *addr)
	log.Fatal(http.ListenAndServe(*addr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL)
		h.ServeHTTP(w, r)
	})))
}

// backend locates git-http-backend, which ships with git.
func backend() string {
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		log.Fatalf("locating git: %v", err)
	}
	return filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
}
