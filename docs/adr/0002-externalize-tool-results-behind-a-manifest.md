# Externalize tool results behind a per-round Manifest

The naive agent re-sends the entire growing message history every round, so every
verbose tool output (file reads, test logs) is paid for again on every later turn.
Instead, the agent rewrites the conversation before each request: the most recent
tool result is sent in full (the model issued the call precisely to see it), and
every older tool result collapses to a Manifest entry — a name plus a one-line
Description. The model pulls a specific entry's full content back on demand (a
`read`) and forgets entries it no longer needs. File reads are live references
re-fetched from the working tree (never stored, never stale); only non-reproducible
command output is stored as an Artifact, retained by default until forgotten.

## Considered options

- **Naive append-only history.** Simplest; what the agent does today. Rejected:
  cost grows with conversation length as old verbose outputs are re-sent every round.
- **LLM-summarized compaction on a threshold.** Periodically summarize old turns.
  Rejected: adds inference calls and latency and is lossy; for a code agent the bulk
  is tool output, which Manifest-collapse handles without a second model.

## Consequences

- The conversation is rewritten each round, so a stable prompt prefix (and thus
  prompt caching) is sacrificed below the latest turn. Moot today (no `cache_control`
  set, subscription auth), but a future reader optimizing for cache should know it is
  a deliberate trade.
- Collapsing an older tool result must preserve the `tool_use` / `tool_result` block
  pairing — replace the result's *content* with the Manifest entry, keep the IDs — or
  the API rejects the request. Forgetting means dropping the assistant `tool_use` and
  its paired `tool_result` together. (Amended by ADR-0004: this holds only within the
  full-Turn window; a collapsed Turn drops its `tool_use`/`tool_result` plumbing as a
  unit, and the Manifest moves out of `tool_result` positions into a standalone
  section.)
- Requires the structured tools of ADR-0001 to know each call's file identity.
