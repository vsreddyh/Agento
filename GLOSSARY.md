# Agento

Task Manager and supporting services for personal task tracking.

## Language

**Task**:
A unit of work with a name, due date/time, and optional repeat — open, done, or skipped (skipped: a resolved occurrence not completed).
_Avoid_: todo, item, reminder

**Complete**:
Marking a task done; a repeating task rolls over to its next occurrence server-side.
_Avoid_: finish, close, check off

**Skip**:
Marking an open occurrence not done without completing it; a repeating task still rolls over.
_Avoid_: snooze, defer, dismiss

**Reopen**:
Returning a done or skipped task to open.
_Avoid_: uncomplete, restore

**Duplicate**:
A new task made from an existing one, carrying its contents (name, description, due date/time, estimate, parallelable, repeat, voice phrase). Notification timing follows the carried due date/time.
_Avoid_: copy, clone

**Move**:
Changing a task's due date, optionally with time.
_Avoid_: reschedule, push, shift

**Bulk run**:
One user-initiated action applied to a set of selected tasks, e.g. completing five tasks at once.
_Avoid_: multi-select action

**Batch**:
A single server request carrying one bulk run; it commits atomically or not at all.
_Avoid_: bulk request

**Change log**:
The record of every task mutation — what changed, which task, which caller, when — kept 90 days and outliving the task it describes, since a completed task is deleted after 3 days.
_Avoid_: transaction (that means a money record here), activity log

Its surfaces are named after it: the `task_history` agent tool and `GET /api/tasks/mutations`. "History" there is the name of the surface, not a synonym to reach for in prose — the concept is the change log.
