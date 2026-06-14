package ui

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// stdin is a single buffered reader over os.Stdin, shared by the raw-mode line
// editor and the non-TTY fallback so no typed bytes are lost between reads.
var stdin = bufio.NewReader(os.Stdin)

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
	if !isTTY() {
		fmt.Print(dim + "❯" + reset + " ")
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
		fmt.Print("\n" + dim + "❯" + reset + " ")
		return readLineCooked()
	}
	defer term.Restore(fd, old)

	bw := boxWidth()
	bar := strings.Repeat("─", max(0, bw-2))
	// Leading blank line, top border, then the (empty) prompt row.
	fmt.Printf("\r\n%s╭%s╮%s\r\n", dim, bar, reset)
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
// return to column 1, the "│ ❯ " prefix and buffer, padding spaces that overwrite
// any leftover glyphs out to the right border, then the cursor parked at the
// logical end of the input.
func inputRow(buf []rune, bw int) string {
	bufW := stringWidth(buf)
	pad := max(0, bw-5-bufW)
	return fmt.Sprintf("\r%s│%s ❯ %s%s%s│%s\x1b[%dG",
		dim, reset, string(buf), strings.Repeat(" ", pad), dim, reset, 5+bufW)
}

// finish closes the box below the prompt row, leaving one blank line before the
// response, and returns the edited line.
func finish(buf []rune, bw int, ok bool) (string, bool) {
	bar := strings.Repeat("─", max(0, bw-2))
	fmt.Printf("\r\n%s╰%s╯%s\r\n\r\n", dim, bar, reset)
	return string(buf), ok
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

// stringWidth is the display column count of a rune slice.
func stringWidth(rs []rune) int {
	w := 0
	for _, r := range rs {
		w += runeWidth(r)
	}
	return w
}

// runeWidth returns how many terminal columns a rune occupies: 0 for control
// characters, 2 for wide (CJK / fullwidth / emoji) runes, 1 otherwise.
func runeWidth(r rune) int {
	switch {
	case r < 0x20:
		return 0
	case isWide(r):
		return 2
	default:
		return 1
	}
}

// isWide reports whether a rune is rendered two columns wide, covering the CJK,
// Japanese, Korean, fullwidth, and emoji blocks that show up in practice.
func isWide(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F, // Hangul Jamo
		r >= 0x2E80 && r <= 0x303E, // CJK radicals, Kangxi, punctuation
		r >= 0x3041 && r <= 0x33FF, // Hiragana, Katakana, CJK symbols
		r >= 0x3400 && r <= 0x4DBF, // CJK Extension A
		r >= 0x4E00 && r <= 0x9FFF, // CJK Unified Ideographs
		r >= 0xA000 && r <= 0xA4CF, // Yi
		r >= 0xAC00 && r <= 0xD7A3, // Hangul Syllables
		r >= 0xF900 && r <= 0xFAFF, // CJK Compatibility Ideographs
		r >= 0xFE10 && r <= 0xFE19, // vertical forms
		r >= 0xFE30 && r <= 0xFE6F, // CJK compatibility forms
		r >= 0xFF00 && r <= 0xFF60, // fullwidth forms
		r >= 0xFFE0 && r <= 0xFFE6, // fullwidth signs
		r >= 0x1F300 && r <= 0x1FAFF, // emoji & symbols
		r >= 0x20000 && r <= 0x3FFFD: // CJK Extension B and beyond
		return true
	}
	return false
}
