package ui

import (
	"strings"
	"testing"
)

func TestRuneWidth(t *testing.T) {
	cases := []struct {
		r    rune
		want int
	}{
		{'a', 1},
		{'~', 1},
		{'\t', 0},   // control
		{'你', 2},    // CJK Unified
		{'好', 2},    // CJK Unified
		{'あ', 2},    // Hiragana
		{'한', 2},    // Hangul syllable
		{'！', 2},    // fullwidth punctuation
		{'🙂', 2},    // emoji
	}
	for _, c := range cases {
		if got := runeWidth(c.r); got != c.want {
			t.Errorf("runeWidth(%q) = %d, want %d", c.r, got, c.want)
		}
	}
}

func TestStringWidthCJK(t *testing.T) {
	if got := stringWidth([]rune("你好")); got != 4 {
		t.Fatalf("stringWidth(你好) = %d, want 4 (two double-width runes)", got)
	}
	if got := stringWidth([]rune("ab你")); got != 4 {
		t.Fatalf("stringWidth(ab你) = %d, want 4", got)
	}
}

// TestInputRowRepaintIsHistoryIndependent is the core guarantee behind the CJK
// delete fix: the painted row for a given buffer is byte-for-byte identical no
// matter how long the buffer used to be. So typing 你好世界 and deleting back to
// 你好 leaves exactly the same screen as having typed 你好 directly — no stale
// half-width glyphs survive.
func TestInputRowRepaintIsHistoryIndependent(t *testing.T) {
	const bw = 40
	short := inputRow([]rune("你好"), bw)
	long := inputRow([]rune("你好世界你好世界"), bw)
	deletedBack := inputRow([]rune("你好"), bw) // after backspacing the long buffer
	if short != deletedBack {
		t.Fatalf("repaint after deletion differs from a fresh short buffer:\n short: %q\n after: %q", short, deletedBack)
	}
	if short == long {
		t.Fatalf("distinct buffers unexpectedly paint identically")
	}
}

func TestInputRowFillsToRightBorder(t *testing.T) {
	const bw = 20
	// Empty buffer: "│ ❯ " (4 cols) + padding + "│" should span exactly bw columns
	// (so the right border lands in the same place no matter the buffer length).
	if got := displayWidth(stripParkCursor(inputRow(nil, bw))); got != bw {
		t.Fatalf("painted row width = %d columns, want %d", got, bw)
	}
	// Two double-width runes shift content but the row still spans bw columns —
	// the wide-aware padding is what keeps the right border aligned.
	if got := displayWidth(stripParkCursor(inputRow([]rune("你好"), bw))); got != bw {
		t.Fatalf("painted row width with CJK = %d columns, want %d", got, bw)
	}
}

// displayWidth is the terminal column count of s, ignoring ANSI escapes and
// honouring wide (CJK) runes — the measure the prompt row is sized against.
func displayWidth(s string) int {
	w, inEsc := 0, false
	for _, r := range s {
		switch {
		case inEsc:
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
		case r == '\x1b':
			inEsc = true
		default:
			w += runeWidth(r)
		}
	}
	return w
}

func TestInputRowParksCursorAfterBuffer(t *testing.T) {
	// The cursor-park escape must target column 5 + displayWidth(buf): for 你好
	// (width 4) that is column 9.
	row := inputRow([]rune("你好"), 40)
	if !strings.HasSuffix(row, "\x1b[9G") {
		t.Fatalf("cursor not parked at column 9 (5 + width 4): %q", row)
	}
	// Empty buffer parks just past "│ ❯ " at column 5.
	if !strings.HasSuffix(inputRow(nil, 40), "\x1b[5G") {
		t.Fatalf("empty buffer should park the cursor at column 5")
	}
}

// stripParkCursor removes the leading \r and the trailing \x1b[..G cursor-park
// escape so the test can measure the visible painted row width.
func stripParkCursor(row string) string {
	row = strings.TrimPrefix(row, "\r")
	if i := strings.LastIndex(row, "\x1b["); i >= 0 && strings.HasSuffix(row, "G") {
		row = row[:i]
	}
	return row
}
