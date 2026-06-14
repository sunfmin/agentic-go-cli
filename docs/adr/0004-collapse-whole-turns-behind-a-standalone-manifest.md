# Collapse whole Turns behind a standalone Manifest

ADR-0002 externalized tool *output* but still re-sent every past Turn's prose (user
prompts and assistant replies) in full on every round. This lifts the same
collapse-to-a-Description scheme up to the Turn: the most recent N Turns are sent in
full, and every older Turn collapses to a single one-line Description (its user
prompt kept verbatim, its entire reply replaced by the gist) which the model recalls
on demand by reading that Turn's transcript file (`turns/NNN.md`). To make room for
this, the Manifest is **decoupled from the conversation** — it is no longer woven
into `tool_result` positions but rendered as its own always-current section
(attached to the active prompt), so a Turn's `tool_use`/`tool_result` plumbing can be
dropped wholesale when it collapses while its live Artifacts stay listed.

A Turn's Description bootstraps for free from the user prompt's first line and, like a
run Artifact, can be upgraded by the model via `describe` — there is no second
summarizer model (ADR-0002 rejected that, and this preserves it).

## Considered options

- **Keep re-sending all prose; only collapse tool output.** The state after ADR-0002.
  Rejected: prose grows unboundedly with conversation length and is re-paid every round.
- **A second model summarizes old Turns at close.** Rejected: reopens exactly what
  ADR-0002 turned down — extra inference, latency, and lossiness — when a free
  bootstrap (the prompt's first line) plus an optional in-loop `describe` suffices.
- **Keep the Manifest woven inline and only shrink each Round's prose.** Rejected: it
  isn't one Description per Turn, and it leaves the old `tool_use`/`tool_result`
  skeleton on the wire every round; decoupling the Manifest is cleaner.
- **Mandatory turn-gist emission at close / next-turn summarization.** Rejected: both
  constrain or delay the model; the free bootstrap removes the need for either.

## Consequences

- **The most recent tool result is full, the most recent N Turns are full** (N a small
  constant), so a tight follow-up keeps its immediately-preceding exchange in detail;
  anything older costs a Recall (`read turns/NNN.md`) if the gist isn't enough.
- **Turn Descriptions are mutable** (describe upgrades them), so they are materialized
  in `index.json` (the Working Set index of ADR-0003), not in the immutable turn
  frontmatter. The bootstrap value is derivable from the prompt for free.
- **Recall reuses `read`** against the transcript file (immutable, never stale), the
  same mechanism a run Artifact already uses — no new tool.
- **Supersedes the inline-Manifest consequence of ADR-0002.** The pairing rule there
  (collapse a tool result's *content* but keep the `tool_use`/`tool_result` IDs) no
  longer applies to collapsed Turns, whose plumbing is dropped as a unit; it still
  applies within the full-Turn window.
