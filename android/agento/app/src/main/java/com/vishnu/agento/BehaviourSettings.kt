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
            // Same humanizer as the lead copy: adjacent cards must agree.
            m > 60L -> "Task and widget refresh runs every ${leadCopy(m)}."
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

    fun leadLabel(minutes: Long): String {
        // Total via the import coercion: negatives coerce to 0, the
        // at-start branch — never "0 minutes before start".
        val m = coerceImport(KEY_REMINDER_LEAD_MINUTES, minutes)
        return when (m) {
            0L -> "At start time (no heads-up)"
            // Shared with the notification copy (leadCopy): the picker and
            // the lock screen must agree, including on hand-imported values.
            else -> "${leadCopy(m)} before start"
        }
    }

    /** Picker choice labels; pinned — the section renders these verbatim.
     * Total (coerced): unreachable via state, but public like the cadence
     * lines, so they never render "0 hours" to a direct caller. */
    fun intervalChoiceLabel(hours: Long): String {
        val h = coerceImport(KEY_SYNC_INTERVAL_HOURS, hours)
        return if (h == 1L) "1 hour" else "$h hours"
    }

    fun minutesChoiceLabel(minutes: Long): String {
        val m = coerceImport(KEY_TASK_SYNC_INTERVAL_MINUTES, minutes)
        return when {
            m == 60L -> "1 hour"
            m % 60L == 0L -> "${m / 60L} hours"
            // Same humanizer as the cadence line: picker and hint agree.
            m > 60L -> leadCopy(m)
            else -> "$m minutes"
        }
    }

    // ── TTS voice / rate / pitch ─────────────────────────────────────────
    const val KEY_TTS_VOICE = "tts_voice"
    const val KEY_TTS_RATE_PCT = "tts_rate_pct"
    const val KEY_TTS_PITCH_PCT = "tts_pitch_pct"
    const val DEFAULT_TTS_PCT = 100L
    /** Engine range is roughly half to double speed; the picker offers steps. */
    val TTS_PCT_OPTIONS = listOf(50L, 75L, 100L, 125L, 150L, 200L)

    /** Voice name, or "" for the engine default (today's behaviour).
     * Trimmed like the setter, so a hand-imported padded name still
     * matches the engine's voice instead of missing silently. */
    fun ttsVoice(ctx: Context): String =
        prefs(ctx).getString(KEY_TTS_VOICE, "").orEmpty().trim()

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

    /** Keyed label: rate and pitch share 50..200 today, but the coercion
     * rides the caller's key so a future range split can't silently
     * mis-coerce one side while setters/import stay correct. There is
     * deliberately no unkeyed shorthand: the shorter name is exactly how
     * the next caller would reintroduce the hazard. */
    fun ttsPctLabelFor(key: String, pct: Long): String = "${coerceImport(key, pct)}%"

    // ── Notification kinds ───────────────────────────────────────────────
    // The shared skipped-not-queued clause, spelled once: the master copy
    // and the kind copy below must never drift apart again.
    const val SKIPPED_NOT_QUEUED = "Runs from while off are skipped, not queued."
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

    // ── App update checks (#366) ─────────────────────────────────────────
    // Startup check is on and the periodic poll is daily, so an install
    // that never opens this section notices releases the way every other
    // app does; the notification stays OFF because a silent badge in
    // Settings is the least surprising default (the issue's suggestion).
    const val KEY_UPDATE_STARTUP = "update_check_startup"
    const val KEY_UPDATE_INTERVAL_HOURS = "update_check_interval_hours"
    const val KEY_UPDATE_NOTIFY = "update_check_notify"
    const val DEFAULT_UPDATE_INTERVAL_HOURS = 24L
    /** 0 means "off": no periodic job at all, exactly like paused sync. */
    const val UPDATE_OFF_HOURS = 0L
    /** 168 = weekly: GitHub's unauthenticated API allows 60 req/hr, and a
     * device never needs more than one check per hour anyway. */
    val UPDATE_INTERVAL_OPTIONS_HOURS =
        listOf(UPDATE_OFF_HOURS, 1L, 2L, 4L, 6L, 12L, 24L, 168L)

    fun updateStartup(ctx: Context): Boolean =
        prefs(ctx).getBoolean(KEY_UPDATE_STARTUP, true)

    fun updateIntervalHours(ctx: Context): Long =
        coerceImport(KEY_UPDATE_INTERVAL_HOURS,
            prefs(ctx).getLong(KEY_UPDATE_INTERVAL_HOURS, DEFAULT_UPDATE_INTERVAL_HOURS))

    fun updateNotify(ctx: Context): Boolean =
        prefs(ctx).getBoolean(KEY_UPDATE_NOTIFY, false)

    fun setUpdateStartup(ctx: Context, on: Boolean) {
        prefs(ctx).edit { putBoolean(KEY_UPDATE_STARTUP, on) }
        AgentoApp.maybeCheckForUpdatesOnStartup(ctx)
    }

    fun setUpdateIntervalHours(ctx: Context, hours: Long) {
        prefs(ctx).edit {
            putLong(KEY_UPDATE_INTERVAL_HOURS, hours.coerceIn(UPDATE_OFF_HOURS, 168L))
        }
        // Re-enqueue (or cancel) now: the user just picked a cadence, so
        // waiting for the next cold start would be indistinguishable from
        // the setting not working.
        AgentoApp.scheduleUpdateChecks(ctx)
    }

    fun setUpdateNotify(ctx: Context, on: Boolean) {
        prefs(ctx).edit { putBoolean(KEY_UPDATE_NOTIFY, on) }
    }

    /** Picker label; `0` is "Off", never "every 0 hours". */
    fun updateIntervalChoiceLabel(hours: Long): String {
        val h = coerceImport(KEY_UPDATE_INTERVAL_HOURS, hours)
        return when {
            h == UPDATE_OFF_HOURS -> "Off"
            h == 1L -> "Hourly"
            h == 24L -> "Daily"
            h == 168L -> "Weekly"
            h % 24L == 0L -> "Every ${h / 24L} days"
            else -> "Every $h hours"
        }
    }

    /** Cadence line for the App updates card; total like its siblings. */
    fun updateCadenceLine(hours: Long, startup: Boolean): String {
        val h = coerceImport(KEY_UPDATE_INTERVAL_HOURS, hours)
        val periodic = when {
            h == UPDATE_OFF_HOURS -> "Automatic checks are off."
            h == 1L -> "Automatic checks run every hour."
            h == 24L -> "Automatic checks run daily."
            h == 168L -> "Automatic checks run weekly."
            h % 24L == 0L -> "Automatic checks run every ${h / 24L} days."
            else -> "Automatic checks run every $h hours."
        }
        return if (startup) "$periodic Plus once on startup." else periodic
    }

    // ── Auto-read (surfaced here; the chat bar keeps its toggle) ─────────
    const val KEY_TTS_AUTO = "tts_auto"

    fun ttsAuto(ctx: Context): Boolean =
        prefs(ctx).getBoolean(KEY_TTS_AUTO, false)

    fun setTtsAuto(ctx: Context, on: Boolean) {
        prefs(ctx).edit { putBoolean(KEY_TTS_AUTO, on) }
    }

    /** Hub one-liner: how many knobs differ from their defaults. */
    fun customCount(ctx: Context): Int = customCount(
        syncHours = syncIntervalHours(ctx),
        wifiOnly = syncWifiOnly(ctx),
        paused = syncPaused(ctx),
        taskMinutes = taskSyncIntervalMinutes(ctx),
        leadMinutes = reminderLeadMinutes(ctx),
        voice = ttsVoice(ctx),
        ratePct = ttsRatePct(ctx),
        pitchPct = ttsPitchPct(ctx),
        kindFailed = notifyFailedRaw(ctx),
        kindInstruct = notifyInstructRaw(ctx),
        kindCron = notifyCronRaw(ctx),
        remindersOn = TaskReminders.isEnabled(ctx),
        autoRead = ttsAuto(ctx),
        updateHours = updateIntervalHours(ctx),
        updateStartup = updateStartup(ctx),
        updateNotify = updateNotify(ctx),
    )

    /** Pure deviation count, JVM-pinned: every knob that differs from its
     * default, including the surfaced-but-pre-existing switches (enabling
     * reminders or auto-read elsewhere still counts — deliberate, and the
     * hub copy says "custom", not "changed here"). */
    fun customCount(
        syncHours: Long,
        wifiOnly: Boolean,
        paused: Boolean,
        taskMinutes: Long,
        leadMinutes: Long,
        voice: String,
        ratePct: Long,
        pitchPct: Long,
        kindFailed: Boolean,
        kindInstruct: Boolean,
        kindCron: Boolean,
        remindersOn: Boolean,
        autoRead: Boolean,
        // #366 append-only: existing callers (and the pinned test helper)
        // keep compiling, and the defaults match the getters so "nothing
        // configured" still counts zero.
        updateHours: Long = DEFAULT_UPDATE_INTERVAL_HOURS,
        updateStartup: Boolean = true,
        updateNotify: Boolean = false,
    ): Int {
        // Total like the cadence lines: coerce, so both overloads agree on
        // the same effective state (a stored 0 reads back as its floor).
        val sh = coerceImport(KEY_SYNC_INTERVAL_HOURS, syncHours)
        val tm = coerceImport(KEY_TASK_SYNC_INTERVAL_MINUTES, taskMinutes)
        val lm = coerceImport(KEY_REMINDER_LEAD_MINUTES, leadMinutes)
        val rp = coerceImport(KEY_TTS_RATE_PCT, ratePct)
        val pp = coerceImport(KEY_TTS_PITCH_PCT, pitchPct)
        var n = 0
        // Paused suspends the whole cadence card: interval and wifiOnly
        // are both moot while no sync runs, so neither counts then — or
        // pausing with a custom cadence reads as two changes for zero
        // effective behaviour change.
        if (!paused) {
            if (sh != DEFAULT_SYNC_INTERVAL_HOURS) n++
            if (wifiOnly) n++
        }
        if (paused) n++
        if (tm != DEFAULT_TASK_SYNC_INTERVAL_MINUTES) n++
        if (lm != TaskReminders.BEFORE_START_MINUTES) n++
        if (voice.trim().isNotEmpty()) n++
        if (rp != DEFAULT_TTS_PCT) n++
        if (pp != DEFAULT_TTS_PCT) n++
        if (!kindFailed) n++
        if (!kindInstruct) n++
        if (!kindCron) n++
        // Surfaced-but-pre-existing keys count too: enabling either is a
        // deliberate behaviour change made in this section.
        if (remindersOn) n++
        if (autoRead) n++
        // Update checks (#366): each knob is independently meaningful, so
        // no mootness folding here — a weekly cadence with a startup check
        // is two deliberate picks, not one.
        if (coerceImport(KEY_UPDATE_INTERVAL_HOURS, updateHours) != DEFAULT_UPDATE_INTERVAL_HOURS) n++
        if (!updateStartup) n++
        if (updateNotify) n++
        return n
    }

    fun hubStatus(ctx: Context): String = hubStatus(customCount(ctx))

    fun hubStatus(custom: Int): String {
        // Total: the counter can never go negative, but the overload is
        // public alongside the coerced helpers.
        val n = maxOf(0, custom)
        return if (n == 0) "Defaults" else "$n custom"
    }

    /**
     * Coerces one imported numeric value to its sane range: the single
     * source for floors/ceilings, shared by the live getters above and the
     * backup import below. A hand-edited backup importing sync=0 stores a
     * value the getter would otherwise silently disagree with (stored 0,
     * read 1) — coercing at both ends keeps stored and shown identical.
     * Unknown keys keep the legacy non-negative clamp.
     *
     * Ceilings can coincide with offered picker values (sync 48h → 24h,
     * rate 500 → 200): such an import renders as a normal pick with no
     * hint. That is accepted — ceilings must exist, and the alternative
     * (preserving an un-runnable 48h cadence) is worse than clamping to
     * the nearest runnable one.
     */
    fun coerceImport(key: String, value: Long): Long = when (key) {
        KEY_SYNC_INTERVAL_HOURS -> value.coerceIn(1L, 24L)
        KEY_TASK_SYNC_INTERVAL_MINUTES -> value.coerceIn(15L, 24L * 60L)
        KEY_REMINDER_LEAD_MINUTES -> value.coerceIn(0L, 24L * 60L)
        KEY_TTS_RATE_PCT, KEY_TTS_PITCH_PCT -> value.coerceIn(50L, 200L)
        // 0 is a real value here (off), unlike the cadences above whose
        // floor is the shortest offered interval.
        KEY_UPDATE_INTERVAL_HOURS -> value.coerceIn(UPDATE_OFF_HOURS, 168L)
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
