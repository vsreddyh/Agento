package com.vishnu.agento

import android.Manifest
import android.app.AlarmManager
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import android.provider.Settings
import android.speech.tts.TextToSpeech
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.*
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.core.content.ContextCompat
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.compose.LifecycleEventEffect
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.coroutines.withTimeoutOrNull
import kotlin.coroutines.resume

/**
 * Settings → Behaviour (#355): every device-local knob in one place —
 * sync cadence, task refresh, reminder lead time + enable, TTS voice/rate/
 * pitch + auto-read, and the per-kind notification gates.
 *
 * All reads/writes go through [BehaviourSettings] (the single inventory);
 * this file owns no pref key spelling. [refreshTick] re-reads everything
 * after a settings import, like the hub does.
 */
@Composable
internal fun BehaviourSection(refreshTick: Int = 0) {
    val context = LocalContext.current
    val appCtx = context.applicationContext

    // Re-read on every resume (declared first: state below keys on it).
    // Covers flips made outside this section — the chat-bar auto-read
    // toggle, the exact-alarm grant — without waiting for an import.
    var resumeTick by remember { mutableIntStateOf(0) }
    LifecycleEventEffect(Lifecycle.Event.ON_RESUME) { resumeTick++ }

    var syncHours by remember(refreshTick) {
        mutableStateOf(BehaviourSettings.syncIntervalHours(context))
    }
    var wifiOnly by remember(refreshTick) {
        mutableStateOf(BehaviourSettings.syncWifiOnly(context))
    }
    var paused by remember(refreshTick) {
        mutableStateOf(BehaviourSettings.syncPaused(context))
    }
    var taskMinutes by remember(refreshTick) {
        mutableStateOf(BehaviourSettings.taskSyncIntervalMinutes(context))
    }
    var leadMinutes by remember(refreshTick) {
        mutableStateOf(BehaviourSettings.reminderLeadMinutes(context))
    }
    var remindersOn by remember(refreshTick, resumeTick) {
        mutableStateOf(TaskReminders.isEnabled(context))
    }
    var ttsVoice by remember(refreshTick) {
        mutableStateOf(BehaviourSettings.ttsVoice(context))
    }
    var ttsRate by remember(refreshTick) {
        mutableStateOf(BehaviourSettings.ttsRatePct(context))
    }
    var ttsPitch by remember(refreshTick) {
        mutableStateOf(BehaviourSettings.ttsPitchPct(context))
    }
    var autoRead by remember(refreshTick, resumeTick) {
        mutableStateOf(BehaviourSettings.ttsAuto(context))
    }
    var kindFailed by remember(refreshTick) {
        mutableStateOf(BehaviourSettings.notifyFailedRaw(context))
    }
    var kindInstruct by remember(refreshTick) {
        mutableStateOf(BehaviourSettings.notifyInstructRaw(context))
    }
    var kindCron by remember(refreshTick) {
        mutableStateOf(BehaviourSettings.notifyCronRaw(context))
    }
    val masterOn = ChatNotifications.isEnabled(context)

    // Reminder enable flow (moved from the Task Manager bell): exact alarms
    // need a settings redirect on S+, notifications a runtime grant on 33+.
    var remindersDenied by remember(refreshTick) { mutableStateOf(false) }
    val notifPermission = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestPermission(),
    ) { granted ->
        if (granted) {
            TaskReminders.setEnabled(context, true)
            remindersOn = true
            remindersDenied = false
        } else {
            // The switch stays off with an explanation, like the old bell's
            // denial toast — silence would read as a broken toggle.
            remindersDenied = true
        }
    }

    fun setReminders(on: Boolean) {
        if (!on) {
            TaskReminders.setEnabled(context, false)
            remindersOn = false
            remindersDenied = false
            return
        }
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            val mgr = appCtx.getSystemService(Context.ALARM_SERVICE) as AlarmManager
            if (!mgr.canScheduleExactAlarms()) {
                runCatching {
                    context.startActivity(
                        Intent(Settings.ACTION_REQUEST_SCHEDULE_EXACT_ALARM)
                            // No-op from this Activity today; crash-proof if
                            // the composition context ever isn't one.
                            .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK),
                    )
                }
            }
        }
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU &&
            ContextCompat.checkSelfPermission(
                appCtx, Manifest.permission.POST_NOTIFICATIONS,
            ) != PackageManager.PERMISSION_GRANTED
        ) {
            notifPermission.launch(Manifest.permission.POST_NOTIFICATIONS)
            return
        }
        TaskReminders.setEnabled(context, true)
        remindersOn = true
        remindersDenied = false
    }

    // Engine voices for the picker: a short-lived engine lists, then dies.
    // Matched by name at speak time (Voice objects are engine-specific).
    // Bounded: a broken engine answers empty, never hangs the section.
    var voices by remember(refreshTick) { mutableStateOf<List<Pair<String, String>>>(emptyList()) }
    // Re-list on resume only when it could matter (empty list, or the
    // stored voice missing from it): a blind re-spin on every resume pays
    // a binder init + enumeration on the resume path for nothing.
    LaunchedEffect(refreshTick, resumeTick) {
        val cur = voices
        if (cur.isEmpty() || (ttsVoice.isNotEmpty() && cur.none { it.first == ttsVoice })) {
            voices = withTimeoutOrNull(10_000L) { listTtsVoices(appCtx) }.orEmpty()
        }
    }

    // Exact-alarm grant, re-read on the same resume tick as the switches
    // above: enabling without it falls back to inexact alarms, and the
    // copy must not promise otherwise.
    val exactAlarms = remember(resumeTick) {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.S) true
        else runCatching {
            (appCtx.getSystemService(Context.ALARM_SERVICE) as AlarmManager)
                .canScheduleExactAlarms()
        }.getOrDefault(true)
    }

    SectionCard(
        title = "Health sync cadence",
        subtitle = "How often health data pushes while the app is idle. Changes apply from the next scheduled run.",
    ) {
        OptionMenu(
            label = "Sync every",
            shown = BehaviourSettings.intervalChoiceLabel(syncHours),
            options = BehaviourSettings.SYNC_INTERVAL_OPTIONS_HOURS.map {
                it.toString() to BehaviourSettings.intervalChoiceLabel(it)
            },
            onPick = {
                it.toLongOrNull()?.let { h ->
                    BehaviourSettings.setSyncIntervalHours(context, h)
                    syncHours = BehaviourSettings.syncIntervalHours(context)
                }
            },
        )
        SwitchRow(
            text = "Wi-Fi only",
            sub = "Sync on unmetered Wi-Fi; mobile data never syncs.",
            checked = wifiOnly,
            onChange = {
                BehaviourSettings.setSyncWifiOnly(context, it)
                wifiOnly = it
            },
        )
        SwitchRow(
            text = "Pause sync",
            sub = "Stops health sync entirely until resumed (task refresh keeps running).",
            checked = paused,
            onChange = {
                BehaviourSettings.setSyncPaused(context, it)
                paused = it
            },
        )
        HintLine(BehaviourSettings.syncCadenceLine(syncHours, wifiOnly, paused))
        if (syncHours !in BehaviourSettings.SYNC_INTERVAL_OPTIONS_HOURS) {
            HintLine("Kept from an import: this cadence isn't offered above, but it still applies.")
        }
    }

    SectionCard(
        title = "Task and widget refresh",
        subtitle = "How often open tasks, the widget and reminder alarms re-derive. Changes apply from the next scheduled run.",
    ) {
        OptionMenu(
            label = "Refresh every",
            shown = BehaviourSettings.minutesChoiceLabel(taskMinutes),
            options = BehaviourSettings.TASK_SYNC_INTERVAL_OPTIONS_MINUTES.map {
                it.toString() to BehaviourSettings.minutesChoiceLabel(it)
            },
            onPick = {
                it.toLongOrNull()?.let { m ->
                    BehaviourSettings.setTaskSyncIntervalMinutes(context, m)
                    taskMinutes = BehaviourSettings.taskSyncIntervalMinutes(context)
                }
            },
        )
        HintLine(BehaviourSettings.taskSyncCadenceLine(taskMinutes))
        if (taskMinutes !in BehaviourSettings.TASK_SYNC_INTERVAL_OPTIONS_MINUTES) {
            HintLine("Kept from an import: this cadence isn't offered above, but it still applies.")
        }
        if (taskMinutes > (BehaviourSettings.TASK_SYNC_INTERVAL_OPTIONS_MINUTES.maxOrNull() ?: 120L)) {
            HintLine("Above the offered range (kept from an import): refresh may lag up to a day behind task changes.")
        }
    }

    SectionCard(
        title = "Task reminders",
        subtitle = "Due-time alarms with a spoken heads-up.",
    ) {
        SwitchRow(
            text = "Due-time reminders",
            sub = "Moved here from the Task Manager bell — one switch, same alarms.",
            checked = remindersOn,
            onChange = ::setReminders,
        )
        if (remindersDenied) {
            HintLine("Notifications are blocked — reminders stay off until allowed.")
        }
        if (remindersOn && !exactAlarms) {
            HintLine("Exact alarms aren't allowed — reminders still fire, but may be inexact until allowed in system settings.")
        }
        OptionMenu(
            label = "Warn me",
            shown = BehaviourSettings.leadLabel(leadMinutes),
            options = BehaviourSettings.REMINDER_LEAD_OPTIONS_MINUTES.map {
                it.toString() to BehaviourSettings.leadLabel(it)
            },
            onPick = {
                it.toLongOrNull()?.let { m ->
                    BehaviourSettings.setReminderLeadMinutes(context, m)
                    leadMinutes = BehaviourSettings.reminderLeadMinutes(context)
                }
            },
        )
        if (leadMinutes !in BehaviourSettings.REMINDER_LEAD_OPTIONS_MINUTES) {
            HintLine("Kept from an import: this lead time isn't offered above, but it still applies.")
        }
        if (leadMinutes > (BehaviourSettings.REMINDER_LEAD_OPTIONS_MINUTES.maxOrNull() ?: 60L)) {
            HintLine("Above the offered range (kept from an import): the warning reads in hours.")
        }
    }

    SectionCard(
        title = "Speech",
        subtitle = "Voice and delivery for chat read-aloud and reminder speech.",
    ) {
        OptionMenu(
            label = "Voice",
            shown = voiceShown(ttsVoice, voices),
            options = listOf("" to "Engine default") + voices,
            onPick = {
                BehaviourSettings.setTtsVoice(context, it)
                ttsVoice = BehaviourSettings.ttsVoice(context)
            },
        )
        if (ttsVoice.isNotEmpty() && voices.isNotEmpty() &&
            voices.none { it.first == ttsVoice }
        ) {
            HintLine("Kept from an import: this engine no longer offers that voice, but the pick is kept.")
        }
        OptionMenu(
            label = "Rate",
            shown = BehaviourSettings.ttsPctLabel(ttsRate),
            options = BehaviourSettings.TTS_PCT_OPTIONS.map {
                it.toString() to BehaviourSettings.ttsPctLabel(it)
            },
            onPick = {
                it.toLongOrNull()?.let { p ->
                    BehaviourSettings.setTtsRatePct(context, p)
                    ttsRate = BehaviourSettings.ttsRatePct(context)
                }
            },
        )
        OptionMenu(
            label = "Pitch",
            shown = BehaviourSettings.ttsPctLabel(ttsPitch),
            options = BehaviourSettings.TTS_PCT_OPTIONS.map {
                it.toString() to BehaviourSettings.ttsPctLabel(it)
            },
            onPick = {
                it.toLongOrNull()?.let { p ->
                    BehaviourSettings.setTtsPitchPct(context, p)
                    ttsPitch = BehaviourSettings.ttsPitchPct(context)
                }
            },
        )
        if (ttsRate !in BehaviourSettings.TTS_PCT_OPTIONS) {
            HintLine("Kept from an import: this rate isn't offered above, but it still applies.")
        }
        if (ttsPitch !in BehaviourSettings.TTS_PCT_OPTIONS) {
            HintLine("Kept from an import: this pitch isn't offered above, but it still applies.")
        }
        SwitchRow(
            text = "Auto-read replies",
            sub = "Same switch as the chat bar toggle — both write one key.",
            checked = autoRead,
            onChange = {
                BehaviourSettings.setTtsAuto(context, it)
                autoRead = it
            },
        )
    }

    SectionCard(
        title = "Notification kinds",
        subtitle = "Each applies only while the master reply-alerts switch " +
            "(Settings → Notifications) is on; all default on.",
    ) {
        if (!masterOn) {
            HintLine("Master reply alerts are off — every kind below stays silent until re-enabled in Notifications.")
        }
        SwitchRow(
            text = "Reply failed",
            sub = "The turn was cut off or errored while backgrounded.",
            checked = kindFailed,
            onChange = {
                BehaviourSettings.setNotifyFailed(context, it)
                kindFailed = it
            },
        )
        SwitchRow(
            text = "Instruction saved",
            sub = "A task instruction card is ready to start.",
            checked = kindInstruct,
            onChange = {
                BehaviourSettings.setNotifyInstruct(context, it)
                kindInstruct = it
            },
        )
        SwitchRow(
            text = "Scheduler responses",
            sub = "A scheduled job produced a new response. Runs from while off are skipped, not queued.",
            checked = kindCron,
            onChange = {
                BehaviourSettings.setNotifyCron(context, it)
                kindCron = it
            },
        )
    }
}

/** One labelled switch row, the Notifications section's shape reused. */
@Composable
private fun SwitchRow(
    text: String,
    sub: String,
    checked: Boolean,
    onChange: (Boolean) -> Unit,
) {
    Column(modifier = Modifier.fillMaxWidth()) {
        Row(
            modifier = Modifier.fillMaxWidth(),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Text(
                text,
                style = MaterialTheme.typography.bodyMedium,
                modifier = Modifier.weight(1f),
            )
            Switch(checked = checked, onCheckedChange = onChange)
        }
        if (sub.isNotEmpty()) {
            HintLine(sub)
        }
    }
}

private fun voiceShown(saved: String, voices: List<Pair<String, String>>): String {
    if (saved.isEmpty()) return "Engine default"
    return voices.firstOrNull { it.first == saved }?.second ?: saved
}

/** Lists engine voices as (name to display) pairs, then shuts down.
 * Never throws and never hangs the caller: a constructor failure answers
 * empty (pair with the timeout in the caller, which cancels and resumes
 * empty via invokeOnCancellation's shutdown). */
private suspend fun listTtsVoices(appCtx: Context): List<Pair<String, String>> =
    suspendCancellableCoroutine { cont ->
        val ref = java.util.concurrent.atomic.AtomicReference<TextToSpeech?>(null)
        val statusRef = java.util.concurrent.atomic.AtomicInteger(Int.MIN_VALUE)
        val done = java.util.concurrent.atomic.AtomicBoolean(false)
        fun finish(tts: TextToSpeech, status: Int) {
            if (!done.compareAndSet(false, true)) return
            val out = if (status == TextToSpeech.SUCCESS) {
                runCatching {
                    // Stable sort key (locale tag + name), localized label:
                    // sorting by displayName would reorder the picker on
                    // every system-language change.
                    tts.voices.orEmpty()
                        .sortedWith(compareBy({ it.locale?.toString().orEmpty() }, { it.name }))
                        .map { v ->
                            val loc = v.locale?.displayName?.takeIf { it.isNotBlank() }
                            v.name to if (loc != null) "$loc — ${v.name}" else v.name
                        }
                }.getOrDefault(emptyList())
            } else {
                emptyList()
            }
            runCatching { tts.shutdown() }
            if (cont.isActive) cont.resume(out)
        }
        // Synchronous-init OEMs run the callback inside the constructor,
        // before `ref` is assigned (see ReminderTts): the callback then only
        // stashes its status, and the tail below finishes explicitly. The
        // CAS keeps a racing async callback from finishing twice.
        //
        // A constructor that throws (missing/broken engine) answers empty
        // instead of escaping the LaunchedEffect with `cont` unresumed.
        val tts = runCatching {
            TextToSpeech(appCtx) { status ->
                statusRef.set(status)
                ref.get()?.let { finish(it, status) }
            }
        }.getOrNull()
        if (tts == null) {
            if (done.compareAndSet(false, true) && cont.isActive) cont.resume(emptyList())
            return@suspendCancellableCoroutine
        }
        ref.set(tts)
        val s = statusRef.get()
        if (s != Int.MIN_VALUE) finish(tts, s)
        cont.invokeOnCancellation { runCatching { ref.get()?.shutdown() } }
    }
