// Package ui renders the terminal interface — the welcome banner, the input
// prompt box, and the agent's reply (prose and tool calls) — in agentic-go-cli's
// own visual identity: Go's brand cyan as the accent, tool verbs colour-coded by
// kind, and a continuous left rail that binds one reply's events into a Turn.
package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"golang.org/x/term"
)

// out routes every styled write through a colorprofile writer, which downsamples
// truecolor → 256 → 16 → plain to match the terminal and honours NO_COLOR and
// non-interactive output. So render.go styles at full fidelity and lets the
// writer degrade gracefully. (The raw-mode input editor in input.go must NOT use
// this writer — it strips the cursor-position escapes the editor relies on.)
var out io.Writer = colorprofile.NewWriter(os.Stdout, os.Environ())

func emit(s string) { fmt.Fprint(out, s) }

// The palette. Go's own brand cyan is the program's identity; tool verbs are
// colour-coded by kind so a glance down the rail reads as a sequence of actions.
var (
	cAccent = lipgloss.Color("#00ADD8") // Go cyan — assistant, prompt, title
	cRead   = lipgloss.Color("#56C2E6") // read: a lighter cyan (a live reference)
	cEdit   = lipgloss.Color("#E0A94E") // edit: amber (a write changes the tree)
	cRun    = lipgloss.Color("#C58AF0") // run: violet (executes a command)
	cErr    = lipgloss.Color("#F2787C") // errors
	cRail   = lipgloss.Color("#5A6373") // the rail, branches, and box borders
	cMeta   = lipgloss.Color("#8A93A0") // paths, counts, hints
)

var (
	railStyle  = lipgloss.NewStyle().Foreground(cRail)
	metaStyle  = lipgloss.NewStyle().Foreground(cMeta)
	errStyle   = lipgloss.NewStyle().Foreground(cErr)
	asstMarker = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	titleStyle = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
)

// The glyph system. The rail (│) connects a reply's events; the assistant speaks
// at a diamond (◆); a tool call branches off the rail (├─); its output hangs
// under it (⎿). glyphPrompt is consumed by the input editor in input.go.
const (
	glyphAsst   = "◆"
	glyphRail   = "│"
	glyphBranch = "├─"
	glyphResult = "⎿"
	glyphPrompt = "›"
)

// isTTY reports whether stdout is an interactive terminal. When it isn't (output
// piped to a file, or the test harness), the framed UI degrades to plain lines.
// It is a var so tests can force rich rendering without a real terminal.
var isTTY = func() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// termWidth is the terminal's column count, falling back to 80 off a TTY. A var
// so tests can pin a deterministic width.
var termWidth = func() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		return 80
	}
	return w
}

// boxWidth is the total width of the framed UI boxes: the terminal width less a
// one-column right margin, which keeps the right border off the last column
// (where a trailing write can trigger an unwanted line wrap).
func boxWidth() int {
	w := termWidth()
	if w >= 8 {
		w--
	}
	return w
}

// railOpen tracks whether the current reply's rail has started. The first event
// of a reply opens it with no leading connector; later events emit a bare rail
// line first so they stay visually linked. closeRail (called by ReadInput at each
// Turn boundary) ends the rail so the next reply starts fresh.
var railOpen bool

func openRail() {
	if !isTTY() {
		return
	}
	if railOpen {
		emit(" " + railStyle.Render(glyphRail) + "\n")
	}
	railOpen = true
}

func closeRail() { railOpen = false }

// ManifestEntry is one collapsed Turn shown in the Manifest panel: its number and
// the one-line Description that stands in for it.
type ManifestEntry struct {
	Turn int
	Desc string
}

// manifest is the collapsed Turns shown in the panel below the input box, oldest
// first. SetManifest is called by the agent before each prompt.
var manifest []ManifestEntry

// SetManifest records the Manifest (collapsed Turns) rendered in the panel.
func SetManifest(entries []ManifestEntry) { manifest = entries }

// PrintWelcome prints the rounded welcome banner shown once at startup. The same
// diamond the assistant speaks at opens it, so the banner reads as the agent's
// first word rather than chrome.
func PrintWelcome(model, cwd string) {
	if !isTTY() {
		emit(fmt.Sprintf("agentic-go-cli — %s\n", model))
		return
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(cRail).
		Padding(0, 1).
		Width(boxWidth())
	body := lipgloss.JoinVertical(lipgloss.Left,
		asstMarker.Render(glyphAsst)+" "+titleStyle.Render("agentic-go-cli"),
		"",
		metaStyle.Render(model),
		metaStyle.Render(cwd),
	)
	emit(box.Render(body) + "\n")
}

// PrintHint prints a single dim status line (e.g. resume notices).
func PrintHint(s string) {
	emit(metaStyle.Render(s) + "\n")
}

// PrintAssistant prints a block of assistant prose. The diamond marks the first
// line; continuation lines ride the rail so multi-paragraph replies stay bound.
func PrintAssistant(text string) {
	openRail()
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, ln := range lines {
		if i == 0 {
			emit(" " + asstMarker.Render(glyphAsst) + "  " + ln + "\n")
		} else {
			emit(" " + railStyle.Render(glyphRail) + "  " + ln + "\n")
		}
	}
}

// PrintToolCall renders a tool invocation as a branch off the rail: the verb
// colour-coded by kind, then its primary target (a path or command). Secondary
// arguments — notably an edit's content — hang under the rail beneath it.
//
//	├─ read   main.go
//	├─ edit   hello.txt
//	│  content:
//	│    hello
func PrintToolCall(name string, input []byte) {
	openRail()

	var args map[string]any
	_ = json.Unmarshal(input, &args)
	target, rest := splitArgs(name, args)

	line := " " + verbStyle(name).Render(glyphBranch+" "+name)
	if target != "" {
		// Pad the verb to a fixed field so targets line up in a column, reading
		// down the rail as "verb → what". Tool names are ASCII, so len is width.
		const verbField = 6
		pad := max(0, verbField-len(name))
		line += strings.Repeat(" ", pad) + "   " + metaStyle.Render(target)
	}
	emit(line + "\n")

	for _, k := range rest {
		v := fmt.Sprint(args[k])
		if strings.Contains(v, "\n") {
			emit(" " + railStyle.Render(glyphRail) + "  " + metaStyle.Render(k+":") + "\n")
			for _, ln := range strings.Split(strings.TrimRight(v, "\n"), "\n") {
				emit(" " + railStyle.Render(glyphRail) + "    " + ln + "\n")
			}
		} else {
			emit(" " + railStyle.Render(glyphRail) + "  " + metaStyle.Render(k+": ") + v + "\n")
		}
	}
}

// PrintToolResult prints the tool's output verbatim under the ⎿ branch, riding
// the rail, red on error.
func PrintToolResult(response string, isError bool) {
	// A result always follows its own call, so the rail is already open — don't
	// open it again here, or a stray connector lands between call and output.
	lines := strings.Split(strings.TrimRight(response, "\n"), "\n")
	for i, ln := range lines {
		var gutter string
		if i == 0 {
			gutter = " " + railStyle.Render(glyphRail) + "  " + railStyle.Render(glyphResult) + " "
		} else {
			gutter = " " + railStyle.Render(glyphRail) + "    "
		}
		if isError {
			emit(gutter + errStyle.Render(ln) + "\n")
		} else {
			emit(gutter + ln + "\n")
		}
	}
}

// verbStyle colours a tool verb by kind: read is cyan (a live reference), edit
// amber (a write), run violet (an execution), everything else the accent.
func verbStyle(name string) lipgloss.Style {
	c := cAccent
	switch name {
	case "read":
		c = cRead
	case "edit":
		c = cEdit
	case "run":
		c = cRun
	}
	return lipgloss.NewStyle().Foreground(c).Bold(true)
}

// splitArgs separates a tool's primary target (the path or command shown on the
// branch line) from its remaining arguments (sorted, shown under the rail).
func splitArgs(name string, args map[string]any) (target string, rest []string) {
	pk := primaryKey(name)
	for k := range args {
		if k != pk {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	if pk != "" {
		if v, ok := args[pk]; ok {
			target = fmt.Sprint(v)
		}
	}
	return target, rest
}

// primaryKey is the argument shown inline on a tool's branch line.
func primaryKey(name string) string {
	switch name {
	case "read", "edit":
		return "path"
	case "run":
		return "command"
	case "forget", "describe":
		return "ref"
	}
	return ""
}
