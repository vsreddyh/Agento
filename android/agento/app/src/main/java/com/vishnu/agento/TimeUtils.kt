package com.vishnu.agento

import java.time.LocalDate
import java.time.ZoneId
import java.time.format.DateTimeFormatter
import java.util.Locale

/** Display zone for every user-visible time (#124). Single-user app with
 * no per-user zone: IST is pinned so chat, tasks, reminders and sync
 * stamps read identically on any device zone. Lives here (not in a UI
 * file) so non-UI code like TaskReminders reads it without reaching
 * across screens. */
val IST: ZoneId = ZoneId.of("Asia/Kolkata")

/** Human due line for task rows and the detail sheet: relative day +
 * short date, IST-pinned like every other displayed date (#124).
 * Empty when the task has no due date. Presentation only — buckets
 * and overdue rules still read the raw ISO strings. Patterns are
 * pinned to English so output is stable across device locales. */
fun friendlyDue(dueDate: String, dueTime: String, today: LocalDate): String {
    if (!dueDate.isIsoDate()) return ""
    val due = runCatching { LocalDate.parse(dueDate) }.getOrNull() ?: return ""
    fun LocalDate.fmt(pat: String): String =
        format(DateTimeFormatter.ofPattern(pat, Locale.ENGLISH))
    val short = if (due.year == today.year) "d MMM" else "d MMM yyyy"
    val datePart = when {
        due == today -> "Today"
        due == today.plusDays(1) -> "Tomorrow"
        due <= today.plusDays(7) && due.isAfter(today) -> due.fmt("EEEE")
        else -> due.fmt(short)
    }
    val time = dueTime.trim()
    return if (time.isNotEmpty()) "$datePart, $time" else datePart
}
