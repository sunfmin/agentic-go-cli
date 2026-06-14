# Summarize a collapsing Turn with an LLM call

ADR-0005 makes a collapsed Turn's one-line Description the *only* thing the model sees
of that Turn until it recalls the transcript. So the Description's quality is now
load-bearing — and the free bootstrap it inherits, the **first line of the user's
prompt**, captures what was *asked*, never what the reply *accomplished*. "Turn 3 —
fix the parser" tells a future model nothing about how the parser was fixed or what
was discovered. In practice the synopses are uninformative.

This ADR generates the Description with an **LLM call at the moment the Turn
collapses** — when it slides out of the recent-N window. The call reads the Turn's
full transcript and returns a one-line gist focused on the outcome and the key facts a
future round would need. The result is cached in `index.json`, where ADR-0004 already
keeps mutable Descriptions. The prompt's first line remains only as the fallback if the
call fails.

This **reverses the stance of ADR-0002 and ADR-0004**, which both rejected LLM
summarization on the premise that "a free bootstrap suffices." ADR-0005 makes that
premise false: with no independent Artifacts to lean on, a meaningless Turn synopsis is
the model's entire memory of the Turn.

## Considered options

- **Free bootstrap only (status quo).** Rejected: the prompt's first line is the
  request, not the result; under ADR-0005 that is the model's whole memory of a
  collapsed Turn.
- **Summarize at Turn close, synchronously.** Rejected: adds dead latency *after every
  Turn*, before the user can type the next prompt — exactly ADR-0004's objection to
  close-time summarization.
- **Summarize asynchronously after close.** Rejected: a background goroutine plus
  `index.json` write-concurrency is more machinery than a ~300-line agent should carry.
- **Summarize lazily, at collapse (this ADR).** Chosen: a Turn collapses at the *start
  of a later Turn*, when the agent is already producing the next response — so the
  call's latency hides inside a wait the user already pays. One short call per Turn in
  steady state, never speculative.
- **Rely on the model self-describing via the `describe` tool.** Rejected as the sole
  mechanism: voluntary and unreliable. Kept as an optional manual refinement on top of
  the automatic summary.

## Consequences

- **One extra LLM call per collapsed Turn**, one-time and cached. Cost grows with
  conversation length, but each call is small (one Turn in, one line out) and never
  repeated.
- **The latency lands inside an existing wait** (the next Turn's first response), not
  as idle time after a Turn.
- **The `describe` tool stays** — the model can refine any Turn's Description by number;
  the automatic summary is the reliable default it overrides.
- **The agent never blocks on summarization**: a failed call falls back to the prompt's
  first line, so a collapse always yields *some* Description.
- **`describe` no longer applies to Artifacts** (ADR-0005 removed independent
  Artifacts); it applies to Turns.
