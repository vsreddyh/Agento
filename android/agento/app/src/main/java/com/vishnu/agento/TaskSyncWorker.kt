package com.vishnu.agento

import android.content.Context
import android.util.Log
import androidx.work.CoroutineWorker
import androidx.work.WorkerParameters
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.awaitAll

/**
 * Periodic refresh of the task surfaces: the reminder alarms (#170) and
 * project snoozes.
 *
 * The home-screen widget used to ride this same tick; it now has its own
 * schedule ([WidgetSyncWorker], Settings → Widget) so each runs as often as
 * its section asks.
 *
 * Two calls, both no-ops when they have nothing to do: `refresh` returns
 * at once if reminders are off. The snooze check is the exception — it
 * lists the board every tick, because seeing a snooze the *agent* or
 * another device set is the only way this device learns to expect its
 * wake. That is one request on a tick that is already running, not a
 * second job.
 *
 * Neither call is gated on the task change marker (#178), and that is
 * deliberate. The marker answers "did the DATA change", but reminder alarms
 * are armed against a horizon that advances with the clock: gating on the
 * marker would leave an untouched list with tomorrow's alarms unarmed for a
 * day. The change marker gates the other path — the open Task Manager list
 * re-reads on resume when it changed — where the question really is "did
 * anything happen while I was away".
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
        // the refresh that was halfway through is the one that gets lost.
        val outcomes = awaitAll(
            TaskReminders.refresh(applicationContext),
            // Project snoozes ride the task tick instead of a second periodic
            // job: one wakeup, one network constraint, one retry policy.
            ProjectSnooze.check(applicationContext),
        )
        // Both report failure by swallowing it, so none throws
        // and a bare try/catch here would be a promise this class cannot
        // keep. Each returns its own outcome for this run instead — a
        // per-run value, not a shared field, because an in-app mutation
        // can start a refresh while this one is in flight.
        if (outcomes.any { it == RefreshOutcome.Failed }) {
            Log.w(TAG, "task refresh failed: $outcomes")
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
        Log.w(TAG, "task refresh failed", e)
        Result.retry()
    }
}
