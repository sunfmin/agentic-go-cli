// Package ui renders the terminal interface — the welcome banner, the input
// prompt box, and tool calls with their output — styled to resemble Claude Code.
package ui

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"golang.org/x/term"
)

// ANSI styling and the glyphs Claude Code uses in its terminal UI.
const (
	reset  = "\x1b[0m"
	bold   = "\x1b[1m"
	dim    = "\x1b[2m"
	green  = "\x1b[92m"
	red    = "\x1b[91m"
	orange = "\x1b[38;5;208m"
)

// isTTY reports whether stdout is an interactive terminal. When it isn't (output
// piped to a file, or the test harness), the box-drawing UI degrades to plain
// lines so nothing emits stray cursor-movement escapes.
func isTTY() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// termWidth is the terminal's column count, falling back to 80 off a TTY.
func termWidth() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		return 80
	}
	return w
}

// boxWidth is the width of the framed UI boxes: the terminal width less a
// one-column right margin, which also keeps the right border off the last
// column (where a trailing write can trigger an unwanted line wrap).
func boxWidth() int {
	w := termWidth()
	if w >= 8 {
		w--
	}
	return w
}

// visibleLen counts printable columns, ignoring ANSI escape sequences so a
// styled string can be padded to a fixed width (assumes single-column runes).
func visibleLen(s string) int {
	n, inEsc := 0, false
	for _, r := range s {
		switch {
		case inEsc:
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
		case r == '\x1b':
			inEsc = true
		default:
			n++
		}
	}
	return n
}

// PrintWelcome prints the rounded welcome banner shown once at startup.
func PrintWelcome(model, cwd string) {
	if !isTTY() {
		fmt.Printf("agentic-go-cli — %s\n", model)
		return
	}
	w := boxWidth()
	bar := strings.Repeat("─", max(0, w-2))
	inner := w - 4
	line := func(body string) {
		pad := strings.Repeat(" ", max(0, inner-visibleLen(body)))
		fmt.Printf("%s│%s %s%s %s│%s\n", dim, reset, body, pad, dim, reset)
	}
	fmt.Printf("%s╭%s╮%s\n", dim, bar, reset)
	line(orange + "✻" + reset + " " + bold + "Welcome to agentic-go-cli" + reset)
	line("")
	line(dim + "  " + model + reset)
	line(dim + "  " + cwd + reset)
	fmt.Printf("%s╰%s╯%s\n", dim, bar, reset)
}

// PrintHint prints a single dim status line (e.g. resume notices).
func PrintHint(s string) {
	fmt.Printf("%s%s%s\n", dim, s, reset)
}

// PrintAssistant prints a block of assistant prose with Claude Code's ⏺ bullet.
func PrintAssistant(text string) {
	fmt.Printf("⏺ %s\n", text)
}

// PrintToolCall renders a tool invocation in a human-readable form instead of
// raw JSON, e.g.:
//
//	⏺ read(path: main.go)
//	⏺ edit
//	    path: hello.txt
//	    content:
//	      hello
func PrintToolCall(name string, input []byte) {
	var args map[string]any
	_ = json.Unmarshal(input, &args)

	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	inline := true
	for _, k := range keys {
		s := fmt.Sprint(args[k])
		if strings.Contains(s, "\n") || len(s) > 60 {
			inline = false
			break
		}
	}

	bullet := green + "⏺" + reset + " " + bold + name + reset

	// Single-argument tools don't need the key label — show the value directly.
	if len(keys) == 1 {
		s := fmt.Sprint(args[keys[0]])
		if inline {
			fmt.Printf("%s(%s)\n", bullet, s)
			return
		}
		fmt.Printf("%s\n", bullet)
		for _, line := range strings.Split(s, "\n") {
			fmt.Printf("    %s\n", line)
		}
		return
	}

	if inline {
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s: %v", k, args[k]))
		}
		fmt.Printf("%s(%s)\n", bullet, strings.Join(parts, ", "))
		return
	}

	fmt.Printf("%s\n", bullet)
	for _, k := range keys {
		s := fmt.Sprint(args[k])
		if strings.Contains(s, "\n") {
			fmt.Printf("    %s:\n", k)
			for _, line := range strings.Split(s, "\n") {
				fmt.Printf("      %s\n", line)
			}
		} else {
			fmt.Printf("    %s: %s\n", k, s)
		}
	}
}

// PrintToolResult prints the tool's output verbatim (original formatting
// preserved), under the ⎿ tree branch Claude Code uses, red on error.
func PrintToolResult(response string, isError bool) {
	lines := strings.Split(strings.TrimRight(response, "\n"), "\n")
	for i, line := range lines {
		if i == 0 {
			fmt.Printf("  %s⎿%s  ", dim, reset)
		} else {
			fmt.Print("     ")
		}
		if isError {
			fmt.Printf("%s%s%s\n", red, line, reset)
		} else {
			fmt.Printf("%s\n", line)
		}
	}
}
