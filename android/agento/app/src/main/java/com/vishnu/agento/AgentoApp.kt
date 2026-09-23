package com.vishnu.agento

import android.app.Application
import android.content.Context
import android.os.Bundle
import androidx.work.ExistingPeriodicWorkPolicy
import androidx.work.PeriodicWorkRequestBuilder
import androidx.work.WorkManager
import java.util.concurrent.TimeUnit

/** Application entry point; ensures hourly sync survives process death. */
class AgentoApp : Application() {

    /** Re-enqueues hourly sync on every cold start; safe to call repeatedly. */
    override fun onCreate() {
        super.onCreate()
        ForegroundTracker.install(this)
        scheduleSync(this)
    }

    companion object {
        // NOTE: Agento ships as a NEW app (applicationId com.vishnu.agento),
        // so it installs alongside the old Health Gateway build instead of
        // updating it. No state is shared across packages (separate
        // /data/data/<package> and WorkManager DBs), so there is deliberately
        // NO automatic migration of the old app's settings — re-enter config
        // in Agento Settings (see README/docs side-by-side install notice).
        const val PREFS_NAME = "agento"
        const val SYNC_WORK_NAME = "agento_sync"
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

/**
 * Tracks whether any activity is in the foreground so reply-done
 * notifications (#58) only fire when the user isn't looking at the app.
 */
object ForegroundTracker {

    @Volatile
    var isForeground: Boolean = false
        private set

    private var started = 0

    fun install(app: Application) {
        app.registerActivityLifecycleCallbacks(object : Application.ActivityLifecycleCallbacks {
            override fun onActivityStarted(activity: android.app.Activity) {
                started++
                isForeground = true
            }

            override fun onActivityStopped(activity: android.app.Activity) {
                started = (started - 1).coerceAtLeast(0)
                if (started == 0) isForeground = false
            }

            override fun onActivityCreated(activity: android.app.Activity, savedInstanceState: Bundle?) {}
            override fun onActivityResumed(activity: android.app.Activity) {}
            override fun onActivityPaused(activity: android.app.Activity) {}
            override fun onActivitySaveInstanceState(activity: android.app.Activity, outState: Bundle) {}
            override fun onActivityDestroyed(activity: android.app.Activity) {}
        })
    }
}
