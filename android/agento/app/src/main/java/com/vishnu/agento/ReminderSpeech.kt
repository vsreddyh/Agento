package com.vishnu.agento

/**
 * Spoken copy for task reminders (#306). Distinct from [reminderMessage],
 * which is the lock-screen line: speech names the task so a hands-free
 * listener knows *what* fired without looking.
 */
internal fun reminderSpeech(
    kind: ReminderKind,
    taskName: String,
    speakName: String = "",
    overdueElapsed: Long? = null,
): String {
    val name = speakName.ifBlank { taskName }.ifEmpty { "Task due" }
    return when (kind) {
        ReminderKind.BeforeStart ->
            "In 5 minutes you need to start \"$name\""
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
