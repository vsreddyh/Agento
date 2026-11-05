package com.vishnu.agento

import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.SharedFlow

/**
 * One place for text-to-speech failures to become visible (#434).
 *
 * TTS failures used to end in silence the user could not distinguish from
 * success: [ChatTts] cleared its speaking state and stopped, [ReminderTts]
 * logged and advanced its queue. Every one of those now reports here
 * instead — except live milestone cues, which stay silent by decision (a
 * dead engine never becomes cue-ready), and a broken engine listing as an
 * empty voice picker (which renders as the Settings "isn't answering" hint
 * — see BehaviourSection — instead of reporting here).
 *
 * Process-scoped, deliberately: reminders fire from a broadcast with no
 * Activity to toast into, so the failure is recorded and the next
 * foreground collector surfaces it — provided the process is still alive
 * (#450 review): this is memory, not storage, so a broadcast process that
 * dies before the next open loses the value.
 *
 * A queue, not last-wins (#450 review): the reminder pump can report per
 * queued item in one tight pass, and a single `StateFlow<Failure?>` would
 * keep only the last before the collector runs. Up to [MAX_BUFFERED]
 * reports surface once each; beyond that the newest are dropped rather
 * than growing this object for the life of the process.
 */
object TtsFailures {

    /** A reported failure: what happened, and how it renders. Info is
     * for benign user-action outcomes (a superseded tap) that must not
     * wear the error styling (#450 review). */
    data class Failure(val message: String, val kind: ToastKind = ToastKind.Error)

    private const val MAX_BUFFERED = 64

    private val _events =
        MutableSharedFlow<Failure>(extraBufferCapacity = MAX_BUFFERED)

    /** Reported failures, in order. Buffered while no collector runs (up
     * to [MAX_BUFFERED]; beyond that the newest reports are dropped). */
    val events: SharedFlow<Failure> = _events

    /** Records a failure for the next foreground surface. Thread-safe: the
     * TTS callbacks arrive off the main thread. Non-suspending: bursting
     * past a full buffer drops rather than blocking the audio thread. */
    fun report(message: String, kind: ToastKind = ToastKind.Error) {
        if (message.isBlank()) return
        _events.tryEmit(Failure(message, kind))
    }
}

/**
 * Process-scoped toast queue for TTS failures (#450 review).
 *
 * The SharedFlow buffer only covers the never-collected case: once the
 * activity collects an event into its own ToastCenter, a rotation drops
 * the activity (and its queued toasts) while the buffer is already
 * consumed. This queue rides out config changes, so "each surfaces once"
 * holds across rotation too — within the same caps as the buffer (up to 64
 * queued; backgrounding mid-display still lets the 4.5s dismiss timer run).
 * Scope is process-lifetime by design (SupervisorJob, never cancelled);
 * display still belongs to whichever activity instance hosts the ToastHost.
 */
internal object TtsToastQueue {
    val center = ToastCenter(
        CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate),
    )
}
