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
	a.sessionDir = t.TempDir()
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(fm.calls) < 3 {
		t.Fatalf("model called %d times, want >= 3", len(fm.calls))
	}
	third := fm.calls[2]
	if got := findToolResult(third, "c1"); !strings.Contains(got, "run: echo AAAA") || got == "AAAA\n" {
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
	a.sessionDir = t.TempDir()
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
	if dir := filepath.Base(filepath.Dir(en.path)); dir != "runs" {
		t.Fatalf("artifact stored in %q, want a runs/ dir", dir)
	}
	data, err := os.ReadFile(en.path)
	if err != nil {
		t.Fatalf("artifact file: %v", err)
	}
	// The command travels with its output in the same file.
	if want := "$ echo HELLO\n\nHELLO\n"; string(data) != want {
		t.Fatalf("artifact content = %q, want %q", data, want)
	}

	// Recall without re-running: read the Artifact file via the read tool.
	recalled, err := tool.ReadDefinition.Function(readInput("path", en.path))
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if !strings.Contains(recalled, "$ echo HELLO") || !strings.Contains(recalled, "HELLO\n") {
		t.Fatalf("recalled = %q, want it to contain the command and its output", recalled)
	}
}

func readInput(key, value string) []byte {
	b, _ := json.Marshal(map[string]string{key: value})
	return b
}

// hasToolUse reports whether any assistant message in the payload contains a
// tool_use block with the given ID.
func hasToolUse(payload []anthropic.MessageParam, id string) bool {
	for _, m := range payload {
		for _, b := range m.Content {
			if b.OfToolUse != nil && b.OfToolUse.ID == id {
				return true
			}
		}
	}
	return false
}

func TestBuildPayloadForgetDropsBothHalves(t *testing.T) {
	ws := newWorkingSet()
	ws.put(&entry{id: "a", ref: "#1", kind: kindRun, desc: "run: go build", content: "buildout", forgotten: true})
	ws.put(&entry{id: "b", ref: "#2", kind: kindRun, desc: "run: go test", content: "testout"})

	a1 := assistantMessage(t, `{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"run","input":{"command":"go build"}}]}`)
	a2 := assistantMessage(t, `{"role":"assistant","content":[{"type":"tool_use","id":"b","name":"run","input":{"command":"go test"}}]}`)
	events := []event{
		{kind: evUser, text: "x"},
		{kind: evAssistant, asst: a1},
		{kind: evToolResults, ids: []string{"a"}},
		{kind: evAssistant, asst: a2},
		{kind: evToolResults, ids: []string{"b"}},
	}

	payload := buildPayload(events, ws)
	if hasToolUse(payload, "a") {
		t.Fatalf("forgotten tool_use 'a' is still present")
	}
	if findToolResult(payload, "a") != "" {
		t.Fatalf("forgotten tool_result 'a' is still present")
	}
	if !hasToolUse(payload, "b") || findToolResult(payload, "b") != "testout" {
		t.Fatalf("kept entry 'b' should remain in full")
	}
	for _, m := range payload {
		if len(m.Content) == 0 {
			t.Fatalf("payload contains an empty message")
		}
	}
}

func TestLoopDescribeReplacesRunLabelWithGist(t *testing.T) {
	fm := &fakeModel{replies: []*anthropic.Message{
		assistantMessage(t, `{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"run","input":{"command":"echo data"}}]}`),
		assistantMessage(t, `{"role":"assistant","content":[{"type":"tool_use","id":"d1","name":"describe","input":{"ref":"#1","gist":"echoed the data"}}]}`),
		assistantMessage(t, `{"role":"assistant","content":[{"type":"text","text":"done"}]}`),
	}}

	a := New(fm, scriptedInput("go"), []tool.ToolDefinition{tool.RunDefinition, tool.ForgetDefinition, tool.DescribeDefinition})
	a.sessionDir = t.TempDir()
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	last := fm.calls[len(fm.calls)-1]
	got := findToolResult(last, "c1")
	if !strings.Contains(got, "echoed the data") {
		t.Fatalf("collapsed run = %q, want it to show the gist", got)
	}
	if strings.Contains(got, "run: echo data") {
		t.Fatalf("collapsed run = %q, should no longer show the command label", got)
	}
}

func TestDescribeFallsBackToCommandLabel(t *testing.T) {
	// An undescribed run keeps its command label in the Manifest.
	ws := newWorkingSet()
	ws.put(&entry{id: "a", ref: "#1", kind: kindRun, desc: "run: go vet", content: "out"})
	ws.put(&entry{id: "b", ref: "#2", kind: kindRun, desc: "run: go test", content: "fresh"})
	a1 := assistantMessage(t, `{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"run","input":{"command":"go vet"}}]}`)
	a2 := assistantMessage(t, `{"role":"assistant","content":[{"type":"tool_use","id":"b","name":"run","input":{"command":"go test"}}]}`)
	events := []event{
		{kind: evAssistant, asst: a1},
		{kind: evToolResults, ids: []string{"a"}},
		{kind: evAssistant, asst: a2},
		{kind: evToolResults, ids: []string{"b"}},
	}
	if got := findToolResult(buildPayload(events, ws), "a"); !strings.Contains(got, "run: go vet") {
		t.Fatalf("undescribed run = %q, want the command label fallback", got)
	}
}

func TestSessionPersistedToDisk(t *testing.T) {
	fm := &fakeModel{replies: []*anthropic.Message{
		assistantMessage(t, `{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"run","input":{"command":"echo hi"}}]}`),
		assistantMessage(t, `{"role":"assistant","content":[{"type":"tool_use","id":"d1","name":"describe","input":{"ref":"#1","gist":"printed hi"}}]}`),
		assistantMessage(t, `{"role":"assistant","content":[{"type":"text","text":"done"}]}`),
	}}

	a := New(fm, scriptedInput("go"), []tool.ToolDefinition{tool.RunDefinition, tool.DescribeDefinition})
	a.sessionDir = t.TempDir()
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	// session.json is gone; the lean index.json takes its place.
	if _, err := os.Stat(filepath.Join(a.sessionDir, "session.json")); !os.IsNotExist(err) {
		t.Fatalf("session.json should no longer be written")
	}
	data, err := os.ReadFile(filepath.Join(a.sessionDir, indexFileName))
	if err != nil {
		t.Fatalf("index file not written: %v", err)
	}
	// The index carries no tool content — output lives in runs/, prose in turns/.
	if strings.Contains(string(data), `"content"`) {
		t.Fatalf("index.json leaked tool content:\n%s", data)
	}
	var st indexState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("index JSON: %v", err)
	}
	if st.Turn != 1 {
		t.Fatalf("index turn = %d, want 1", st.Turn)
	}
	var runEntry *entryIndex
	for i := range st.Entries {
		if st.Entries[i].ID == "c1" {
			runEntry = &st.Entries[i]
		}
	}
	if runEntry == nil || runEntry.Kind != "run" {
		t.Fatalf("working set missing the run entry")
	}
	// describe upgraded the entry's Description to the gist.
	if runEntry.Desc != "printed hi" {
		t.Fatalf("run entry desc = %q, want the gist %q", runEntry.Desc, "printed hi")
	}
}

func TestLoopForgetRemovesEntryFromPayload(t *testing.T) {
	fm := &fakeModel{replies: []*anthropic.Message{
		assistantMessage(t, `{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"run","input":{"command":"echo X"}}]}`),
		assistantMessage(t, `{"role":"assistant","content":[{"type":"tool_use","id":"f1","name":"forget","input":{"ref":"#1"}}]}`),
		assistantMessage(t, `{"role":"assistant","content":[{"type":"text","text":"done"}]}`),
	}}

	a := New(fm, scriptedInput("go"), []tool.ToolDefinition{tool.ReadDefinition, tool.EditDefinition, tool.RunDefinition, tool.ForgetDefinition})
	a.sessionDir = t.TempDir()
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if !a.ws.get("c1").forgotten {
		t.Fatalf("entry c1 should be marked forgotten")
	}
	last := fm.calls[len(fm.calls)-1]
	if hasToolUse(last, "c1") {
		t.Fatalf("forgotten tool_use c1 still in payload (orphan risk)")
	}
	if findToolResult(last, "c1") != "" {
		t.Fatalf("forgotten tool_result c1 still in payload")
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

	a := New(fm, scriptedInput("read the file"), []tool.ToolDefinition{tool.ReadDefinition, tool.EditDefinition, tool.RunDefinition})
	a.sessionDir = t.TempDir()
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

func TestFormatRunFileCarriesCommand(t *testing.T) {
	// A multi-line command round-trips: "$ " first line, "> " continuations,
	// a blank line, then the output (which may itself contain blank lines).
	got := formatRunFile("git add .\ngit commit -m x", "ok\n\ndone\n")
	want := "$ git add .\n> git commit -m x\n\nok\n\ndone\n"
	if got != want {
		t.Fatalf("formatRunFile = %q, want %q", got, want)
	}
}

func TestTurnFileWrittenWithFrontmatterAndBody(t *testing.T) {
	fm := &fakeModel{replies: []*anthropic.Message{
		assistantMessage(t, `{"role":"assistant","content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"c1","name":"run","input":{"command":"echo hi"}}]}`),
		assistantMessage(t, `{"role":"assistant","content":[{"type":"text","text":"all done"}]}`),
	}}
	a := New(fm, scriptedInput("what changed?"), []tool.ToolDefinition{tool.RunDefinition})
	a.sessionDir = t.TempDir()
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	// One user exchange => Turn 1; two model responses => Round counter 2.
	if a.turn != 1 {
		t.Fatalf("turn = %d, want 1", a.turn)
	}
	if a.round != 2 {
		t.Fatalf("round = %d, want 2", a.round)
	}

	data, err := os.ReadFile(filepath.Join(a.sessionDir, "turns", "001.md"))
	if err != nil {
		t.Fatalf("turn file: %v", err)
	}
	s := string(data)
	// Frontmatter: structure only (per-Round refs; the prose-only round is empty).
	for _, want := range []string{"turn: 1", "rounds:", `- ["#1"]`, "- []"} {
		if !strings.Contains(s, want) {
			t.Fatalf("frontmatter missing %q:\n%s", want, s)
		}
	}
	// Body: readable transcript with the terminal's markers.
	for _, want := range []string{"❯ what changed?", "⏺ checking", "● run(echo hi)", "⏺ all done"} {
		if !strings.Contains(s, want) {
			t.Fatalf("body missing %q:\n%s", want, s)
		}
	}
}

func TestNewSessionDirsDoNotClobber(t *testing.T) {
	t.Chdir(t.TempDir())
	d1, err := newSessionDir()
	if err != nil {
		t.Fatal(err)
	}
	d2, err := newSessionDir()
	if err != nil {
		t.Fatal(err)
	}
	if d1 == d2 {
		t.Fatalf("two Sessions share a directory: %q", d1)
	}
	for _, d := range []string{d1, d2} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Fatalf("session dir %q not created", d)
		}
	}
}
