package com.vishnu.agento

import android.content.Context
import android.util.Log
import androidx.work.CoroutineWorker
import androidx.work.ForegroundInfo
import androidx.work.WorkerParameters

/** Periodic WorkManager entry point; success ends the run, failure asks WorkManager to retry with backoff. */
class SyncWorker(
    context: Context,
    params: WorkerParameters,
) : CoroutineWorker(context, params) {

    private companion object {
        const val TAG = "SyncWorker"
    }

    /** Runs one sync under a foreground notification; tolerates setForeground failure on constrained devices. */
    override suspend fun doWork(): Result {
        try {
            setForeground(SyncNotifications.foregroundInfo(applicationContext))
        } catch (e: Exception) {
            Log.w(TAG, "setForeground failed, continuing without foreground", e)
        }
        val ok = SyncRunner.run(applicationContext).success
        return if (ok) Result.success() else Result.retry()
    }
}
