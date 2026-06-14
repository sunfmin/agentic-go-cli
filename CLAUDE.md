# agentic-go-cli

A minimal code-editing agent in ~300 lines of Go, authenticated with the local
Claude Code subscription (OAuth token) instead of an API key.

## Agent skills

### Issue tracker

Issues and PRDs are tracked as GitHub issues via the `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

Default label vocabulary — `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `CONTEXT.md` + `docs/adr/` at the repo root. See `docs/agents/domain.md`.
