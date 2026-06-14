// Package tool defines the structured tools the agent exposes to the model.
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

type ToolDefinition struct {
	Name        string
	Description string
	InputSchema anthropic.ToolInputSchemaParam
	Function    func(input []byte) (string, error)
}

// ReadDefinition reads a file's current contents. Reads are a live view of the
// working tree, so they never go stale.
var ReadDefinition = ToolDefinition{
	Name:        "read",
	Description: "Read a file from the working directory and return its current contents.",
	InputSchema: anthropic.ToolInputSchemaParam{
		Properties: map[string]any{
			"path": map[string]any{"type": "string", "description": "Path to the file to read."},
		},
		Required: []string{"path"},
	},
	Function: func(input []byte) (string, error) {
		var in struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return "", err
		}
		data, err := os.ReadFile(in.Path)
		if err != nil {
			return "", err
		}
		if len(data) == 0 {
			return "(empty file)", nil
		}
		return string(data), nil
	},
}

// EditDefinition replaces a file's entire contents. (Whole-file write; patch vs
// write was left open in the PRD.)
var EditDefinition = ToolDefinition{
	Name:        "edit",
	Description: "Write a file in the working directory, replacing its entire contents (creating it if needed).",
	InputSchema: anthropic.ToolInputSchemaParam{
		Properties: map[string]any{
			"path":    map[string]any{"type": "string", "description": "Path to the file to write."},
			"content": map[string]any{"type": "string", "description": "The full new contents of the file."},
		},
		Required: []string{"path", "content"},
	},
	Function: func(input []byte) (string, error) {
		var in struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return "", err
		}
		if err := os.WriteFile(in.Path, []byte(in.Content), 0o644); err != nil {
			return "", err
		}
		return fmt.Sprintf("wrote %d bytes to %s", len(in.Content), in.Path), nil
	},
}

// ForgetDefinition lets the model drop an entry from the working set. It is
// declared here for its schema, but the agent intercepts and handles it (it
// needs to mutate working-set state), so this Function is never called.
var ForgetDefinition = ToolDefinition{
	Name: "forget",
	Description: "Drop an entry from the working set by its Manifest reference (the #N shown in a " +
		"collapsed entry). Use it to discard output you no longer need so it stops taking room.",
	InputSchema: anthropic.ToolInputSchemaParam{
		Properties: map[string]any{
			"ref": map[string]any{"type": "string", "description": "The #N reference of the entry to forget, as shown in the Manifest."},
		},
		Required: []string{"ref"},
	},
	Function: func(input []byte) (string, error) {
		return "", fmt.Errorf("forget is handled by the agent")
	},
}

// DescribeDefinition lets the model attach a one-line gist to a run Artifact so
// its Manifest entry says what the output was, not just which command ran. Like
// forget, it is intercepted and handled by the agent; this Function is never
// called.
var DescribeDefinition = ToolDefinition{
	Name: "describe",
	Description: "Attach a one-line gist as a Description. Target either a Working Set entry by its " +
		"Manifest reference (ref: the #N), summarizing what a run produced (e.g. \"go test: 3 failures in " +
		"store\"), or a past Turn by its number (turn: N), summarizing the outcome of that whole exchange so " +
		"its collapsed synopsis reads better than the prompt's first line. Give exactly one of ref or turn.",
	InputSchema: anthropic.ToolInputSchemaParam{
		Properties: map[string]any{
			"ref":  map[string]any{"type": "string", "description": "The #N reference of the Working Set entry to describe."},
			"turn": map[string]any{"type": "integer", "description": "The number of the Turn to describe (as shown in a collapsed synopsis or the Working Set header)."},
			"gist": map[string]any{"type": "string", "description": "A one-line summary."},
		},
		Required: []string{"gist"},
	},
	Function: func(input []byte) (string, error) {
		return "", fmt.Errorf("describe is handled by the agent")
	},
}

// RunDefinition executes a shell command. Its description fences it off from file
// reads/edits, which must go through read/edit so file state stays tracked
// (ADR-0001).
var RunDefinition = ToolDefinition{
	Name: "run",
	Description: "Run a shell command in the working directory and return its combined stdout and stderr. " +
		"Use this for commands: builds, tests, git, and the like. " +
		"Do NOT use this to read or edit files — use the read and edit tools, which track file state. " +
		"Files read or written through here are not tracked and may go stale.",
	InputSchema: anthropic.ToolInputSchemaParam{
		Properties: map[string]any{
			"command": map[string]any{"type": "string", "description": "The shell command to run."},
		},
		Required: []string{"command"},
	},
	Function: func(input []byte) (string, error) {
		var in struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return "", err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		out, err := exec.CommandContext(ctx, "bash", "-c", in.Command).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("%s\n%s", err, out)
		}
		if len(out) == 0 {
			return "(no output)", nil
		}
		return string(out), nil
	},
}
