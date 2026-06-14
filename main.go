package main

import (
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
	"github.com/sunfmin/agentic-go-cli/internal/ui"
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

	opts := []option.RequestOption{
		option.WithAuthToken(token),
		option.WithHeader("anthropic-beta", "oauth-2025-04-20"),
	}
	// An explicit base URL (ANTHROPIC_BASE_URL) lets requests be pointed at a
	// proxy or gateway; unset means the SDK default endpoint.
	if base := os.Getenv("ANTHROPIC_BASE_URL"); base != "" {
		opts = append(opts, option.WithBaseURL(base))
	}

	client := anthropic.NewClient(opts...)

	a := agent.New(
		agent.NewAnthropicModel(&client),
		ui.ReadInput,
		[]tool.ToolDefinition{
			tool.ReadDefinition,
			tool.EditDefinition,
			tool.RunDefinition,
			tool.ForgetDefinition,
			tool.DescribeDefinition,
		},
	)

	cwd, _ := os.Getwd()
	ui.PrintWelcome("Claude Opus 4.8 · Claude Code OAuth", cwd)

	// Bare startup resumes the most recent Session; --new opts out; --resume
	// targets a specific one.
	switch {
	case *resumeID != "":
		dir := filepath.Join(".agentic-artifacts", "sessions", *resumeID)
		if err := a.Load(dir); err != nil {
			fmt.Fprintln(os.Stderr, "failed to resume session:", err)
			os.Exit(1)
		}
		ui.PrintHint("Resumed session " + *resumeID)
	case !*newSession:
		if dir, ok := agent.MostRecentSession(); ok {
			if err := a.Load(dir); err != nil {
				fmt.Fprintln(os.Stderr, "failed to resume session:", err)
				os.Exit(1)
			}
			ui.PrintHint("Resumed " + dir + "  (--new for a fresh session)")
		}
	}

	if err := a.Run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// claudeCodeToken resolves the auth token used for the Anthropic API. An
// environment variable takes precedence (ANTHROPIC_AUTH_TOKEN, or the common
// ANTHROPIC_API_KEY), letting the token be injected without touching the
// Keychain. Failing that, it reads the OAuth access token Claude Code stores in
// the macOS Keychain (falling back to ~/.claude/.credentials.json on Linux).
func claudeCodeToken() (string, error) {
	if t := firstNonEmptyEnv("ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY"); t != "" {
		return t, nil
	}

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
		return "", fmt.Errorf("no accessToken found — set ANTHROPIC_AUTH_TOKEN, or run `claude` and /login first")
	}
	return creds.ClaudeAiOauth.AccessToken, nil
}

// firstNonEmptyEnv returns the value of the first environment variable in names
// that is set to a non-empty value, or "" if none are.
func firstNonEmptyEnv(names ...string) string {
	for _, name := range names {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return ""
}
