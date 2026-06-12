package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

func main() {
	token, err := claudeCodeToken()
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to load Claude Code credentials:", err)
		os.Exit(1)
	}

	client := anthropic.NewClient(
		option.WithAuthToken(token),
		option.WithHeader("anthropic-beta", "oauth-2025-04-20"),
	)

	scanner := bufio.NewScanner(os.Stdin)
	getUserMessage := func() (string, bool) {
		if !scanner.Scan() {
			return "", false
		}
		return scanner.Text(), true
	}

	agent := &Agent{
		client:         &client,
		getUserMessage: getUserMessage,
		tools:          []ToolDefinition{BashDefinition},
	}
	if err := agent.Run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// claudeCodeToken reads the OAuth access token Claude Code stores in the
// macOS Keychain (falling back to ~/.claude/.credentials.json on Linux).
func claudeCodeToken() (string, error) {
	var raw []byte
	out, err := exec.Command("security", "find-generic-password", "-s", "Claude Code-credentials", "-w").Output()
	if err == nil {
		raw = out
	} else {
		home, _ := os.UserHomeDir()
		raw, err = os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
		if err != nil {
			return "", err
		}
	}
	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &creds); err != nil {
		return "", err
	}
	if creds.ClaudeAiOauth.AccessToken == "" {
		return "", fmt.Errorf("no accessToken found — run `claude` and /login first")
	}
	return creds.ClaudeAiOauth.AccessToken, nil
}

type Agent struct {
	client         *anthropic.Client
	getUserMessage func() (string, bool)
	tools          []ToolDefinition
}

func (a *Agent) Run(ctx context.Context) error {
	conversation := []anthropic.MessageParam{}

	fmt.Println("Chat with Claude (ctrl-c to quit)")

	readUserInput := true
	for {
		if readUserInput {
			fmt.Print("[94mYou[0m: ")
			userInput, ok := a.getUserMessage()
			if !ok {
				break
			}
			conversation = append(conversation, anthropic.NewUserMessage(anthropic.NewTextBlock(userInput)))
		}

		message, err := a.runInference(ctx, conversation)
		if err != nil {
			return err
		}
		conversation = append(conversation, message.ToParam())

		toolResults := []anthropic.ContentBlockParamUnion{}
		for _, block := range message.Content {
			switch variant := block.AsAny().(type) {
			case anthropic.TextBlock:
				fmt.Printf("[93mClaude[0m: %s\n", variant.Text)
			case anthropic.ToolUseBlock:
				result := a.executeTool(variant.ID, variant.Name, []byte(variant.JSON.Input.Raw()))
				toolResults = append(toolResults, result)
			}
		}
		if len(toolResults) == 0 {
			readUserInput = true
			continue
		}
		readUserInput = false
		conversation = append(conversation, anthropic.NewUserMessage(toolResults...))
	}
	return nil
}

func (a *Agent) runInference(ctx context.Context, conversation []anthropic.MessageParam) (*anthropic.Message, error) {
	tools := []anthropic.ToolUnionParam{}
	for _, tool := range a.tools {
		tools = append(tools, anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name:        tool.Name,
			Description: anthropic.String(tool.Description),
			InputSchema: tool.InputSchema,
		}})
	}

	return a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeOpus4_8,
		MaxTokens: 16000,
		// Claude Code OAuth tokens are only valid for requests that
		// identify as Claude Code in the first system block.
		System: []anthropic.TextBlockParam{
			{Text: "You are Claude Code, Anthropic's official CLI for Claude."},
		},
		Messages: conversation,
		Tools:    tools,
	})
}

func (a *Agent) executeTool(id, name string, input []byte) anthropic.ContentBlockParamUnion {
	var toolDef ToolDefinition
	found := false
	for _, tool := range a.tools {
		if tool.Name == name {
			toolDef = tool
			found = true
			break
		}
	}
	if !found {
		return anthropic.NewToolResultBlock(id, "tool not found", true)
	}

	printToolCall(name, input)
	response, err := toolDef.Function(input)
	if err != nil {
		printToolResult(err.Error(), true)
		return anthropic.NewToolResultBlock(id, err.Error(), true)
	}
	printToolResult(response, false)
	return anthropic.NewToolResultBlock(id, response, false)
}

// printToolCall renders a tool invocation in a human-readable form instead of
// raw JSON, e.g.:
//
//	● bash(command: go test ./...)
//	● bash
//	    command:
//	      cat > hello.txt <<'EOF'
//	      hello
//	      EOF
func printToolCall(name string, input []byte) {
	var args map[string]any
	_ = json.Unmarshal(input, &args)

	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	inline := true
	for _, k := range keys {
		s := fmt.Sprint(args[k])
		if strings.Contains(s, "\n") || len(s) > 60 {
			inline = false
			break
		}
	}

	// Single-argument tools (like bash) don't need the key label —
	// show the value directly.
	if len(keys) == 1 {
		s := fmt.Sprint(args[keys[0]])
		if inline {
			fmt.Printf("\x1b[92m●\x1b[0m \x1b[1m%s\x1b[0m(%s)\n", name, s)
			return
		}
		fmt.Printf("\x1b[92m●\x1b[0m \x1b[1m%s\x1b[0m\n", name)
		for _, line := range strings.Split(s, "\n") {
			fmt.Printf("    %s\n", line)
		}
		return
	}

	if inline {
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s: %v", k, args[k]))
		}
		fmt.Printf("\x1b[92m●\x1b[0m \x1b[1m%s\x1b[0m(%s)\n", name, strings.Join(parts, ", "))
		return
	}

	fmt.Printf("\x1b[92m●\x1b[0m \x1b[1m%s\x1b[0m\n", name)
	for _, k := range keys {
		s := fmt.Sprint(args[k])
		if strings.Contains(s, "\n") {
			fmt.Printf("    %s:\n", k)
			for _, line := range strings.Split(s, "\n") {
				fmt.Printf("      %s\n", line)
			}
		} else {
			fmt.Printf("    %s: %s\n", k, s)
		}
	}
}

// printToolResult prints the tool's output verbatim (original formatting
// preserved), indented so it reads as output, red on error.
func printToolResult(response string, isError bool) {
	for _, line := range strings.Split(strings.TrimRight(response, "\n"), "\n") {
		if isError {
			fmt.Printf("  \x1b[91m%s\x1b[0m\n", line)
		} else {
			fmt.Printf("  %s\n", line)
		}
	}
}

type ToolDefinition struct {
	Name        string
	Description string
	InputSchema anthropic.ToolInputSchemaParam
	Function    func(input []byte) (string, error)
}

// bash

var BashDefinition = ToolDefinition{
	Name:        "bash",
	Description: "Execute a bash command in the working directory and return its combined stdout and stderr. Use this for everything: reading files (cat), listing directories (ls), editing files (heredocs, sed), running builds, tests, and git commands.",
	InputSchema: anthropic.ToolInputSchemaParam{
		Properties: map[string]any{
			"command": map[string]any{"type": "string", "description": "The bash command to execute."},
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
