# agentic-go-cli

A minimal code-editing agent in ~300 lines of Go, following
[How to Build an Agent](https://ampcode.com/notes/how-to-build-an-agent) —
but authenticated with this machine's **Claude Code subscription** instead of an API key.

## How auth works

On startup it reads the OAuth token Claude Code stores in the macOS Keychain
(`security find-generic-password -s "Claude Code-credentials" -w`), falling back to
`~/.claude/.credentials.json`. Requests use:

- `Authorization: Bearer sk-ant-oat01-...` (not `x-api-key`)
- `anthropic-beta: oauth-2025-04-20`
- a system prompt identifying as Claude Code (required for subscription tokens)

No `ANTHROPIC_API_KEY` needed. Requires being logged in to Claude Code (`/login`).

## Install

```sh
go install github.com/sunfmin/agentic-go-cli@latest
```

## Run

```sh
agentic-go-cli
```

Then chat. The agent has a single tool — `bash` — which covers everything
(reading, listing, editing files, builds, tests, git), and loops on tool
calls until Claude produces a final answer. Tool calls are printed in a
readable form and tool output is shown verbatim.

```
You: what does main.go do? add a comment to the top of it
```

Note: the OAuth access token is short-lived; if requests start failing with 401,
open Claude Code once so it refreshes the token.
