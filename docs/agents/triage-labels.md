# Triage Labels

The skills speak in terms of five canonical triage roles. This file maps those roles to the actual label strings used in this repo's issue tracker ([beads](issue-tracker.md)).

| Label in mattpocock/skills | Label in our tracker | Meaning                                  |
| -------------------------- | -------------------- | ---------------------------------------- |
| `needs-triage`             | `needs-triage`       | Maintainer needs to evaluate this issue  |
| `needs-info`               | `needs-info`         | Waiting on reporter for more information |
| `ready-for-agent`          | `ready-for-agent`    | Fully specified, ready for an AFK agent  |
| `ready-for-human`          | `ready-for-human`    | Requires human implementation            |
| `wontfix`                  | `wontfix`            | Will not be actioned                     |

When a skill mentions a role (e.g. "apply the AFK-ready triage label"), use the corresponding label string from this table. The label strings are identical to the canonical role names.

## Applying labels in beads

- `bd update <id> --add-label <label>` to attach a triage role (repeat the flag for several labels).
- A fully triaged issue should carry exactly one state label; remove superseded ones (`bd label list` shows the existing vocabulary).
- State labels are orthogonal to the issue's type and priority, which keep their beads meanings (see `issue-tracker.md`).
