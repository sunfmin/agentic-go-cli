// Package agent runs the chat loop and rewrites what is sent to the model each
// round from an internal event log plus the working set.
package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/sunfmin/agentic-go-cli/internal/tool"
	"github.com/sunfmin/agentic-go-cli/internal/ui"
)

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
		Model:     anthropic.ModelClaudeOpus4_8,
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
	id      string
	kind    entryKind
	name    string // short label, e.g. "read main.go" or "run"
	path    string // for read/edit entries: the file path
	desc    string // Description shown when collapsed into the Manifest
	content string // full tool output (sent only while this is the latest result)
	isErr   bool
	turn    int
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
	if e.kind == kindRead {
		return fmt.Sprintf("[read %s @turn %d — re-read for current contents]", e.path, e.turn)
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

	events []event
	ws     *workingSet
	turn   int
}

// New builds an agent over a model, an input source, and a set of tools.
func New(model Model, getUserMessage func() (string, bool), tools []tool.ToolDefinition) *Agent {
	return &Agent{
		model:          model,
		getUserMessage: getUserMessage,
		tools:          tools,
		ws:             newWorkingSet(),
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
			a.events = append(a.events, event{kind: evUser, text: userInput})
		}

		message, err := a.model.Next(ctx, buildPayload(a.events, a.ws), a.toolParams())
		if err != nil {
			return err
		}
		a.events = append(a.events, event{kind: evAssistant, asst: message})
		a.turn++

		ids := []string{}
		for _, block := range message.Content {
			switch variant := block.AsAny().(type) {
			case anthropic.TextBlock:
				fmt.Printf("⏺ %s\n", variant.Text)
			case anthropic.ToolUseBlock:
				content, isErr := a.executeTool(variant.Name, []byte(variant.Input))
				a.ws.put(newEntry(variant.ID, variant.Name, []byte(variant.Input), content, isErr, a.turn))
				ids = append(ids, variant.ID)
			}
		}
		if len(ids) == 0 {
			readUserInput = true
			continue
		}
		a.events = append(a.events, event{kind: evToolResults, ids: ids})
		readUserInput = false
	}
	return nil
}

// buildPayload reconstructs the messages to send to the model from the event log
// and the working set. Only the most recent tool result is sent in full; every
// older tool result collapses to its one-line Manifest entry, while keeping the
// tool_use/tool_result pairing intact so the request stays valid.
func buildPayload(events []event, ws *workingSet) []anthropic.MessageParam {
	// The latest results are full only when they are literally the last thing in
	// the log — i.e. the model is about to respond to them. Once a user or
	// assistant turn follows, they too collapse into the Manifest.
	latest := map[string]bool{}
	if n := len(events); n > 0 && events[n-1].kind == evToolResults {
		for _, id := range events[n-1].ids {
			latest[id] = true
		}
	}

	out := []anthropic.MessageParam{}
	for _, e := range events {
		switch e.kind {
		case evUser:
			out = append(out, anthropic.NewUserMessage(anthropic.NewTextBlock(e.text)))
		case evAssistant:
			out = append(out, e.asst.ToParam())
		case evToolResults:
			blocks := []anthropic.ContentBlockParamUnion{}
			for _, id := range e.ids {
				en := ws.get(id)
				if en == nil {
					continue
				}
				if latest[id] {
					blocks = append(blocks, anthropic.NewToolResultBlock(id, en.content, en.isErr))
				} else {
					blocks = append(blocks, anthropic.NewToolResultBlock(id, en.manifestLine(), false))
				}
			}
			out = append(out, anthropic.NewUserMessage(blocks...))
		}
	}
	return out
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

func (a *Agent) executeTool(name string, input []byte) (string, bool) {
	var toolDef tool.ToolDefinition
	found := false
	for _, t := range a.tools {
		if t.Name == name {
			toolDef = t
			found = true
			break
		}
	}
	if !found {
		return "tool not found: " + name, true
	}

	ui.PrintToolCall(name, input)
	response, err := toolDef.Function(input)
	if err != nil {
		ui.PrintToolResult(err.Error(), true)
		return err.Error(), true
	}
	ui.PrintToolResult(response, false)
	return response, false
}
