package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/sunfmin/agentic-go-cli/internal/tool"
)

// fakeModel returns scripted assistant messages and records the payloads it saw,
// so tests can drive the loop and assert what would be sent to the server. Summarize
// is scripted separately (summary / summaryErr) and records the inputs it was given.
type fakeModel struct {
	replies    []*anthropic.Message
	calls      [][]anthropic.MessageParam
	summary    string
	summaryErr error
	summarized []string
}

func (f *fakeModel) Next(ctx context.Context, messages []anthropic.MessageParam, tools []anthropic.ToolUnionParam) (*anthropic.Message, error) {
	f.calls = append(f.calls, messages)
	reply := f.replies[0]
	f.replies = f.replies[1:]
	return reply, nil
}

func (f *fakeModel) Summarize(ctx context.Context, transcript string) (string, error) {
	f.summarized = append(f.summarized, transcript)
	return f.summary, f.summaryErr
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

	payload := buildPayload(events, ws, nil, nil)
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

	payload := buildPayload(events, ws, nil, nil)
	// The superseded result collapses to a self-contained one-liner in place — no
	// standalone Manifest block, no pointer to chase.
	if got := findToolResult(payload, "a"); got != "[run: go build]" {
		t.Fatalf("older result = %q, want the inline collapsed one-liner", got)
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

	payload := buildPayload(events, ws, nil, nil)
	// The collapsed read is a live reference in its own result position — never its
	// stale content.
	if got := findToolResult(payload, "r1"); got != "[read main.go @turn 2 — re-read for current contents]" {
		t.Fatalf("collapsed read = %q, want the live reference", got)
	}
	if payloadContains(payload, "OLD CONTENT") {
		t.Fatalf("collapsed read leaked stale content")
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
	// The prior run collapses to a self-contained one-liner in its own result
	// position — the command label plus a recall pointer, not a separate Manifest.
	if got := findToolResult(third, "c1"); !strings.Contains(got, "run: echo AAAA") || !strings.Contains(got, "recall: read") {
		t.Fatalf("prior run result = %q, want the inline collapsed one-liner", got)
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

func TestDescribeUpgradesTurnSynopsis(t *testing.T) {
	dir := t.TempDir()
	fm := &fakeModel{replies: []*anthropic.Message{
		assistantMessage(t, `{"role":"assistant","content":[{"type":"text","text":"done A"}]}`), // Turn 1
		assistantMessage(t, `{"role":"assistant","content":[{"type":"text","text":"done B"}]}`), // Turn 2
		// Turn 3: describe Turn 1, then answer.
		assistantMessage(t, `{"role":"assistant","content":[{"type":"tool_use","id":"d1","name":"describe","input":{"turn":1,"gist":"handled task A"}}]}`),
		assistantMessage(t, `{"role":"assistant","content":[{"type":"text","text":"summary"}]}`),
	}}
	a := New(fm, scriptedInput("do A", "do B", "summarize"),
		[]tool.ToolDefinition{tool.RunDefinition, tool.DescribeDefinition})
	a.sessionDir = dir
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	// The collapsed Turn 1 shows the upgraded gist, not the prompt's first line.
	last := fm.calls[len(fm.calls)-1]
	if !payloadContains(last, "[Turn 1 — handled task A") {
		t.Fatalf("collapsed Turn 1 should show the upgraded gist")
	}
	if payloadContains(last, "[Turn 1 — do A") {
		t.Fatalf("collapsed Turn 1 still shows the bootstrap, not the upgrade")
	}

	// It persists in index.json.
	data, err := os.ReadFile(filepath.Join(dir, indexFileName))
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	var st indexState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("index JSON: %v", err)
	}
	if st.Turns[1] != "handled task A" {
		t.Fatalf("turn description not persisted; got %v", st.Turns)
	}

	// And it survives resume.
	a2 := New(&fakeModel{}, scriptedInput(), []tool.ToolDefinition{tool.DescribeDefinition})
	if err := a2.Load(dir); err != nil {
		t.Fatalf("load: %v", err)
	}
	if a2.turnDesc[1] != "handled task A" {
		t.Fatalf("turn description not restored on resume; got %v", a2.turnDesc)
	}
}

func TestOldTurnsCollapseToSynopsis(t *testing.T) {
	// Three Turns; with the window of 2, Turn 1 collapses on the Turn-3 request.
	fm := &fakeModel{replies: []*anthropic.Message{
		assistantMessage(t, `{"role":"assistant","content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"c1","name":"run","input":{"command":"echo one"}}]}`),
		assistantMessage(t, `{"role":"assistant","content":[{"type":"text","text":"did one"}]}`),
		assistantMessage(t, `{"role":"assistant","content":[{"type":"text","text":"answer two"}]}`),
		assistantMessage(t, `{"role":"assistant","content":[{"type":"text","text":"answer three"}]}`),
	}}
	a := New(fm, scriptedInput("first question", "second question", "third question"),
		[]tool.ToolDefinition{tool.RunDefinition})
	a.sessionDir = t.TempDir()
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	// The model call for Turn 3 is the last one.
	last := fm.calls[len(fm.calls)-1]

	// Turn 1 is collapsed: a one-line synopsis pointing at its transcript, with
	// its tool plumbing dropped entirely.
	if !payloadContains(last, "Turn 1") || !payloadContains(last, turnPath(1)) {
		t.Fatalf("Turn 1 should collapse to a synopsis pointing at its transcript")
	}
	if hasToolUse(last, "c1") {
		t.Fatalf("collapsed Turn 1 still carries its tool_use plumbing")
	}
	// Its prompt is kept verbatim; the bootstrap description is the prompt's first line.
	if !payloadContains(last, "first question") {
		t.Fatalf("collapsed Turn should keep its user prompt verbatim")
	}
	// Recent Turns (2 and 3) are still present in full.
	if !payloadContains(last, "second question") || !payloadContains(last, "answer two") {
		t.Fatalf("recent Turn 2 should remain full")
	}
	if !payloadContains(last, "third question") {
		t.Fatalf("current Turn 3 prompt should be present")
	}
	// The collapsed Turn's tool calls fold into its synopsis — no independent
	// Artifact entry, and no command, survives the collapse.
	if payloadContains(last, "echo one") {
		t.Fatalf("collapsed Turn's tool plumbing should be gone, folded into the synopsis")
	}
}

func TestNoStandaloneManifestBlock(t *testing.T) {
	ws := newWorkingSet()
	ws.put(&entry{id: "a", kind: kindRun, name: "run", desc: "run: go build", content: "old build output"})
	ws.put(&entry{id: "b", kind: kindRun, name: "run", desc: "run: go test", content: "latest test output"})
	a1 := assistantMessage(t, `{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"run","input":{"command":"go build"}}]}`)
	a2 := assistantMessage(t, `{"role":"assistant","content":[{"type":"tool_use","id":"b","name":"run","input":{"command":"go test"}}]}`)
	events := []event{
		{kind: evUser, text: "build then test"},
		{kind: evAssistant, asst: a1},
		{kind: evToolResults, ids: []string{"a"}},
		{kind: evAssistant, asst: a2},
		{kind: evToolResults, ids: []string{"b"}},
	}
	payload := buildPayload(events, ws, nil, nil)

	// There is no standalone Manifest / Working Set block any more.
	if payloadContains(payload, "Working Set") {
		t.Fatalf("payload still carries a standalone Working Set block")
	}
	// The superseded result's one-liner lives in its own tool_result position.
	if got := findToolResult(payload, "a"); got != "[run: go build]" {
		t.Fatalf("older result = %q, want the inline one-liner", got)
	}
	// The latest result is still full inline.
	if got := findToolResult(payload, "b"); got != "latest test output" {
		t.Fatalf("latest result = %q, want the full content", got)
	}
}

func TestSessionPersistedToDisk(t *testing.T) {
	fm := &fakeModel{replies: []*anthropic.Message{
		assistantMessage(t, `{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"run","input":{"command":"echo hi"}}]}`),
		assistantMessage(t, `{"role":"assistant","content":[{"type":"text","text":"done"}]}`),
	}}

	a := New(fm, scriptedInput("go"), []tool.ToolDefinition{tool.RunDefinition})
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
		t.Fatalf("index missing the run entry")
	}
}

func TestForgetDropsCollapsedTurn(t *testing.T) {
	// Three Turns; on Turn 3 the model forgets the now-collapsed Turn 1, which
	// should vanish from the payload built for the following Round.
	fm := &fakeModel{replies: []*anthropic.Message{
		assistantMessage(t, `{"role":"assistant","content":[{"type":"text","text":"answer one"}]}`), // Turn 1
		assistantMessage(t, `{"role":"assistant","content":[{"type":"text","text":"answer two"}]}`), // Turn 2
		assistantMessage(t, `{"role":"assistant","content":[{"type":"tool_use","id":"f1","name":"forget","input":{"turn":1}}]}`),
		assistantMessage(t, `{"role":"assistant","content":[{"type":"text","text":"answer three"}]}`),
	}}

	a := New(fm, scriptedInput("first question", "second question", "third question"),
		[]tool.ToolDefinition{tool.ForgetDefinition})
	a.sessionDir = t.TempDir()
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if !a.forgottenTurns[1] {
		t.Fatalf("Turn 1 should be marked forgotten")
	}
	// The model call after the forget drops Turn 1 entirely — prompt and synopsis.
	last := fm.calls[len(fm.calls)-1]
	if payloadContains(last, "first question") {
		t.Fatalf("forgotten Turn 1 prompt still in payload")
	}
	if payloadContains(last, turnPath(1)) {
		t.Fatalf("forgotten Turn 1 synopsis still in payload")
	}
	// Turn 2 (still inside the window) is unaffected.
	if !payloadContains(last, "second question") {
		t.Fatalf("recent Turn 2 should remain in the payload")
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
	for _, want := range []string{"❯ what changed?", "⏺ checking", "⏺ run(echo hi)", "⏺ all done"} {
		if !strings.Contains(s, want) {
			t.Fatalf("body missing %q:\n%s", want, s)
		}
	}
}

func TestSummarizeCollapsedTurnAtCollapse(t *testing.T) {
	// Three Turns; with the window of 2, Turn 1 collapses on the Turn-3 request and
	// is gisted by an LLM call whose input carries the tool RESULT, not just the call.
	fm := &fakeModel{
		summary: "ran echo, printed hi",
		replies: []*anthropic.Message{
			assistantMessage(t, `{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"run","input":{"command":"echo hi"}}]}`), // Turn 1, Round 1
			assistantMessage(t, `{"role":"assistant","content":[{"type":"text","text":"done one"}]}`),                                        // Turn 1, Round 2
			assistantMessage(t, `{"role":"assistant","content":[{"type":"text","text":"two"}]}`),                                             // Turn 2
			assistantMessage(t, `{"role":"assistant","content":[{"type":"text","text":"three"}]}`),                                           // Turn 3
		},
	}
	a := New(fm, scriptedInput("first question", "second question", "third question"),
		[]tool.ToolDefinition{tool.RunDefinition})
	a.sessionDir = t.TempDir()
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	// The collapsed Turn shows the LLM summary, not the prompt's first line.
	if a.turnDesc[1] != "ran echo, printed hi" {
		t.Fatalf("Turn 1 desc = %q, want the LLM summary", a.turnDesc[1])
	}
	last := fm.calls[len(fm.calls)-1]
	if !payloadContains(last, "[Turn 1 — ran echo, printed hi") {
		t.Fatalf("collapsed Turn 1 should show the LLM summary")
	}
	if payloadContains(last, "[Turn 1 — first question") {
		t.Fatalf("collapsed Turn 1 still shows the prompt bootstrap, not the summary")
	}
	// Summarized exactly once (cached across Turn 3's rounds) ...
	if len(fm.summarized) != 1 {
		t.Fatalf("summarize called %d times, want 1 (cached)", len(fm.summarized))
	}
	// ... and the input it summarized included the tool call AND its result.
	in := fm.summarized[0]
	for _, want := range []string{"first question", "echo hi", "Tool result", "hi"} {
		if !strings.Contains(in, want) {
			t.Fatalf("summary input missing %q:\n%s", want, in)
		}
	}
}

func TestSummarizeFallsBackToBootstrapOnError(t *testing.T) {
	fm := &fakeModel{
		summaryErr: errors.New("summary unavailable"),
		replies: []*anthropic.Message{
			assistantMessage(t, `{"role":"assistant","content":[{"type":"text","text":"one"}]}`),
			assistantMessage(t, `{"role":"assistant","content":[{"type":"text","text":"two"}]}`),
			assistantMessage(t, `{"role":"assistant","content":[{"type":"text","text":"three"}]}`),
		},
	}
	a := New(fm, scriptedInput("first question", "second question", "third question"),
		[]tool.ToolDefinition{tool.RunDefinition})
	a.sessionDir = t.TempDir()
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("run must not fail when the summary call errors: %v", err)
	}

	if a.turnDesc[1] != "" {
		t.Fatalf("a failed summary should leave no Description, got %q", a.turnDesc[1])
	}
	last := fm.calls[len(fm.calls)-1]
	if !payloadContains(last, "[Turn 1 — first question") {
		t.Fatalf("collapsed Turn 1 should fall back to the prompt's first line")
	}
}

// payloadContains reports whether any text or tool_result text in the payload
// contains sub.
func payloadContains(payload []anthropic.MessageParam, sub string) bool {
	for _, m := range payload {
		for _, b := range m.Content {
			if b.OfText != nil && strings.Contains(b.OfText.Text, sub) {
				return true
			}
			if b.OfToolResult != nil {
				for _, c := range b.OfToolResult.Content {
					if c.OfText != nil && strings.Contains(c.OfText.Text, sub) {
						return true
					}
				}
			}
		}
	}
	return false
}

func TestResumeRebuildsPayloadAndContinues(t *testing.T) {
	dir := t.TempDir()

	// Session 1: a run plus a final answer, then quit.
	fm1 := &fakeModel{replies: []*anthropic.Message{
		assistantMessage(t, `{"role":"assistant","content":[{"type":"text","text":"let me check"},{"type":"tool_use","id":"c1","name":"run","input":{"command":"echo hi"}}]}`),
		assistantMessage(t, `{"role":"assistant","content":[{"type":"text","text":"it said hi"}]}`),
	}}
	a1 := New(fm1, scriptedInput("what does echo say?"), []tool.ToolDefinition{tool.RunDefinition})
	a1.sessionDir = dir
	if err := a1.Run(context.Background()); err != nil {
		t.Fatalf("session 1: %v", err)
	}

	// Resume into a fresh Agent and ask a follow-up.
	fm2 := &fakeModel{replies: []*anthropic.Message{
		assistantMessage(t, `{"role":"assistant","content":[{"type":"text","text":"you're welcome"}]}`),
	}}
	a2 := New(fm2, scriptedInput("thanks"), []tool.ToolDefinition{tool.RunDefinition})
	if err := a2.Load(dir); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := a2.Run(context.Background()); err != nil {
		t.Fatalf("session 2: %v", err)
	}

	if len(fm2.calls) == 0 {
		t.Fatal("resumed session made no model call")
	}
	sent := fm2.calls[0]

	// The prior conversation and the new prompt are both present.
	if !payloadContains(sent, "what does echo say?") {
		t.Fatalf("resumed payload missing the prior prompt")
	}
	if !payloadContains(sent, "thanks") {
		t.Fatalf("resumed payload missing the new prompt")
	}
	// The prior tool exchange is rebuilt with pairing intact (legal request).
	if !hasToolUse(sent, "c1") {
		t.Fatalf("resumed payload missing the prior tool_use")
	}
	if findToolResult(sent, "c1") == "" {
		t.Fatalf("resumed payload missing the paired tool_result")
	}
	// The Turn counter continued from where Session 1 left off.
	if a2.turn != 2 {
		t.Fatalf("turn = %d, want 2 (resumed Turn 1 + one new Turn)", a2.turn)
	}
	// It kept writing into the same Session directory.
	if a2.sessionDir != dir {
		t.Fatalf("resumed session dir = %q, want %q", a2.sessionDir, dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "turns", "002.md")); err != nil {
		t.Fatalf("new Turn not persisted into the resumed Session: %v", err)
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
