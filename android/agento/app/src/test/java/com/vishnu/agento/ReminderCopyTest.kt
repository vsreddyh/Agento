package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Test

/**
 * Reminder notification copy (#287).
 *
 * The collapsed line names only the moment; the times live in the expanded
 * view. Overdue names how long past due ("Overdue for x hr y min", minutes
 * alone when under an hour).
 */
class ReminderCopyTest {

    @Test
    fun `before-start names only the lead time`() {
        assertEquals(
            "Starting in ${TaskReminders.BEFORE_START_MINUTES} minutes",
            reminderMessage(ReminderKind.BeforeStart, "Today, 08:30", "Today, 08:30"),
        )
    }

    @Test
    fun `start names only the moment`() {
        assertEquals(
            "Start now",
            reminderMessage(ReminderKind.Start, "Today, 09:00", "Today, 08:00"),
        )
    }

    @Test
    fun `due names only the moment`() {
        assertEquals(
            "Due now",
            reminderMessage(ReminderKind.Due, "Today, 09:00", null),
        )
    }

    @Test
    fun `overdue under an hour shows minutes alone`() {
        assertEquals("Overdue for 5 min", overdueText(5 * 60_000L))
        assertEquals(
            "Overdue for 5 min",
            reminderMessage(ReminderKind.Overdue, "Today, 09:00", null, 5 * 60_000L),
        )
    }

    @Test
    fun `overdue over an hour shows hours and minutes`() {
        assertEquals("Overdue for 2 hr 5 min", overdueText(125 * 60_000L))
    }

    @Test
    fun `overdue exact hour shows no zero minutes`() {
        assertEquals("Overdue for 2 hr", overdueText(120 * 60_000L))
    }

    @Test
    fun `overdue under a minute still reads one minute`() {
        assertEquals("Overdue for 1 min", overdueText(0L))
    }

    @Test
    fun `non-overdue kinds ignore a passed elapsed duration`() {
        assertEquals(
            "Due now",
            reminderMessage(ReminderKind.Due, "Today, 09:00", null, 90 * 60_000L),
        )
        assertEquals(
            "Start now",
            reminderMessage(ReminderKind.Start, "Today, 09:00", "Today, 08:00", 90 * 60_000L),
        )
    }

    @Test
    fun `overdue just over an hour`() {
        assertEquals("Overdue for 1 hr 1 min", overdueText(61 * 60_000L))
    }

    @Test
    fun `overdue without an elapsed duration falls back`() {
        assertEquals(
            "Overdue since Today, 09:00",
            reminderMessage(ReminderKind.Overdue, "Today, 09:00", null, null),
        )
        assertEquals(
            "Overdue",
            reminderMessage(ReminderKind.Overdue, "", null, null),
        )
    }
}
