// Package agent runs the chat loop and rewrites what is sent to the model each
// round from an internal event log plus the working set.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/sunfmin/agentic-go-cli/internal/tool"
	"github.com/sunfmin/agentic-go-cli/internal/ui"
)

// defaultModel is the Anthropic model the real Model uses and the name recorded
// in the Session index for provenance.
const defaultModel = anthropic.ModelClaudeOpus4_8

// Model is the seam over the Anthropic Messages API so the agent loop can be
// driven by a scripted fake in tests. Summarize is a separate one-shot call used
// to gist a Turn at collapse time (ADR-0006), kept off Next so it neither consumes
// the main conversation nor shows up in the loop's request log.
type Model interface {
	Next(ctx context.Context, messages []anthropic.MessageParam, tools []anthropic.ToolUnionParam) (*anthropic.Message, error)
	Summarize(ctx context.Context, transcript string) (string, error)
}

type anthropicModel struct {
	client *anthropic.Client

	// lastReq/lastResp hold the most recent exchange captured by the capture
	// middleware: the wire request as a curl-runnable command, and the server's
	// verbatim response body. The agent persists them to the Session's api/ dir.
	lastReq  []byte
	lastResp []byte
}

// NewAnthropicModel wraps a real Anthropic client as a Model.
func NewAnthropicModel(client *anthropic.Client) Model {
	return &anthropicModel{client: client}
}

func (m *anthropicModel) Next(ctx context.Context, messages []anthropic.MessageParam, tools []anthropic.ToolUnionParam) (*anthropic.Message, error) {
	m.lastReq, m.lastResp = nil, nil
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
	}, option.WithMiddleware(m.capture))
}

// capture records the exact request the SDK is about to send (rebuilt as a
// curl-runnable command) and the verbatim response body, so the agent can save
// each Round's exchange. A retry re-runs this and the final attempt wins.
func (m *anthropicModel) capture(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
	if curl, err := requestToCurl(req); err == nil {
		m.lastReq = curl
	}
	resp, err := next(req)
	if err != nil || resp == nil {
		return resp, err
	}
	// Drain the response so we hold the verbatim bytes, then hand the SDK a
	// fresh reader over the same bytes so its own decoding is unaffected.
	body, rerr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if rerr == nil {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		m.lastResp = body
	}
	return resp, err
}

// LastExchange returns the request/response captured during the most recent
// Next call. Either may be empty if the request never left the client.
func (m *anthropicModel) LastExchange() (request, response []byte) {
	return m.lastReq, m.lastResp
}

// Summarize gists one Turn's transcript into a single outcome-focused line. It is
// a plain one-shot request — no tools, no capture middleware (so it doesn't clobber
// the main exchange the agent persists), and a tight token budget.
func (m *anthropicModel) Summarize(ctx context.Context, transcript string) (string, error) {
	msg, err := m.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     defaultModel,
		MaxTokens: 256,
		System: []anthropic.TextBlockParam{
			{Text: "You are Claude Code, Anthropic's official CLI for Claude."},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(summaryPrompt(transcript))),
		},
	})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, blk := range msg.Content {
		if t, ok := blk.AsAny().(anthropic.TextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	return strings.TrimSpace(b.String()), nil
}

// summaryPrompt asks for the gist that stands in for a collapsed Turn: its outcome,
// not the request, in one line the model can scan to decide whether to recall it.
func summaryPrompt(transcript string) string {
	return "Summarize this Turn of a coding session in ONE concise line — the outcome " +
		"and any key facts a future you would need to continue, not the original request. " +
		"Reply with only that line, no preamble.\n\n" + transcript
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

// collapsedResult is the self-contained one-liner that stands in for a superseded
// in-window tool result. A read renders as a live reference (no content), so the
// model never reasons against a stale copy; a run points at its recallable output.
func (e *entry) collapsedResult() string {
	switch e.kind {
	case kindRead:
		return fmt.Sprintf("[read %s @turn %d — re-read for current contents]", e.path, e.turn)
	case kindRun:
		if e.path != "" {
			return fmt.Sprintf("[%s — recall: read %s]", e.desc, e.path)
		}
		return fmt.Sprintf("[%s]", e.desc)
	}
	return fmt.Sprintf("[%s — %s]", e.name, e.desc)
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

	events         []event
	ws             *workingSet
	turn           int            // Turn: one user prompt plus the entire AI reply that follows
	round          int            // Round: one model response within a Turn
	turnDesc       map[int]string // one-line Descriptions for collapsed Turns, by turn number
	forgottenTurns map[int]bool   // collapsed Turns the model has forgotten, by turn number
	summaryTried   map[int]bool   // collapsed Turns whose LLM summary has been attempted this session

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
		forgottenTurns: map[int]bool{},
		summaryTried:   map[int]bool{},
	}
}

func (a *Agent) Run(ctx context.Context) error {
	readUserInput := true
	for {
		if readUserInput {
			ui.SetManifest(a.manifestEntries())
			userInput, ok := a.getUserMessage()
			if !ok {
				break
			}
			a.turn++
			a.events = append(a.events, event{kind: evUser, text: userInput})
		}

		a.summarizeCollapsedTurns(ctx)
		message, err := a.model.Next(ctx, buildPayload(a.events, a.ws, a.turnDesc, a.forgottenTurns), a.toolParams())
		if err != nil {
			return err
		}
		a.events = append(a.events, event{kind: evAssistant, asst: message})
		a.round++

		ids := []string{}
		for _, block := range message.Content {
			switch variant := block.AsAny().(type) {
			case anthropic.TextBlock:
				ui.PrintAssistant(variant.Text)
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
		if err := a.persistAPIExchange(); err != nil {
			fmt.Fprintln(os.Stderr, "warning: failed to persist API exchange:", err)
		}
	}
	return nil
}

// buildPayload reconstructs the messages to send to the model from the event log.
// Recent Turns are sent in full; every older Turn collapses to a one-line synopsis
// (its tool calls folding in), and a forgotten collapsed Turn is dropped entirely.
// Within the recent window the latest tool result is full and superseded results
// collapse to a self-contained one-liner, keeping tool_use/tool_result pairing
// intact so the request stays valid. There is no standalone Manifest block: the
// Manifest is exactly the collapsed-Turn synopses, woven in where the Turns sat.
func buildPayload(events []event, ws *workingSet, turnDesc map[int]string, forgotten map[int]bool) []anthropic.MessageParam {
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
		// their tool_use/tool_result plumbing dropped wholesale and their tool
		// calls folding into the synopsis; the full exchange is recalled by
		// reading the Turn's transcript. A forgotten collapsed Turn is dropped
		// entirely — prompt and synopsis both.
		if curTurn <= totalTurns-fullTurnWindow {
			if forgotten[curTurn] {
				continue
			}
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
					// A superseded in-window result collapses to a self-contained
					// one-liner in place — enough to know what it was, recallable
					// from the live file or the Turn's transcript.
					blocks = append(blocks, anthropic.NewToolResultBlock(id, en.collapsedResult(), false))
				}
			}
			if len(blocks) > 0 {
				out = append(out, anthropic.NewUserMessage(blocks...))
			}
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

// forget drops a collapsed Turn from the Manifest by its number, so buildPayload
// stops sending its synopsis at all. Only collapsed Turns can be forgotten.
func (a *Agent) forget(input []byte) (string, bool) {
	var in struct {
		Turn int `json:"turn"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return err.Error(), true
	}
	if in.Turn <= 0 {
		return "forget requires a turn number", true
	}
	a.forgottenTurns[in.Turn] = true
	return fmt.Sprintf("forgot Turn %d", in.Turn), false
}

// describe upgrades a collapsed Turn's one-line Description, so its synopsis reads
// better than the prompt's first line.
func (a *Agent) describe(input []byte) (string, bool) {
	var in struct {
		Turn int    `json:"turn"`
		Gist string `json:"gist"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return err.Error(), true
	}
	if in.Turn <= 0 {
		return "describe requires a turn number", true
	}
	a.turnDesc[in.Turn] = in.Gist
	return fmt.Sprintf("described Turn %d", in.Turn), false
}

// manifestEntries is the Manifest surfaced in the UI panel: one entry per
// collapsed, non-forgotten Turn, carrying the same Description the payload sends
// (the upgraded gist, else the prompt's first line). Oldest first.
func (a *Agent) manifestEntries() []ui.ManifestEntry {
	var es []ui.ManifestEntry
	for t := 1; t <= a.turn-fullTurnWindow; t++ {
		if a.forgottenTurns[t] {
			continue
		}
		desc := a.turnDesc[t]
		if strings.TrimSpace(desc) == "" {
			desc = firstLine(a.turnPrompt(t))
		}
		es = append(es, ui.ManifestEntry{Turn: t, Desc: desc})
	}
	return es
}

// turnPrompt returns the user prompt that opened the given Turn.
func (a *Agent) turnPrompt(turn int) string {
	cur := 0
	for _, e := range a.events {
		if e.kind == evUser {
			cur++
			if cur == turn {
				return e.text
			}
		}
	}
	return ""
}

// summarizeCollapsedTurns gives every newly-collapsed Turn that still lacks a
// Description an LLM-generated one (ADR-0006), at the moment it falls outside the
// recent window — when its detail is about to be dropped. Each Turn is attempted
// once per session; a failed or empty summary leaves the Turn to fall back to the
// prompt's first line, so a collapse never blocks the loop.
func (a *Agent) summarizeCollapsedTurns(ctx context.Context) {
	for t := 1; t <= a.turn-fullTurnWindow; t++ {
		if a.summaryTried[t] || a.forgottenTurns[t] {
			continue
		}
		a.summaryTried[t] = true
		if strings.TrimSpace(a.turnDesc[t]) != "" {
			continue // already described (by the model, or restored on resume)
		}
		input := a.turnSummaryInput(t)
		if input == "" {
			continue
		}
		gist, err := a.model.Summarize(ctx, input)
		if err != nil || strings.TrimSpace(gist) == "" {
			continue // fall back to the prompt's first line
		}
		a.turnDesc[t] = strings.TrimSpace(gist)
	}
}

// turnSummaryInput renders one Turn for summarization: its prompt, the assistant's
// prose, and each tool call paired with its full result — so the gist reflects what
// the tools produced, not just that they ran. Long results are truncated to keep
// the summary request bounded.
func (a *Agent) turnSummaryInput(turn int) string {
	var b strings.Builder
	cur := 0
	for _, e := range a.events {
		if e.kind == evUser {
			cur++
		}
		if cur != turn {
			continue
		}
		switch e.kind {
		case evUser:
			b.WriteString("User: " + e.text + "\n\n")
		case evAssistant:
			if e.asst == nil {
				continue
			}
			for _, blk := range e.asst.Content {
				switch v := blk.AsAny().(type) {
				case anthropic.TextBlock:
					if strings.TrimSpace(v.Text) != "" {
						b.WriteString("Assistant: " + v.Text + "\n\n")
					}
				case anthropic.ToolUseBlock:
					b.WriteString("Tool call: " + v.Name + " " + shortInput(v.Input) + "\n")
				}
			}
		case evToolResults:
			for _, id := range e.ids {
				en := a.ws.get(id)
				if en == nil {
					continue
				}
				b.WriteString(fmt.Sprintf("Tool result (%s):\n%s\n\n", en.name, truncate(en.content, 4000)))
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// truncate caps a tool result at max runes for the summary input, marking the cut.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "\n… (truncated)"
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
