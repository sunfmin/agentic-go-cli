package ui

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/mattn/go-runewidth"
	"golang.org/x/term"
)

// stdin is a single buffered reader over os.Stdin, shared by the raw-mode line
// editor and the non-TTY fallback so no typed bytes are lost between reads.
var stdin = bufio.NewReader(os.Stdin)

// Raw ANSI for the input editor. It writes cursor-position escapes directly to
// stdout (never through render.go's colorprofile writer, which strips them), so
// its colours are inline SGR sequences rather than lipgloss styles.
const (
	reset     = "\x1b[0m"
	dim       = "\x1b[2m"
	railSeq   = "\x1b[38;2;90;99;115m" // cRail, the box border
	accentSeq = "\x1b[38;2;0;173;216m" // cAccent (Go cyan), the prompt glyph
	metaSeq   = dim                    // the footer hint
)

// ReadInput draws the input prompt and returns the next line the user enters,
// reporting false on EOF / Ctrl-C / Ctrl-D.
//
// On an interactive terminal it runs a small raw-mode line editor inside a
// rounded box and repaints the line on every keystroke. That repaint (rather
// than relying on the terminal's cooked-mode erase) is what lets wide CJK
// characters delete cleanly — cooked-mode editing erases by cell and leaves
// half of every two-column glyph on screen. Off a TTY it falls back to a plain
// prompt and a buffered line read.
func ReadInput() (string, bool) {
	// A new prompt ends the previous reply's rail, so the next reply starts fresh.
	closeRail()
	if !isTTY() {
		fmt.Print(accentSeq + glyphPrompt + reset + " ")
		return readLineCooked()
	}
	return readLineRaw()
}

// readLineCooked reads one line via the shared buffered reader, for non-TTY use.
func readLineCooked() (string, bool) {
	line, err := stdin.ReadString('\n')
	if err != nil && line == "" {
		return "", false
	}
	return strings.TrimRight(line, "\r\n"), true
}

// readLineRaw runs the box-framed line editor with the terminal in raw mode.
func readLineRaw() (string, bool) {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		fmt.Print("\n" + accentSeq + glyphPrompt + reset + " ")
		return readLineCooked()
	}
	defer term.Restore(fd, old)

	bw := boxWidth()
	bar := strings.Repeat("─", max(0, bw-2))
	// A blank line, the Working Set + shortcuts footer, the top border, then the
	// (empty) prompt row. The footer sits above the box so the in-place line
	// editor below it can repaint without disturbing it.
	fmt.Printf("\r\n%s\r\n%s╭%s╮%s\r\n", statusLine(bw), railSeq, bar, reset)
	var buf []rune
	drawInputRow(buf, bw)

	for {
		r, _, err := stdin.ReadRune()
		if err != nil {
			return finish(buf, bw, false)
		}
		switch r {
		case '\r', '\n': // submit
			return finish(buf, bw, true)
		case 0x03: // Ctrl-C — quit
			return finish(nil, bw, false)
		case 0x04: // Ctrl-D — quit on an empty line
			if len(buf) == 0 {
				return finish(nil, bw, false)
			}
		case 0x15: // Ctrl-U — clear the line
			buf = buf[:0]
			drawInputRow(buf, bw)
		case 0x7f, 0x08: // Backspace / Delete — drop the last rune and repaint
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
				drawInputRow(buf, bw)
			}
		case 0x1b: // swallow escape sequences (arrow keys, etc.)
			consumeEscape()
		default:
			if r >= 0x20 { // printable
				buf = append(buf, r)
				drawInputRow(buf, bw)
			}
		}
	}
}

// drawInputRow repaints the prompt row from scratch so the rendered row depends
// only on the current buffer, never on a longer previous one. That is what makes
// wide CJK characters erase cleanly: every keystroke redraws the whole row.
func drawInputRow(buf []rune, bw int) {
	fmt.Print(inputRow(buf, bw))
}

// inputRow is the control sequence that paints the prompt row for buf: carriage
// return to column 1, the "│ › " prefix (rail border, Go-cyan prompt) and buffer,
// padding spaces that overwrite any leftover glyphs out to the right border, then
// the cursor parked at the logical end of the input. The prefix is four columns
// wide, so the buffer (and the cursor) start at column five.
func inputRow(buf []rune, bw int) string {
	bufW := stringWidth(buf)
	pad := max(0, bw-5-bufW)
	return fmt.Sprintf("\r%s│%s %s%s%s %s%s%s│%s\x1b[%dG",
		railSeq, reset, accentSeq, glyphPrompt, reset,
		string(buf), strings.Repeat(" ", pad), railSeq, reset, 5+bufW)
}

// finish closes the box below the prompt row, leaving one blank line before the
// response, and returns the edited line.
func finish(buf []rune, bw int, ok bool) (string, bool) {
	bar := strings.Repeat("─", max(0, bw-2))
	fmt.Printf("\r\n%s╰%s╯%s\r\n\r\n", railSeq, bar, reset)
	return string(buf), ok
}

// statusLine is the dim footer shown above the input box. It surfaces the Manifest
// size — the collapsed Turns the agent still carries, the thing that sets it apart
// from a plain chat loop — alongside the key hints, so both stay in view as you type.
func statusLine(bw int) string {
	parts := make([]string, 0, 3)
	if manifestCount > 0 {
		parts = append(parts, fmt.Sprintf("manifest: %d", manifestCount))
	}
	parts = append(parts, "⏎ send", "⌃C quit")
	return metaSeq + " " + strings.Join(parts, "   ·   ") + reset
}

// consumeEscape discards the rest of an escape sequence (e.g. an arrow key)
// already buffered after the ESC, so it never lands in the input as garbage. A
// lone ESC (nothing buffered behind it) is ignored.
func consumeEscape() {
	if stdin.Buffered() == 0 {
		return
	}
	b, err := stdin.ReadByte()
	if err != nil || (b != '[' && b != 'O') {
		return
	}
	for stdin.Buffered() > 0 {
		c, err := stdin.ReadByte()
		if err != nil || (c >= 0x40 && c <= 0x7e) { // CSI final byte
			return
		}
	}
}

// stringWidth is the display column count of a rune slice — what the repaint
// uses to size padding and park the cursor. It delegates to go-runewidth (the
// width table lipgloss itself uses), so a wide CJK / fullwidth / emoji rune
// counts as two columns and stays consistent with render.go's measurements.
func stringWidth(rs []rune) int {
	return runewidth.StringWidth(string(rs))
}

// runeWidth returns how many terminal columns a rune occupies: 0 for control
// characters (which go-runewidth would otherwise count as one), and the
// go-runewidth value — 2 for wide runes, 1 otherwise — for everything else.
func runeWidth(r rune) int {
	if r < 0x20 {
		return 0
	}
	return runewidth.RuneWidth(r)
}
