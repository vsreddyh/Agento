package com.vishnu.agento

/**
 * Spoken copy for task reminders (#306). Distinct from [reminderMessage],
 * which is the lock-screen line: speech names the task so a hands-free
 * listener knows *what* fired without looking.
 */
/** Name spoken when the task could not be fetched — one string, not two. */
internal const val FALLBACK_TASK_NAME = "Task due"

internal fun reminderSpeech(
    kind: ReminderKind,
    taskName: String,
    speakName: String = "",
    overdueElapsed: Long? = null,
): String {
    // Capped like speak_name: the list name has no server-side cap, so the
    // fallback path most tasks actually speak gets the same 120-byte budget.
    // Both stages blank-checked: a whitespace-only name speaks the fallback.
    val name = truncateUtf8Bytes(
        speakName.ifBlank { taskName }.ifBlank { FALLBACK_TASK_NAME },
        SPEAK_NAME_MAX_BYTES,
    )
    return when (kind) {
        ReminderKind.BeforeStart ->
            "In ${TaskReminders.BEFORE_START_MINUTES} minutes you need to start \"$name\""
        ReminderKind.Start ->
            "You need to start \"$name\""
        ReminderKind.Due ->
            "Time to \"$name\" is up"
        ReminderKind.Overdue ->
            if (overdueElapsed != null) {
                val totalMin = (overdueElapsed / 60_000L).toInt().coerceAtLeast(1)
                val h = totalMin / 60
                val m = totalMin % 60
                val dur = when {
                    h == 0 -> "$m min"
                    m == 0 -> "$h hr"
                    else -> "$h hr $m min"
                }
                "\"$name\" has been pending for $dur"
            } else {
                "\"$name\" is overdue"
            }
    }
}
