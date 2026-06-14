package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/sunfmin/agentic-go-cli/internal/agent"
	"github.com/sunfmin/agentic-go-cli/internal/tool"
)

func main() {
	newSession := flag.Bool("new", false, "start a fresh Session instead of resuming the most recent one")
	resumeID := flag.String("resume", "", "resume a specific Session by id (the sessions/<id> directory name)")
	flag.Parse()

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

	a := agent.New(
		agent.NewAnthropicModel(&client),
		getUserMessage,
		[]tool.ToolDefinition{
			tool.ReadDefinition,
			tool.EditDefinition,
			tool.RunDefinition,
			tool.ForgetDefinition,
			tool.DescribeDefinition,
		},
	)

	// Bare startup resumes the most recent Session; --new opts out; --resume
	// targets a specific one.
	switch {
	case *resumeID != "":
		dir := filepath.Join(".agentic-artifacts", "sessions", *resumeID)
		if err := a.Load(dir); err != nil {
			fmt.Fprintln(os.Stderr, "failed to resume session:", err)
			os.Exit(1)
		}
		fmt.Println("Resumed session", *resumeID)
	case !*newSession:
		if dir, ok := agent.MostRecentSession(); ok {
			if err := a.Load(dir); err != nil {
				fmt.Fprintln(os.Stderr, "failed to resume session:", err)
				os.Exit(1)
			}
			fmt.Println("Resumed", dir, "(--new for a fresh session)")
		}
	}

	if err := a.Run(context.Background()); err != nil {
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
