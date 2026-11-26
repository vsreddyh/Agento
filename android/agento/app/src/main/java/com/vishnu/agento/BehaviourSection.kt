package com.vishnu.agento

import android.Manifest
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import android.provider.Settings
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

/**
 * Settings → Behaviour (#355): every device-local knob in one place —
 * task refresh, reminder lead time + enable, and the app update checks.
 * Reminder speech runs on engine defaults (the Speech card is gone, #596).
 * (Health sync cadence lives with the Sync card in Settings → Health sync,
 * #588; widget refresh lives with the Widget section, #595.)
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
    // Covers flips made outside this section — the exact-alarm grant —
    // without waiting for an import.
    var resumeTick by remember { mutableIntStateOf(0) }
    LifecycleEventEffect(Lifecycle.Event.ON_RESUME) { resumeTick++ }

    var taskMinutes by remember(refreshTick) {
        mutableStateOf(BehaviourSettings.taskSyncIntervalMinutes(context))
    }
    var leadMinutes by remember(refreshTick) {
        mutableStateOf(BehaviourSettings.reminderLeadMinutes(context))
    }
    var remindersOn by remember(refreshTick, resumeTick) {
        mutableStateOf(TaskReminders.isEnabled(context))
    }
    var updateStartup by remember(refreshTick) {
        mutableStateOf(BehaviourSettings.updateStartup(context))
    }
    var updateHours by remember(refreshTick) {
        mutableStateOf(BehaviourSettings.updateIntervalHours(context))
    }
    var updateNotify by remember(refreshTick) {
        mutableStateOf(BehaviourSettings.updateNotify(context))
    }

    // Reminder enable flow (moved from the Task Manager bell): exact alarms
    // need a settings redirect on S+, notifications a runtime grant on 33+.
    // The denial hint re-derives from the live grant on resume: an explicit
    // deny persists across resumes while still denied, and vanishes the
    // moment the grant appears — keying the flag itself on resumeTick would
    // wipe it unconditionally, granted or not.
    var remindersDenied by remember(refreshTick) { mutableStateOf(false) }
    val notifGrantedNow = remember(refreshTick, resumeTick) { hasNotifPermission(appCtx) }
    // A tap that couldn't finish for permissions re-drives on resume: the
    // exact-alarm redirect returns no result, so coming back re-runs the
    // checks instead of stranding the toggle halfway on.
    var pendingReminders by remember(refreshTick) { mutableStateOf(false) }
    // True while the permission dialog is in flight: a resume during the
    // dialog must not relaunch it (double-launch while awaiting a result).
    // Cleared by the callback either way, so the re-drive below only fires
    // when no request is outstanding. Unkeyed on purpose: an import landing
    // mid-dialog must not reset it while the dialog is outstanding, or the
    // next resume double-launches before the first callback clears it.
    var permissionInFlight by remember { mutableStateOf(false) }
    // The settings redirect fires at most once per tap: without this every
    // resume without a grant would throw system settings open again.
    var exactAsked by remember(refreshTick) { mutableStateOf(false) }
    val notifPermission = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestPermission(),
    ) { granted ->
        permissionInFlight = false
        pendingReminders = false
        if (granted) {
            TaskReminders.setEnabled(context, true)
            remindersOn = true
            remindersDenied = false
        } else {
            // Explicit deny: stay off with an explanation. Do NOT re-drive
            // on resume — re-prompting a decided user is the dark pattern
            // the permission model exists to prevent.
            remindersDenied = true
        }
    }

    fun setReminders(on: Boolean) {
        if (!on) {
            TaskReminders.setEnabled(context, false)
            remindersOn = false
            remindersDenied = false
            pendingReminders = false
            exactAsked = false
            return
        }
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            // Same reader as the toggle and the arming path
            // ([canScheduleExactAlarms]): a throwing manager reads as
            // granted, and the arm-time fallback keeps that safe.
            if (!canScheduleExactAlarms(appCtx)) {
                // Best-effort, like the old bell: the first pass fires the
                // redirect and returns so the notification step below never
                // launches underneath system settings; the resume re-drive
                // then enables anyway (inexact fallback) when the grant is
                // still missing — never strand the toggle off for declining
                // exact. `exactAsked` keeps the redirect to once per tap.
                if (!exactAsked) {
                    exactAsked = true
                    pendingReminders = true
                    runCatching {
                        context.startActivity(
                            Intent(Settings.ACTION_REQUEST_SCHEDULE_EXACT_ALARM)
                                // No-op from this Activity today; crash-proof
                                // if the composition context ever isn't one.
                                .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK),
                        )
                    }
                    return
                }
            }
        }
        if (!hasNotifPermission(appCtx)) {
            pendingReminders = true
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU && !permissionInFlight) {
                permissionInFlight = true
                notifPermission.launch(Manifest.permission.POST_NOTIFICATIONS)
            }
            return
        }
        TaskReminders.setEnabled(context, true)
        remindersOn = true
        remindersDenied = false
        pendingReminders = false
    }

    // Re-drive a tap the permission flow interrupted once the user returns.
    // Keyed on resumeTick only, deliberately not on pendingReminders: the
    // flag is set synchronously beside the dialog launch it belongs to (and
    // cleared synchronously by its callback), so keying on it would relaunch
    // the dialog in a loop. Resume is the only other producer of progress.
    // The in-flight guard skips resumes landing while the dialog is still
    // showing, so the dialog is never launched twice for one tap.
    LaunchedEffect(resumeTick) {
        if (pendingReminders && !permissionInFlight) setReminders(true)
    }

    // Exact-alarm grant, re-read on the same resume tick as the switches
    // above: enabling without it falls back to inexact alarms, and the
    // copy must not promise otherwise. Same reader as the enable and
    // arming paths ([canScheduleExactAlarms]) — one source, though a grant
    // flip between this read and the arm can still disagree about that
    // instance's mode.
    val exactAlarms = remember(resumeTick) { canScheduleExactAlarms(appCtx) }

    SectionCard(
        title = "Task refresh",
        subtitle = "How often reminders and snoozed projects re-check.",
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
    }

    SectionCard(
        title = "Task reminders",
        subtitle = "Due-time alarms with a spoken heads-up.",
    ) {
        SwitchRow(
            text = "Due-time reminders",
            sub = reminderToggleSub(exactAlarms),
            checked = remindersOn,
            onChange = ::setReminders,
        )
        if (remindersDenied && !notifGrantedNow) {
            HintLine("Notifications are blocked — reminders stay off until allowed.")
        }
        // Persistent (#436): whenever the grant is missing, on or off — an
        // off toggle that enables inexact must not read exact either.
        if (!exactAlarms) {
            HintLine(
                if (remindersOn) {
                    "Exact alarms aren't allowed — reminders still fire, but may be inexact until allowed in system settings."
                } else {
                    "Exact alarms aren't allowed — if turned on, reminders fire inexact and may be late until allowed in system settings."
                }
            )
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
        title = "App update checks",
        subtitle = "Automatic checks for newer Agento releases. Checking " +
            "never downloads anything — install stays a tap in " +
            "Settings → About → Version.",
    ) {
        SwitchRow(
            text = "Check on startup",
            sub = "Run one check at most once every 6 hours on cold start.",
            checked = updateStartup,
            onChange = {
                BehaviourSettings.setUpdateStartup(context, it)
                updateStartup = it
            },
        )
        OptionMenu(
            label = "Check periodically",
            shown = BehaviourSettings.updateIntervalChoiceLabel(updateHours),
            options = BehaviourSettings.UPDATE_INTERVAL_OPTIONS_HOURS.map {
                it.toString() to BehaviourSettings.updateIntervalChoiceLabel(it)
            },
            onPick = {
                it.toLongOrNull()?.let { h ->
                    BehaviourSettings.setUpdateIntervalHours(context, h)
                    updateHours = BehaviourSettings.updateIntervalHours(context)
                }
            },
        )
        SwitchRow(
            text = "Notify when a release is ready",
            sub = "Post a notification (background only, one per release) " +
                "when a check finds a newer release. Needs notification permission.",
            checked = updateNotify,
            onChange = {
                BehaviourSettings.setUpdateNotify(context, it)
                updateNotify = it
            },
        )
        HintLine(BehaviourSettings.updateCadenceLine(updateHours, updateStartup))
        if (updateHours !in BehaviourSettings.UPDATE_INTERVAL_OPTIONS_HOURS) {
            HintLine("Kept from an import: this cadence isn't offered above, but it still applies.")
        }
    }
}

private fun hasNotifPermission(appCtx: Context): Boolean =
    Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU ||
        ContextCompat.checkSelfPermission(
            appCtx, Manifest.permission.POST_NOTIFICATIONS,
        ) == PackageManager.PERMISSION_GRANTED

/** One labelled switch row, the Notifications section's shape reused. */
@Composable
internal fun SwitchRow(
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
