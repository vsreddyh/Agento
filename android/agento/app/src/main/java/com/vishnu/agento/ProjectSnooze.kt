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
import org.json.JSONObject

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

    /** Server-side page cap (projects.MaxLimit). A short page is the whole
     * board; a full one may be truncated — which is why [splitArmed] only
     * ages missing ids out of the cache on a short page. */
    internal const val LIST_LIMIT = 500

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

    /** Server rule (projects.SnoozeLead): a snooze must sit *strictly* more
     * than a minute ahead, because the ticker wakes it a minute early. The
     * dialog rejects anything closer so a save cannot land in an instant
     * transition. */
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
        if (paused && project.snoozeAt.isNotBlank()) arm(context, project.id, project.snoozeAt)
        else forget(context, project.id)
    }

    /** Drops one project from the cache (delete/undo paths). */
    fun forget(context: Context, id: String) {
        writeArmed(context, readArmed(context) - id)
    }

    /** True when this row is the ticker waking a snooze this device armed.
     *
     * The server stamps [ServerProject.snoozeFiredAt] with the snooze instant
     * it fired, so `fired >= armed` identifies a wake. A manual
     * Paused -> Ongoing move clears the snooze without a marker, which is why
     * "Ongoing with a blank snooze" alone is not enough — that shape is the
     * false-positive the first implementation produced on every other device.
     *
     * `>=`, not `==`: a row resnoozed (by the agent or another device) after
     * this device cached T1 still ends in a genuine ticker wake at T2, and
     * "snooze ended — moved to Ongoing" is true of it — so it notifies rather
     * than going silent on a move the user asked to hear about.
     */
    fun isWake(project: ServerProject, armedAt: String): Boolean {
        if (!project.status.equals("Ongoing", ignoreCase = true)) return false
        if (project.snoozeAt.isNotBlank()) return false
        val fired = epochMillis(project.snoozeFiredAt) ?: return false
        val armed = epochMillis(armedAt) ?: return false
        return fired >= armed
    }

    private fun epochMillis(iso: String): Long? =
        runCatching { Instant.parse(iso.trim()).toEpochMilli() }.getOrNull()

    /**
     * One poll: list the board, notify for every cached project the ticker
     * actually woke, then rebuild the cache from what is Paused and snoozed
     * now. The merge itself is [splitArmed]; a missing id ages out on a short
     * page (the whole board fit, so it is gone) but stays armed on a full one
     * (the page may be truncated, so it may simply be out of view).
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
        val (fired, stillArmed) = splitArmed(readArmed(ctx), rows)
        for (p in fired) postNotification(ctx, p)
        writeArmed(ctx, stillArmed)
        return RefreshOutcome.Ok
    }

    /** Pure merge step of [checkInner], separated so the truncation rule is
     * pinned without Android: returns the cached rows the ticker woke, and
     * the cache to keep.
     *
     * A row that was manually moved while snoozed is dropped silently — only
     * the ticker's marker ([isWake]) notifies, because a manual move is not a
     * snooze ending.
     */
    internal fun splitArmed(
        armed: Map<String, String>,
        rows: List<ServerProject>,
    ): Pair<List<ServerProject>, Map<String, String>> {
        val byId = rows.associateBy { it.id }
        val fired = armed.mapNotNull { (id, at) ->
            val p = byId[id] ?: return@mapNotNull null
            if (isWake(p, at)) p else null
        }
        val firedIds = fired.map { it.id }.toSet()
        val still = rows.filter {
            it.status.equals("Paused", ignoreCase = true) && it.snoozeAt.isNotBlank()
        }.associate { it.id to it.snoozeAt }.toMutableMap()
        // A full page may be truncated: a tracked id outside it is
        // indistinguishable from a deleted one, so it stays armed rather than
        // risk dropping a real notification.
        if (rows.size >= LIST_LIMIT) {
            for ((id, at) in armed) {
                if (id !in byId) still.putIfAbsent(id, at)
            }
        }
        return fired to (still - firedIds)
    }

    private fun arm(context: Context, id: String, at: String) {
        if (id.isBlank() || at.isBlank()) return
        writeArmed(context, readArmed(context) + (id to at))
    }

    /** id -> the snooze instant this device saw armed. A map (not a set)
     * because the wake marker is compared with that exact instant, so a row
     * resnoozed to a later time is not mistaken for the old wake. */
    private fun readArmed(context: Context): Map<String, String> {
        val raw = try {
            context.applicationContext
                .getSharedPreferences(PREFS, Context.MODE_PRIVATE)
                .getString(KEY_ARMED, "") ?: ""
        } catch (e: ClassCastException) {
            // An install that ran the set-based cache stored this key as a
            // StringSet; reading it as a string throws. The old shape cannot
            // say *which* instant was armed, so it is unusable — start empty.
            ""
        }
        if (raw.isBlank()) return emptyMap()
        val obj = runCatching { JSONObject(raw) }.getOrNull() ?: return emptyMap()
        val out = LinkedHashMap<String, String>()
        for (id in obj.keys()) {
            val at = obj.optString(id, "").trim()
            if (at.isNotEmpty()) out[id] = at
        }
        return out
    }

    private fun writeArmed(context: Context, ids: Map<String, String>) {
        val obj = JSONObject()
        ids.entries.take(MAX_ARMED).forEach { (id, at) -> obj.put(id, at) }
        context.applicationContext
            .getSharedPreferences(PREFS, Context.MODE_PRIVATE)
            .edit { putString(KEY_ARMED, obj.toString()) }
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
