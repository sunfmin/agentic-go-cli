package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// persistTurn rewrites the in-progress Turn's transcript file (turns/NNN.md) via
// a temp file + rename, so a crash never leaves a partial file. It runs at the
// end of every Round, so the file grows as the Turn proceeds and the latest
// Round is always on disk.
func (a *Agent) persistTurn() error {
	if a.turn == 0 {
		return nil
	}
	dir, err := a.ensureDir()
	if err != nil {
		return err
	}
	turnsDir := filepath.Join(dir, "turns")
	if err := os.MkdirAll(turnsDir, 0o755); err != nil {
		return err
	}
	content := renderTurnFile(a.turn, a.currentTurnEvents(), a.ws)
	path := filepath.Join(turnsDir, fmt.Sprintf("%03d.md", a.turn))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// currentTurnEvents returns the events of the in-progress Turn — those from the
// most recent user prompt onward.
func (a *Agent) currentTurnEvents() []event {
	start := 0
	for i := len(a.events) - 1; i >= 0; i-- {
		if a.events[i].kind == evUser {
			start = i
			break
		}
	}
	return a.events[start:]
}

// renderTurnFile renders one Turn as YAML frontmatter carrying only structure
// (the Manifest refs that fired in each Round, in order) plus a human-readable
// transcript body that reuses the terminal's markers: ❯ user prompt, ⏺ assistant
// prose, ● tool calls. Structure lives in the frontmatter; prose lives in the body.
func renderTurnFile(turnNo int, events []event, ws *workingSet) string {
	var fm, body strings.Builder
	fm.WriteString("---\n")
	fm.WriteString(fmt.Sprintf("turn: %d\n", turnNo))
	fm.WriteString("rounds:\n")

	for _, e := range events {
		switch e.kind {
		case evUser:
			body.WriteString("❯ " + e.text + "\n\n")
		case evAssistant:
			fm.WriteString("  - [" + strings.Join(assistantRefs(e.asst, ws), ", ") + "]\n")
			writeAssistantBody(&body, e.asst, ws)
		}
	}
	fm.WriteString("---\n\n")
	return fm.String() + body.String()
}

// assistantRefs lists the Manifest refs of the tool calls in one Round, in order.
func assistantRefs(m *anthropic.Message, ws *workingSet) []string {
	if m == nil {
		return nil
	}
	var refs []string
	for _, b := range m.Content {
		if v, ok := b.AsAny().(anthropic.ToolUseBlock); ok {
			ref := ""
			if en := ws.get(v.ID); en != nil {
				ref = en.ref
			}
			refs = append(refs, fmt.Sprintf("%q", ref))
		}
	}
	return refs
}

// writeAssistantBody writes a Round's prose then its tool-call lines.
func writeAssistantBody(body *strings.Builder, m *anthropic.Message, ws *workingSet) {
	if m == nil {
		return
	}
	for _, b := range m.Content {
		if v, ok := b.AsAny().(anthropic.TextBlock); ok && strings.TrimSpace(v.Text) != "" {
			body.WriteString("⏺ " + v.Text + "\n")
		}
	}
	for _, b := range m.Content {
		if v, ok := b.AsAny().(anthropic.ToolUseBlock); ok {
			body.WriteString("  " + renderCallLine(v, ws) + "\n")
		}
	}
	body.WriteString("\n")
}

// renderCallLine renders one tool call as a decorative, human-readable line. The
// resume parser ignores these — the authoritative calls are the frontmatter refs.
func renderCallLine(v anthropic.ToolUseBlock, ws *workingSet) string {
	line := fmt.Sprintf("● %s(%s)", v.Name, shortInput(v.Input))
	en := ws.get(v.ID)
	if en == nil {
		return line
	}
	line += "   " + en.ref
	switch en.kind {
	case kindRun:
		if en.path != "" {
			line += " → " + filepath.Join("runs", filepath.Base(en.path))
		}
	case kindRead:
		line += " → live"
	}
	return line
}

// shortInput renders a tool call's input compactly for the transcript body.
func shortInput(input json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(input, &m); err != nil {
		return ""
	}
	for _, k := range []string{"command", "path", "ref"} {
		if v, ok := m[k]; ok {
			s := fmt.Sprint(v)
			if g, ok := m["gist"]; ok {
				s += ": " + fmt.Sprint(g)
			}
			return s
		}
	}
	var parts []string
	for k, v := range m {
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	return strings.Join(parts, " ")
}
