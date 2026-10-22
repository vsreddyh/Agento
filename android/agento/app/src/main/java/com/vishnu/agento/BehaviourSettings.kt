package com.vishnu.agento

import android.content.Context
import android.speech.tts.TextToSpeech
import androidx.core.content.edit

/**
 * Device-local behaviour knobs (#355): the single inventory of user-facing
 * settings, so "preconfigured and unreachable" cannot quietly come back.
 *
 * Everything here is adjustable in-app with an obvious default, and an
 * install that never opens the section behaves exactly like 5.12.0. All
 * keys are additive ([SettingsBackup] skips unknown keys, so old backups
 * keep importing and new keys round-trip).
 *
 * Out-of-option values (a hand-edited backup importing lead=1440, say) are
 * preserved, not clamped to the picker: getters coerce only to hard
 * sanity floors/ceilings, and the copy renders them literally. The pickers
 * offer the sane choices; anything else is unpickable-but-kept, which beats
 * silently rewriting a value the user deliberately set.
 *
 * Deliberately NOT here (operator-set by design, see documentation.md):
 * money/health retention windows, gateway turn/inventory timeouts, and the
 * profile prompt. Health-sync failures stay in-app status text (there is no
 * system notification for them to gate). `tts_auto` predates this section:
 * the chat bar keeps its toggle and this section surfaces the same key.
 */
object BehaviourSettings {

    // ── Health sync cadence ──────────────────────────────────────────────
    const val KEY_SYNC_INTERVAL_HOURS = "sync_interval_hours"
    // Defaults live on AgentoApp (the scheduler's home) and are re-exported
    // here, never re-spelled: one source, so a default change cannot drift
    // between the section and the scheduler.
    const val DEFAULT_SYNC_INTERVAL_HOURS = AgentoApp.SYNC_INTERVAL_HOURS
    /** Displayed choices; WorkManager periodic work floors at 15 minutes. */
    val SYNC_INTERVAL_OPTIONS_HOURS = listOf(1L, 2L, 4L, 6L, 12L, 24L)

    const val KEY_SYNC_WIFI_ONLY = "sync_wifi_only"
    const val KEY_SYNC_PAUSED = "sync_paused"

    fun syncIntervalHours(ctx: Context): Long =
        coerceImport(KEY_SYNC_INTERVAL_HOURS,
            prefs(ctx).getLong(KEY_SYNC_INTERVAL_HOURS, DEFAULT_SYNC_INTERVAL_HOURS))

    fun syncWifiOnly(ctx: Context): Boolean =
        prefs(ctx).getBoolean(KEY_SYNC_WIFI_ONLY, false)

    fun syncPaused(ctx: Context): Boolean =
        prefs(ctx).getBoolean(KEY_SYNC_PAUSED, false)

    fun setSyncIntervalHours(ctx: Context, hours: Long) {
        prefs(ctx).edit { putLong(KEY_SYNC_INTERVAL_HOURS, hours.coerceIn(1L, 24L)) }
        AgentoApp.scheduleSync(ctx)
    }

    fun setSyncWifiOnly(ctx: Context, wifiOnly: Boolean) {
        prefs(ctx).edit { putBoolean(KEY_SYNC_WIFI_ONLY, wifiOnly) }
        AgentoApp.scheduleSync(ctx)
    }

    fun setSyncPaused(ctx: Context, paused: Boolean) {
        prefs(ctx).edit { putBoolean(KEY_SYNC_PAUSED, paused) }
        AgentoApp.scheduleSync(ctx)
    }

    /** User-facing cadence line; never asserts a constant (#355). */
    fun syncCadenceLine(ctx: Context): String =
        syncCadenceLine(syncIntervalHours(ctx), syncWifiOnly(ctx), syncPaused(ctx))

    fun syncCadenceLine(hours: Long, wifiOnly: Boolean, paused: Boolean): String {
        // Total like every pure helper here: coerce, so a direct caller
        // can never render "every 0 hours".
        val h = coerceImport(KEY_SYNC_INTERVAL_HOURS, hours)
        return when {
            paused -> "Automatic sync is paused."
            else -> {
                val every = if (h == 1L) "every hour" else "every $h hours"
                val net = if (wifiOnly) " on unmetered Wi-Fi" else ""
                "Automatic sync runs $every$net."
            }
        }
    }

    // ── Task / widget / reminder refresh ─────────────────────────────────
    const val KEY_TASK_SYNC_INTERVAL_MINUTES = "task_sync_interval_minutes"
    const val DEFAULT_TASK_SYNC_INTERVAL_MINUTES = AgentoApp.TASK_SYNC_INTERVAL_MINUTES
    val TASK_SYNC_INTERVAL_OPTIONS_MINUTES = listOf(15L, 30L, 60L, 120L)

    fun taskSyncIntervalMinutes(ctx: Context): Long =
        coerceImport(KEY_TASK_SYNC_INTERVAL_MINUTES,
            prefs(ctx).getLong(KEY_TASK_SYNC_INTERVAL_MINUTES, DEFAULT_TASK_SYNC_INTERVAL_MINUTES))

    fun setTaskSyncIntervalMinutes(ctx: Context, minutes: Long) {
        prefs(ctx).edit { putLong(KEY_TASK_SYNC_INTERVAL_MINUTES, minutes.coerceIn(15L, 24L * 60L)) }
        AgentoApp.scheduleTaskSync(ctx)
    }

    fun taskSyncCadenceLine(ctx: Context): String =
        taskSyncCadenceLine(taskSyncIntervalMinutes(ctx))

    fun taskSyncCadenceLine(minutes: Long): String {
        val m = coerceImport(KEY_TASK_SYNC_INTERVAL_MINUTES, minutes)
        return when {
            m == 60L -> "Task and widget refresh runs every hour."
            m % 60L == 0L -> "Task and widget refresh runs every ${m / 60L} hours."
            else -> "Task and widget refresh runs every $m minutes."
        }
    }

    // ── Reminder lead time ───────────────────────────────────────────────
    const val KEY_REMINDER_LEAD_MINUTES = "reminder_lead_minutes"
    val REMINDER_LEAD_OPTIONS_MINUTES = listOf(0L, 5L, 10L, 15L, 30L, 60L)

    /** Default stays [TaskReminders.BEFORE_START_MINUTES]: one source. */
    fun reminderLeadMinutes(ctx: Context): Long =
        coerceImport(KEY_REMINDER_LEAD_MINUTES,
            prefs(ctx).getLong(KEY_REMINDER_LEAD_MINUTES, TaskReminders.BEFORE_START_MINUTES))

    fun setReminderLeadMinutes(ctx: Context, minutes: Long) {
        prefs(ctx).edit { putLong(KEY_REMINDER_LEAD_MINUTES, minutes.coerceIn(0L, 24L * 60L)) }
        // Re-arm now: the next half-hourly refresh would catch up, but the
        // user just asked — the alarms they see should match the pick.
        TaskReminders.refresh(ctx)
    }

    fun leadLabel(minutes: Long): String = when (minutes) {
        0L -> "At start time (no heads-up)"
        // Shared with the notification copy (leadCopy): the picker and the
        // lock screen must agree, including on hand-imported values.
        else -> "${leadCopy(minutes)} before start"
    }

    /** Picker choice labels; pinned — the section renders these verbatim. */
    fun intervalChoiceLabel(hours: Long): String =
        if (hours == 1L) "1 hour" else "$hours hours"

    fun minutesChoiceLabel(minutes: Long): String = when {
        minutes == 60L -> "1 hour"
        minutes % 60L == 0L -> "${minutes / 60L} hours"
        else -> "$minutes minutes"
    }

    // ── TTS voice / rate / pitch ─────────────────────────────────────────
    const val KEY_TTS_VOICE = "tts_voice"
    const val KEY_TTS_RATE_PCT = "tts_rate_pct"
    const val KEY_TTS_PITCH_PCT = "tts_pitch_pct"
    const val DEFAULT_TTS_PCT = 100L
    /** Engine range is roughly half to double speed; the picker offers steps. */
    val TTS_PCT_OPTIONS = listOf(50L, 75L, 100L, 125L, 150L, 200L)

    /** Voice name, or "" for the engine default (today's behaviour). */
    fun ttsVoice(ctx: Context): String =
        prefs(ctx).getString(KEY_TTS_VOICE, "").orEmpty()

    fun ttsRate(ctx: Context): Float =
        ttsRatePct(ctx) / 100f

    fun ttsPitch(ctx: Context): Float =
        ttsPitchPct(ctx) / 100f

    fun ttsRatePct(ctx: Context): Long =
        coerceImport(KEY_TTS_RATE_PCT, prefs(ctx).getLong(KEY_TTS_RATE_PCT, DEFAULT_TTS_PCT))

    fun ttsPitchPct(ctx: Context): Long =
        coerceImport(KEY_TTS_PITCH_PCT, prefs(ctx).getLong(KEY_TTS_PITCH_PCT, DEFAULT_TTS_PCT))

    fun setTtsVoice(ctx: Context, name: String) {
        prefs(ctx).edit { putString(KEY_TTS_VOICE, name.trim()) }
    }

    fun setTtsRatePct(ctx: Context, pct: Long) {
        prefs(ctx).edit { putLong(KEY_TTS_RATE_PCT, pct.coerceIn(50L, 200L)) }
    }

    fun setTtsPitchPct(ctx: Context, pct: Long) {
        prefs(ctx).edit { putLong(KEY_TTS_PITCH_PCT, pct.coerceIn(50L, 200L)) }
    }

    fun ttsPctLabel(pct: Long): String = "$pct%"

    // ── Notification kinds ───────────────────────────────────────────────
    // The master switch (notify_reply_done, default on) keeps its exact
    // meaning: off silences everything, as today. Each kind below defaults
    // on and applies only while the master is on, so an install that never
    // opens the section — and an opted-out one — behaves exactly like 5.12.0.
    const val KEY_NOTIFY_FAILED = "notify_reply_failed"
    const val KEY_NOTIFY_INSTRUCT = "notify_instruct_saved"
    const val KEY_NOTIFY_CRON = "notify_cron"

    fun notifyFailed(ctx: Context): Boolean =
        ChatNotifications.isEnabled(ctx) && prefs(ctx).getBoolean(KEY_NOTIFY_FAILED, true)

    fun notifyInstruct(ctx: Context): Boolean =
        ChatNotifications.isEnabled(ctx) && prefs(ctx).getBoolean(KEY_NOTIFY_INSTRUCT, true)

    fun notifyCron(ctx: Context): Boolean =
        ChatNotifications.isEnabled(ctx) && prefs(ctx).getBoolean(KEY_NOTIFY_CRON, true)

    fun notifyFailedRaw(ctx: Context): Boolean =
        prefs(ctx).getBoolean(KEY_NOTIFY_FAILED, true)

    fun notifyInstructRaw(ctx: Context): Boolean =
        prefs(ctx).getBoolean(KEY_NOTIFY_INSTRUCT, true)

    fun notifyCronRaw(ctx: Context): Boolean =
        prefs(ctx).getBoolean(KEY_NOTIFY_CRON, true)

    fun setNotifyFailed(ctx: Context, on: Boolean) {
        prefs(ctx).edit { putBoolean(KEY_NOTIFY_FAILED, on) }
    }

    fun setNotifyInstruct(ctx: Context, on: Boolean) {
        prefs(ctx).edit { putBoolean(KEY_NOTIFY_INSTRUCT, on) }
    }

    fun setNotifyCron(ctx: Context, on: Boolean) {
        prefs(ctx).edit { putBoolean(KEY_NOTIFY_CRON, on) }
    }

    // ── Auto-read (surfaced here; the chat bar keeps its toggle) ─────────
    const val KEY_TTS_AUTO = "tts_auto"

    fun ttsAuto(ctx: Context): Boolean =
        prefs(ctx).getBoolean(KEY_TTS_AUTO, false)

    fun setTtsAuto(ctx: Context, on: Boolean) {
        prefs(ctx).edit { putBoolean(KEY_TTS_AUTO, on) }
    }

    /** Hub one-liner: how many knobs differ from their defaults. */
    fun customCount(ctx: Context): Int {
        var n = 0
        if (syncIntervalHours(ctx) != DEFAULT_SYNC_INTERVAL_HOURS) n++
        if (syncWifiOnly(ctx)) n++
        if (syncPaused(ctx)) n++
        if (taskSyncIntervalMinutes(ctx) != DEFAULT_TASK_SYNC_INTERVAL_MINUTES) n++
        if (reminderLeadMinutes(ctx) != TaskReminders.BEFORE_START_MINUTES) n++
        if (ttsVoice(ctx).isNotEmpty()) n++
        if (ttsRatePct(ctx) != DEFAULT_TTS_PCT) n++
        if (ttsPitchPct(ctx) != DEFAULT_TTS_PCT) n++
        if (!notifyFailedRaw(ctx)) n++
        if (!notifyInstructRaw(ctx)) n++
        if (!notifyCronRaw(ctx)) n++
        // Surfaced-but-pre-existing keys count too: enabling either is a
        // deliberate behaviour change made in this section.
        if (TaskReminders.isEnabled(ctx)) n++
        if (ttsAuto(ctx)) n++
        return n
    }

    fun hubStatus(ctx: Context): String = hubStatus(customCount(ctx))

    fun hubStatus(custom: Int): String =
        if (custom == 0) "Defaults" else "$custom custom"

    /**
     * Coerces one imported numeric value to its sane range: the single
     * source for floors/ceilings, shared by the live getters above and the
     * backup import below. A hand-edited backup importing sync=0 stores a
     * value the getter would otherwise silently disagree with (stored 0,
     * read 1) — coercing at both ends keeps stored and shown identical.
     * Unknown keys keep the legacy non-negative clamp.
     */
    fun coerceImport(key: String, value: Long): Long = when (key) {
        KEY_SYNC_INTERVAL_HOURS -> value.coerceIn(1L, 24L)
        KEY_TASK_SYNC_INTERVAL_MINUTES -> value.coerceIn(15L, 24L * 60L)
        KEY_REMINDER_LEAD_MINUTES -> value.coerceIn(0L, 24L * 60L)
        KEY_TTS_RATE_PCT, KEY_TTS_PITCH_PCT -> value.coerceIn(50L, 200L)
        else -> maxOf(0L, value)
    }

    private fun prefs(ctx: Context) =
        ctx.applicationContext.getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
}

/**
 * Applies the stored TTS voice/rate/pitch to a live engine. Each knob is
 * skipped at its default, so an install that never opens the section speaks
 * exactly like 5.12.0 — engine defaults, no setVoice/setSpeechRate/setPitch.
 * The voice is matched by name against THIS engine's voices (Voice objects
 * are engine-specific); a stored name the engine no longer offers is left
 * alone rather than failing the utterance. Never throws (OEM binder
 * breakage is a warning, like the callers already treat it).
 */
fun applyStoredTtsSettings(tts: TextToSpeech, appCtx: Context) {
    // Compared as stored Long pcts, never as floats: exact, and skipped at
    // defaults so untouched installs speak exactly like 5.12.0.
    runCatching {
        val pct = BehaviourSettings.ttsRatePct(appCtx)
        if (pct != BehaviourSettings.DEFAULT_TTS_PCT) tts.setSpeechRate(pct / 100f)
    }
    runCatching {
        val pct = BehaviourSettings.ttsPitchPct(appCtx)
        if (pct != BehaviourSettings.DEFAULT_TTS_PCT) tts.setPitch(pct / 100f)
    }
    val want = BehaviourSettings.ttsVoice(appCtx)
    if (want.isNotEmpty()) {
        runCatching {
            tts.voices?.firstOrNull { it.name == want }?.let { tts.voice = it }
        }
    }
}
