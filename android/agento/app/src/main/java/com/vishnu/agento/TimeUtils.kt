package com.vishnu.agento

import java.time.ZoneId

/** Display zone for every user-visible time (#124). Single-user app with
 * no per-user zone: IST is pinned so chat, tasks, reminders and sync
 * stamps read identically on any device zone. Lives here (not in a UI
 * file) so non-UI code like TaskReminders reads it without reaching
 * across screens. */
val IST: ZoneId = ZoneId.of("Asia/Kolkata")

/** Human due line for task rows and the detail sheet: relative day +
 * short date, IST-pinned like every other displayed date (#124).
 * Empty when the task has no due date. Presentation only — buckets
 * and overdue rules still read the raw ISO strings. */
fun friendlyDue(dueDate: String, dueTime: String, today: java.time.LocalDate): String {
    if (!dueDate.isIsoDate()) return ""
    val day = when {
        dueDate < today.toString() -> null // overdue: show the date itself
        dueDate == today.toString() -> "Today"
        dueDate == today.plusDays(1).toString() -> "Tomorrow"
        dueDate <= today.plusDays(7).toString() ->
            runCatching {
                java.time.LocalDate.parse(dueDate)
                    .format(java.time.format.DateTimeFormatter.ofPattern("EEEE"))
            }.getOrDefault(dueDate)
        else ->
            runCatching {
                val d = java.time.LocalDate.parse(dueDate)
                val pat = if (d.year == today.year) "d MMM" else "d MMM yyyy"
                d.format(java.time.format.DateTimeFormatter.ofPattern(pat))
            }.getOrDefault(dueDate)
    }
    val datePart = day ?: runCatching {
        java.time.LocalDate.parse(dueDate)
            .format(java.time.format.DateTimeFormatter.ofPattern("d MMM"))
    }.getOrDefault(dueDate)
    val time = dueTime.trim()
    return if (time.isNotEmpty()) "$datePart, $time" else datePart
}
