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
	// Below the prompt row sit the box's bottom border and the Manifest panel (the
	// collapsed Turns). They are drawn once, then the cursor is moved back up to the
	// prompt row so the in-place editor repaints only that row, leaving the panel
	// untouched until the line is submitted.
	below := append([]string{railSeq + "╰" + bar + "╯" + reset}, manifestPanelLines(bw)...)

	// A blank line, the shortcuts footer, the top border, then the (empty) prompt
	// row; the footer sits above the box so the editor can repaint without it.
	fmt.Printf("\r\n%s\r\n%s╭%s╮%s\r\n", statusLine(bw), railSeq, bar, reset)
	var buf []rune
	fmt.Print(inputRow(buf, bw))
	for _, ln := range below {
		fmt.Print("\r\n" + ln)
	}
	fmt.Printf("\x1b[%dA", len(below)) // back up to the prompt row
	fmt.Print(inputRow(buf, bw))       // re-park the cursor at the input position

	for {
		r, _, err := stdin.ReadRune()
		if err != nil {
			return finish(buf, bw, len(below), false)
		}
		switch r {
		case '\r', '\n': // submit
			return finish(buf, bw, len(below), true)
		case 0x03: // Ctrl-C — quit
			return finish(nil, bw, len(below), false)
		case 0x04: // Ctrl-D — quit on an empty line
			if len(buf) == 0 {
				return finish(nil, bw, len(below), false)
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

// finish moves the cursor below the box and the Manifest panel (belowCount lines
// under the prompt row, already drawn), leaving one blank line before the response,
// and returns the edited line.
func finish(buf []rune, bw, belowCount int, ok bool) (string, bool) {
	fmt.Printf("\x1b[%dB\r\n\r\n", belowCount)
	return string(buf), ok
}

// statusLine is the dim footer shown above the input box: just the key hints now.
// The Manifest itself is rendered as its own panel below the box (manifestPanelLines).
func statusLine(bw int) string {
	return metaSeq + " " + strings.Join([]string{"⏎ send", "⌃C quit"}, "   ·   ") + reset
}

// manifestCap bounds the panel: beyond it, the oldest collapsed Turns fold into a
// single "… +K earlier turns" line so the freshest stay visible.
const manifestCap = 8

// manifestPanelLines renders the Manifest — the collapsed Turns the agent still
// carries — as a full-width box drawn below the input box: a titled top border, one
// row per Turn ("Turn N  <description>", oldest at top), and a bottom border. It is
// empty (nil) when nothing has collapsed, and capped at manifestCap rows.
func manifestPanelLines(bw int) []string {
	if len(manifest) == 0 || bw < 8 {
		return nil
	}
	content := bw - 4 // "│ " + content + " │"

	title := fmt.Sprintf("manifest · %d", len(manifest))
	dashes := max(0, bw-runewidth.StringWidth("╭─ "+title+" ")-1)
	top := railSeq + "╭─ " + reset + metaSeq + title + reset + " " + railSeq + strings.Repeat("─", dashes) + "╮" + reset

	row := func(visible, styled string) string {
		pad := max(0, content-runewidth.StringWidth(visible))
		return railSeq + "│" + reset + " " + styled + strings.Repeat(" ", pad) + " " + railSeq + "│" + reset
	}

	entries := manifest
	fold := 0
	if len(entries) > manifestCap {
		fold = len(entries) - (manifestCap - 1)
		entries = entries[len(entries)-(manifestCap-1):]
	}

	rows := []string{top}
	if fold > 0 {
		txt := fmt.Sprintf("… +%d earlier turns", fold)
		rows = append(rows, row(txt, metaSeq+txt+reset))
	}
	for _, e := range entries {
		label := fmt.Sprintf("Turn %d", e.Turn)
		const gap = "  "
		desc := truncateDisplay(strings.ReplaceAll(e.Desc, "\n", " "), max(0, content-runewidth.StringWidth(label)-len(gap)))
		rows = append(rows, row(label+gap+desc, accentSeq+label+reset+gap+desc))
	}
	rows = append(rows, railSeq+"╰"+strings.Repeat("─", max(0, bw-2))+"╯"+reset)
	return rows
}

// truncateDisplay clips s to maxW display columns, marking a cut with an ellipsis.
func truncateDisplay(s string, maxW int) string {
	if maxW <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) <= maxW {
		return s
	}
	var out []rune
	w := 0
	for _, r := range s {
		rw := runewidth.RuneWidth(r)
		if w+rw > maxW-1 {
			break
		}
		out = append(out, r)
		w += rw
	}
	return string(out) + "…"
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
