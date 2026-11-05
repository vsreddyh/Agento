package com.vishnu.agento

import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.atomic.AtomicLong

/**
 * One place for coerced-to-default values to become visible (#437).
 *
 * Every read path below tolerates bad data by falling back to a default —
 * which is right, but until now it was silent: a corrupted pref, a
 * version-skewed writer or a mistyped server field changed what the user
 * saw with nothing to explain it ([TaskManagerScreen]'s filter,
 * [TaskWidget]'s per-placement settings, [ChatThreads]'s interrupted
 * cause, and numeric task fields parsed by [TasksApi]).
 *
 * Reporting is deduplicated per (key, raw value): the same bad value is
 * read on every refresh or recomposition, and this is about *discovering*
 * corruption, not counting it. The set is bounded — once it is full, new
 * pairs stop reporting rather than growing this object for the life of
 * the process.
 *
 * MainActivity collects [last] and toasts it; a reset recorded from a
 * background worker surfaces on the next foreground frame.
 */
object ValueResets {

    /** One reset: [key] identified the value, [raw] is what was stored,
     * [became] is the default that replaced it, and [seq] gives repeated
     * identical reports a distinct identity. */
    data class Reset(
        val key: String,
        val raw: String,
        val became: String,
        val seq: Long,
    ) {
        /** Toast text: names all three facts the issue asks for. */
        val message: String get() = "Reset $key — \"$raw\" isn't valid; using $became."
    }

    private const val MAX_TRACKED = 256
    private const val MAX_RAW_CHARS = 80

    private val _last = MutableStateFlow<Reset?>(null)

    /** Latest unreported reset; null when nothing is pending. */
    val last: StateFlow<Reset?> = _last

    private val counter = AtomicLong(0L)
    private val seen: MutableSet<String> = ConcurrentHashMap.newKeySet()

    /**
     * Records that [raw] could not be used for [key] and [became] was
     * substituted. A null [raw] is absence, not corruption, and is ignored
     * — so is a raw value that already equals the default (nothing was
     * actually reset).
     */
    fun record(key: String, raw: Any?, became: Any) {
        val stored = raw?.toString()?.take(MAX_RAW_CHARS) ?: return
        val fallback = became.toString()
        if (stored == fallback) return
        if (seen.size >= MAX_TRACKED) return
        if (!seen.add("$key\u0000$stored")) return
        _last.value = Reset(key, stored, fallback, counter.incrementAndGet())
    }

    /** Clears [current], but only if nothing newer replaced it. */
    fun consume(current: Reset?) {
        if (current != null) _last.compareAndSet(current, null)
    }
}
