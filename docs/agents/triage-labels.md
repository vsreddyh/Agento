# Triage Labels

The skills speak in terms of five canonical triage roles. This file maps those roles to the actual label strings used in this repo's issue tracker.

| Label in mattpocock/skills | Label in our tracker | Meaning                                  |
| -------------------------- | -------------------- | ---------------------------------------- |
| `needs-triage`             | `needs-triage`       | Maintainer needs to evaluate this issue  |
| `needs-info`               | `needs-decision`     | Waiting on a human answer before it can move |
| `ready-for-agent`          | `ready-for-agent`    | Fully specified, ready for an AFK agent  |
| `ready-for-human`          | `ready-for-human`    | Requires human implementation            |
| `wontfix`                  | `wontfix`            | Will not be actioned                     |

When a skill mentions a role (e.g. "apply the AFK-ready triage label"), use the corresponding label string from this table.

## Relationship to the existing vocabulary

These five are **orthogonal** to the labels already used in this repo, and are applied alongside them, never instead of them:

- `P0`–`P3` — severity / scheduling
- `area:app`, `area:server`, `area:agent`, `area:ci` — which part of the stack
- `size:s`, `size:m`, `size:l` — effort
- `bug`, `enhancement`, `documentation`, `question`, … — GitHub defaults

`needs-decision` doubles as the `needs-info` role: both mean "cannot move until a human answers something". Do not add a separate `needs-info` label.

Edit the "Label in our tracker" column if the vocabulary changes.
