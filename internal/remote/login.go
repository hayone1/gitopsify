package remote

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/hayone1/gitopsify/internal/server"
)

// BrowserLogin signs in through the server's single sign-on in a browser.
type BrowserLogin struct {
	// Open opens the sign-in address in a browser; nil (or Paste) only
	// shows it.
	Open func(url string) error
	// Show tells the person what to do: the address, whether a code is to
	// be pasted, and the check code the browser shows too.
	Show func(url string, paste bool, check string)
	// Paste reads the code the server's page shows, for browsers that
	// can't reach this machine (SSH sessions, containers). When nil, the
	// browser hands the code to a listener on 127.0.0.1.
	Paste func() (string, error)
}

// Login runs a browser sign-in and returns the session it gets.
func (c *Client) Login(ctx context.Context, b BrowserLogin) (*Session, error) {
	verifier := oauth2.GenerateVerifier()
	state := randomState()
	challenge := oauth2.S256ChallengeFromVerifier(verifier)
	q := url.Values{"cli_challenge": {challenge}, "cli_state": {state}}

	codes := make(chan string, 1)
	errs := make(chan error, 1)
	if b.Paste == nil {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("listening for the browser: %w", err)
		}
		srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: callback(state, codes)}
		go func() { _ = srv.Serve(ln) }()
		defer func() {
			sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(sctx)
		}()
		q.Set("cli_port", strconv.Itoa(ln.Addr().(*net.TCPAddr).Port))
	}
	address := c.Server.URL + "/api/v1/auth/login?" + q.Encode()
	if b.Show != nil {
		b.Show(address, b.Paste != nil, server.CLICheckCode(challenge))
	}
	if b.Paste != nil {
		go func() {
			v, err := b.Paste()
			if err != nil {
				errs <- err
				return
			}
			code, got := parsePasted(v)
			if got != "" && got != state {
				errs <- errors.New("that code is from another sign-in; run gitopsify login again")
				return
			}
			codes <- code
		}()
	} else if b.Open != nil {
		if err := b.Open(address); err != nil {
			return nil, err
		}
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	select {
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, errors.New("the sign-in took longer than 10 minutes")
		}
		return nil, ctx.Err()
	case err := <-errs:
		return nil, err
	case code := <-codes:
		if code == "" {
			return nil, errors.New("no code: run gitopsify login again")
		}
		return c.Exchange(ctx, code, verifier)
	}
}

// parsePasted accepts what the page shows (code=...&state=...), or the code
// alone.
func parsePasted(v string) (code, state string) {
	v = strings.TrimSpace(v)
	if i := strings.Index(v, "#"); i >= 0 {
		v = v[i+1:]
	}
	if strings.Contains(v, "code=") {
		q, err := url.ParseQuery(v)
		if err == nil {
			return q.Get("code"), q.Get("state")
		}
	}
	return v, ""
}

// callback handles the browser's redirect to 127.0.0.1 with the code.
func callback(state string, codes chan<- string) http.Handler {
	var once sync.Once
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 || q.Get("code") == "" {
			http.Error(w, "This sign-in wasn't started by this gitopsify login. Run it again.", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		_, _ = fmt.Fprintf(w, page, html.EscapeString("Signed in. You can close this tab and go back to the terminal."))
		once.Do(func() { codes <- q.Get("code") })
	})
}

const page = `<!doctype html><html><head><meta charset="utf-8"><title>gitopsify</title>
<style>body{font:16px system-ui,sans-serif;display:grid;place-items:center;height:90vh;color:#161616}</style>
</head><body><p>%s</p></body></html>`

func randomState() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
