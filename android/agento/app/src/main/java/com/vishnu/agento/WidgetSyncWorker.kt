package com.vishnu.agento

import android.content.Context
import android.util.Log
import androidx.work.CoroutineWorker
import androidx.work.WorkerParameters
import kotlinx.coroutines.CancellationException

/**
 * Periodic re-render of the home-screen widget (#595).
 *
 * Split out of [TaskSyncWorker]: task refresh (reminder alarms + project
 * snoozes, Settings → Behaviour) and widget refresh (Settings → Widget)
 * run on independent schedules, so each runs as often as its section asks.
 * Deliberately its own job rather than a second thing [SyncWorker] does:
 * that one is health data behind a foreground notification, and coupling
 * the widget to it would let a failed or backing-off health sync — or a
 * device that is simply asleep — silently starve the glanceable surface.
 */
class WidgetSyncWorker(
    context: Context,
    params: WorkerParameters,
) : CoroutineWorker(context, params) {

    private companion object {
        const val TAG = "WidgetSyncWorker"
    }

    override suspend fun doWork(): Result = try {
        // Awaited rather than fired and forgotten. A worker that returns
        // while its fetch is still in flight gets its process reaped, and
        // the refresh that was halfway through is the one that gets lost.
        val outcome = TaskWidget.refresh(applicationContext).await()
        // The refresh swallows failures and reports them as a value, so a
        // bare try/catch here would be a promise this class cannot keep.
        // NoPlacements is success: nothing installed, nothing to retry.
        if (outcome == RefreshOutcome.Failed) {
            Log.w(TAG, "widget refresh failed: $outcome")
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
        Log.w(TAG, "widget refresh failed", e)
        Result.retry()
    }
}
