package com.vishnu.agento

import android.app.Application
import android.content.Context
import androidx.work.ExistingPeriodicWorkPolicy
import androidx.work.PeriodicWorkRequestBuilder
import androidx.work.WorkManager
import java.util.concurrent.TimeUnit

/** Application entry point; ensures hourly sync survives process death. */
class AgentoApp : Application() {

    /** Migrates pre-rebrand state once, then re-enqueues hourly sync; safe to call repeatedly. */
    override fun onCreate() {
        super.onCreate()
        migrateLegacyState(this)
        scheduleSync(this)
    }

    companion object {
        const val PREFS_NAME = "agento"
        const val SYNC_WORK_NAME = "agento_sync"
        const val SYNC_INTERVAL_HOURS = 1L

        /** Pre-rebrand (Health Gateway) identifiers; only read by the one-shot migration below. */
        private const val LEGACY_PREFS_NAME = "health_gateway"
        private const val LEGACY_SYNC_WORK_NAME = "health_gateway_sync"

        /**
         * Copies pre-rebrand settings into [PREFS_NAME] once so updates keep the
         * saved chat/sync config, and cancels the legacy periodic work so only
         * one hourly sync runs. No-op when there is nothing to migrate.
         */
        fun migrateLegacyState(context: Context) {
            val legacy = context.getSharedPreferences(LEGACY_PREFS_NAME, Context.MODE_PRIVATE)
            if (legacy.all.isNotEmpty()) {
                val next = context.getSharedPreferences(PREFS_NAME, Context.MODE_PRIVATE)
                if (next.all.isEmpty()) {
                    val edit = next.edit()
                    for ((key, value) in legacy.all) {
                        when (value) {
                            is String -> edit.putString(key, value)
                            is Boolean -> edit.putBoolean(key, value)
                            is Int -> edit.putInt(key, value)
                            is Long -> edit.putLong(key, value)
                            is Float -> edit.putFloat(key, value)
                            is Set<*> -> {
                                @Suppress("UNCHECKED_CAST")
                                edit.putStringSet(key, value as Set<String>)
                            }
                        }
                    }
                    edit.apply()
                }
                legacy.edit().clear().apply()
            }
            runCatching {
                WorkManager.getInstance(context).cancelUniqueWork(LEGACY_SYNC_WORK_NAME)
            }
        }

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
