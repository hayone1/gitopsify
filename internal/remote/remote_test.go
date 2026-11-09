package remote

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hayone1/gitopsify/internal/server"
)

func TestNormalizeURL(t *testing.T) {
	for in, want := range map[string]string{
		"gitopsify.acme.io":              "https://gitopsify.acme.io",
		"https://gitopsify.acme.io/":     "https://gitopsify.acme.io",
		"http://127.0.0.1:8080/sub/":     "http://127.0.0.1:8080/sub",
		"ftp://x":                        "",
		"https://user:pw@gitopsify.io":   "",
		"https://gitopsify.io/?token=1":  "",
		"":                               "",
		"https://gitopsify.acme.io#frag": "",
	} {
		got, err := NormalizeURL(in)
		if want == "" {
			if err == nil {
				t.Errorf("%q: accepted as %q", in, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
}

func TestResolve(t *testing.T) {
	t.Setenv(EnvFile, filepath.Join(t.TempDir(), "servers.yaml"))
	t.Setenv(EnvServer, "")
	t.Setenv(EnvToken, "")
	if _, err := Resolve(""); err == nil || !strings.Contains(err.Error(), "gitopsify login") {
		t.Errorf("nothing saved: %v", err)
	}
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	f := &File{}
	f.Put(Server{Name: "old", URL: "https://old.acme.io", Token: "gts_a", Expires: &past})
	f.Put(Server{Name: "prod", URL: "https://prod.acme.io", Token: "gts_b", Expires: &future})
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
	if s, err := Resolve(""); err != nil || s.Name != "prod" {
		t.Errorf("current: %+v %v", s, err)
	}
	if s, err := Resolve("https://prod.acme.io/"); err != nil || s.Name != "prod" {
		t.Errorf("by address: %+v %v", s, err)
	}
	if _, err := Resolve("old"); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("expired: %v", err)
	}
	if _, err := Resolve("new.acme.io"); err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Errorf("unknown server without a token: %v", err)
	}
	t.Setenv(EnvServer, "ci.acme.io")
	t.Setenv(EnvToken, "gtk_ci")
	if s, err := Resolve(""); err != nil || s.URL != "https://ci.acme.io" || s.Token != "gtk_ci" {
		t.Errorf("from the environment: %+v %v", s, err)
	}
	// The token goes to the server the variable names, no other.
	if s, err := Resolve("prod"); err != nil || s.Token != "gts_b" {
		t.Errorf("--remote prod with the CI variables set: %+v %v", s, err)
	}
	if _, err := Resolve("new.acme.io"); err == nil {
		t.Error("the CI token went to another server")
	}
	g, _ := Load()
	if !g.Remove("old") || g.Remove("old") || len(g.Servers) != 1 {
		t.Errorf("remove: %+v", g)
	}
}

func TestParsePasted(t *testing.T) {
	for in, want := range map[string][2]string{
		"abc\n":                                 {"abc", ""},
		"code=abc&state=s1":                     {"abc", "s1"},
		"https://x/cli-login#code=abc&state=s1": {"abc", "s1"},
	} {
		code, state := parsePasted(in)
		if code != want[0] || state != want[1] {
			t.Errorf("%q: %q %q", in, code, state)
		}
	}
}

// TestFollow: a dropped event stream is resumed after the last log line.
func TestFollow(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"sign in first"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		switch calls.Add(1) {
		case 1:
			fmt.Fprint(w, "retry: 3000\n\nid: 0\nevent: log\ndata: {\"message\":\"one\"}\n\nid: 1\nevent: log\ndata: {\"message\":\"two\"}\n\n")
			// The connection drops without an end.
		default:
			if got := r.Header.Get("Last-Event-ID"); got != "1" {
				t.Errorf("resumed from %q", got)
			}
			fmt.Fprint(w, "id: 2\nevent: log\ndata: {\"message\":\"three\"}\n\n: keep-alive\n\nevent: run\ndata: {\"id\":\"r1\",\"status\":\"succeeded\"}\n\nevent: end\ndata: {}\n\n")
		}
	}))
	defer srv.Close()
	c, err := New(&Server{URL: srv.URL, Token: "tok"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	run, err := c.Follow(context.Background(), "r1", func(l server.LogLine) { got = append(got, l.Message) }, nil)
	if err != nil || run == nil || run.Status != "succeeded" {
		t.Fatalf("follow: %+v %v", run, err)
	}
	if strings.Join(got, ",") != "one,two,three" {
		t.Errorf("lines: %v", got)
	}
	c.Server.Token = "wrong"
	if _, err := c.Follow(context.Background(), "r1", nil, nil); err == nil || !strings.Contains(err.Error(), "gitopsify login") {
		t.Errorf("unauthorized: %v", err)
	}
}
