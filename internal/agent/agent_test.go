package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/sunfmin/agentic-go-cli/internal/tool"
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

// toolResultText returns the text of the first tool_result block in a message.
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

// findToolResult returns the text of the tool_result with the given tool_use ID
// across a whole payload, or "" if absent.
func findToolResult(payload []anthropic.MessageParam, id string) string {
	for _, m := range payload {
		for _, b := range m.Content {
			if b.OfToolResult != nil && b.OfToolResult.ToolUseID == id {
				for _, c := range b.OfToolResult.Content {
					if c.OfText != nil {
						return c.OfText.Text
					}
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

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildPayloadLatestResultFull(t *testing.T) {
	ws := newWorkingSet()
	ws.put(&entry{id: "t1", kind: kindRead, path: "main.go", content: "file contents here"})

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

func TestBuildPayloadCollapsesOlderResults(t *testing.T) {
	ws := newWorkingSet()
	ws.put(&entry{id: "a", kind: kindRun, name: "run", desc: "run: go build", content: "lots of build output"})
	ws.put(&entry{id: "b", kind: kindRun, name: "run", desc: "run: go test", content: "FRESH test output"})

	a1 := assistantMessage(t, `{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"run","input":{"command":"go build"}}]}`)
	a2 := assistantMessage(t, `{"role":"assistant","content":[{"type":"tool_use","id":"b","name":"run","input":{"command":"go test"}}]}`)
	events := []event{
		{kind: evUser, text: "build then test"},
		{kind: evAssistant, asst: a1},
		{kind: evToolResults, ids: []string{"a"}},
		{kind: evAssistant, asst: a2},
		{kind: evToolResults, ids: []string{"b"}},
	}

	payload := buildPayload(events, ws)
	if got := findToolResult(payload, "a"); got != "[run: go build]" {
		t.Fatalf("older result = %q, want the collapsed Manifest line", got)
	}
	if got := findToolResult(payload, "b"); got != "FRESH test output" {
		t.Fatalf("latest result = %q, want the full content", got)
	}
}

func TestBuildPayloadCollapsedReadIsLiveReference(t *testing.T) {
	ws := newWorkingSet()
	ws.put(&entry{id: "r1", kind: kindRead, path: "main.go", turn: 2, content: "OLD CONTENT MUST NOT APPEAR"})
	ws.put(&entry{id: "r2", kind: kindRead, path: "other.go", turn: 5, content: "fresh"})

	a1 := assistantMessage(t, `{"role":"assistant","content":[{"type":"tool_use","id":"r1","name":"read","input":{"path":"main.go"}}]}`)
	a2 := assistantMessage(t, `{"role":"assistant","content":[{"type":"tool_use","id":"r2","name":"read","input":{"path":"other.go"}}]}`)
	events := []event{
		{kind: evAssistant, asst: a1},
		{kind: evToolResults, ids: []string{"r1"}},
		{kind: evAssistant, asst: a2},
		{kind: evToolResults, ids: []string{"r2"}},
	}

	payload := buildPayload(events, ws)
	ref := findToolResult(payload, "r1")
	if ref != "[read main.go @turn 2 — re-read for current contents]" {
		t.Fatalf("collapsed read = %q, want a live reference", ref)
	}
	if strings.Contains(ref, "OLD CONTENT") {
		t.Fatalf("collapsed read leaked stale content: %q", ref)
	}
}

func TestLoopCollapsesPriorRunResult(t *testing.T) {
	fm := &fakeModel{replies: []*anthropic.Message{
		assistantMessage(t, `{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"run","input":{"command":"echo AAAA"}}]}`),
		assistantMessage(t, `{"role":"assistant","content":[{"type":"tool_use","id":"c2","name":"run","input":{"command":"echo BBBB"}}]}`),
		assistantMessage(t, `{"role":"assistant","content":[{"type":"text","text":"done"}]}`),
	}}

	a := New(fm, scriptedInput("go"), []tool.ToolDefinition{tool.ReadDefinition, tool.EditDefinition, tool.RunDefinition})
	a.artifactsDir = t.TempDir()
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(fm.calls) < 3 {
		t.Fatalf("model called %d times, want >= 3", len(fm.calls))
	}
	third := fm.calls[2]
	if got := findToolResult(third, "c1"); !strings.HasPrefix(got, "[run: echo AAAA") || got == "AAAA\n" {
		t.Fatalf("prior run result = %q, want collapsed Manifest line", got)
	}
	if got := findToolResult(third, "c2"); got != "BBBB\n" {
		t.Fatalf("latest run result = %q, want full output", got)
	}
}

func TestRunArtifactStoredAndRecallable(t *testing.T) {
	fm := &fakeModel{replies: []*anthropic.Message{
		assistantMessage(t, `{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"run","input":{"command":"echo HELLO"}}]}`),
		assistantMessage(t, `{"role":"assistant","content":[{"type":"text","text":"ok"}]}`),
	}}

	a := New(fm, scriptedInput("run it"), []tool.ToolDefinition{tool.ReadDefinition, tool.EditDefinition, tool.RunDefinition})
	a.artifactsDir = t.TempDir()
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	en := a.ws.get("c1")
	if en == nil || en.kind != kindRun {
		t.Fatalf("missing run entry")
	}
	if en.path == "" {
		t.Fatalf("run entry has no Artifact path")
	}
	data, err := os.ReadFile(en.path)
	if err != nil {
		t.Fatalf("artifact file: %v", err)
	}
	if string(data) != "HELLO\n" {
		t.Fatalf("artifact content = %q, want %q", data, "HELLO\n")
	}

	// Recall without re-running: read the Artifact file via the read tool.
	recalled, err := tool.ReadDefinition.Function(readInput("path", en.path))
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if recalled != "HELLO\n" {
		t.Fatalf("recalled = %q, want %q", recalled, "HELLO\n")
	}
}

func readInput(key, value string) []byte {
	b, _ := json.Marshal(map[string]string{key: value})
	return b
}

func TestLoopExecutesToolAndResultEntersPayload(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	writeFile(t, p, "payload check")

	fm := &fakeModel{replies: []*anthropic.Message{
		assistantMessage(t, `{"role":"assistant","content":[{"type":"tool_use","id":"r1","name":"read","input":{"path":"`+p+`"}}]}`),
		assistantMessage(t, `{"role":"assistant","content":[{"type":"text","text":"done"}]}`),
	}}

	a := New(fm, scriptedInput("read the file"), []tool.ToolDefinition{tool.ReadDefinition, tool.EditDefinition, tool.RunDefinition})
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
