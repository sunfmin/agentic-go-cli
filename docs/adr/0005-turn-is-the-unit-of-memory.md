# The Turn is the unit of memory

ADR-0002 externalized individual tool results behind a Manifest; ADR-0004 lifted the
same collapse up to whole Turns but *decoupled* a Turn's Artifacts from it — a
collapsed Turn became a one-line synopsis while the `#N` Artifacts it produced stayed
listed independently in a standalone Manifest block, separately recall-able.

That decoupling earns its complexity rarely. Once a Turn is old enough to collapse,
its individual tool calls are almost never independently useful: a `run` output from
several Turns ago is usually stale (the code moved on), a `read` is re-readable from
the live tree at any time, and an `edit` already landed on disk. In exchange, the
independent entries accumulate as clutter the model must manually forget, and they
make any structured view of context misrepresent the conversation — Artifacts
orphaned from the Turn that created them.

This ADR makes the **Turn the single unit of memory** — of collapse, recall, and
forget:

- **Recent N Turns are sent in full.** Their tool calls are present and in order.
- **Older Turns collapse to one Description.** A collapsed Turn's tool calls fold
  into it; no independent Artifact entries survive the collapse.
- **Recall is per-Turn.** To see a collapsed Turn's detail — including any
  non-reproducible `run` output — the model reads its transcript `turns/NNN.md`, in
  order and immutable.
- **Forget is per-Turn.** The model drops a collapsed Turn's Description to reclaim
  its line.

This supersedes ADR-0004's decoupling: a collapsed Turn no longer leaves its Artifacts
listed, and the standalone Manifest block ADR-0004 introduced — which existed only to
hold those independent entries — is retired. ADR-0004's other half, collapsing a whole
Turn to an inline synopsis, stays.

## Considered options

- **Keep per-Artifact independence (ADR-0004).** Rejected: the still-relevant-old-
  Artifact case it optimizes for is rare; in exchange every session accrues stale,
  independently-managed entries the model must prune, and no faithful view of context
  is possible (orphaned Artifacts).
- **Nest tool calls under their Turn but keep them independently forgettable.**
  Rejected: re-couples the visual hierarchy while leaving the lifecycle split —
  forgetting a parent Turn would orphan a child the model meant to keep, the worst of
  both.
- **The Turn as the unit (this ADR).** Chosen: one kind of thing to collapse, recall,
  and forget; old detail rests in the immutable transcript and is recalled whole on
  the rare occasion it is needed.

## Consequences

- **Recall always goes through the Turn transcript.** A within-window Turn is already
  full in context, so recall only ever targets a collapsed Turn. This makes standalone
  per-Artifact files (ADR-0003) unnecessary *for recall*; whether to drop them from
  the on-disk layout is a follow-on that touches ADR-0003.
- **No `#N` Artifact ref scheme in steady state.** `forget` and `describe` take a Turn
  (`Turn 3`). The latest tool result of the current round is still sent in full, and
  ADR-0002's within-Turn collapse of superseded results still applies, but it produces
  no lasting independent entries.
- **Loss: surgical recall of a single old output.** The model recalls the whole Turn
  instead. Acceptable — old `run` outputs are usually stale (re-run beats recall), and
  nothing is lost: a non-reproducible output lives on in the transcript.
- **The Manifest is now just the collapsed-Turn synopses** (inline, where ADR-0004
  placed them); there is no separate Artifact section. A terminal panel can mirror it
  as a flat list of Turns.
- **CONTEXT.md** moves `Manifest`, `Forget`, `Recall`, `Turn`, and `Artifact` to the
  Turn-centric model and retires `Stale Artifact` — old outputs live in clearly
  historical transcripts, not as live entries that could be mistaken for current.
