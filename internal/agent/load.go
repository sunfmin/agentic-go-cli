package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// MostRecentSession returns the lexicographically greatest Session directory
// (timestamp ids sort chronologically), or false if none exist.
func MostRecentSession() (string, bool) {
	base := filepath.Join(artifactsRoot, sessionsSubdir)
	ents, err := os.ReadDir(base)
	if err != nil {
		return "", false
	}
	var dirs []string
	for _, e := range ents {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) == 0 {
		return "", false
	}
	sort.Strings(dirs)
	return filepath.Join(base, dirs[len(dirs)-1]), true
}

// Load rebuilds the in-memory event log and Working Set from a Session directory
// so the conversation can continue. The Working Set comes from index.json
// (per-ref state); the conversation comes from the turn files (frontmatter for
// Round ordering, body for prose); run output comes from the run files and read
// content is re-read live. Subsequent persistence writes back to the same dir.
func (a *Agent) Load(dir string) error {
	data, err := os.ReadFile(filepath.Join(dir, indexFileName))
	if err != nil {
		return fmt.Errorf("read index: %w", err)
	}
	var idx indexState
	if err := json.Unmarshal(data, &idx); err != nil {
		return fmt.Errorf("parse index: %w", err)
	}

	a.sessionDir = dir
	a.createdAt = idx.CreatedAt
	a.turn = idx.Turn
	a.round = idx.Round
	a.refSeq = idx.RefSeq
	a.artifactSeq = idx.ArtifactSeq
	if idx.Turns != nil {
		a.turnDesc = idx.Turns
	}
	if idx.Forgotten != nil {
		a.forgottenTurns = idx.Forgotten
	}

	for _, e := range idx.Entries {
		en := &entry{
			id: e.ID, ref: e.Ref, kind: entryKind(e.Kind), name: e.Name,
			path: e.Path, desc: e.Desc, isErr: e.IsErr, turn: e.Turn, forgotten: e.Forgotten,
		}
		en.content = loadContent(en)
		a.ws.put(en)
	}

	for t := 1; t <= idx.Turn; t++ {
		p := filepath.Join(dir, "turns", fmt.Sprintf("%03d.md", t))
		body, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		a.events = append(a.events, a.reconstructTurn(string(body))...)
	}
	return nil
}

// loadContent recovers an entry's content from disk: a run's output from its run
// file, a read from the live working tree. Edits and bookkeeping carry none — it
// is only needed when an entry is the latest (full) result, which after a normal
// turn-boundary resume nothing is.
func loadContent(en *entry) string {
	switch en.kind {
	case kindRun:
		_, out := parseRunFile(en.path)
		return out
	case kindRead:
		if en.path != "" {
			if b, err := os.ReadFile(en.path); err == nil {
				return string(b)
			}
		}
	}
	return ""
}

// reconstructTurn rebuilds one Turn's events from its transcript file. The
// frontmatter refs are authoritative for which tool calls fired per Round; the
// body supplies prose. forget/describe calls are not replayed — their effect is
// already materialized in the Working Set index — so they are skipped here.
func (a *Agent) reconstructTurn(content string) []event {
	rounds, body := parseFrontmatter(content)
	prompt, items := parseBody(body)

	var evs []event
	if prompt != "" {
		evs = append(evs, event{kind: evUser, text: prompt})
	}

	ti := 0
	for _, refs := range rounds {
		var prose []string
		for ti < len(items) && items[ti].isProse {
			prose = append(prose, items[ti].text)
			ti++
		}
		for n := 0; n < len(refs) && ti < len(items) && !items[ti].isProse; n++ {
			ti++ // skip the decorative tool line; the ref is authoritative
		}

		var blocks []wireBlock
		if joined := strings.Join(prose, "\n\n"); strings.TrimSpace(joined) != "" {
			blocks = append(blocks, wireBlock{Type: "text", Text: joined})
		}
		var ids []string
		for _, ref := range refs {
			id, name, input, skip := a.toolUseRef(ref)
			if skip {
				continue
			}
			blocks = append(blocks, wireBlock{Type: "tool_use", ID: id, Name: name, Input: input})
			ids = append(ids, id)
		}

		if len(blocks) > 0 {
			if msg, err := buildAssistantMessage(blocks); err == nil {
				evs = append(evs, event{kind: evAssistant, asst: msg})
			}
		}
		if len(ids) > 0 {
			evs = append(evs, event{kind: evToolResults, ids: ids})
		}
	}
	return evs
}

// toolUseRef reconstructs the tool_use (id, name, input) for a Working Set ref.
// run inputs come from the run file; read/edit inputs from the stored path (an
// edit's content is compacted — the live file is the truth); bookkeeping calls
// (forget/describe) are skipped, their effect already being in the index.
func (a *Agent) toolUseRef(ref string) (id, name string, input json.RawMessage, skip bool) {
	en := a.ws.getByRef(ref)
	if en == nil {
		return "", "", nil, true
	}
	switch en.kind {
	case kindRun:
		cmd, _ := parseRunFile(en.path)
		input, _ = json.Marshal(map[string]string{"command": cmd})
		return en.id, "run", input, false
	case kindRead:
		input, _ = json.Marshal(map[string]string{"path": en.path})
		return en.id, "read", input, false
	case kindEdit:
		input, _ = json.Marshal(map[string]string{"path": en.path, "content": ""})
		return en.id, "edit", input, false
	default:
		return "", "", nil, true
	}
}

// wireBlock is a minimal content block we marshal to JSON and let the SDK
// unmarshal back into an anthropic.Message — the simplest faithful way to
// reconstruct an assistant turn's blocks.
type wireBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

func buildAssistantMessage(blocks []wireBlock) (*anthropic.Message, error) {
	data, err := json.Marshal(struct {
		Role    string      `json:"role"`
		Content []wireBlock `json:"content"`
	}{Role: "assistant", Content: blocks})
	if err != nil {
		return nil, err
	}
	var m anthropic.Message
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// parseRunFile splits a run file back into its command (the "$ "/"> " prefixed
// lines) and its output (everything after the first unprefixed line).
func parseRunFile(path string) (command, output string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	lines := strings.Split(string(data), "\n")
	var cmd []string
	i := 0
	for i < len(lines) {
		switch {
		case strings.HasPrefix(lines[i], "$ "):
			cmd = append(cmd, strings.TrimPrefix(lines[i], "$ "))
		case strings.HasPrefix(lines[i], "> "):
			cmd = append(cmd, strings.TrimPrefix(lines[i], "> "))
		default:
			goto done
		}
		i++
	}
done:
	if i < len(lines) && lines[i] == "" {
		i++ // the blank line separating command from output
	}
	return strings.Join(cmd, "\n"), strings.Join(lines[i:], "\n")
}

// parseFrontmatter splits a turn file into its Round structure (the refs that
// fired in each Round) and the transcript body.
func parseFrontmatter(content string) (rounds [][]string, body string) {
	if !strings.HasPrefix(content, "---\n") {
		return nil, content
	}
	rest := content[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return nil, content
	}
	fm := rest[:end]
	body = rest[end+len("\n---\n"):]

	inRounds := false
	for _, line := range strings.Split(fm, "\n") {
		switch {
		case strings.HasPrefix(line, "rounds:"):
			inRounds = true
		case inRounds && strings.HasPrefix(strings.TrimSpace(line), "- ["):
			inner := strings.TrimSpace(line)
			inner = strings.TrimSuffix(strings.TrimPrefix(inner, "- ["), "]")
			var refs []string
			for _, part := range strings.Split(inner, ",") {
				if p := strings.Trim(strings.TrimSpace(part), `"`); p != "" {
					refs = append(refs, p)
				}
			}
			rounds = append(rounds, refs)
		case inRounds && !strings.HasPrefix(line, " "):
			inRounds = false
		}
	}
	return rounds, body
}

// bodyItem is one parsed transcript element: an assistant prose block or a
// (decorative) tool-call line.
type bodyItem struct {
	isProse bool
	text    string
}

// parseBody extracts the user prompt and the ordered prose/tool items from a
// transcript body. User prompts are single lines (the CLI reads a line at a
// time); assistant prose blocks run from a ⏺ marker to the next marker.
func parseBody(body string) (prompt string, items []bodyItem) {
	lines := strings.Split(body, "\n")
	isTool := func(l string) bool { return strings.HasPrefix(strings.TrimLeft(l, " "), "● ") }
	isMarker := func(l string) bool {
		return strings.HasPrefix(l, "❯ ") || strings.HasPrefix(l, "⏺ ") || isTool(l)
	}
	for i := 0; i < len(lines); {
		l := lines[i]
		switch {
		case strings.HasPrefix(l, "❯ "):
			prompt = strings.TrimPrefix(l, "❯ ")
			i++
		case strings.HasPrefix(l, "⏺ "):
			block := strings.TrimPrefix(l, "⏺ ")
			i++
			for i < len(lines) && !isMarker(lines[i]) {
				block += "\n" + lines[i]
				i++
			}
			items = append(items, bodyItem{isProse: true, text: strings.TrimRight(block, "\n")})
		case isTool(l):
			items = append(items, bodyItem{})
			i++
		default:
			i++
		}
	}
	return prompt, items
}
