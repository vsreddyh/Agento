package com.vishnu.agento

import android.content.Context
import android.util.Log
import androidx.work.Constraints
import androidx.work.CoroutineWorker
import androidx.work.ExistingPeriodicWorkPolicy
import androidx.work.NetworkType
import androidx.work.PeriodicWorkRequestBuilder
import androidx.work.WorkManager
import androidx.work.WorkerParameters
import kotlinx.coroutines.CancellationException
import java.util.concurrent.TimeUnit

/**
 * Deep-link contract for cron chats: a cron notification tap opens the
 * Scheduler on the stored job, the way a reminder tap opens Task Manager
 * on its task.
 */
internal object CronIntents {
    const val ACTION_CRON = "com.vishnu.agento.action.CRON_JOB"
    const val EXTRA_JOB_ID = "com.vishnu.agento.extra.JOB_ID"
}

/**
 * Periodic cron discovery: stored responses the gateway fired while away
 * surface as local notifications (#289).
 *
 * Its own worker rather than a second thing TaskSyncWorker does — same
 * reasoning as that class documents: one job, one cadence, and a cron
 * fetch failure must not retry the task refresh. Same half-hour cadence,
 * same network constraint; a tick with no fresh runs costs one small
 * fetch and no notification.
 */
class CronSyncWorker(
    context: Context,
    params: WorkerParameters,
) : CoroutineWorker(context, params) {

    override suspend fun doWork(): Result = try {
        CronCheck.check(applicationContext)
        Result.success()
    } catch (e: Exception) {
        if (e is CancellationException) throw e
        Log.w(TAG, "cron check failed", e)
        Result.retry()
    }

    companion object {
        private const val TAG = "CronSyncWorker"
        const val WORK_NAME = "agento_cron_sync"
        const val INTERVAL_MINUTES = 30L

        /** Unique periodic work with UPDATE policy, like the task sync. */
        fun schedule(context: Context) {
            val request = PeriodicWorkRequestBuilder<CronSyncWorker>(
                INTERVAL_MINUTES, TimeUnit.MINUTES,
            ).setConstraints(
                Constraints.Builder()
                    .setRequiredNetworkType(NetworkType.CONNECTED)
                    .build(),
            ).build()
            WorkManager.getInstance(context).enqueueUniquePeriodicWork(
                WORK_NAME,
                ExistingPeriodicWorkPolicy.UPDATE,
                request,
            )
        }
    }
}
