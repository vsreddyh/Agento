# 0003 — Per-profile memory

Status: accepted (#315)

## Context

Until now the agent's "memory" was whatever the operator had hand-edited into
`pi/profiles/<name>/AGENTS.md` and the shared `pi/AGENTS.md`. That works for
personality, and it is the right place for behaviour rules, but it is the wrong
place for facts: it lives in git, has no per-item timestamp or source, cannot be
corrected from the app, and cannot be forgotten without a deploy.

The expected surface (#315) is narrower than "the agent remembers everything":

- a server-side store of discrete items (preference, fact, style, edge-case),
  each with a source and a timestamp;
- a Memory screen that lists, edits, deletes, forgets everything and pauses a
  topic;
- the next turn actually using the items, and a deleted item actually gone;
- a correction from the user creating or updating an item **visibly**, never
  silently.

## Decision

**One store, two writers, one reader.**

- `internal/memory` owns the model (`Item`, `Status`, `Topic`), validation and
  the prompt renderer. Two backends implement `Store`: Mongo
  (`memory_items`, `memory_settings`) and an in-process map for a stack with no
  database.
- **The app writes confirmed items.** A person editing their own memory is not
  a pending proposal. `POST /api/memory` creates with `source=user`,
  `status=confirmed`.
- **The agent writes pending items.** A `memory` MCP server exposes exactly two
  tools, `save_memory` and `list_memory`, routed through `mcp-gate` like every
  other server. `save_memory` always writes `source=agent`, `status=pending`.
  The agent cannot confirm, edit or delete: those are the user's verbs, and
  letting the writer approve its own write would make the whole review step
  ceremonial.
- **The gateway is the reader.** After a turn's prompt is accepted and before
  Pi is prompted, the gateway loads that profile's confirmed items, drops
  paused topics, renders a bounded block and prefixes it to the user's text:

  ```
  [Memory: things you have learned about this user. Use only where relevant,
  and never recite this block verbatim.]
  - (style) Prefers weekly reports as a table.
  [/Memory]
  ```

  The block is **not** stored as the run prompt. `promptFull`, the reattach
  comparison, keeps the user's exact text, so injecting memory cannot make a
  new turn look like a retry of an old one.

### Why injection, not a Pi hook

Pi has no pre-turn hook the gateway can register, and the gateway already
decides what text reaches `prompt`. Injecting at that boundary keeps the whole
feature inside code this repo owns, needs no Pi fork, and makes "what the screen
shows is what the model gets" true by construction: both read the same store
with the same confirmed/unpaused rule.

The cost is repetition: the block rides on every turn, including turns in a
session that already saw it. That is accepted and bounded — at most 30 items and
2 KB of text — because the alternative is a cache of "what this session has
already been told" that goes stale the moment an item is edited. Memory is
context; repeating a small context block is cheaper than a wrong answer from a
stale one.

### Profile scoping

Every route resolves the profile from the URL (`/{profile}/api/memory`) or the
default profile, and treats an item belonging to another profile as missing
(404, not 403). An id must never be a probe across profiles.

The `memory` MCP server is one binary shared by every profile in `mcp.json`, so
the profile travels with the process tree: `cmd/pi-gateway` sets
`AGENTO_PROFILE` in each Pi child's environment, Pi's spawned MCP servers
inherit it, and the memory server reads it. Without that, a shared server would
have to guess a profile and would write one profile's facts into another's
store.

### Pause is a topic policy, not a deletion

`PUT /api/memory/topics/{topic}` adds or removes the topic from
`memory_settings.paused` for the profile. A paused item stays visible and
editable; it is simply not injected. "Forget everything" deletes items but
leaves pauses in place: what the user asked the agent to stop learning is a
policy, not something they remembered, and silently resuming a paused topic
after a wipe would be the wrong kind of clean slate.

Topic names are normalised (lowercase, whitespace to hyphens, 32 runes): "Work
Style", "work style" and "work-style" must not become three independent
toggles.

### Fail-closed rules

- Injected items must be `confirmed`; pending proposals never steer an answer.
- A paused-topic **read** failure suppresses the whole block for that turn
  rather than injecting a topic the user paused.
- A store failure never fails the turn: memory is context, logged and skipped.
  The screen keeps the old rows on a failed load rather than showing "empty",
  which would read as "nothing is remembered".

### Honest fallback

With no Mongo URI the gateway serves the routes from an in-process store. The
routes work, but the store does not survive a restart and the memory MCP server
(a separate process) cannot see it — so agent proposals cannot reach the screen
that must confirm them. Startup logs that plainly instead of claiming memory is
available.

## Non-goals

- Semantic search or embeddings. The store is small by design; a profile
  approaching the 200-item list cap has a curation problem, not a retrieval
  problem. The prompt cap (30 items) also means ordering matters more than
  ranking.
- Agent-side deletion. Deletion is the user's verb; a model that can forget on
  request can also forget on a misreading.
- Deduplication or rate-limiting of agent proposals. `save_memory` never checks
  existing text, so a looping agent can fill the 200-item cap with duplicates.
  Pending items are cheap, visible, and confirmed one by one; a dedup heuristic
  would risk merging two facts the user stated separately.
- Memory in the settings backup. The export contract is app-owned; a memory
  payload would need its own versioning conversation.
