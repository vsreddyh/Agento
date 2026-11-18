package com.vishnu.agento

import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.util.Log
import androidx.core.app.NotificationCompat
import androidx.core.content.edit
import androidx.core.net.toUri
import java.time.Instant
import java.time.LocalDate
import java.time.LocalDateTime
import java.time.LocalTime
import java.time.format.DateTimeFormatter
import java.util.Locale
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Deferred
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.async

/**
 * Project snooze, app side: the value helpers, the local "what was armed"
 * cache, and the one notification.
 *
 * The *transition* is server-side (health-api's snooze ticker): the board is
 * shared by the app, the agent and any future client, so a phone-side alarm
 * would only work while that phone was awake and would fire once per client.
 * That leaves the app with exactly one job: notice that a project this device
 * saw as snoozed is now Ongoing with no snooze, and say so. [check] does that
 * on the same periodic worker that refreshes task reminders — no separate
 * cadence, no extra wakeup.
 *
 * The cache lives in its own preferences file, deliberately not in
 * [AgentoApp.PREFS_NAME]: settings backups must not carry another device's
 * armed snoozes, or a restore would notify about a move this device never saw.
 * A row that moves while the app is not running is still reported on the next
 * worker tick, however late; a row deleted while snoozed simply never matches
 * and ages out of the cache.
 */
internal object ProjectSnooze {

    /** Its own channel: a snooze ending is not a task reminder, and the two
     * must be silenceable independently in system settings. */
    const val CHANNEL_ID = "project_snooze"

    /** Stable notification id: identity comes from the per-project tag. */
    private const val NOTIFICATION_ID = 3

    private const val PREFS = "project_snooze"
    private const val KEY_ARMED = "armed_ids"

    /** Server-side list cap (projects.DefaultLimit/MaxLimit): one page sees
     * the whole board, so a snoozed row cannot silently fall out of view. */
    private const val LIST_LIMIT = 500

    /** Bound on the cache: only snoozed ids live here, so this is far above
     * any real board — it just stops a pathological set growing forever. */
    private const val MAX_ARMED = 500

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

    /** Snooze default when a Paused project is first snoozed: 6 PM tomorrow,
     * IST like every other user-facing time (#124). The picker still asks for
     * the date and then the time; this is the suggested pair, not a substitute
     * for choosing both. */
    private fun defaultLocal(): LocalDateTime =
        LocalDate.now(IST).plusDays(1).atTime(18, 0)

    fun defaultSnooze(): String = defaultLocal().atZone(IST).toInstant().toString()

    /** RFC3339 instant → wall-clock in IST, or null when the stored value is
     * unusable. Pure so the UI and the notification formatter agree. */
    fun localDateTime(iso: String): LocalDateTime? {
        if (iso.isBlank()) return null
        return runCatching {
            Instant.parse(iso.trim()).atZone(IST).toLocalDateTime()
        }.getOrNull()
    }

    /** A date and time as one RFC3339 instant. Returns null unless BOTH parts
     * parse — the snooze is an instant and a bare date or bare time is not
     * one, so a half-answer leaves the previous value untouched instead of
     * inventing midnight or today. */
    fun iso(date: String, time: String): String? {
        val d = runCatching { LocalDate.parse(date.trim()) }.getOrNull() ?: return null
        val t = runCatching { LocalTime.parse(time.trim()) }.getOrNull() ?: return null
        return d.atTime(t).atZone(IST).toInstant().toString()
    }

    /** Chip/card text: "19 Nov, 6:00 PM", blank when there is no snooze. */
    fun label(iso: String): String =
        localDateTime(iso)?.format(
            DateTimeFormatter.ofPattern("d MMM, h:mm a", Locale.ENGLISH)
        ) ?: ""

    /** Local date/time parts for seeding the pickers. */
    fun datePart(iso: String): String =
        localDateTime(iso)?.toLocalDate()?.toString() ?: defaultLocal().toLocalDate().toString()

    fun timePart(iso: String): String =
        localDateTime(iso)?.toLocalTime()
            ?.format(DateTimeFormatter.ofPattern("HH:mm", Locale.US))
            ?: defaultLocal().toLocalTime().format(DateTimeFormatter.ofPattern("HH:mm", Locale.US))

    /** Server rule (projects.SnoozeLead): a snooze must sit at least a minute
     * ahead, because the ticker wakes it a minute early. The dialog rejects
     * anything closer so a save cannot land in an instant transition. */
    fun isFuture(iso: String, nowMillis: Long = System.currentTimeMillis()): Boolean {
        val at = runCatching { Instant.parse(iso.trim()).toEpochMilli() }.getOrNull() ?: return false
        return at > nowMillis + 60_000L
    }

    /** The saved row decides what this device should remember: Paused with a
     * snooze is armed, anything else is not. Called after an in-app save so a
     * user who clears a snooze or moves the project themselves never gets a
     * "snooze ended" notification for their own action. */
    fun remember(context: Context, project: ServerProject) {
        val paused = project.status.equals("Paused", ignoreCase = true)
        if (paused && project.snoozeAt.isNotBlank()) arm(context, project.id)
        else forget(context, project.id)
    }

    /** Drops one project from the cache (delete/undo paths). */
    fun forget(context: Context, id: String) {
        writeArmed(context, readArmed(context) - id)
    }

    /**
     * One poll: list the board, notify for every cached project that has moved
     * Paused → Ongoing (snooze cleared by the server), then rebuild the cache
     * from what is Paused and snoozed now. Rows missing from the page are kept
     * armed — the page is capped, and dropping an unseen row would lose its
     * notification.
     *
     * Returns a [Deferred] so [TaskSyncWorker] can await the run and count a
     * failure, like the widget and reminder refreshes.
     */
    fun check(context: Context): Deferred<RefreshOutcome> {
        val appCtx = context.applicationContext
        return scope.async {
            try {
                checkInner(appCtx)
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                Log.w("ProjectSnooze", "snooze check failed", e)
                RefreshOutcome.Failed
            }
        }
    }

    private suspend fun checkInner(ctx: Context): RefreshOutcome {
        val rows = ProjectsApi(ctx).list("all", limit = LIST_LIMIT).getOrNull()
            ?: return RefreshOutcome.Failed
        val byId = rows.associateBy { it.id }
        val wasArmed = readArmed(ctx)
        val fired = wasArmed.mapNotNull { id ->
            val p = byId[id] ?: return@mapNotNull null
            // The server wakes the row to Ongoing and clears the snooze in one
            // write, so both facts together are the move we cached it for.
            if (p.status.equals("Ongoing", ignoreCase = true) && p.snoozeAt.isBlank()) p else null
        }
        for (p in fired) postNotification(ctx, p)
        val stillArmed = rows.filter {
            it.status.equals("Paused", ignoreCase = true) && it.snoozeAt.isNotBlank()
        }.map { it.id }.toMutableSet()
        // Not in this page: either past the cap or gone. Keep it armed — a
        // deletion simply never matches, and losing an unseen row would lose
        // a real notification.
        stillArmed.addAll(wasArmed - byId.keys)
        stillArmed.removeAll(fired.map { it.id }.toSet())
        writeArmed(ctx, stillArmed)
        return RefreshOutcome.Ok
    }

    private fun arm(context: Context, id: String) {
        if (id.isBlank()) return
        writeArmed(context, readArmed(context) + id)
    }

    private fun readArmed(context: Context): MutableSet<String> {
        val stored = context.applicationContext
            .getSharedPreferences(PREFS, Context.MODE_PRIVATE)
            .getStringSet(KEY_ARMED, emptySet()) ?: emptySet()
        return stored.toMutableSet()
    }

    private fun writeArmed(context: Context, ids: Set<String>) {
        context.applicationContext
            .getSharedPreferences(PREFS, Context.MODE_PRIVATE)
            .edit { putStringSet(KEY_ARMED, ids.take(MAX_ARMED).toSet()) }
    }

    /** One combined notification: the snooze ended *because* the project moved
     * to Ongoing, so two notifications would say the same thing twice. */
    private fun postNotification(ctx: Context, project: ServerProject) {
        val mgr = ctx.getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        mgr.createNotificationChannel(
            NotificationChannel(
                CHANNEL_ID,
                ctx.getString(R.string.project_snooze_channel),
                NotificationManager.IMPORTANCE_HIGH,
            ).apply {
                description = ctx.getString(R.string.project_snooze_channel_desc)
            },
        )
        val launch = Intent(ctx, MainActivity::class.java)
            .setData("agento://project/${android.net.Uri.encode(project.id)}".toUri())
        val tap = PendingIntent.getActivity(
            ctx,
            project.id.hashCode(),
            launch,
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val notif = NotificationCompat.Builder(ctx, CHANNEL_ID)
            .setSmallIcon(android.R.drawable.ic_menu_agenda)
            .setContentTitle(project.name.ifEmpty { "(untitled)" })
            .setContentText(ctx.getString(R.string.project_snooze_moved))
            .setPriority(NotificationCompat.PRIORITY_HIGH)
            .setAutoCancel(true)
            .setContentIntent(tap)
            .build()
        // Tag on the project id: two projects firing in one tick occupy two
        // slots instead of replacing each other.
        mgr.notify(project.id, NOTIFICATION_ID, notif)
    }
}
