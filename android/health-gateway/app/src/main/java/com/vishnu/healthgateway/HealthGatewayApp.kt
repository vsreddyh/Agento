package com.vishnu.healthgateway

import android.app.Application
import android.content.Context
import androidx.work.ExistingPeriodicWorkPolicy
import androidx.work.PeriodicWorkRequestBuilder
import androidx.work.WorkManager
import java.util.concurrent.TimeUnit

/** Application entry point; ensures hourly sync survives process death. */
class HealthGatewayApp : Application() {

    /** Re-enqueues hourly sync on every cold start; safe to call repeatedly. */
    override fun onCreate() {
        super.onCreate()
        scheduleSync(this)
    }

    companion object {
        const val SYNC_WORK_NAME = "health_gateway_sync"
        const val SYNC_INTERVAL_HOURS = 1L

        /** Unique periodic work with UPDATE policy so reinstalls never duplicate. */
        fun scheduleSync(context: Context) {
            val request = PeriodicWorkRequestBuilder<SyncWorker>(
                SYNC_INTERVAL_HOURS, TimeUnit.HOURS
            ).build()
            WorkManager.getInstance(context).enqueueUniquePeriodicWork(
                SYNC_WORK_NAME,
                ExistingPeriodicWorkPolicy.UPDATE,
                request,
            )
        }
    }
}
