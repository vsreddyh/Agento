package com.vishnu.agento

import java.time.LocalDate
import java.time.ZoneId
import java.time.format.DateTimeFormatter
import java.util.Locale

/** Display zone for every user-visible time (#124). Single-user app with
 * no per-user zone: IST is pinned so tasks, reminders and sync
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

/**
 * Whether this build is debuggable, cached once at app start (#339): the
 * IO-thread gate must not do a PackageManager-adjacent read on the very
 * thread it is trying to keep clean. Set by [AgentoApp.onCreate]; before
 * that (and in JVM tests) it stays false and the gate only logs.
 */
internal object DebugBuild {
    @Volatile
    var debuggable: Boolean = false
}

internal fun requireIoThread(site: String) {
    // runCatching (not try/catch): the fallback documents the JVM case inline.
    val onMain = runCatching {
        val mainLooper = android.os.Looper.getMainLooper()
        android.os.Looper.myLooper() !== null && android.os.Looper.myLooper() === mainLooper
    }.getOrDefault(false)
    if (onMain) {
        // Debug builds FAIL FAST (#339): a debuggable app throws, so a dev
        // device surfaces the slip immediately instead of janking silently.
        // Production only logs — a crash there would take the app
        // down over a performance mistake.
        if (DebugBuild.debuggable) {
            throw IllegalStateException("$site must run on an IO thread, never Main")
        }
        // Stacked, not thrown: a Main-thread caller still does the right work.
        android.util.Log.e("Agento", "$site ran on Main (should be IO)", IllegalStateException(site))
    }
}
