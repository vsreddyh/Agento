package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import java.time.LocalDate
import java.time.LocalDateTime
import java.time.ZoneId

/**
 * Behaviour knobs (#355): the adjustable lead time and the pure label
 * helpers behind the section. Pref reads need a Context (not under JVM
 * test); the scheduling/copy rules and the labels are pure and pinned here.
 */
class BehaviourSettingsTest {

    private val zone = ZoneId.of("Asia/Kolkata")
    private val today = LocalDate.of(2026, 8, 8)

    private fun at(date: LocalDate, hour: Int, minute: Int = 0): Long =
        LocalDateTime.of(date, java.time.LocalTime.of(hour, minute))
            .atZone(zone).toInstant().toEpochMilli()

    @Test
    fun `custom lead moves the warning`() {
        val now = at(today, 6)
        val pts = reminderPoints(today.toString(), "09:00", 60, now, leadMinutes = 15L)
        // Start is 08:00 for a 60-min estimate on a 09:00 due; the warning
        // leads it by the pick, not by the old constant.
        assertEquals(at(today, 7, 45), pts[ReminderKind.BeforeStart])
        assertEquals(at(today, 8), pts[ReminderKind.Start])
        assertEquals(at(today, 9), pts[ReminderKind.Due])
    }

    @Test
    fun `zero lead collapses the warning into start`() {
        val now = at(today, 6)
        val pts = reminderPoints(today.toString(), "09:00", 60, now, leadMinutes = 0L)
        // The warning lands on the start moment and the MIN_GAP collapse
        // keeps the later one: Start survives, BeforeStart never fires.
        assertFalse(pts.containsKey(ReminderKind.BeforeStart))
        assertEquals(at(today, 8), pts[ReminderKind.Start])
    }

    @Test
    fun `default lead is the old constant`() {
        val now = at(today, 6)
        val a = reminderPoints(today.toString(), "09:00", 60, now)
        val b = reminderPoints(
            today.toString(), "09:00", 60, now,
            leadMinutes = TaskReminders.BEFORE_START_MINUTES,
        )
        assertEquals(a, b)
    }

    @Test
    fun `message names the picked lead`() {
        assertEquals(
            "Starting in 15 minutes",
            reminderMessage(ReminderKind.BeforeStart, "Today, 08:30", "Today, 07:45", null, 15L),
        )
    }

    @Test
    fun `zero lead message says now`() {
        assertEquals(
            "Starting now",
            reminderMessage(ReminderKind.BeforeStart, "Today, 08:00", "Today, 08:00", null, 0L),
        )
    }

    @Test
    fun `speech names the picked lead`() {
        assertEquals(
            "In 15 minutes you need to start \"pay the power bill\"",
            reminderSpeech(ReminderKind.BeforeStart, "pay the power bill", leadMinutes = 15L),
        )
    }

    @Test
    fun `zero lead is total, not singular`() {
        assertEquals("0 minutes", leadCopy(0L))
    }

    @Test
    fun `cadence lines never assert a constant`() {
        assertEquals("Automatic sync is paused.", BehaviourSettings.syncCadenceLine(1L, false, true))
        assertEquals("Automatic sync runs every hour.", BehaviourSettings.syncCadenceLine(1L, false, false))
        assertEquals(
            "Automatic sync runs every 6 hours on unmetered Wi-Fi.",
            BehaviourSettings.syncCadenceLine(6L, true, false),
        )
        // Pure overloads coerce: no caller renders "every 0 hours".
        assertEquals("Automatic sync runs every hour.", BehaviourSettings.syncCadenceLine(0L, false, false))
        assertEquals(
            "Task and widget refresh runs every hour.",
            BehaviourSettings.taskSyncCadenceLine(60L),
        )
        assertEquals(
            "Task and widget refresh runs every 2 hours.",
            BehaviourSettings.taskSyncCadenceLine(120L),
        )
        assertEquals(
            "Task and widget refresh runs every 45 minutes.",
            BehaviourSettings.taskSyncCadenceLine(45L),
        )
        assertEquals(
            "Task and widget refresh runs every 15 minutes.",
            BehaviourSettings.taskSyncCadenceLine(0L),
        )
    }

    @Test
    fun `hub status counts deviations`() {
        assertEquals("Defaults", BehaviourSettings.hubStatus(0))
        assertEquals("1 custom", BehaviourSettings.hubStatus(1))
        assertEquals("3 custom", BehaviourSettings.hubStatus(3))
    }

    @Test
    fun `custom count pins every knob`() {
        fun all(
            syncHours: Long = 1L,
            wifiOnly: Boolean = false,
            paused: Boolean = false,
            taskMinutes: Long = 30L,
            leadMinutes: Long = 5L,
            voice: String = "",
            ratePct: Long = 100L,
            pitchPct: Long = 100L,
            kindFailed: Boolean = true,
            kindInstruct: Boolean = true,
            kindCron: Boolean = true,
            remindersOn: Boolean = false,
            autoRead: Boolean = false,
        ) = BehaviourSettings.customCount(
            syncHours, wifiOnly, paused, taskMinutes, leadMinutes, voice,
            ratePct, pitchPct, kindFailed, kindInstruct, kindCron,
            remindersOn, autoRead,
        )
        assertEquals(0, all())
        assertEquals(1, all(syncHours = 2L))
        assertEquals(1, all(paused = true))
        assertEquals(1, all(leadMinutes = 15L))
        assertEquals(1, all(voice = "en-x"))
        assertEquals(1, all(ratePct = 150L))
        assertEquals(1, all(kindCron = false))
        assertEquals(1, all(remindersOn = true))
        assertEquals(1, all(autoRead = true))
        assertEquals(3, all(syncHours = 2L, kindFailed = false, autoRead = true))
        // wifiOnly is moot while paused: one effective change, not two.
        assertEquals(1, all(wifiOnly = true, paused = true))
        assertEquals(1, all(wifiOnly = true))
        // Pure overload coerces like the getters: out-of-floor inputs read
        // as their defaults, so both overloads agree on effective state.
        // (Coerced-to-floor need not be default: 5min reads as 15, custom.)
        assertEquals(0, all(syncHours = 0L))
        assertEquals(1, all(taskMinutes = 5L))
        assertEquals(1, all(ratePct = 0L))
    }

    @Test
    fun `import coercion matches live floors`() {
        assertEquals(1L, BehaviourSettings.coerceImport(BehaviourSettings.KEY_SYNC_INTERVAL_HOURS, 0L))
        assertEquals(15L, BehaviourSettings.coerceImport(BehaviourSettings.KEY_TASK_SYNC_INTERVAL_MINUTES, 5L))
        assertEquals(0L, BehaviourSettings.coerceImport(BehaviourSettings.KEY_REMINDER_LEAD_MINUTES, 0L))
        assertEquals(50L, BehaviourSettings.coerceImport(BehaviourSettings.KEY_TTS_RATE_PCT, 0L))
        assertEquals(0L, BehaviourSettings.coerceImport("usage_total_chat", -3L))
    }

    @Test
    fun `lead labels read naturally`() {
        assertEquals("At start time (no heads-up)", BehaviourSettings.leadLabel(0L))
        assertEquals("1 minute before start", BehaviourSettings.leadLabel(1L))
        assertEquals("15 minutes before start", BehaviourSettings.leadLabel(15L))
        // Picker and notification agree past the hour.
        assertEquals("1 hour 1 minute before start", BehaviourSettings.leadLabel(61L))
    }

    @Test
    fun `choice labels are pinned`() {
        assertEquals("1 hour", BehaviourSettings.intervalChoiceLabel(1L))
        assertEquals("6 hours", BehaviourSettings.intervalChoiceLabel(6L))
        assertEquals("1 hour", BehaviourSettings.intervalChoiceLabel(0L))
        assertEquals("1 hour", BehaviourSettings.minutesChoiceLabel(60L))
        assertEquals("2 hours", BehaviourSettings.minutesChoiceLabel(120L))
        assertEquals("45 minutes", BehaviourSettings.minutesChoiceLabel(45L))
        assertEquals("15 minutes", BehaviourSettings.minutesChoiceLabel(0L))
    }

    @Test
    fun `out-of-option leads humanize in copy`() {
        assertEquals(
            "Starting in 24 hours",
            reminderMessage(ReminderKind.BeforeStart, "d", "s", null, 1440L),
        )
        assertEquals(
            "In 1 hour 30 minutes you need to start \"x\"",
            reminderSpeech(ReminderKind.BeforeStart, "x", leadMinutes = 90L),
        )
        assertEquals(
            "Starting in 1 hour 1 minute",
            reminderMessage(ReminderKind.BeforeStart, "d", "s", null, 61L),
        )
    }

    @Test
    fun `tts pct labels carry the percent suffix`() {
        val rate = BehaviourSettings.KEY_TTS_RATE_PCT
        val pitch = BehaviourSettings.KEY_TTS_PITCH_PCT
        assertEquals("100%", BehaviourSettings.ttsPctLabelFor(rate, 100L))
        assertEquals("150%", BehaviourSettings.ttsPctLabelFor(rate, 150L))
        assertEquals("50%", BehaviourSettings.ttsPctLabelFor(pitch, 0L))
    }

    @Test
    fun `backup allowlists carry the behaviour keys`() {
        for (k in listOf(
            BehaviourSettings.KEY_TTS_VOICE,
        )) {
            assertTrue("STRING_KEYS lacks $k", SettingsBackup.STRING_KEYS.contains(k))
        }
        for (k in listOf(
            BehaviourSettings.KEY_NOTIFY_FAILED,
            BehaviourSettings.KEY_NOTIFY_INSTRUCT,
            BehaviourSettings.KEY_NOTIFY_CRON,
            BehaviourSettings.KEY_SYNC_WIFI_ONLY,
            BehaviourSettings.KEY_SYNC_PAUSED,
            // Surfaced-but-pre-existing switches: auto-read must survive a
            // restore exactly like the reminders switch it sits beside —
            // dropping either silently is the regression this pins.
            BehaviourSettings.KEY_TTS_AUTO,
            // The pre-existing switch this section surfaces: pinned by
            // reference (PREF_ENABLED is internal for exactly this), so a
            // rename cannot silently drop it from restores.
            TaskReminders.PREF_ENABLED,
        )) {
            assertTrue("BOOLEAN_KEYS lacks $k", SettingsBackup.BOOLEAN_KEYS.contains(k))
        }
        for (k in listOf(
            BehaviourSettings.KEY_SYNC_INTERVAL_HOURS,
            BehaviourSettings.KEY_TASK_SYNC_INTERVAL_MINUTES,
            BehaviourSettings.KEY_REMINDER_LEAD_MINUTES,
            BehaviourSettings.KEY_TTS_RATE_PCT,
            BehaviourSettings.KEY_TTS_PITCH_PCT,
        )) {
            assertTrue("LONG_KEYS lacks $k", SettingsBackup.LONG_KEYS.contains(k))
        }
        // The backup format itself did not move: still version 1.
        assertEquals(1, SettingsBackup.VERSION)
    }
}
