// Package ui prints gitopsify's terminal output and runs its interactive
// prompts. Prompts are only used when stdin and stdout are terminals and
// --no-input is not set; everything else degrades to plain text.
package ui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"golang.org/x/term"
)

// ErrAborted is returned when the user cancels a prompt.
var ErrAborted = errors.New("aborted")

// UI writes results to Out and progress to Err.
type UI struct {
	Out, Err    io.Writer
	Interactive bool
	Color       bool
	Quiet       bool
}

// New returns a UI for the process's standard streams.
func New(noInput, noColor, quiet bool) *UI {
	tty := term.IsTerminal(int(os.Stdout.Fd()))
	return &UI{
		Out:         os.Stdout,
		Err:         os.Stderr,
		Interactive: !noInput && tty && term.IsTerminal(int(os.Stdin.Fd())),
		Color:       !noColor && os.Getenv("NO_COLOR") == "" && term.IsTerminal(int(os.Stderr.Fd())),
		Quiet:       quiet,
	}
}

var (
	styleDim     = lipgloss.NewStyle().Faint(true)
	styleOK      = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleWarn    = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleErr     = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)
	styleHeading = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("5"))
	styleBold    = lipgloss.NewStyle().Bold(true)
)

func (u *UI) style(s lipgloss.Style, text string) string {
	if !u.Color {
		return text
	}
	return s.Render(text)
}

// Step reports progress on stderr.
func (u *UI) Step(format string, args ...any) {
	if u.Quiet {
		return
	}
	fmt.Fprintln(u.Err, u.style(styleDim, "• "+fmt.Sprintf(format, args...)))
}

// Success reports a completed action on stderr.
func (u *UI) Success(format string, args ...any) {
	fmt.Fprintln(u.Err, u.style(styleOK, "✓ ")+fmt.Sprintf(format, args...))
}

// Warn reports a warning on stderr.
func (u *UI) Warn(format string, args ...any) {
	fmt.Fprintln(u.Err, u.style(styleWarn, "! ")+fmt.Sprintf(format, args...))
}

// Errorf reports an error on stderr.
func (u *UI) Errorf(format string, args ...any) {
	fmt.Fprintln(u.Err, u.style(styleErr, "✗ ")+fmt.Sprintf(format, args...))
}

// Heading prints a section heading on stdout.
func (u *UI) Heading(s string) {
	fmt.Fprintln(u.Out)
	fmt.Fprintln(u.Out, u.style(styleHeading, s))
}

// Println prints a line on stdout.
func (u *UI) Println(a ...any) { fmt.Fprintln(u.Out, a...) }

// Printf prints on stdout.
func (u *UI) Printf(format string, a ...any) { fmt.Fprintf(u.Out, format, a...) }

// Bold renders text in bold when colors are on.
func (u *UI) Bold(s string) string { return u.style(styleBold, s) }

// Dim renders text faint when colors are on.
func (u *UI) Dim(s string) string { return u.style(styleDim, s) }

// Colorize renders text by severity: "error", "warning", "ok".
func (u *UI) Colorize(severity, s string) string {
	switch severity {
	case "error":
		return u.style(styleErr, s)
	case "warning":
		return u.style(styleWarn, s)
	case "ok":
		return u.style(styleOK, s)
	}
	return s
}

// Table prints rows under headers on stdout: a bordered table on terminals,
// tab-aligned columns otherwise.
func (u *UI) Table(headers []string, rows [][]string) {
	if len(rows) == 0 {
		return
	}
	if !u.Color {
		w := tabwriter.NewWriter(u.Out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, strings.Join(headers, "\t"))
		for _, r := range rows {
			fmt.Fprintln(w, strings.Join(r, "\t"))
		}
		_ = w.Flush()
		return
	}
	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(styleDim).
		Headers(headers...).
		Rows(rows...).
		StyleFunc(func(row, _ int) lipgloss.Style {
			if row == table.HeaderRow {
				return styleBold.Padding(0, 1)
			}
			return lipgloss.NewStyle().Padding(0, 1)
		})
	_, _ = lipgloss.Fprintln(u.Out, t)
}

func (u *UI) form(groups ...*huh.Group) *huh.Form {
	return huh.NewForm(groups...).WithTheme(huh.ThemeFunc(huh.ThemeCharm))
}

func run(f *huh.Form) error {
	if err := f.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return ErrAborted
		}
		return err
	}
	return nil
}

// Confirm asks a yes/no question. Without a terminal it returns def.
func (u *UI) Confirm(title, description string, def bool) (bool, error) {
	if !u.Interactive {
		return def, nil
	}
	v := def
	err := run(u.form(huh.NewGroup(huh.NewConfirm().Title(title).Description(description).Affirmative("Yes").Negative("No").Value(&v))))
	return v, err
}

// Option is a choice in a prompt.
type Option struct {
	Label string
	Value string
}

// Select asks for one of the options.
func (u *UI) Select(title, description string, options []Option, value *string) error {
	opts := make([]huh.Option[string], 0, len(options))
	for _, o := range options {
		opts = append(opts, huh.NewOption(o.Label, o.Value))
	}
	return run(u.form(huh.NewGroup(huh.NewSelect[string]().Title(title).Description(description).Options(opts...).Value(value))))
}

// MultiSelect asks for any number of the options; selected holds the
// preselected values and receives the answer.
func (u *UI) MultiSelect(title, description string, options []Option, selected *[]string) error {
	pre := map[string]bool{}
	for _, s := range *selected {
		pre[s] = true
	}
	opts := make([]huh.Option[string], 0, len(options))
	for _, o := range options {
		opts = append(opts, huh.NewOption(o.Label, o.Value).Selected(pre[o.Value]))
	}
	height := len(opts) + 4
	if height > 20 {
		height = 20
	}
	return run(u.form(huh.NewGroup(huh.NewMultiSelect[string]().Title(title).Description(description).Options(opts...).Filterable(true).Height(height).Value(selected))))
}

// Input asks for a line of text.
func (u *UI) Input(title, description, placeholder string, value *string, validate func(string) error) error {
	in := huh.NewInput().Title(title).Description(description).Placeholder(placeholder).Value(value)
	if validate != nil {
		in = in.Validate(validate)
	}
	return run(u.form(huh.NewGroup(in)))
}
