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
		model:          anthropicModel{client: &client},
		getUserMessage: getUserMessage,
		tools:          []ToolDefinition{ReadDefinition, EditDefinition, RunDefinition},
		ws:             newWorkingSet(),
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

// printToolCall renders a tool invocation in a human-readable form instead of
// raw JSON, e.g.:
//
//	● read(path: main.go)
//	● edit
//	    path: hello.txt
//	    content:
//	      hello
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

	// Single-argument tools don't need the key label — show the value directly.
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
