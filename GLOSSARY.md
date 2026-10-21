# Agento

Task Manager and supporting services for personal task tracking.

## Language

**Task**:
A unit of work with a name, due date/time, and optional repeat — open, done, or skipped (a done occurrence not completed).
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
A new task made from an existing one, carrying its contents (details, due date/time, estimate, parallelable, repeat, voice phrase). Reminders follow the carried due date/time.
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
