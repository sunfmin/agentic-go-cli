// Package agent runs the chat loop and rewrites what is sent to the model each
// round from an internal event log plus the working set.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/sunfmin/agentic-go-cli/internal/tool"
	"github.com/sunfmin/agentic-go-cli/internal/ui"
)

// defaultModel is the Anthropic model the real Model uses and the name recorded
// in the Session index for provenance.
const defaultModel = anthropic.ModelClaudeOpus4_8

// Model is the seam over the Anthropic Messages API so the agent loop can be
// driven by a scripted fake in tests.
type Model interface {
	Next(ctx context.Context, messages []anthropic.MessageParam, tools []anthropic.ToolUnionParam) (*anthropic.Message, error)
}

type anthropicModel struct {
	client *anthropic.Client
}

// NewAnthropicModel wraps a real Anthropic client as a Model.
func NewAnthropicModel(client *anthropic.Client) Model {
	return anthropicModel{client: client}
}

func (m anthropicModel) Next(ctx context.Context, messages []anthropic.MessageParam, tools []anthropic.ToolUnionParam) (*anthropic.Message, error) {
	return m.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     defaultModel,
		MaxTokens: 16000,
		// Claude Code OAuth tokens are only valid for requests that
		// identify as Claude Code in the first system block.
		System: []anthropic.TextBlockParam{
			{Text: "You are Claude Code, Anthropic's official CLI for Claude."},
		},
		Messages: messages,
		Tools:    tools,
	})
}

// entryKind distinguishes how a tool result is held and rendered when collapsed.
type entryKind string

const (
	kindRead  entryKind = "read"  // a live reference to a working-tree file
	kindEdit  entryKind = "edit"  // a file write
	kindRun   entryKind = "run"   // command output
	kindPlain entryKind = "plain" // anything else
)

// entry is one tool result the agent holds onto, keyed by the tool_use ID that
// produced it.
type entry struct {
	id        string
	ref       string // model-visible handle shown in the Manifest, e.g. "#3"
	kind      entryKind
	name      string // short label, e.g. "read main.go" or "run"
	path      string // for read/edit entries: the file path
	desc      string // Description shown when collapsed into the Manifest
	content   string // full tool output (sent only while this is the latest result)
	isErr     bool
	turn      int
	forgotten bool
}

// newEntry classifies a tool result so it can be rendered in the Manifest. Read
// and edit Descriptions are derived for free from the path.
func newEntry(id, toolName string, input []byte, content string, isErr bool, turn int) *entry {
	e := &entry{id: id, kind: kindPlain, name: toolName, desc: toolName, content: content, isErr: isErr, turn: turn}
	switch toolName {
	case "read":
		var in struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(input, &in)
		e.kind, e.path, e.name, e.desc = kindRead, in.Path, "read "+in.Path, "contents of "+in.Path
	case "edit":
		var in struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(input, &in)
		e.kind, e.path, e.name, e.desc = kindEdit, in.Path, "edit "+in.Path, "edited "+in.Path
	case "run":
		var in struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(input, &in)
		e.kind, e.name, e.desc = kindRun, "run", "run: "+in.Command
	}
	return e
}

// manifestLine is the one-line form sent in place of full content once this entry
// is no longer the latest result. A read renders as a live reference (no content),
// so the model never reasons against a stale copy.
func (e *entry) manifestLine() string {
	prefix := ""
	if e.ref != "" {
		prefix = e.ref + " "
	}
	switch e.kind {
	case kindRead:
		return fmt.Sprintf("[%sread %s @turn %d — re-read for current contents]", prefix, e.path, e.turn)
	case kindRun:
		if e.path != "" {
			return fmt.Sprintf("[%s%s — recall: read %s]", prefix, e.desc, e.path)
		}
		return fmt.Sprintf("[%s%s]", prefix, e.desc)
	}
	return fmt.Sprintf("[%s%s — %s]", prefix, e.name, e.desc)
}

// workingSet holds the live entries, keyed by tool_use ID and kept in insertion
// order so the payload can be rebuilt deterministically.
type workingSet struct {
	byID  map[string]*entry
	order []string
}

func newWorkingSet() *workingSet {
	return &workingSet{byID: map[string]*entry{}}
}

func (w *workingSet) put(e *entry) {
	if _, ok := w.byID[e.id]; !ok {
		w.order = append(w.order, e.id)
	}
	w.byID[e.id] = e
}

func (w *workingSet) get(id string) *entry {
	return w.byID[id]
}

func (w *workingSet) getByRef(ref string) *entry {
	for _, id := range w.order {
		if e := w.byID[id]; e.ref == ref {
			return e
		}
	}
	return nil
}

type eventKind int

const (
	evUser eventKind = iota
	evAssistant
	evToolResults
)

// event is one step of the logical conversation. buildPayload reconstructs the
// messages to send from the event log plus the working set, so the rewrite logic
// stays a pure function with no I/O.
type event struct {
	kind eventKind
	text string             // evUser
	asst *anthropic.Message // evAssistant
	ids  []string           // evToolResults: tool_use IDs produced this turn, in order
}

type Agent struct {
	model          Model
	getUserMessage func() (string, bool)
	tools          []tool.ToolDefinition

	events   []event
	ws       *workingSet
	turn     int            // Turn: one user prompt plus the entire AI reply that follows
	round    int            // Round: one model response within a Turn
	turnDesc map[int]string // upgraded one-line Descriptions for collapsed Turns, by turn number

	sessionDir  string // this Session's on-disk directory; created lazily
	createdAt   string // RFC3339 timestamp recorded in the index, set on first persist
	artifactSeq int
	refSeq      int
}

// New builds an agent over a model, an input source, and a set of tools.
func New(model Model, getUserMessage func() (string, bool), tools []tool.ToolDefinition) *Agent {
	return &Agent{
		model:          model,
		getUserMessage: getUserMessage,
		tools:          tools,
		ws:             newWorkingSet(),
		turnDesc:       map[int]string{},
	}
}

func (a *Agent) Run(ctx context.Context) error {
	fmt.Println("Chat with Claude (ctrl-c to quit)")

	readUserInput := true
	for {
		if readUserInput {
			fmt.Print("\x1b[2m❯\x1b[0m ")
			userInput, ok := a.getUserMessage()
			if !ok {
				break
			}
			a.turn++
			a.events = append(a.events, event{kind: evUser, text: userInput})
		}

		message, err := a.model.Next(ctx, buildPayload(a.events, a.ws, a.turnDesc), a.toolParams())
		if err != nil {
			return err
		}
		a.events = append(a.events, event{kind: evAssistant, asst: message})
		a.round++

		ids := []string{}
		for _, block := range message.Content {
			switch variant := block.AsAny().(type) {
			case anthropic.TextBlock:
				fmt.Printf("⏺ %s\n", variant.Text)
			case anthropic.ToolUseBlock:
				content, isErr := a.handleTool(variant.Name, []byte(variant.Input))
				a.ws.put(a.record(variant.ID, variant.Name, []byte(variant.Input), content, isErr))
				ids = append(ids, variant.ID)
			}
		}
		if len(ids) == 0 {
			readUserInput = true
		} else {
			a.events = append(a.events, event{kind: evToolResults, ids: ids})
			readUserInput = false
		}
		if err := a.persist(); err != nil {
			fmt.Fprintln(os.Stderr, "warning: failed to persist session:", err)
		}
		if err := a.persistTurn(); err != nil {
			fmt.Fprintln(os.Stderr, "warning: failed to persist turn:", err)
		}
	}
	return nil
}

// buildPayload reconstructs the messages to send to the model from the event log
// and the working set. Only the most recent tool result is sent in full; every
// older tool result collapses to its one-line Manifest entry, while keeping the
// tool_use/tool_result pairing intact so the request stays valid.
func buildPayload(events []event, ws *workingSet, turnDesc map[int]string) []anthropic.MessageParam {
	// The latest results are full only when they are literally the last thing in
	// the log — i.e. the model is about to respond to them. Once a user or
	// assistant turn follows, they too collapse into the Manifest.
	latest := latestIDs(events)
	totalTurns := countTurns(events)

	out := []anthropic.MessageParam{}
	curTurn := 0
	for _, e := range events {
		if e.kind == evUser {
			curTurn++
		}
		// Turns older than the most recent few collapse to a one-line synopsis,
		// and their tool_use/tool_result plumbing is dropped wholesale. The live
		// Artifacts they produced stay listed in the Manifest, recallable, and
		// the full exchange is recalled by reading the Turn's transcript.
		if curTurn <= totalTurns-fullTurnWindow {
			if e.kind == evUser {
				out = append(out, anthropic.NewUserMessage(anthropic.NewTextBlock(e.text)))
				out = append(out, anthropic.NewAssistantMessage(anthropic.NewTextBlock(turnSynopsis(curTurn, e.text, turnDesc))))
			}
			continue
		}
		switch e.kind {
		case evUser:
			out = append(out, anthropic.NewUserMessage(anthropic.NewTextBlock(e.text)))
		case evAssistant:
			// Rebuild the assistant turn, dropping tool_use blocks for forgotten
			// entries so they pair with no orphaned tool_result.
			blocks := []anthropic.ContentBlockParamUnion{}
			for _, b := range e.asst.Content {
				switch v := b.AsAny().(type) {
				case anthropic.TextBlock:
					if v.Text != "" {
						blocks = append(blocks, anthropic.NewTextBlock(v.Text))
					}
				case anthropic.ToolUseBlock:
					if en := ws.get(v.ID); en != nil && en.forgotten {
						continue
					}
					blocks = append(blocks, anthropic.NewToolUseBlock(v.ID, v.Input, v.Name))
				}
			}
			if len(blocks) > 0 {
				out = append(out, anthropic.NewAssistantMessage(blocks...))
			}
		case evToolResults:
			blocks := []anthropic.ContentBlockParamUnion{}
			for _, id := range e.ids {
				en := ws.get(id)
				if en == nil || en.forgotten {
					continue
				}
				if latest[id] {
					blocks = append(blocks, anthropic.NewToolResultBlock(id, en.content, en.isErr))
				} else {
					// The description has moved to the standalone Manifest; the
					// result position keeps only a pointer so pairing stays valid.
					blocks = append(blocks, anthropic.NewToolResultBlock(id, collapsedPointer(en), false))
				}
			}
			if len(blocks) > 0 {
				out = append(out, anthropic.NewUserMessage(blocks...))
			}
		}
	}

	// The Manifest is its own always-current section rather than woven into the
	// tool_result positions. Attach it to the active prompt (the last message is
	// always a user message when the model is about to respond).
	if mani := manifestText(ws, latest, totalTurns); mani != "" {
		if n := len(out); n > 0 && out[n-1].Role == anthropic.MessageParamRoleUser {
			out[n-1].Content = append(out[n-1].Content, anthropic.NewTextBlock(mani))
		} else {
			out = append(out, anthropic.NewUserMessage(anthropic.NewTextBlock(mani)))
		}
	}
	return out
}

// fullTurnWindow is how many of the most recent Turns are sent in full; older
// Turns collapse to a one-line synopsis the model can recall on demand.
const fullTurnWindow = 2

// countTurns counts the user prompts in the log — one per Turn.
func countTurns(events []event) int {
	n := 0
	for _, e := range events {
		if e.kind == evUser {
			n++
		}
	}
	return n
}

// turnSynopsis is the one-line stand-in for a collapsed Turn's whole reply. It
// bootstraps from the prompt's first line and is upgraded by an explicit
// describe (carried in turnDesc); either way it points at the Turn's transcript
// so the model can recall the full exchange.
func turnSynopsis(turn int, prompt string, turnDesc map[int]string) string {
	desc := firstLine(prompt)
	if d, ok := turnDesc[turn]; ok && strings.TrimSpace(d) != "" {
		desc = d
	}
	return fmt.Sprintf("[Turn %d — %s — recall the full exchange: read %s]", turn, desc, turnPath(turn))
}

// turnPath is the transcript file for a Turn, relative to the Session directory.
func turnPath(turn int) string {
	return fmt.Sprintf("turns/%03d.md", turn)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// latestIDs returns the tool_use IDs whose results are the last thing in the
// log, so they are sent in full instead of collapsing into the Manifest.
func latestIDs(events []event) map[string]bool {
	latest := map[string]bool{}
	if n := len(events); n > 0 && events[n-1].kind == evToolResults {
		for _, id := range events[n-1].ids {
			latest[id] = true
		}
	}
	return latest
}

// collapsedPointer is the minimal content left in a collapsed result's
// tool_result position — just enough to point at its Manifest entry.
func collapsedPointer(en *entry) string {
	if en.ref != "" {
		return en.ref + " (see Working Set)"
	}
	return "(see Working Set)"
}

// manifestText renders the standalone Manifest: one line per live, non-latest
// entry, by which the model decides whether to recall an entry's full content.
func manifestText(ws *workingSet, latest map[string]bool, curTurn int) string {
	var lines []string
	for _, id := range ws.order {
		en := ws.byID[id]
		if en.forgotten || latest[id] {
			continue
		}
		lines = append(lines, en.manifestLine())
	}
	if len(lines) == 0 {
		return ""
	}
	header := fmt.Sprintf("Working Set (you are on Turn %d; describe a Turn by its number to improve its collapsed synopsis) — recall an entry by reading its file:\n", curTurn)
	return header + strings.Join(lines, "\n")
}

func (a *Agent) toolParams() []anthropic.ToolUnionParam {
	tools := []anthropic.ToolUnionParam{}
	for _, t := range a.tools {
		tools = append(tools, anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name:        t.Name,
			Description: anthropic.String(t.Description),
			InputSchema: t.InputSchema,
		}})
	}
	return tools
}

// handleTool runs a tool call, intercepting agent built-ins (forget) that need
// to mutate the working set, and dispatching everything else to the tool table.
func (a *Agent) handleTool(name string, input []byte) (string, bool) {
	ui.PrintToolCall(name, input)
	var content string
	var isErr bool
	switch name {
	case "forget":
		content, isErr = a.forget(input)
	case "describe":
		content, isErr = a.describe(input)
	default:
		content, isErr = a.runTool(name, input)
	}
	ui.PrintToolResult(content, isErr)
	return content, isErr
}

func (a *Agent) runTool(name string, input []byte) (string, bool) {
	for _, t := range a.tools {
		if t.Name == name {
			response, err := t.Function(input)
			if err != nil {
				return err.Error(), true
			}
			return response, false
		}
	}
	return "tool not found: " + name, true
}

// forget marks the entry with the given Manifest reference as forgotten, so
// buildPayload drops both its tool_use and tool_result from later payloads.
func (a *Agent) forget(input []byte) (string, bool) {
	var in struct {
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return err.Error(), true
	}
	en := a.ws.getByRef(in.Ref)
	if en == nil {
		return "no entry with reference " + in.Ref, true
	}
	en.forgotten = true
	return "forgot " + in.Ref, false
}

// describe attaches a one-line gist to an entry, replacing its Manifest
// Description (for a run Artifact, the bare command label until then).
func (a *Agent) describe(input []byte) (string, bool) {
	var in struct {
		Ref  string `json:"ref"`
		Turn int    `json:"turn"`
		Gist string `json:"gist"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return err.Error(), true
	}
	// A Turn target upgrades that Turn's collapsed synopsis; a ref target
	// upgrades a Working Set entry's Manifest Description.
	if in.Turn > 0 {
		a.turnDesc[in.Turn] = in.Gist
		return fmt.Sprintf("described Turn %d", in.Turn), false
	}
	en := a.ws.getByRef(in.Ref)
	if en == nil {
		return "no entry with reference " + in.Ref, true
	}
	en.desc = in.Gist
	return "described " + in.Ref, false
}

// record classifies a tool result into a working-set entry. A run's output is
// non-reproducible, so it is persisted to an Artifact file on disk and can be
// recalled later by reading that file.
func (a *Agent) record(id, toolName string, input []byte, content string, isErr bool) *entry {
	e := newEntry(id, toolName, input, content, isErr, a.turn)
	a.refSeq++
	e.ref = fmt.Sprintf("#%d", a.refSeq)
	if e.kind == kindRun {
		var in struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(input, &in)
		if p, err := a.writeArtifact(in.Command, content); err == nil {
			e.path = p
		}
	}
	return e
}

// artifactsRoot is the gitignored root, relative to the working directory, under
// which every Session's directory is created.
const (
	artifactsRoot  = ".agentic-artifacts"
	sessionsSubdir = "sessions"
)

// ensureDir lazily creates this Session's on-disk directory
// (.agentic-artifacts/sessions/<timestamp>/) and returns its path.
func (a *Agent) ensureDir() (string, error) {
	if a.sessionDir == "" {
		dir, err := newSessionDir()
		if err != nil {
			return "", err
		}
		a.sessionDir = dir
	}
	return a.sessionDir, nil
}

// newSessionDir creates a fresh per-Session directory named by the startup
// timestamp (lexicographically sortable), disambiguating same-second collisions.
func newSessionDir() (string, error) {
	base := filepath.Join(artifactsRoot, sessionsSubdir)
	id := time.Now().Format("20060102-150405")
	dir := filepath.Join(base, id)
	for i := 2; ; i++ {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			break
		}
		dir = filepath.Join(base, fmt.Sprintf("%s-%d", id, i))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// writeArtifact persists a run's command and output together to a new
// runs/NNN.txt file in the Session directory and returns its path.
func (a *Agent) writeArtifact(command, output string) (string, error) {
	dir, err := a.ensureDir()
	if err != nil {
		return "", err
	}
	runsDir := filepath.Join(dir, "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		return "", err
	}
	a.artifactSeq++
	p := filepath.Join(runsDir, fmt.Sprintf("%03d.txt", a.artifactSeq))
	if err := os.WriteFile(p, []byte(formatRunFile(command, output)), 0o644); err != nil {
		return "", err
	}
	return p, nil
}

// formatRunFile renders a run Artifact as its command (shell-prompt style — "$ "
// for the first line, "> " for continuations) followed by a blank line and the
// combined output, so the command travels with its result and a reader (or the
// resume parser) can recover the command from lines until the first unprefixed one.
func formatRunFile(command, output string) string {
	var b strings.Builder
	for i, line := range strings.Split(command, "\n") {
		if i == 0 {
			b.WriteString("$ ")
		} else {
			b.WriteString("> ")
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(output)
	return b.String()
}
