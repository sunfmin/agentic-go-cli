package agent

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/anthropics/anthropic-sdk-go"
)

// sessionFileName is the session-state snapshot written next to the run
// Artifacts. It holds the whole conversation so a session can be inspected (and,
// later, resumed) from disk.
const sessionFileName = "session.json"

// sessionState is the on-disk form of everything the agent holds in memory: the
// event log (the conversation history), the working set, a rendered Manifest,
// and the counters. It is a snapshot rewritten in full each turn.
type sessionState struct {
	Turn        int          `json:"turn"`
	RefSeq      int          `json:"ref_seq"`
	ArtifactSeq int          `json:"artifact_seq"`
	EventLog    []eventState `json:"event_log"`
	WorkingSet  []entryState `json:"working_set"`
	Manifest    []string     `json:"manifest"`
}

type eventState struct {
	Kind       string           `json:"kind"` // "user" | "assistant" | "tool_results"
	Text       string           `json:"text,omitempty"`
	Assistant  []assistantBlock `json:"assistant,omitempty"`
	ToolUseIDs []string         `json:"tool_use_ids,omitempty"`
}

type assistantBlock struct {
	Type  string          `json:"type"` // "text" | "tool_use"
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type entryState struct {
	ID        string `json:"id"`
	Ref       string `json:"ref"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Path      string `json:"path,omitempty"`
	Desc      string `json:"desc"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
	Turn      int    `json:"turn"`
	Forgotten bool   `json:"forgotten,omitempty"`
}

// persist writes the current session state to disk as a single JSON snapshot.
func (a *Agent) persist() error {
	dir, err := a.ensureDir()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(a.sessionState(), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, sessionFileName), data, 0o644)
}

func (a *Agent) sessionState() sessionState {
	st := sessionState{Turn: a.turn, RefSeq: a.refSeq, ArtifactSeq: a.artifactSeq}
	for _, e := range a.events {
		es := eventState{Text: e.text, ToolUseIDs: e.ids}
		switch e.kind {
		case evUser:
			es.Kind = "user"
		case evAssistant:
			es.Kind = "assistant"
			es.Assistant = assistantBlocksOf(e.asst)
		case evToolResults:
			es.Kind = "tool_results"
		}
		st.EventLog = append(st.EventLog, es)
	}
	for _, id := range a.ws.order {
		en := a.ws.byID[id]
		st.WorkingSet = append(st.WorkingSet, entryState{
			ID: en.id, Ref: en.ref, Kind: string(en.kind), Name: en.name,
			Path: en.path, Desc: en.desc, Content: en.content, IsError: en.isErr,
			Turn: en.turn, Forgotten: en.forgotten,
		})
		if !en.forgotten {
			st.Manifest = append(st.Manifest, en.manifestLine())
		}
	}
	return st
}

func assistantBlocksOf(m *anthropic.Message) []assistantBlock {
	if m == nil {
		return nil
	}
	var blocks []assistantBlock
	for _, b := range m.Content {
		switch v := b.AsAny().(type) {
		case anthropic.TextBlock:
			blocks = append(blocks, assistantBlock{Type: "text", Text: v.Text})
		case anthropic.ToolUseBlock:
			blocks = append(blocks, assistantBlock{Type: "tool_use", ID: v.ID, Name: v.Name, Input: json.RawMessage(v.Input)})
		}
	}
	return blocks
}
