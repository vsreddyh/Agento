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
import kotlinx.coroutines.suspendCancellableCoroutine
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
    var remindersOn by remember(refreshTick) {
        mutableStateOf(TaskReminders.isEnabled(context))
    }
    var ttsVoice by remember(refreshTick) {
        mutableStateOf(BehaviourSettings.ttsVoice(context))
    }
    var ttsRate by remember(refreshTick) {
        mutableStateOf(BehaviourSettings.ttsRate(context) * 100f)
    }
    var ttsPitch by remember(refreshTick) {
        mutableStateOf(BehaviourSettings.ttsPitch(context) * 100f)
    }
    var autoRead by remember(refreshTick) {
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
    val notifPermission = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestPermission(),
    ) { granted ->
        if (granted) {
            TaskReminders.setEnabled(context, true)
            remindersOn = true
        }
    }

    fun setReminders(on: Boolean) {
        if (!on) {
            TaskReminders.setEnabled(context, false)
            remindersOn = false
            return
        }
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            val mgr = appCtx.getSystemService(Context.ALARM_SERVICE) as AlarmManager
            if (!mgr.canScheduleExactAlarms()) {
                runCatching {
                    context.startActivity(
                        Intent(Settings.ACTION_REQUEST_SCHEDULE_EXACT_ALARM),
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
    }

    // Engine voices for the picker: a short-lived engine lists, then dies.
    // Matched by name at speak time (Voice objects are engine-specific).
    var voices by remember(refreshTick) { mutableStateOf<List<Pair<String, String>>>(emptyList()) }
    LaunchedEffect(refreshTick) {
        voices = listTtsVoices(appCtx)
    }

    SectionCard(
        title = "Health sync cadence",
        subtitle = "How often health data pushes while the app is idle.",
    ) {
        OptionMenu(
            label = "Sync every",
            shown = intervalLabel(syncHours),
            options = BehaviourSettings.SYNC_INTERVAL_OPTIONS_HOURS.map {
                it.toString() to intervalLabel(it)
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
            sub = "Stops the periodic job entirely until resumed.",
            checked = paused,
            onChange = {
                BehaviourSettings.setSyncPaused(context, it)
                paused = it
            },
        )
        HintLine(BehaviourSettings.syncCadenceLine(context))
    }

    SectionCard(
        title = "Task and widget refresh",
        subtitle = "How often open tasks, the widget and reminder alarms re-derive.",
    ) {
        OptionMenu(
            label = "Refresh every",
            shown = minutesLabel(taskMinutes),
            options = BehaviourSettings.TASK_SYNC_INTERVAL_OPTIONS_MINUTES.map {
                it.toString() to minutesLabel(it)
            },
            onPick = {
                it.toLongOrNull()?.let { m ->
                    BehaviourSettings.setTaskSyncIntervalMinutes(context, m)
                    taskMinutes = BehaviourSettings.taskSyncIntervalMinutes(context)
                }
            },
        )
        HintLine(BehaviourSettings.taskSyncCadenceLine(context))
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
                ttsVoice = it
            },
        )
        OptionMenu(
            label = "Rate",
            shown = BehaviourSettings.ttsPctLabel(ttsRate.toLong()),
            options = BehaviourSettings.TTS_PCT_OPTIONS.map {
                it.toString() to BehaviourSettings.ttsPctLabel(it)
            },
            onPick = {
                it.toLongOrNull()?.let { p ->
                    BehaviourSettings.setTtsRatePct(context, p)
                    ttsRate = BehaviourSettings.ttsRate(context) * 100f
                }
            },
        )
        OptionMenu(
            label = "Pitch",
            shown = BehaviourSettings.ttsPctLabel(ttsPitch.toLong()),
            options = BehaviourSettings.TTS_PCT_OPTIONS.map {
                it.toString() to BehaviourSettings.ttsPctLabel(it)
            },
            onPick = {
                it.toLongOrNull()?.let { p ->
                    BehaviourSettings.setTtsPitchPct(context, p)
                    ttsPitch = BehaviourSettings.ttsPitch(context) * 100f
                }
            },
        )
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
            sub = "A scheduled job produced a new response.",
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

private fun intervalLabel(hours: Long): String =
    if (hours == 1L) "1 hour" else "$hours hours"

private fun minutesLabel(minutes: Long): String = when {
    minutes == 60L -> "1 hour"
    minutes % 60L == 0L -> "${minutes / 60L} hours"
    else -> "$minutes minutes"
}

private fun voiceShown(saved: String, voices: List<Pair<String, String>>): String {
    if (saved.isEmpty()) return "Engine default"
    return voices.firstOrNull { it.first == saved }?.second ?: saved
}

/** Lists engine voices as (name to display) pairs, then shuts down. */
private suspend fun listTtsVoices(appCtx: Context): List<Pair<String, String>> =
    suspendCancellableCoroutine { cont ->
        val ref = java.util.concurrent.atomic.AtomicReference<TextToSpeech?>(null)
        val statusRef = java.util.concurrent.atomic.AtomicInteger(Int.MIN_VALUE)
        val done = java.util.concurrent.atomic.AtomicBoolean(false)
        fun finish(tts: TextToSpeech, status: Int) {
            if (!done.compareAndSet(false, true)) return
            val out = if (status == TextToSpeech.SUCCESS) {
                runCatching {
                    tts.voices.orEmpty()
                        .sortedWith(compareBy({ it.locale?.displayName.orEmpty() }, { it.name }))
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
        ref.set(TextToSpeech(appCtx) { status ->
            statusRef.set(status)
            ref.get()?.let { finish(it, status) }
        })
        val e = ref.get()
        val s = statusRef.get()
        if (e != null && s != Int.MIN_VALUE) finish(e, s)
        cont.invokeOnCancellation { runCatching { ref.get()?.shutdown() } }
    }
