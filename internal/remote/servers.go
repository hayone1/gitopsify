// Package remote is the command line's side of a gitopsify server: the
// servers it signed in to, and a client of the server's API.
package remote

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// Environment variables that choose the server without a saved sign-in, for
// CI and CronJobs.
const (
	EnvServer = "GITOPSIFY_REMOTE"
	EnvToken  = "GITOPSIFY_REMOTE_TOKEN"
	// EnvFile overrides where sign-ins are saved.
	EnvFile = "GITOPSIFY_REMOTE_CONFIG"
)

// Server is a server the command line signed in to.
type Server struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// Token is a session token from a browser sign-in (gts_...) or an API
	// token (gtk_...).
	Token   string     `json:"token,omitempty"`
	User    string     `json:"user,omitempty"`
	Expires *time.Time `json:"expires,omitempty"`
	// CAFile is a PEM file the server's certificate is checked against.
	CAFile string `json:"caFile,omitempty"`
	// Insecure skips checking the server's certificate (test servers).
	Insecure bool `json:"insecureSkipTLSVerify,omitempty"`
}

// Expired reports whether the sign-in has expired.
func (s *Server) Expired() bool { return s.Expires != nil && time.Now().After(*s.Expires) }

// File is the saved sign-ins.
type File struct {
	Current string   `json:"current,omitempty"`
	Servers []Server `json:"servers"`
}

// Path is where sign-ins are saved: $GITOPSIFY_REMOTE_CONFIG, or
// servers.yaml in the user's configuration directory.
func Path() (string, error) {
	if p := os.Getenv(EnvFile); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "gitopsify", "servers.yaml"), nil
}

// Load reads the saved sign-ins; a missing file is none.
func Load() (*File, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return &File{}, nil
	}
	if err != nil {
		return nil, err
	}
	f := &File{}
	if err := yaml.UnmarshalStrict(b, f); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return f, nil
}

// Save writes the sign-ins, readable by this user only: they hold tokens.
func (f *File) Save() error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := yaml.Marshal(f)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".servers-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// Find returns the saved server with this name or URL.
func (f *File) Find(nameOrURL string) *Server {
	norm, _ := NormalizeURL(nameOrURL)
	for i := range f.Servers {
		if f.Servers[i].Name == nameOrURL || (norm != "" && f.Servers[i].URL == norm) {
			return &f.Servers[i]
		}
	}
	return nil
}

// Put saves a server, replacing the one with its name, and makes it current.
func (f *File) Put(s Server) {
	f.Current = s.Name
	for i := range f.Servers {
		if f.Servers[i].Name == s.Name {
			f.Servers[i] = s
			return
		}
	}
	f.Servers = append(f.Servers, s)
	slices.SortFunc(f.Servers, func(a, b Server) int { return strings.Compare(a.Name, b.Name) })
}

// Remove forgets a server; the current one is then none.
func (f *File) Remove(name string) bool {
	i := slices.IndexFunc(f.Servers, func(s Server) bool { return s.Name == name })
	if i < 0 {
		return false
	}
	f.Servers = slices.Delete(f.Servers, i, i+1)
	if f.Current == name {
		f.Current = ""
	}
	return true
}

// NormalizeURL checks a server address and returns it without a trailing
// slash, adding https:// when there is no scheme.
func NormalizeURL(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("no server address")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("server address %q: %w", s, err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("server address %q: want https://host[:port][/path]", s)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u.String(), nil
}

// NameOf is the name a server is saved under by default: its host, and
// path when it has one.
func NameOf(serverURL string) string {
	u, err := url.Parse(serverURL)
	if err != nil {
		return serverURL
	}
	return u.Host + u.Path
}

// Resolve picks the server a command talks to: the --remote flag (a saved
// name or a URL), then $GITOPSIFY_REMOTE, then the current sign-in.
// $GITOPSIFY_REMOTE_TOKEN is the token of the server $GITOPSIFY_REMOTE
// names, and of no other.
func Resolve(flag string) (*Server, error) {
	f, err := Load()
	if err != nil {
		return nil, err
	}
	envServer := os.Getenv(EnvServer)
	want := flag
	if want == "" {
		want = envServer
	}
	if want == "" {
		want = f.Current
	}
	token := ""
	if envServer != "" && os.Getenv(EnvToken) != "" {
		a, _ := NormalizeURL(want)
		b, _ := NormalizeURL(envServer)
		if want == envServer || (a != "" && a == b) {
			token = os.Getenv(EnvToken)
		}
	}
	if want == "" {
		return nil, errors.New("no gitopsify server: sign in with gitopsify login <url>, or set " + EnvServer + " and " + EnvToken)
	}
	var s Server
	if saved := f.Find(want); saved != nil {
		s = *saved
	} else {
		u, err := NormalizeURL(want)
		if err != nil {
			return nil, fmt.Errorf("%q is neither a server you signed in to nor an address: %w", want, err)
		}
		s = Server{Name: NameOf(u), URL: u}
	}
	if token != "" {
		s.Token, s.Expires = token, nil
	}
	if s.Token == "" {
		return nil, fmt.Errorf("not signed in to %s: run gitopsify login %s (or set %s)", s.URL, s.URL, EnvToken)
	}
	if s.Expired() {
		return nil, fmt.Errorf("your sign-in to %s expired: run gitopsify login %s", s.Name, s.Name)
	}
	return &s, nil
}
