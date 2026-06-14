package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// fakeModel returns scripted assistant messages and records the payloads it saw,
// so tests can drive the loop and assert what would be sent to the server.
type fakeModel struct {
	replies []*anthropic.Message
	calls   [][]anthropic.MessageParam
}

func (f *fakeModel) Next(ctx context.Context, messages []anthropic.MessageParam, tools []anthropic.ToolUnionParam) (*anthropic.Message, error) {
	f.calls = append(f.calls, messages)
	reply := f.replies[0]
	f.replies = f.replies[1:]
	return reply, nil
}

func assistantMessage(t *testing.T, jsonStr string) *anthropic.Message {
	t.Helper()
	var m anthropic.Message
	if err := json.Unmarshal([]byte(jsonStr), &m); err != nil {
		t.Fatalf("unmarshal assistant message: %v", err)
	}
	return &m
}

// toolResultText returns the text of the first tool_result block in a message,
// or "" if there is none.
func toolResultText(m anthropic.MessageParam) string {
	for _, b := range m.Content {
		if b.OfToolResult != nil {
			for _, c := range b.OfToolResult.Content {
				if c.OfText != nil {
					return c.OfText.Text
				}
			}
		}
	}
	return ""
}

func scriptedInput(inputs ...string) func() (string, bool) {
	i := 0
	return func() (string, bool) {
		if i >= len(inputs) {
			return "", false
		}
		s := inputs[i]
		i++
		return s, true
	}
}

func TestBuildPayloadIdentity(t *testing.T) {
	ws := newWorkingSet()
	ws.put(&entry{id: "t1", content: "file contents here"})

	asst := assistantMessage(t, `{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"read","input":{"path":"main.go"}}]}`)
	events := []event{
		{kind: evUser, text: "read main.go"},
		{kind: evAssistant, asst: asst},
		{kind: evToolResults, ids: []string{"t1"}},
	}

	payload := buildPayload(events, ws)
	if len(payload) != 3 {
		t.Fatalf("payload has %d messages, want 3", len(payload))
	}
	if got := toolResultText(payload[2]); got != "file contents here" {
		t.Fatalf("tool result = %q, want the full content", got)
	}
}

func TestLoopExecutesToolAndResultEntersPayload(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	writeFile(t, p, "payload check")

	fm := &fakeModel{replies: []*anthropic.Message{
		assistantMessage(t, `{"role":"assistant","content":[{"type":"tool_use","id":"r1","name":"read","input":{"path":"`+p+`"}}]}`),
		assistantMessage(t, `{"role":"assistant","content":[{"type":"text","text":"done"}]}`),
	}}

	a := &Agent{
		model:          fm,
		getUserMessage: scriptedInput("read the file"),
		tools:          []ToolDefinition{ReadDefinition, EditDefinition, RunDefinition},
		ws:             newWorkingSet(),
	}
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(fm.calls) < 2 {
		t.Fatalf("model called %d times, want >= 2", len(fm.calls))
	}
	found := false
	for _, m := range fm.calls[1] {
		if toolResultText(m) == "payload check" {
			found = true
		}
	}
	if !found {
		t.Fatalf("second payload is missing the read result content")
	}
}
