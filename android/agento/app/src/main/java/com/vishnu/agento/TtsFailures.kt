package com.vishnu.agento

import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow

/**
 * One place for text-to-speech failures to become visible (#434).
 *
 * TTS failures used to end in silence the user could not distinguish from
 * success: [ChatTts] cleared its speaking state and stopped, [ReminderTts]
 * logged and advanced its queue, and a broken engine rendered as an empty
 * voice list. Every one of those now reports here instead.
 *
 * Process-scoped, deliberately: reminders fire from a broadcast with no
 * Activity to toast into, so the failure is recorded and the next foreground
 * collector surfaces it. Reporting is idempotent-by-growth — the counter
 * moves so a repeated identical failure still surfaces once per occurrence,
 * and [consume] clears what was shown.
 */
object TtsFailures {

    /** A reported failure: what happened, and a trip count so repeated
     * identical failures still notify. */
    data class Failure(val message: String, val seq: Long)

    private val _last = MutableStateFlow<Failure?>(null)

    /** Latest unreported failure; null when nothing is pending. */
    val last: StateFlow<Failure?> = _last

    private val counter = java.util.concurrent.atomic.AtomicLong(0L)

    /** Records a failure for the next foreground surface. Thread-safe: the
     * TTS callbacks arrive off the main thread. */
    fun report(message: String) {
        if (message.isBlank()) return
        _last.value = Failure(message, counter.incrementAndGet())
    }

    /** Clears [message], but only if nothing newer replaced it — a failure
     * reported between the read and this call must stay pending. */
    fun consume(current: Failure?) {
        if (current != null) _last.compareAndSet(current, null)
    }
}
