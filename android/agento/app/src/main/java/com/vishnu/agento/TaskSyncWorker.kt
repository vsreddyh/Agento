package com.vishnu.agento

import android.content.Context
import android.util.Log
import androidx.work.CoroutineWorker
import androidx.work.WorkerParameters
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.awaitAll

/**
 * Periodic refresh of the task surfaces: the home-screen widget and the
 * reminder alarms (#170).
 *
 * These used to move only when the app was open and a task was changed *in
 * the app*. A task created or completed by the agent — the whole point of
 * having an assistant write to the same list — reached the widget only when
 * the app was next opened and something else was mutated, so the most
 * glanceable surface in the app went stale exactly when the app was not
 * being looked at.
 *
 * Deliberately its own job rather than a second thing [SyncWorker] does:
 * that one is health data behind a foreground notification, and coupling
 * the widget to it would let a failed or backing-off health sync — or a
 * device that is simply asleep — silently starve the task list. This one
 * has one job and one cadence.
 *
 * Both calls are no-ops when they have nothing to do: `refresh` returns at
 * once if reminders are off, and the widget's push returns if no placement
 * is installed. A user with neither still pays one no-op wakeup.
 *
 * Neither call is gated on the task change marker (#178), and that is
 * deliberate. The marker answers "did the DATA change", but these two
 * surfaces also have to re-render because TIME moved: the widget's rows print
 * "Today"/"Tomorrow" from a date that ages, and reminder alarms are armed
 * against a horizon that advances with the clock. Gating on the marker would
 * leave an untouched list showing yesterday's labels and tomorrow's alarms
 * unarmed for a day. The change marker gates the other path — the open Task
 * Manager list re-reads on resume when it changed — where the question
 * really is "did anything happen while I was away".
 */
class TaskSyncWorker(
    context: Context,
    params: WorkerParameters,
) : CoroutineWorker(context, params) {

    private companion object {
        const val TAG = "TaskSyncWorker"
    }

    override suspend fun doWork(): Result = try {
        // Awaited rather than fired and forgotten. A worker that returns
        // while its fetch is still in flight gets its process reaped, and
        // the refresh that was halfway through is the one that gets lost —
        // which would leave the widget stale and look like the job is not
        // working at all.
        val outcomes = awaitAll(
            TaskWidget.refresh(applicationContext),
            TaskReminders.refresh(applicationContext),
        )
        // Both refreshes report failure by swallowing it, so neither throws
        // and a bare try/catch here would be a promise this class cannot
        // keep. Each returns its own outcome for this run instead — a
        // per-run value, not a shared field, because an in-app mutation
        // can start a refresh while this one is in flight.
        if (outcomes.any { it == RefreshOutcome.Failed }) {
            Log.w(TAG, "task surface refresh failed: $outcomes")
            Result.retry()
        } else {
            Result.success()
        }
    } catch (e: Exception) {
        // A cancelled worker must not be retried: cancellation is how
        // WorkManager and the system stop a run, and it is an
        // IllegalStateException by inheritance, so a bare catch would turn
        // "stop" into "try again".
        if (e is CancellationException) throw e
        Log.w(TAG, "task surface refresh failed", e)
        Result.retry()
    }
}
