package com.vishnu.agento

import android.content.Context
import android.util.Log
import androidx.work.CoroutineWorker
import androidx.work.WorkerParameters
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.joinAll

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
 */
class TaskSyncWorker(
    context: Context,
    params: WorkerParameters,
) : CoroutineWorker(context, params) {

    private companion object {
        const val TAG = "TaskSyncWorker"
    }

    override suspend fun doWork(): Result = try {
        // Joined rather than fired and forgotten. A worker that returns
        // while its fetch is still in flight gets its process reaped, and
        // the refresh that was halfway through is the one that gets lost —
        // which would leave the widget stale and look like the job is not
        // working at all.
        joinAll(
            TaskWidget.refresh(applicationContext),
            TaskReminders.refresh(applicationContext),
        )
        // Both refreshes report failure by swallowing it — a Job that
        // completes threw nothing, so a bare try/catch here would be a
        // promise this class cannot keep. The widget does record how its
        // last push ended, and that is the honest signal: when the server
        // is unreachable every state fails, which also covers the reminder
        // arming that failed silently for the same reason.
        when (TaskWidget.lastRefreshOutcome()) {
            RefreshOutcome.Failed -> {
                Log.w(TAG, "every widget state failed to fetch")
                Result.retry()
            }
            RefreshOutcome.Ok, RefreshOutcome.NoPlacements -> Result.success()
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
