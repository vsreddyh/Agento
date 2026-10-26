package com.vishnu.agento

import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import androidx.core.app.NotificationCompat
import androidx.core.content.edit
import java.util.concurrent.TimeUnit

/**
 * Automatic update checks (#366).
 *
 * [UpdateManager] stays the transport (fetch + install); this object owns
 * the *policy*: when a check runs, what the last one found, and whether a
 * newer release is worth a notification. Every path — the manual button,
 * the startup one-shot and the periodic worker — funnels through [run], so
 * the App updates section and the notification can never disagree about
 * what the last check saw.
 *
 * The last result is persisted, not held in memory: the worker usually runs
 * with no UI alive, and reopening Settings must show "checked 2 h ago ·
 * update available" without waiting for another network call.
 */
object UpdateCheck {

    /** Cold-start checks are debounced independently of the periodic
     * cadence: an app that is opened ten times a day must not ask GitHub
     * ten times, and a user who picked "weekly" and then restarts is not
     * owed an immediate check of their own. */
    const val STARTUP_DEBOUNCE_MS = 6L * 60L * 60L * 1000L

    const val ACTION_UPDATE = "com.vishnu.agento.action.UPDATE"

    private const val CHANNEL_ID = "app_updates"
    private const val NOTIFICATION_ID = 4266

    // Store keys live here, not in BehaviourSettings: they are bookkeeping
    // about the last run, not user-facing knobs, and must never enter a
    // backup (a restored "8.0.0 available" on a fresh install would be a
    // lie). SettingsBackup's allowlist excludes them by construction.
    private const val KEY_CHECKED_AT = "update_last_check_at"
    private const val KEY_TAG = "update_last_tag"
    private const val KEY_NAME = "update_last_name"
    private const val KEY_URL = "update_last_apk_url"
    private const val KEY_SIZE = "update_last_apk_size"
    private const val KEY_NOTES = "update_last_notes"
    private const val KEY_ERROR = "update_last_error"
    private const val KEY_NOTIFIED_TAG = "update_notified_tag"

    /** Persisted outcome of the most recent completed check. */
    data class Snapshot(
        val checkedAt: Long,
        val tag: String,
        val name: String,
        val apkUrl: String,
        val apkSizeBytes: Long,
        val notes: String,
        val error: String,
    )

    private fun prefs(ctx: Context) =
        ctx.applicationContext.getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)

    fun snapshot(ctx: Context): Snapshot {
        val p = prefs(ctx)
        return Snapshot(
            checkedAt = p.getLong(KEY_CHECKED_AT, 0L),
            tag = p.getString(KEY_TAG, "").orEmpty(),
            name = p.getString(KEY_NAME, "").orEmpty(),
            apkUrl = p.getString(KEY_URL, "").orEmpty(),
            apkSizeBytes = p.getLong(KEY_SIZE, 0L),
            notes = p.getString(KEY_NOTES, "").orEmpty(),
            error = p.getString(KEY_ERROR, "").orEmpty(),
        )
    }

    /** The stored release when it is still newer than the installed build. */
    fun pendingRelease(ctx: Context): AppRelease? {
        val snap = snapshot(ctx)
        if (snap.tag.isBlank() || snap.apkUrl.isBlank()) return null
        val (name, code) = UpdateManager.currentVersion(ctx)
        if (!UpdateManager.isNewer(snap.tag, name, code)) return null
        return AppRelease(
            tag = snap.tag,
            name = snap.name.ifBlank { snap.tag },
            apkUrl = snap.apkUrl,
            apkSizeBytes = snap.apkSizeBytes,
            publishedAt = "",
            notes = snap.notes,
        )
    }

    /**
     * Runs one check and persists the outcome. [notify] is false for the
     * user-initiated path: they are looking at the section already, so a
     * notification would duplicate what is on screen.
     */
    suspend fun run(ctx: Context, notify: Boolean): Result<AppRelease> {
        val appCtx = ctx.applicationContext
        val result = UpdateManager.fetchLatest()
        // Stamped at completion, not dispatch: the debounce window should
        // measure between finished checks, not include fetch latency (and
        // a failed startup check still advances the window — by design the
        // startup path then stays quiet for 6 h while the worker's own
        // backoff keeps retrying underneath).
        val now = System.currentTimeMillis()
        result
            .onSuccess { rel ->
                saveSuccess(appCtx, rel, now)
                if (notify && shouldPostNotification(
                        newer = isPending(appCtx, rel),
                        enabled = BehaviourSettings.updateNotify(appCtx),
                        canPost = ChatNotifications.canPost(appCtx),
                        inForeground = ForegroundTracker.isForeground,
                        alreadyPostedTag = prefs(appCtx).getString(KEY_NOTIFIED_TAG, "").orEmpty(),
                        tag = rel.tag,
                    )
                ) {
                    post(appCtx, rel)
                }
            }
            .onFailure { e ->
                saveFailure(appCtx, now, e.message ?: e.javaClass.simpleName)
            }
        return result
    }

    /** True when [rel] is newer than the installed build right now. */
    private fun isPending(ctx: Context, rel: AppRelease): Boolean {
        val (name, code) = UpdateManager.currentVersion(ctx)
        return UpdateManager.isNewer(rel.tag, name, code)
    }

    /** Startup checks are on only when enabled AND outside the debounce. */
    fun startupDue(lastCheckAt: Long, now: Long, enabled: Boolean): Boolean {
        if (!enabled) return false
        if (lastCheckAt <= 0L) return true
        return now - lastCheckAt >= STARTUP_DEBOUNCE_MS
    }

    /**
     * Pure notification gate. Foreground means the App updates section is
     * one tap away (or already open), and an already-posted tag must not
     * be repeated every cadence tick — the notification is a nudge for a
     * new release, not a heartbeat.
     */
    fun shouldPostNotification(
        newer: Boolean,
        enabled: Boolean,
        canPost: Boolean,
        inForeground: Boolean,
        alreadyPostedTag: String,
        tag: String,
    ): Boolean = newer && enabled && canPost && !inForeground && tag.isNotBlank() &&
        alreadyPostedTag != tag

    /** Short status line for the App updates card; pure, JVM-pinned. A
     * failed check never hides a known pending release: the download stays
     * available behind the error, and the line says so. */
    fun statusLine(snap: Snapshot, now: Long, pending: Boolean): String {
        if (snap.checkedAt <= 0L) return "Not checked yet"
        val age = (now - snap.checkedAt).coerceAtLeast(0L)
        val whenText = when {
            age < TimeUnit.MINUTES.toMillis(1) -> "just now"
            age < TimeUnit.HOURS.toMillis(1) ->
                "${TimeUnit.MILLISECONDS.toMinutes(age)} min ago"
            age < TimeUnit.DAYS.toMillis(1) ->
                "${TimeUnit.MILLISECONDS.toHours(age)} h ago"
            else -> "${TimeUnit.MILLISECONDS.toDays(age)} d ago"
        }
        val base = if (pending) {
            "Update available: ${snap.name.ifBlank { snap.tag }} · checked $whenText"
        } else {
            "Up to date · checked $whenText"
        }
        return if (snap.error.isNotBlank()) {
            "$base — last check failed: ${snap.error}"
        } else {
            base
        }
    }

    private fun saveSuccess(ctx: Context, rel: AppRelease, now: Long) {
        prefs(ctx).edit {
            putLong(KEY_CHECKED_AT, now)
            putString(KEY_TAG, rel.tag)
            putString(KEY_NAME, rel.name)
            putString(KEY_URL, rel.apkUrl)
            putLong(KEY_SIZE, rel.apkSizeBytes)
            putString(KEY_NOTES, rel.notes)
            putString(KEY_ERROR, "")
        }
    }

    private fun saveFailure(ctx: Context, now: Long, error: String) {
        // The previous success (if any) is deliberately kept: a transient
        // API failure must not hide a release the user has not installed
        // yet — only the timestamp and the error line move.
        prefs(ctx).edit {
            putLong(KEY_CHECKED_AT, now)
            putString(KEY_ERROR, error.take(200))
        }
    }

    /**
     * Creates the per-release nudge channel, quietly (#383): one notification
     * per release is a nudge, not an interruption, so it is IMPORTANCE_LOW —
     * no sound, no heads-up — where DEFAULT would make it one.
     */
    fun ensureChannel(ctx: Context) {
        val nm = ctx.getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        val existing = nm.getNotificationChannel(CHANNEL_ID)
        // Importance is fixed at creation, so an install that already has the
        // channel keeps whatever it was born with. Recreate it only when the
        // user has never touched importance: deleting a channel the user
        // muted would re-arm it and undo their choice, which matters more
        // than the quieter default.
        val quieten = existing != null &&
            !existing.hasUserSetImportance() &&
            existing.importance != NotificationManager.IMPORTANCE_LOW
        if (existing == null || quieten) {
            if (existing != null) nm.deleteNotificationChannel(CHANNEL_ID)
            nm.createNotificationChannel(
                NotificationChannel(
                    CHANNEL_ID,
                    "App updates",
                    NotificationManager.IMPORTANCE_LOW,
                ).apply {
                    description = "Posted when a newer Agento release is published"
                },
            )
        }
    }

    private fun post(ctx: Context, rel: AppRelease) {
        ensureChannel(ctx)
        // Deep link into Settings → About (where App updates lives); the
        // activity consumes ACTION_UPDATE and opens that section.
        val open = PendingIntent.getActivity(
            ctx, NOTIFICATION_ID,
            Intent(ctx, MainActivity::class.java).setAction(ACTION_UPDATE),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val notification = NotificationCompat.Builder(ctx, CHANNEL_ID)
            .setSmallIcon(android.R.drawable.stat_sys_download_done)
            .setContentTitle("Agento ${rel.name} is available")
            .setContentText("Tap to open App updates.")
            .setContentIntent(open)
            .setAutoCancel(true)
            .setPriority(NotificationCompat.PRIORITY_LOW)
            .build()
        val nm = ctx.getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        nm.notify(NOTIFICATION_ID, notification)
        prefs(ctx).edit { putString(KEY_NOTIFIED_TAG, rel.tag) }
    }
}
