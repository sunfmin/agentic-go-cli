# Structured read/edit/run tools instead of a single bash tool

The agent externalizes tool results to disk and sends a Manifest of Descriptions
instead of full content (see CONTEXT.md). For that to pay off, the tool layer must
know a call's *file identity*: a `read` can then be a free, never-stale live
reference to the working-tree file (re-fetched on demand, so a later `edit` can
never strand stale content in the context), and only opaque command output needs to
be stored as an Artifact. A single generic `bash` tool can't provide this — code
can't tell that `cat main.go` "is" main.go — so we split it into structured `read`,
`edit`, and `run` tools.

## Considered options

- **Keep the single `bash` tool.** The original design's deliberate minimalism — the
  README celebrates "a single tool — bash — which covers everything." Rejected:
  Descriptions can't be derived for free, stale reads can't be auto-invalidated, and
  every file read becomes a stored, staleable Artifact.

## Consequences

- The command tool (named `run` or `shell`, **not** `bash`) must be fenced in its
  description against reading or editing files. Routing `cat` / `sed` / `>`
  redirects through it bypasses file-identity tracking and silently reintroduces
  Stale Artifacts and broken auto-forget. The name matters behaviorally: `bash`
  carries the strongest "filesystem swiss-army knife" prior, so the fence does the
  real work and the name should not undercut it.
