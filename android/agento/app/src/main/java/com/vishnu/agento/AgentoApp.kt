package com.vishnu.agento

import android.app.Application
import android.content.Context
import android.os.Bundle
import androidx.work.BackoffPolicy
import androidx.work.Constraints
import androidx.work.ExistingPeriodicWorkPolicy
import androidx.work.ExistingWorkPolicy
import androidx.work.NetworkType
import androidx.work.OneTimeWorkRequestBuilder
import androidx.work.PeriodicWorkRequestBuilder
import androidx.work.WorkManager
import java.util.concurrent.TimeUnit

/**
 * Application entry point; ensures the health sync and the task refresh
 * both survive process death. Cadences come from [BehaviourSettings]
 * (defaults: hourly sync, half-hourly refresh); see the Behaviour section.
 */
class AgentoApp : Application() {

    /** Re-enqueues all periodic jobs on every cold start; safe to call repeatedly. */
    override fun onCreate() {
        super.onCreate()
        // First: a crash later in this method must still be recorded.
        CrashLogs.install(this)
        ForegroundTracker.install(this)
        // Cached once, read by the IO-thread gate: a debuggable build fails
        // fast on a Main-thread boundary call (#339), a release build only
        // logs it.
        DebugBuild.debuggable =
            (applicationInfo.flags and android.content.pm.ApplicationInfo.FLAG_DEBUGGABLE) != 0
        scheduleSync(this)
        scheduleTaskSync(this)
        CronSyncWorker.schedule(this)
        scheduleUpdateChecks(this)
        // After scheduling, so a disabled periodic poll cancels the job
        // first and a due startup check is the only one enqueued.
        maybeCheckForUpdatesOnStartup(this)
    }

    companion object {
        const val UPDATE_WORK_NAME = "agento_update_check"
        const val UPDATE_STARTUP_WORK_NAME = "agento_update_check_startup"
        // NOTE: Agento ships as a NEW app (applicationId com.vishnu.agento),
        // so it installs alongside the old Health Gateway build instead of
        // updating it. No state is shared across packages (separate
        // /data/data/<package> and WorkManager DBs), so there is deliberately
        // NO automatic migration of the old app's settings — re-enter config
        // in Agento Settings (see README/docs side-by-side install notice).
        const val PREFS_NAME = "agento"
        const val SYNC_WORK_NAME = "agento_sync"
        const val SYNC_INTERVAL_HOURS = 1L
        const val TASK_SYNC_WORK_NAME = "agento_task_sync"

        /**
         * How often the task widget and reminder alarms re-derive
         * themselves. Half an hour sits between "often enough that a task
         * the agent just made shows up while you are still looking at the
         * home screen" and "often enough to notice": it is the smallest
         * interval that keeps a glanceable surface honest, and it is well
         * inside the battery case for a single small fetch.
         */
        const val TASK_SYNC_INTERVAL_MINUTES = 30L

        /** Unique periodic work with UPDATE policy so reinstalls never duplicate. */
        fun scheduleSync(context: Context) {
            val appCtx = context.applicationContext
            val wm = WorkManager.getInstance(appCtx)
            // Paused means no job at all (#355): cancelling (not merely
            // skipping) keeps the process from waking hourly for nothing.
            // Re-enabling re-enqueues below with UPDATE, so resume is one call.
            // Keys and ranges come from the single inventory — never re-spelled
            // here, so a default change cannot drift between the two.
            if (BehaviourSettings.syncPaused(appCtx)) {
                wm.cancelUniqueWork(SYNC_WORK_NAME)
                return
            }
            val hours = BehaviourSettings.syncIntervalHours(appCtx)
            val builder = PeriodicWorkRequestBuilder<SyncWorker>(hours, TimeUnit.HOURS)
            // Baseline CONNECTED like the task sync: offline there is
            // nothing to push, and an unconstrained tick just wakes, fails
            // and backs off. Wi-Fi-only upgrades the constraint instead of
            // replacing it.
            val netType = if (BehaviourSettings.syncWifiOnly(appCtx)) {
                NetworkType.UNMETERED
            } else {
                NetworkType.CONNECTED
            }
            builder.setConstraints(
                Constraints.Builder()
                    .setRequiredNetworkType(netType)
                    .build(),
            )
            wm.enqueueUniquePeriodicWork(
                SYNC_WORK_NAME,
                ExistingPeriodicWorkPolicy.UPDATE,
                builder.build(),
            )
        }

        /**
         * Unique periodic work with UPDATE policy, as above. Safe to call
         * repeatedly: WorkManager keeps one job and replaces its definition,
         * so a cold start, a reboot and a reinstall all land on the same one.
         */
        fun scheduleTaskSync(context: Context) {
            val appCtx = context.applicationContext
            // Range and default from the single inventory, like above.
            val minutes = BehaviourSettings.taskSyncIntervalMinutes(appCtx)
            val request = PeriodicWorkRequestBuilder<TaskSyncWorker>(
                minutes, TimeUnit.MINUTES
            ).setConstraints(
                // Offline, there is nothing to fetch and nothing to report:
                // without this every tick wakes the process, fails all the
                // requests and schedules a backoff retry. Constrained, an
                // offline window simply passes and the run happens when the
                // network is back.
                Constraints.Builder()
                    .setRequiredNetworkType(NetworkType.CONNECTED)
                    .build(),
            ).build()
            WorkManager.getInstance(appCtx).enqueueUniquePeriodicWork(
                TASK_SYNC_WORK_NAME,
                ExistingPeriodicWorkPolicy.UPDATE,
                request,
            )
        }

        /**
         * Periodic app-update poll (#366). `0` hours (Off) cancels the job
         * rather than enqueuing a never-allowed request: an Off pick keeps
         * the process from waking on a schedule at all. Anything else
         * enqueues unique periodic work with UPDATE, so a cold start, a
         * reboot and a cadence change all land on the same one.
         */
        fun scheduleUpdateChecks(context: Context) {
            val appCtx = context.applicationContext
            val wm = WorkManager.getInstance(appCtx)
            val hours = BehaviourSettings.updateIntervalHours(appCtx)
            // `updateIntervalHours` already coerces the stored value into
            // [OFF, 168], so the off case is exactly equality — a `<=` here
            // would read as if negatives were still reachable (#408).
            if (hours == BehaviourSettings.UPDATE_OFF_HOURS) {
                wm.cancelUniqueWork(UPDATE_WORK_NAME)
                return
            }
            val request = PeriodicWorkRequestBuilder<UpdateCheckWorker>(
                hours, TimeUnit.HOURS
            ).setConstraints(
                // The check is a small public-API GET, but offline a tick
                // just wakes the process and fails — constrained, the window
                // passes and the run happens when the network is back.
                Constraints.Builder()
                    .setRequiredNetworkType(NetworkType.CONNECTED)
                    .build(),
            ).setBackoffCriteria(
                BackoffPolicy.EXPONENTIAL,
                UpdateCheckWorker.BACKOFF_MS, TimeUnit.MILLISECONDS,
            ).build()
            wm.enqueueUniquePeriodicWork(
                UPDATE_WORK_NAME,
                ExistingPeriodicWorkPolicy.UPDATE,
                request,
            )
        }

        /**
         * Enqueues the startup one-shot (#366) when the toggle is on and
         * the last check is older than the debounce. Called from
         * [onCreate]; also from the toggle's setter so flipping it on
         * takes effect without killing the process. Requires its own
         * connectivity constraint — like the periodic job, there is no
         * point waking offline.
         */
        fun maybeCheckForUpdatesOnStartup(context: Context) {
            val appCtx = context.applicationContext
            val snap = UpdateCheck.snapshot(appCtx)
            if (!UpdateCheck.startupDue(
                    snap.checkedAt, System.currentTimeMillis(),
                    BehaviourSettings.updateStartup(appCtx),
                )
            ) {
                return
            }
            val request = OneTimeWorkRequestBuilder<UpdateCheckWorker>()
                .setConstraints(
                    Constraints.Builder()
                        .setRequiredNetworkType(NetworkType.CONNECTED)
                        .build(),
                )
                .setBackoffCriteria(
                    BackoffPolicy.EXPONENTIAL,
                    UpdateCheckWorker.BACKOFF_MS, TimeUnit.MILLISECONDS,
                )
                .build()
            WorkManager.getInstance(appCtx).enqueueUniqueWork(
                UPDATE_STARTUP_WORK_NAME,
                ExistingWorkPolicy.KEEP,
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
