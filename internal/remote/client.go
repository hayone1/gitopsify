package remote

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hayone1/gitopsify/internal/server"
	"github.com/hayone1/gitopsify/internal/telemetry"
)

// Client calls a gitopsify server's API.
type Client struct {
	Server *Server
	http   *http.Client
	// stream has no overall timeout: a run's events last as long as it.
	stream    *http.Client
	userAgent string
}

// New returns a client of a server.
func New(s *Server, version string) (*Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if s.CAFile != "" || s.Insecure {
		cfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: s.Insecure} //nolint:gosec // asked for, for test servers
		if s.CAFile != "" {
			pem, err := os.ReadFile(s.CAFile)
			if err != nil {
				return nil, fmt.Errorf("the server's CA: %w", err)
			}
			pool, _ := x509.SystemCertPool()
			if pool == nil {
				pool = x509.NewCertPool()
			}
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("%s holds no PEM certificates", s.CAFile)
			}
			cfg.RootCAs = pool
		}
		tr.TLSClientConfig = cfg
	}
	rt := telemetry.Transport(tr)
	// Go keeps the Authorization header on a redirect to the same host,
	// even from https to http: refuse that.
	noDowngrade := func(r *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		if via[0].URL.Scheme == "https" && r.URL.Scheme != "https" {
			return fmt.Errorf("the server redirected from https to %s; not following", r.URL.Scheme)
		}
		return nil
	}
	return &Client{Server: s, http: &http.Client{Transport: rt, Timeout: 2 * time.Minute, CheckRedirect: noDowngrade},
		stream: &http.Client{Transport: rt, CheckRedirect: noDowngrade}, userAgent: "gitopsify/" + version}, nil
}

// Clean removes control characters (terminal escape sequences) from text
// the server sends, before it is printed: run logs relay what clusters and
// tools say.
func Clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' {
			return r
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, s)
}

// Error is an error the server answered with.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	switch e.Status {
	case http.StatusUnauthorized:
		return "the server doesn't accept your sign-in (" + e.Message + "): run gitopsify login again, or check $" + EnvToken
	}
	return e.Message
}

// IsNotFound reports whether err is the server's 404.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

func (c *Client) request(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Server.URL+path, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if c.Server.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Server.Token)
	}
	return req, nil
}

// call sends a request and decodes the JSON answer into out (when not nil).
func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	req, err := c.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := check(resp); err != nil {
		return err
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("reading the server's answer: %w", err)
	}
	return nil
}

// check turns an error answer into an *Error.
func check(resp *http.Response) error {
	if resp.StatusCode < 300 {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e struct {
		Error string `json:"error"`
	}
	msg := strings.TrimSpace(string(b))
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		msg = e.Error
	}
	if msg == "" || strings.HasPrefix(msg, "<") {
		msg = resp.Status
	}
	return &Error{Status: resp.StatusCode, Message: Clean(msg)}
}

// Info is what the server says about itself before sign-in.
type Info struct {
	Version  string `json:"version"`
	AuthMode string `json:"authMode"`
	Local    bool   `json:"local"`
}

// Info asks the server about itself.
func (c *Client) Info(ctx context.Context) (*Info, error) {
	var i Info
	if err := c.call(ctx, http.MethodGet, "/api/v1/info", nil, &i); err != nil {
		var e *Error
		if errors.As(err, &e) && e.Status == http.StatusNotFound {
			return nil, fmt.Errorf("%s doesn't look like a gitopsify server", c.Server.URL)
		}
		return nil, err
	}
	return &i, nil
}

// Me is who the caller is and what they may do.
type Me struct {
	User       server.User       `json:"user"`
	Role       string            `json:"role"`
	ServerRole string            `json:"serverRole"`
	Clusters   map[string]string `json:"clusters"`
	Tools      map[string]string `json:"tools"`
	AuthMode   string            `json:"authMode"`
	Version    string            `json:"version"`
	Expires    *time.Time        `json:"expires,omitempty"`
}

// Me asks who the caller is.
func (c *Client) Me(ctx context.Context) (*Me, error) {
	var m Me
	return &m, c.call(ctx, http.MethodGet, "/api/v1/me", nil, &m)
}

// Session is a command-line session from a browser sign-in.
type Session struct {
	Token   string      `json:"token"`
	Expires time.Time   `json:"expires"`
	User    server.User `json:"user"`
}

// Exchange turns a browser sign-in's code into a session.
func (c *Client) Exchange(ctx context.Context, code, verifier string) (*Session, error) {
	var s Session
	err := c.call(ctx, http.MethodPost, "/api/v1/auth/token", map[string]string{"code": code, "verifier": verifier}, &s)
	return &s, err
}

// RunRequest starts a run.
type RunRequest struct {
	Type         string          `json:"type"`
	Cluster      string          `json:"cluster,omitempty"`
	Migration    string          `json:"migration,omitempty"`
	Tool         string          `json:"tool,omitempty"`
	Export       string          `json:"export,omitempty"`
	Repo         *server.RunRepo `json:"repo,omitempty"`
	AllowPartial bool            `json:"allowPartial,omitempty"`
	Publish      *PublishRequest `json:"publish,omitempty"`
}

// PublishRequest pushes a run's output.
type PublishRequest struct {
	Message string `json:"message,omitempty"`
	Branch  string `json:"branch,omitempty"`
}

// StartRun starts a run.
func (c *Client) StartRun(ctx context.Context, req RunRequest) (*server.Run, error) {
	var r server.Run
	return &r, c.call(ctx, http.MethodPost, "/api/v1/runs", req, &r)
}

// RunFilter narrows ListRuns.
type RunFilter struct {
	Type, Cluster, Status, CreatedBy string
	Limit                            int
}

// ListRuns lists runs, newest first.
func (c *Client) ListRuns(ctx context.Context, f RunFilter) ([]server.Run, error) {
	q := url.Values{}
	for k, v := range map[string]string{"type": f.Type, "cluster": f.Cluster, "status": f.Status, "createdBy": f.CreatedBy} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	var runs []server.Run
	return runs, c.call(ctx, http.MethodGet, "/api/v1/runs?"+q.Encode(), nil, &runs)
}

// Run returns a run.
func (c *Client) Run(ctx context.Context, id string) (*server.Run, error) {
	var r server.Run
	return &r, c.call(ctx, http.MethodGet, "/api/v1/runs/"+url.PathEscape(id), nil, &r)
}

// CancelRun asks a run to stop.
func (c *Client) CancelRun(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodPost, "/api/v1/runs/"+url.PathEscape(id)+"/cancel", nil, nil)
}

// DeleteRun deletes a finished run and its files.
func (c *Client) DeleteRun(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodDelete, "/api/v1/runs/"+url.PathEscape(id), nil, nil)
}

// Publish pushes a run's output.
func (c *Client) Publish(ctx context.Context, id string, req PublishRequest) (*server.Run, error) {
	var r server.Run
	// Pushes can take a while: the server allows ten minutes.
	ctx, cancel := context.WithTimeout(ctx, 11*time.Minute)
	defer cancel()
	req2, err := c.request(ctx, http.MethodPost, "/api/v1/runs/"+url.PathEscape(id)+"/publish", req)
	if err != nil {
		return nil, err
	}
	resp, err := c.stream.Do(req2)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := check(resp); err != nil {
		return nil, err
	}
	return &r, json.NewDecoder(resp.Body).Decode(&r)
}

// Log returns a run's log.
func (c *Client) Log(ctx context.Context, id string) ([]server.LogLine, error) {
	var lines []server.LogLine
	return lines, c.call(ctx, http.MethodGet, "/api/v1/runs/"+url.PathEscape(id)+"/log", nil, &lines)
}

// Archive returns the run's files as a tar.gz stream.
func (c *Client) Archive(ctx context.Context, id string) (io.ReadCloser, error) {
	req, err := c.request(ctx, http.MethodGet, "/api/v1/runs/"+url.PathEscape(id)+"/archive", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.stream.Do(req)
	if err != nil {
		return nil, err
	}
	if err := check(resp); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp.Body, nil
}

// Follow streams a run's log lines (from the first, or after the last
// one seen when it reconnects) and its updates until it ends. It
// reconnects when the connection drops, as browsers do.
func (c *Client) Follow(ctx context.Context, id string, onLog func(server.LogLine), onRun func(*server.Run)) (*server.Run, error) {
	last := -1
	var final *server.Run
	failures := 0
	for {
		got := false
		ended, err := c.follow(ctx, id, &last, func(l server.LogLine) {
			got = true
			if onLog != nil {
				onLog(l)
			}
		}, func(r *server.Run) {
			got = true
			final = r
			if onRun != nil {
				onRun(r)
			}
		})
		if ended {
			return final, nil
		}
		if got {
			failures = 0 // a proxy that cuts long streams isn't a failure
		}
		if ctx.Err() != nil {
			return final, ctx.Err()
		}
		var e *Error
		if errors.As(err, &e) && e.Status < 500 {
			return final, err
		}
		if failures++; failures > 10 {
			return final, err
		}
		select {
		case <-ctx.Done():
			return final, ctx.Err()
		case <-time.After(time.Duration(min(failures, 5)) * time.Second):
		}
	}
}

func (c *Client) follow(ctx context.Context, id string, last *int, onLog func(server.LogLine), onRun func(*server.Run)) (bool, error) {
	req, err := c.request(ctx, http.MethodGet, "/api/v1/runs/"+url.PathEscape(id)+"/events", nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if *last >= 0 {
		req.Header.Set("Last-Event-ID", strconv.Itoa(*last))
	}
	resp, err := c.stream.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if err := check(resp); err != nil {
		return false, err
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	var event, data, eid string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			switch event {
			case "log":
				var l server.LogLine
				if json.Unmarshal([]byte(data), &l) == nil {
					if n, err := strconv.Atoi(eid); err == nil {
						*last = n
					}
					if onLog != nil {
						onLog(l)
					}
				}
			case "run":
				var r server.Run
				if json.Unmarshal([]byte(data), &r) == nil {
					onRun(&r)
				}
			case "end":
				return true, nil
			}
			event, data, eid = "", "", ""
		case strings.HasPrefix(line, ":"):
		default:
			k, v, _ := strings.Cut(line, ":")
			v = strings.TrimPrefix(v, " ")
			switch k {
			case "event":
				event = v
			case "data":
				if len(data)+len(v) > 16<<20 {
					return false, errors.New("an event from the server is too large")
				}
				if data != "" {
					data += "\n"
				}
				data += v
			case "id":
				eid = v
			}
		}
	}
	if err := sc.Err(); err != nil {
		return false, err
	}
	return false, io.ErrUnexpectedEOF
}
