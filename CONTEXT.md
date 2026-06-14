# agentic-go-cli

A minimal code-editing agent that, instead of re-sending an ever-growing message
history, externalizes each tool result to disk and sends only a lightweight index —
letting the model pull full content on demand and forget what it no longer needs.

## Context management

**Artifact**:
A non-reproducible tool output (a `run` command's result — test runs, git output,
network calls) persisted to a file so it can be recalled without re-executing.
A read is NOT an Artifact: it is a live reference to the working-tree file, re-read
on demand, so it can never go stale.
_Avoid_: tool result, output file, blob

**Manifest**:
The per-round index sent to the model in place of full content. Each entry is a
name plus a Description, pointing at either a live working-tree file (from a read)
or a stored Artifact (from a run). Exception: the most recent tool result is sent
in full, since the model issued the call precisely to see its content.
_Avoid_: index, catalog, listing, 清单

**Description**:
The short summary shown in the Manifest, by which the model decides whether to pull
an entry's full content. For reads/edits it is derived for free from the call (the
path). For a run Artifact it is a one-line gist the model writes on the turn after
it first sees the output — capturing the result, not just the command.
_Avoid_: summary, label, caption

**Forget**:
The model's action of dropping an entry from the Working Set entirely — not just
its content (which collapses to a Description automatically once it is no longer the
latest result) but its Manifest entry too, so it stops being sent at all.
_Avoid_: delete, prune, evict

**Working Set**:
The Artifacts currently live — not yet forgotten — whose Manifest is sent each round.
_Avoid_: context, window, history

**Stale Artifact**:
A stored `run` Artifact whose output no longer reflects current reality — e.g. a
`go test` result from before a later edit. Impossible for reads (they reference the
live file); the model forgets stale Artifacts by its own judgment.
_Avoid_: stale snapshot, outdated read, old version
