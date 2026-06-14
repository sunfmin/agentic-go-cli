# agentic-go-cli

A minimal code-editing agent that, instead of re-sending an ever-growing message
history, externalizes each tool result to disk and sends only a lightweight index —
letting the model pull full content on demand and forget what it no longer needs.

## Context management

**Artifact**:
A non-reproducible tool output (a `run` command's result — test runs, git output,
network calls) persisted so it can be recalled without re-executing. It pairs the
command with the output it produced. A read is NOT an Artifact: it is a live
reference to the working-tree file, re-read on demand, so it can never go stale.
_Avoid_: tool result, output file, blob

**Manifest**:
The index sent to the model in place of full content — its own always-current
section, not woven into the conversation. Each entry is a Description pointing at
recall-able content: a live working-tree file (a read), a stored Artifact (a run),
or a collapsed past Turn (its transcript). The most recent tool result and the most
recent Turns are sent in full; everything older lives only as its Description until
recalled.
_Avoid_: index, catalog, listing, 清单

**Description**:
The short summary shown in the Manifest, by which the model decides whether to pull
an entry's full content. For reads/edits it is derived for free from the path. For a
run Artifact it is a one-line gist the model writes via describe after seeing the
output — capturing the result, not just the command. For a collapsed Turn it
bootstraps for free from the user prompt's first line and can likewise be upgraded
via describe to capture the outcome of the reply.
_Avoid_: summary, label, caption

**Forget**:
The model's action of dropping an entry from the Working Set entirely — not just
its content (which collapses to a Description automatically once it is no longer the
latest result) but its Manifest entry too, so it stops being sent at all.
_Avoid_: delete, prune, evict

**Recall**:
The model's action of pulling a collapsed entry's full content back into context by
reading its file — a stored Artifact or a past Turn's transcript. The inverse of
letting something rest as just its Description.
_Avoid_: reload, fetch, expand, rehydrate

**Working Set**:
The Artifacts currently live — not yet forgotten — whose Manifest is sent each round.
_Avoid_: context, window, history

**Stale Artifact**:
A stored `run` Artifact whose output no longer reflects current reality — e.g. a
`go test` result from before a later edit. Impossible for reads (they reference the
live file); the model forgets stale Artifacts by its own judgment.
_Avoid_: stale snapshot, outdated read, old version

## Session layout

**Session**:
One conversation, from the first user prompt to exit, persisted as a unit and
resumable later. Bare startup resumes the most recent Session; `--new` starts a
fresh one.
_Avoid_: chat, run, conversation log

**Turn**:
One user prompt plus the entire AI reply that follows it — however many model
responses and tool calls that takes — up until the agent asks the user for input
again. The unit of a per-Turn transcript file on disk.
_Avoid_: exchange, conversation, interaction

**Round**:
A single model response within a Turn (one call to the Messages API) plus the tool
results it triggers. The unit the Manifest is rebuilt for. (The in-memory counter
named `turn` today is really a Round counter.)
_Avoid_: step, iteration, request, turn
