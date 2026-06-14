# On-disk session layout: transcript files, run files, and a materialized index

A Session is persisted under `.agentic-artifacts/sessions/<timestamp>/` as three
kinds of files instead of one monolithic `session.json`, and is resumable:

- **`turns/NNN.md`** — one Turn (a user prompt plus the entire AI reply, however
  many Rounds it takes). YAML frontmatter carries only the structure (`rounds: [[refs]]`
  — which entries fired in which Round, in order); the body is a human-readable
  transcript reusing the terminal's own markers (`❯` user, `⏺` assistant prose,
  `●` tool calls). The turn file is the source of truth for prose and Round ordering.
- **`runs/NNN.txt`** — one `run` Artifact: the command (`$ ` prefixed, `> ` for
  continuations) followed by its combined output. The command lives with its output.
- **`index.json`** — the materialized Working Set (per `ref`: `id`, `kind`, `tool`,
  `path`, `desc`, `is_err`, `run` file, `turn`, `forgotten`), plus counters and
  session metadata. It carries **no** content. Rewritten every Round via temp+rename;
  the turn file for the in-progress Turn is rewritten the same way, so a crash mid-Turn
  resumes from the pending tool results.

Resume (the default on bare startup; `--new` opts out): read `index.json` to rebuild
the Working Set, then walk the turn files — frontmatter gives ordering, the body
gives prose, each `ref` resolves against the index for `id`/`kind`/`path`, run files
give output, reads re-read the live working tree, edits replay a compacted input —
and feed the result back through `buildPayload`.

## Considered options

- **Keep one monolithic `session.json`.** What the agent does today. Rejected: it is
  unreadable, duplicates every run's output (inline `content` *and* `run-N.txt`), and
  buries the conversation in JSON.
- **JSON as the structural event log, Markdown as a derived human view.** Rejected:
  two representations of the same conversation drift, and the JSON grows back toward
  the monolith.
- **`index.json` as counters only, with the Working Set folded from the immutable
  history.** The purest single-source design and the most on-brand with ADR-0002's
  "Manifest is a derived view." Rejected for now in favor of a materialized index:
  less resume code, at the cost of a drift risk that a single in-memory writer
  contains.

## Consequences

- **Edit inputs are not byte-faithful on resume.** An edit's new content is not
  stored; the replayed `tool_use` is compacted and the model re-reads the live file.
  This is deliberate and consistent with the project's "never reason against a stale
  copy" stance (a past edit's content is stale the moment a later edit lands).
- **Resume parses Markdown.** The turn body is the source of truth for prose, so the
  reader extracts `❯`/`⏺` blocks (the `●` tool lines are decorative — the calls are
  authoritative in the frontmatter). A prose line beginning with `●` is the one
  collision to guard against on write.
- **The materialized index can drift from the history** if a write is buggy; the
  index wins. Acceptable because there is exactly one writer.
- Requires the structured tools of ADR-0001 (file identity) and inherits the
  Manifest/Working Set vocabulary of ADR-0002.
