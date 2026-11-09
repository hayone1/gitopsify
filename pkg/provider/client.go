package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hayone1/gitopsify/sdk"
)

// LogLine is one progress line a provider wrote to stderr.
type LogLine struct {
	// Level is DEBUG, INFO, WARN or ERROR; plain text lines are DEBUG.
	Level   string
	Message string
	// Attrs are the line's other fields, such as partition or kind.
	Attrs map[string]string
}

// String renders the message with its attributes.
func (l LogLine) String() string {
	if len(l.Attrs) == 0 {
		return l.Message
	}
	keys := make([]string, 0, len(l.Attrs))
	for k := range l.Attrs {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var b strings.Builder
	b.WriteString(l.Message)
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%s", k, l.Attrs[k])
	}
	return b.String()
}

// Client runs one provider executable.
type Client struct {
	Name string
	Path string
	// Log receives the provider's progress; nil drops it.
	Log func(LogLine)
	// Env is the provider's environment; nil passes this process's.
	Env []string
}

// Error is a failure of a provider call.
type Error struct {
	Provider string
	Method   string
	// Err is a *sdk.Error when the provider reported the failure itself.
	Err error
	// Stderr holds the last lines the provider wrote that weren't JSON logs.
	Stderr []string
}

func (e *Error) Error() string {
	var se *sdk.Error
	if errors.As(e.Err, &se) {
		return fmt.Sprintf("%s: %s", e.Provider, se.Error())
	}
	msg := fmt.Sprintf("%s %s: %v", e.Provider, e.Method, e.Err)
	if len(e.Stderr) > 0 {
		msg += "\n  " + strings.Join(e.Stderr, "\n  ")
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// Code returns the provider's error code, or sdk.CodeInternal.
func (e *Error) Code() string {
	var se *sdk.Error
	if errors.As(e.Err, &se) {
		return se.Code
	}
	return sdk.CodeInternal
}

// Describe runs describe and checks that the provider speaks this
// protocol.
func (c *Client) Describe(ctx context.Context) (*sdk.Description, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var d *sdk.Description
	err := c.run(ctx, sdk.MethodDescribe, nil, func(r io.Reader) error {
		var err error
		d, err = sdk.ReadDescription(r)
		return err
	})
	if err != nil {
		return nil, err
	}
	if !slices.Contains(d.Protocols, sdk.Protocol) {
		return nil, &Error{Provider: c.label(), Method: sdk.MethodDescribe, Err: fmt.Errorf(
			"it speaks protocol %s and this gitopsify speaks %s; install a gitopsify or a provider version that match",
			strings.Join(d.Protocols, ", "), sdk.Protocol)}
	}
	if d.Name != c.Name && c.Name != "" {
		return nil, &Error{Provider: c.label(), Method: sdk.MethodDescribe, Err: fmt.Errorf("it calls itself %q", d.Name)}
	}
	return d, nil
}

// Check runs check.
func (c *Client) Check(ctx context.Context, req sdk.Request) (*sdk.CheckResult, error) {
	var res *sdk.CheckResult
	err := c.run(ctx, sdk.MethodCheck, &req, func(r io.Reader) error {
		var err error
		res, err = sdk.ReadCheck(r)
		return err
	})
	return res, err
}

// Discover runs discover; onResource, when not nil, is called for every
// resource as it arrives.
func (c *Client) Discover(ctx context.Context, req sdk.Request, onResource func(sdk.Resource)) (*sdk.Inventory, error) {
	var inv *sdk.Inventory
	err := c.run(ctx, sdk.MethodDiscover, &req, func(r io.Reader) error {
		var err error
		inv, err = sdk.ReadInventory(r, onResource)
		return err
	})
	return inv, err
}

func (c *Client) label() string { return Prefix + c.Name }

func (c *Client) run(ctx context.Context, method string, req *sdk.Request, read func(io.Reader) error) error {
	fail := func(err error, stderr []string) error {
		return &Error{Provider: c.label(), Method: method, Err: err, Stderr: stderr}
	}
	cmd := exec.CommandContext(ctx, c.Path, method)
	// Let the provider stop cleanly first; kill it if it doesn't.
	if runtime.GOOS != "windows" {
		cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	}
	cmd.WaitDelay = 5 * time.Second
	if c.Env != nil {
		cmd.Env = c.Env
	}
	if req != nil {
		r := *req
		if r.APIVersion == "" {
			r.APIVersion = sdk.APIVersion
		}
		b, err := json.Marshal(r)
		if err != nil {
			return fail(err, nil)
		}
		cmd.Stdin = bytes.NewReader(b)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fail(err, nil)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fail(err, nil)
	}
	if err := cmd.Start(); err != nil {
		return fail(fmt.Errorf("can't run %s: %w", c.Path, err), nil)
	}

	tail := &tailLines{max: 20}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.readStderr(stderr, tail)
	}()

	readErr := read(&capped{r: stdout, left: MaxAnswer})
	// Drain what is left so the provider never blocks on a full pipe, but
	// don't wait long for one that keeps writing or doesn't exit.
	drained := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, stdout)
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		// A child of the provider may still hold the pipes: close our ends.
		_ = stdout.Close()
		_ = stderr.Close()
		<-drained
	}
	// A child of the provider (a credential helper, a port-forward) can
	// hold stderr open after the provider exits: stop reading it then.
	stderrDone := make(chan struct{})
	go func() { wg.Wait(); close(stderrDone) }()
	select {
	case <-stderrDone:
	case <-time.After(5 * time.Second):
		_ = stderr.Close()
		<-stderrDone
	}
	waitErr := cmd.Wait()

	if ctx.Err() != nil {
		return fail(ctx.Err(), tail.lines())
	}
	var se *sdk.Error
	if errors.As(readErr, &se) {
		return fail(se, tail.lines())
	}
	if readErr != nil {
		if waitErr != nil {
			return fail(fmt.Errorf("%w (%v)", readErr, waitErr), tail.lines())
		}
		return fail(readErr, tail.lines())
	}
	if waitErr != nil {
		return fail(fmt.Errorf("finished its answer but exited with %v", waitErr), tail.lines())
	}
	return nil
}

func (c *Client) readStderr(r io.Reader, tail *tailLines) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		var fields map[string]any
		if len(line) > 0 && line[0] == '{' && json.Unmarshal(line, &fields) == nil {
			if msg, ok := fields["msg"].(string); ok {
				ll := LogLine{Level: strings.ToUpper(fmt.Sprint(fields["level"])), Message: msg, Attrs: map[string]string{}}
				for k, v := range fields {
					switch k {
					case "msg", "level", "time":
						continue
					}
					if s := fmt.Sprint(v); s != "" {
						ll.Attrs[k] = s
					}
				}
				if ll.Level == "ERROR" {
					tail.add(ll.String())
				}
				if c.Log != nil {
					c.Log(ll)
				}
				continue
			}
		}
		text := string(line)
		tail.add(text)
		if c.Log != nil && strings.TrimSpace(text) != "" {
			c.Log(LogLine{Level: "DEBUG", Message: text})
		}
	}
	if sc.Err() != nil {
		tail.add("(a line on stderr was too long to show)")
	}
	// Keep reading whatever comes, so the provider never blocks on stderr.
	_, _ = io.Copy(io.Discard, r)
}

// MaxAnswer caps how much a provider may write to stdout in one answer.
const MaxAnswer = 1 << 30

// capped fails reads past a limit, so a runaway provider can't fill memory
// forever.
type capped struct {
	r    io.Reader
	left int64
}

func (c *capped) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, fmt.Errorf("the provider's answer is larger than %d MiB", MaxAnswer>>20)
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

type tailLines struct {
	mu  sync.Mutex
	max int
	buf []string
}

func (t *tailLines) add(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, s)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
}

func (t *tailLines) lines() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.buf...)
}
