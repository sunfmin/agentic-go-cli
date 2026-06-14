package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// indexFileName is the materialized Working Set, rewritten into the Session
// directory each Round. It holds the Working Set index plus counters and a little
// metadata — and no tool content (run output lives in runs/, prose in turns/) —
// so it stays the lean resume backbone, not a second copy of the conversation.
const (
	indexFileName = "index.json"
	indexSchema   = 1
)

// indexState is the on-disk form of the Working Set plus the counters and a
// little session metadata. It is rewritten in full each Round (temp + rename).
type indexState struct {
	Schema      int          `json:"schema"`
	CreatedAt   string       `json:"created_at,omitempty"`
	Model       string       `json:"model,omitempty"`
	Turn        int          `json:"turn"`
	Round       int          `json:"round"`
	RefSeq      int            `json:"ref_seq"`
	ArtifactSeq int            `json:"artifact_seq"`
	Entries     []entryIndex   `json:"entries"`
	Turns       map[int]string `json:"turns,omitempty"` // upgraded collapsed-Turn descriptions, by turn number
}

// entryIndex is one Working Set entry without its content: the content is
// recalled from the run file (runs/), re-read from the working tree (a read), or
// — for a collapsed Turn — read from its transcript. `path` is the working-tree
// file for a read/edit and the run file for a run; `ref` is the join key to the
// turn frontmatter.
type entryIndex struct {
	Ref       string `json:"ref"`
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Path      string `json:"path,omitempty"`
	Desc      string `json:"desc"`
	IsErr     bool   `json:"is_err,omitempty"`
	Turn      int    `json:"turn"`
	Forgotten bool   `json:"forgotten,omitempty"`
}

// persist writes the current Working Set index to index.json via a temp file +
// rename, so a crash never leaves a half-written index.
func (a *Agent) persist() error {
	dir, err := a.ensureDir()
	if err != nil {
		return err
	}
	if a.createdAt == "" {
		a.createdAt = time.Now().Format(time.RFC3339)
	}
	data, err := json.MarshalIndent(a.indexState(), "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, indexFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (a *Agent) indexState() indexState {
	st := indexState{
		Schema:      indexSchema,
		CreatedAt:   a.createdAt,
		Model:       string(defaultModel),
		Turn:        a.turn,
		Round:       a.round,
		RefSeq:      a.refSeq,
		ArtifactSeq: a.artifactSeq,
	}
	if len(a.turnDesc) > 0 {
		st.Turns = a.turnDesc
	}
	for _, id := range a.ws.order {
		en := a.ws.byID[id]
		st.Entries = append(st.Entries, entryIndex{
			Ref: en.ref, ID: en.id, Kind: string(en.kind), Name: en.name,
			Path: en.path, Desc: en.desc, IsErr: en.isErr, Turn: en.turn,
			Forgotten: en.forgotten,
		})
	}
	return st
}
