# Atomic task batch with server-side duplicate

Bulk task actions run as one atomic server batch (`POST /api/tasks/batch`): one bulk run commits fully or not at all, including repeat rollover successors. We chose this over client-side loops with partial success because the requester wanted all-or-nothing semantics, which only a server transaction can guarantee.

## Considered Options

- Client loops over single-task endpoints with `done N, failed M`: simpler, no server change, but can leave partial state.
- Uncapped single transaction for any selection size: rejected; client caps selection at 50 and the server enforces the cap, so every batch stays inside Mongo transaction limits.

## Consequences

- No select-all; selection is capped at 50 client-side with `N/50` counter.
- Duplicate is a canonical server op, not a client `create` copy.
- Custom-repeat follow-ups stay post-commit; move conflicts fail the whole batch for retry after refresh (complete/skip are unguarded by design — only move carries a revision guard).
- 503 + no blind retry: a deployment without transactions refuses the batch, and a timeout leaves the outcome unknown — refresh via the changes marker and resend rather than retrying blind (a retry would duplicate twice).
- Guard split: bulk move carries `expected_revision` per op; bulk resolve filters to open rows in the fetched page. Anything else selected (left the page, already resolved) is set aside with a count — no fail-closed reload, no unguarded send. Bulk complete/skip stay unguarded, like the single verbs.
- Bulk move is all-or-nothing: one row failing validation (e.g. a selected-days date mismatch) aborts the batch with the item named — fix the row or deselect it and retry. The batch never partially applies.
- Single duplicate inherits the batch's transaction requirement (it rides a one-op batch).
- Bulk move is date-only: each row keeps its own stored time. A mixed selection (timed + untimed rows) lands on one date with per-row times, said once in the toast, never per row.
