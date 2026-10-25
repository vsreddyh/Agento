package com.vishnu.agento

import android.content.Context
import android.util.Log
import androidx.work.CoroutineWorker
import androidx.work.WorkerParameters
import java.util.concurrent.TimeUnit
import kotlinx.coroutines.CancellationException

/**
 * One automatic update check (#366): the startup one-shot and the periodic
 * poll share this worker, because the work is identical and only the
 * schedule differs.
 *
 * A failure retries with an hourly exponential backoff rather than the
 * WorkManager default (30 s): the common failure is a rate limit or a flaky
 * connection, and retrying a public GitHub endpoint every 30 seconds is
 * exactly what the unauthenticated quota punishes. The periodic job's own
 * cadence is the long-stop; a failed startup one-shot simply tries again
 * later.
 */
class UpdateCheckWorker(
    context: Context,
    params: WorkerParameters,
) : CoroutineWorker(context, params) {

    override suspend fun doWork(): Result = try {
        // notify = true: automatic checks run with no section on screen
        // (and the gate itself suppresses foreground notifications).
        val result = UpdateCheck.run(applicationContext, notify = true)
        if (result.isSuccess) {
            Result.success()
        } else {
            Log.w(TAG, "update check failed: ${result.exceptionOrNull()?.message}")
            Result.retry()
        }
    } catch (e: Exception) {
        // Cancellation is how WorkManager stops the run; never retry it.
        if (e is CancellationException) throw e
        Log.w(TAG, "update check failed", e)
        Result.retry()
    }

    companion object {
        const val TAG = "UpdateCheckWorker"

        /** Backoff for both the periodic request and the startup one-shot. */
        val BACKOFF_MS: Long = TimeUnit.HOURS.toMillis(1)
    }
}
