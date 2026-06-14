package ui

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// capture runs fn with the UI globals swapped for deterministic rendering — the
// output redirected to a buffer, the TTY check and width pinned, and the rail and
// Working Set state reset — restoring everything afterwards. Lip Gloss always
// renders truecolor into the buffer, so callers strip it (layout) or keep it
// (palette). These tests mutate package globals, so none may run in parallel.
func capture(t *testing.T, tty bool, width int, fn func()) string {
	t.Helper()
	prevOut, prevTTY, prevWidth := out, isTTY, termWidth
	t.Cleanup(func() {
		out, isTTY, termWidth = prevOut, prevTTY, prevWidth
		railOpen, manifestCount = false, 0
	})
	var buf bytes.Buffer
	out = &buf
	isTTY = func() bool { return tty }
	termWidth = func() int { return width }
	railOpen, manifestCount = false, 0
	fn()
	return buf.String()
}

// frame captures fn under a TTY at the given width, records a screenshot of the
// rendered output (see shoot), and returns the raw coloured frame.
func frame(t *testing.T, width int, fn func()) string {
	t.Helper()
	raw := capture(t, true, width, fn)
	shoot(t, raw)
	return raw
}

// richLines renders a frame, strips colour, and splits into lines (trailing
// newline dropped) — the form layout assertions compare.
func richLines(t *testing.T, width int, fn func()) []string {
	t.Helper()
	return strings.Split(strings.TrimRight(stripANSI(frame(t, width, fn)), "\n"), "\n")
}

// stripANSI removes ANSI escape sequences (SGR colour and cursor control alike)
// so layout can be asserted independent of styling.
func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case inEsc:
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
		case r == '\x1b':
			inEsc = true
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func TestPrintAssistant(t *testing.T) {
	tests := []struct {
		name string
		give string
		want []string
	}{
		{name: "single line", give: "Hello there.", want: []string{" ◆  Hello there."}},
		{name: "multi line rides the rail", give: "first\nsecond", want: []string{" ◆  first", " │  second"}},
		{name: "trailing newline trimmed", give: "only\n", want: []string{" ◆  only"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := richLines(t, 80, func() { PrintAssistant(tt.give) })
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("PrintAssistant(%q) mismatch (-want +got):\n%s", tt.give, diff)
			}
		})
	}
}

func TestPrintToolCall(t *testing.T) {
	tests := []struct {
		name  string
		tool  string
		input string
		want  []string
	}{
		{name: "read shows its path", tool: "read", input: `{"path":"main.go"}`,
			want: []string{" ├─ read     main.go"}},
		{name: "run shows its command", tool: "run", input: `{"command":"go test ./..."}`,
			want: []string{" ├─ run      go test ./..."}},
		{name: "forget shows its ref", tool: "forget", input: `{"ref":"#3"}`,
			want: []string{" ├─ forget   #3"}},
		{name: "edit hangs content under the rail", tool: "edit", input: `{"path":"hello.txt","content":"line a\nline b"}`,
			want: []string{
				" ├─ edit     hello.txt",
				" │  content:",
				" │    line a",
				" │    line b",
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := richLines(t, 80, func() { PrintToolCall(tt.tool, []byte(tt.input)) })
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("PrintToolCall(%q, %s) mismatch (-want +got):\n%s", tt.tool, tt.input, diff)
			}
		})
	}
}

// TestToolCallTargetsAlign guards the column alignment: verbs of different
// lengths must still line their targets up in the same column.
func TestToolCallTargetsAlign(t *testing.T) {
	lines := richLines(t, 80, func() {
		PrintToolCall("read", []byte(`{"path":"a.go"}`))
		PrintToolCall("edit", []byte(`{"path":"b.go"}`))
		PrintToolCall("run", []byte(`{"command":"c"}`))
	})
	col := -1
	for _, ln := range lines {
		if !strings.HasPrefix(ln, " ├─") {
			continue // skip the rail connectors between calls
		}
		idx := strings.LastIndex(ln, "   ") + 3 // start of the target field
		switch {
		case col == -1:
			col = idx
		case idx != col:
			t.Errorf("target starts at column %d, want %d (line %q)", idx, col, ln)
		}
	}
	if col == -1 {
		t.Fatal("no tool-call branch lines were rendered")
	}
}

func TestPrintToolResult(t *testing.T) {
	tests := []struct {
		name string
		give string
		want []string
	}{
		{name: "single line under the branch", give: "102 lines", want: []string{" │  ⎿ 102 lines"}},
		{name: "multi line rides the rail", give: "ok\nmore", want: []string{" │  ⎿ ok", " │    more"}},
		{name: "trailing newline trimmed", give: "done\n", want: []string{" │  ⎿ done"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := richLines(t, 80, func() { PrintToolResult(tt.give, false) })
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("PrintToolResult(%q) mismatch (-want +got):\n%s", tt.give, diff)
			}
		})
	}
}

// TestRailConnectsEvents is the heart of the identity: a reply's events are bound
// by a continuous rail. The first event opens it with no connector; a bare rail
// line separates later events; a result has no connector before it (it belongs to
// the call above it).
func TestRailConnectsEvents(t *testing.T) {
	got := richLines(t, 80, func() {
		PrintAssistant("intro")
		PrintToolCall("read", []byte(`{"path":"a.go"}`))
		PrintToolResult("ok", false)
		PrintAssistant("done")
	})
	want := []string{
		" ◆  intro",
		" │",
		" ├─ read     a.go",
		" │  ⎿ ok",
		" │",
		" ◆  done",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("rail rendering mismatch (-want +got):\n%s", diff)
	}
}

// TestCloseRailStartsFreshReply mirrors what ReadInput does at each Turn boundary:
// closing the rail means the next reply's first event has no leading connector.
func TestCloseRailStartsFreshReply(t *testing.T) {
	got := richLines(t, 80, func() {
		PrintAssistant("reply one")
		closeRail() // a new prompt arrives
		PrintAssistant("reply two")
	})
	want := []string{" ◆  reply one", " ◆  reply two"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("close-rail reset mismatch (-want +got):\n%s", diff)
	}
}

func TestPrintWelcome(t *testing.T) {
	t.Run("non-TTY degrades to one plain line", func(t *testing.T) {
		got := capture(t, false, 80, func() { PrintWelcome("Opus 4.8", "/work") })
		shoot(t, got)
		if want := "agentic-go-cli — Opus 4.8\n"; got != want {
			t.Errorf("PrintWelcome off a TTY = %q, want %q", got, want)
		}
	})

	t.Run("TTY draws a box of the terminal width", func(t *testing.T) {
		const width = 50
		lines := richLines(t, width, func() { PrintWelcome("Opus 4.8 · OAuth", "~/dev/agentic-go-cli") })
		top := lines[0]
		if !strings.HasPrefix(top, "╭") || !strings.HasSuffix(top, "╮") {
			t.Fatalf("first line is not a rounded box top: %q", top)
		}
		// The box fills the terminal less the one-column right margin.
		if got := displayWidth(top); got != boxWidth() {
			t.Errorf("box width = %d columns, want %d", got, boxWidth())
		}
		joined := strings.Join(lines, "\n")
		for _, want := range []string{"◆ agentic-go-cli", "Opus 4.8 · OAuth", "~/dev/agentic-go-cli"} {
			if !strings.Contains(joined, want) {
				t.Errorf("welcome box is missing %q:\n%s", want, joined)
			}
		}
	})
}

func TestStatusLine(t *testing.T) {
	tests := []struct {
		name  string
		count int
		want  string
	}{
		{name: "empty manifest is omitted", count: 0, want: " ⏎ send   ·   ⌃C quit"},
		{name: "non-empty manifest is surfaced", count: 2, want: " manifest: 2   ·   ⏎ send   ·   ⌃C quit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// statusLine reads the package-level count; capture resets it after.
			got := stripANSI(frame(t, 80, func() {
				SetManifestCount(tt.count)
				out.Write([]byte(statusLine(80)))
			}))
			if got != tt.want {
				t.Errorf("statusLine(count=%d) = %q, want %q", tt.count, got, tt.want)
			}
		})
	}
}

func TestSplitArgs(t *testing.T) {
	tests := []struct {
		name       string
		tool       string
		give       map[string]any
		wantTarget string
		wantRest   []string
	}{
		{name: "read takes its path as target", tool: "read",
			give: map[string]any{"path": "main.go"}, wantTarget: "main.go", wantRest: nil},
		{name: "edit keeps content as a secondary arg", tool: "edit",
			give: map[string]any{"path": "x.go", "content": "hi"}, wantTarget: "x.go", wantRest: []string{"content"}},
		{name: "describe by ref", tool: "describe",
			give: map[string]any{"ref": "#1", "gist": "g"}, wantTarget: "#1", wantRest: []string{"gist"}},
		{name: "describe by turn has no target", tool: "describe",
			give: map[string]any{"turn": float64(1), "gist": "g"}, wantTarget: "", wantRest: []string{"gist", "turn"}},
		{name: "unknown tool has no primary key", tool: "mystery",
			give: map[string]any{"b": 2, "a": 1}, wantTarget: "", wantRest: []string{"a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotTarget, gotRest := splitArgs(tt.tool, tt.give)
			if gotTarget != tt.wantTarget {
				t.Errorf("splitArgs target = %q, want %q", gotTarget, tt.wantTarget)
			}
			if diff := cmp.Diff(tt.wantRest, gotRest); diff != "" {
				t.Errorf("splitArgs rest mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestVerbColorCoding pins the palette: each tool verb renders in its assigned
// truecolor. The buffer keeps the colour (unstripped) so the codes are visible.
func TestVerbColorCoding(t *testing.T) {
	tests := []struct {
		name     string
		tool     string
		input    string
		wantCode string
	}{
		{name: "read is cyan", tool: "read", input: `{"path":"x"}`, wantCode: "38;2;86;194;230"},
		{name: "edit is amber", tool: "edit", input: `{"path":"x"}`, wantCode: "38;2;224;169;78"},
		{name: "run is violet", tool: "run", input: `{"command":"x"}`, wantCode: "38;2;197;138;240"},
		{name: "built-in is accent cyan", tool: "forget", input: `{"ref":"#1"}`, wantCode: "38;2;0;173;216"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := frame(t, 80, func() { PrintToolCall(tt.tool, []byte(tt.input)) })
			if !strings.Contains(raw, tt.wantCode) {
				t.Errorf("%s call is missing colour %q in:\n%q", tt.tool, tt.wantCode, raw)
			}
		})
	}
}

func TestPrintToolResultErrorIsRed(t *testing.T) {
	raw := frame(t, 80, func() { PrintToolResult("boom", true) })
	const red = "38;2;242;120;124"
	if !strings.Contains(raw, red) {
		t.Errorf("error result is missing the error colour %q in:\n%q", red, raw)
	}
	if !strings.Contains(stripANSI(raw), "⎿ boom") {
		t.Errorf("error result is missing its text: %q", stripANSI(raw))
	}
}

// TestFullReplyFrame renders a complete session frame — welcome banner, a reply
// with all three tool kinds, and the input box — as one screenshot (the hero
// image), and asserts the headline elements are present.
func TestFullReplyFrame(t *testing.T) {
	joined := strings.Join(richLines(t, 80, func() {
		SetManifestCount(2)
		PrintWelcome("Opus 4.8 · Claude Code OAuth", "~/dev/agentic-go-cli")
		PrintHint("Resumed .agentic-artifacts/sessions/20260614-160000  (--new for a fresh session)")
		PrintAssistant("I'll create hello.txt, then run it to confirm the output.")
		PrintToolCall("read", []byte(`{"path":"main.go"}`))
		PrintToolResult("102 lines", false)
		PrintToolCall("edit", []byte(`{"path":"hello.txt","content":"package main\n\nfunc main() { println(\"hello\") }"}`))
		PrintToolResult("wrote hello.txt (3 lines)", false)
		PrintToolCall("run", []byte(`{"command":"go run hello.txt"}`))
		PrintToolResult("hello", false)
		PrintAssistant(`Done — hello.txt prints "hello".`)

		// A display-only paint of the input box (the real editor blocks on stdin
		// and uses cursor escapes; here we render just the visible row).
		bw := boxWidth()
		bar := strings.Repeat("─", max(0, bw-2))
		buf := []rune("add a goodbye file too")
		pad := max(0, bw-5-stringWidth(buf))
		fmt.Fprintf(out, "\n%s\n", statusLine(bw))
		fmt.Fprintf(out, "%s╭%s╮%s\n", railSeq, bar, reset)
		fmt.Fprintf(out, "%s│%s %s%s%s %s%s%s│%s\n",
			railSeq, reset, accentSeq, glyphPrompt, reset,
			string(buf), strings.Repeat(" ", pad), railSeq, reset)
		fmt.Fprintf(out, "%s╰%s╯%s\n", railSeq, bar, reset)
	}), "\n")

	for _, want := range []string{
		"◆ agentic-go-cli", "├─ read", "├─ edit", "├─ run",
		"◆  Done", "manifest: 2", "› add a goodbye file too",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("full frame is missing %q:\n%s", want, joined)
		}
	}
}
