# Atomic task batch with server-side duplicate

Bulk task actions run as one atomic server batch (`POST /api/tasks/batch`): one bulk run commits fully or not at all, including repeat rollover successors. We chose this over client-side loops with partial success because the requester wanted all-or-nothing semantics, which only a server transaction can guarantee.

## Considered Options

- Client loops over single-task endpoints with `done N, failed M`: simpler, no server change, but can leave partial state.
- Uncapped single transaction for any selection size: rejected; client caps selection at 50 and the server enforces the cap, so every batch stays inside Mongo transaction limits.

## Consequences

- No select-all; selection is capped at 50 client-side with `N/50` counter.
- Duplicate is a canonical server op, not a client `create` copy.
- Custom-repeat follow-ups stay post-commit; conflicts fail the whole batch for retry after refresh.
