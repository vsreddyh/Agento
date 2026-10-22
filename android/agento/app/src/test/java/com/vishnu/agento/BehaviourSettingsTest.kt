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
    fun `lead labels read naturally`() {
        assertEquals("At start time (no heads-up)", BehaviourSettings.leadLabel(0L))
        assertEquals("1 minute before start", BehaviourSettings.leadLabel(1L))
        assertEquals("15 minutes before start", BehaviourSettings.leadLabel(15L))
    }

    @Test
    fun `tts pct labels carry the percent suffix`() {
        assertEquals("100%", BehaviourSettings.ttsPctLabel(100L))
        assertEquals("150%", BehaviourSettings.ttsPctLabel(150L))
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
            "task_reminders_enabled",
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
