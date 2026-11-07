package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
)

// Provider is a gitopsify provider written in Go.
type Provider interface {
	// Describe says what the provider reads and produces. It must not
	// connect to anything.
	Describe() Description
	// Check connects with req.Settings and says what it is connected to,
	// which partitions it sees and which kinds it can read.
	Check(ctx context.Context, req Request) (*CheckResult, error)
	// Discover reads the partitions and resources in req.Scope and sends
	// them to out as it finds them. Something that can't be read (one
	// organization forbids alert rules) is a warning, not an error.
	Discover(ctx context.Context, req Request, out Emitter) error
}

// Emitter receives what Discover finds.
type Emitter interface {
	Partition(Partition) error
	Resource(Resource) error
	Skip(Skipped) error
	Warn(Warning) error
}

type loggerKey struct{}

// Logger returns the logger Serve and Run put in the context: JSON lines on
// stderr that gitopsify shows as progress. Info messages are progress, Warn
// and Error messages are shown as warnings, Debug messages only when
// GITOPSIFY_DEBUG is set. Report what couldn't be read with Emitter.Warn,
// not with the logger: only warnings sent there mark a scope incomplete.
func Logger(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

// Serve runs the provider as a process: it reads the method from the command
// line and the request from stdin, answers on stdout, and exits. Call it from
// main.
//
// The answer goes to the real stdout; os.Stdout is pointed at stderr, so a
// stray fmt.Println (in the provider or a library) can't corrupt it.
func Serve(p Provider) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	out := os.Stdout
	os.Stdout = os.Stderr
	code := Run(ctx, p, os.Args[1:], os.Stdin, out, os.Stderr)
	stop()
	os.Exit(code)
}

// Run is Serve without the process: it answers one call and returns the exit
// code. Tests use it directly.
func Run(ctx context.Context, p Provider, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	logger := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx = context.WithValue(ctx, loggerKey{}, logger)
	w := &writer{enc: json.NewEncoder(stdout)}
	w.enc.SetEscapeHTML(false)

	if len(args) != 1 {
		usage(p, stderr)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		usage(p, stdout)
		return 0
	case "-v", "--version", "version":
		d := describe(p)
		fmt.Fprintf(stdout, "%s %s (protocol %s)\n", "gitopsify-provider-"+d.Name, d.Version, strings.Join(d.Protocols, ", "))
		return 0
	case MethodDescribe, MethodCheck, MethodDiscover:
	default:
		fmt.Fprintf(stderr, "unknown method %q\n", args[0])
		usage(p, stderr)
		return 2
	}

	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic", "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
				err = Errorf(CodeInternal, "the provider crashed: %v", r).WithHint("this is a bug in the provider; please report it with the log above")
			}
		}()
		return call(ctx, p, args[0], stdin, w)
	}()
	if err != nil {
		e := AsError(err)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			e = &Error{Code: CodeCanceled, Message: err.Error()}
		}
		_ = w.write(Message{Error: e})
		return 1
	}
	if err := w.write(Message{Done: &struct{}{}}); err != nil {
		fmt.Fprintln(stderr, "writing the answer:", err)
		return 1
	}
	return 0
}

func call(ctx context.Context, p Provider, method string, stdin io.Reader, w *writer) error {
	if method == MethodDescribe {
		d := describe(p)
		return w.write(Message{Description: &d})
	}
	req, err := readRequest(stdin)
	if err != nil {
		return err
	}
	switch method {
	case MethodCheck:
		res, err := p.Check(ctx, req)
		if err != nil {
			return err
		}
		if res == nil {
			res = &CheckResult{}
		}
		return w.write(Message{Check: res})
	default:
		return p.Discover(ctx, req, &emitter{w: w, ctx: ctx})
	}
}

func describe(p Provider) Description {
	d := p.Describe()
	if d.APIVersion == "" {
		d.APIVersion = APIVersion
	}
	if len(d.Protocols) == 0 {
		d.Protocols = []string{Protocol}
	}
	return d
}

func readRequest(r io.Reader) (Request, error) {
	var req Request
	d := json.NewDecoder(r)
	if err := d.Decode(&req); err != nil {
		if errors.Is(err, io.EOF) {
			return req, Errorf(CodeInvalidSettings, "no request on stdin").WithHint("gitopsify sends the request; to run the provider by hand, pipe a JSON request into it")
		}
		return req, Errorf(CodeInvalidSettings, "reading the request: %v", err)
	}
	if req.APIVersion != "" && req.APIVersion != APIVersion {
		return req, Errorf(CodeUnsupported, "request uses %s; this provider speaks %s", req.APIVersion, APIVersion).
			WithHint("upgrade gitopsify or the provider so they speak the same protocol")
	}
	return req, nil
}

func usage(p Provider, out io.Writer) {
	d := describe(p)
	fmt.Fprintf(out, `gitopsify-provider-%[1]s %[2]s: a gitopsify provider for %[3]s.

gitopsify runs it; you don't need to. To use it:

  gitopsify export %[1]s

Methods (for debugging):
  gitopsify-provider-%[1]s describe            what it reads and produces
  gitopsify-provider-%[1]s check    < req.json  connect and list %[4]s
  gitopsify-provider-%[1]s discover < req.json  read everything in scope
`, d.Name, d.Version, d.Title, d.Partition.Plural)
}

// writer writes messages one per line, safely from several goroutines.
type writer struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func (w *writer) write(m Message) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.enc.Encode(m)
}

type emitter struct {
	w   *writer
	ctx context.Context
}

func (e *emitter) Partition(p Partition) error {
	if p.ID == "" {
		return Errorf(CodeInternal, "partition %q has no ID", p.Name)
	}
	return e.send(Message{Partition: &p})
}

func (e *emitter) Resource(r Resource) error {
	if r.Key == "" || r.Kind == "" {
		return Errorf(CodeInternal, "resource %q needs a key and a kind", r.Name)
	}
	return e.send(Message{Resource: &r})
}

func (e *emitter) Skip(s Skipped) error {
	Logger(e.ctx).Debug("skipped", "kind", s.Kind, "name", s.Name, "reason", s.Reason)
	return e.send(Message{Skipped: &s})
}

func (e *emitter) Warn(w Warning) error {
	Logger(e.ctx).Debug("warning: "+w.Message, "partition", w.Partition, "kind", w.Kind)
	return e.send(Message{Warning: &w})
}

func (e *emitter) send(m Message) error {
	if err := e.ctx.Err(); err != nil {
		return err
	}
	return e.w.write(m)
}

// MarshalJSON encodes a resource without escaping <, > and &.
func (r Resource) MarshalJSON() ([]byte, error) {
	type plain Resource
	return marshal(plain(r))
}
