# Atomic task batch with server-side duplicate

Bulk task actions run as one atomic server batch (`POST /api/tasks/batch`): one bulk run commits fully or not at all, including repeat rollover successors. We chose this over client-side loops with partial success because the requester wanted all-or-nothing semantics, which only a server transaction can guarantee.

## Considered Options

- Client loops over single-task endpoints with `done N, failed M`: simpler, no server change, but can leave partial state.
- Uncapped single transaction for any selection size: rejected; client caps selection at 50 and the server enforces the cap, so every batch stays inside Mongo transaction limits.

## Consequences

- No select-all; selection is capped at 50 client-side with `N/50` counter.
- Duplicate is a canonical server op, not a client `create` copy.
- Custom-repeat follow-ups stay post-commit; conflicts fail the whole batch for retry after refresh.
- 503 + no blind retry: a deployment without transactions refuses the batch, and a timeout leaves the outcome unknown — refresh via the changes marker and resend rather than retrying blind (a retry would duplicate twice).
- Guard split: bulk move carries `expected_revision` per op; selected rows that left the fetched page fail closed client-side (reload + reselect) instead of going out unguarded. Bulk complete/skip stay unguarded, like the single verbs.
- Single duplicate inherits the batch's transaction requirement (it rides a one-op batch).
