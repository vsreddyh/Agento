package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Test

/** Spoken copy per reminder kind (#306). */
class ReminderSpeechTest {

    @Test
    fun `before-start speaks lead time with name`() {
        assertEquals(
            "In 5 minutes you need to start \"pay the power bill\"",
            reminderSpeech(ReminderKind.BeforeStart, "pay the power bill"),
        )
    }

    @Test
    fun `start speaks direct prompt`() {
        assertEquals(
            "You need to start \"pay the power bill\"",
            reminderSpeech(ReminderKind.Start, "pay the power bill"),
        )
    }

    @Test
    fun `due speaks time-up`() {
        assertEquals(
            "Time to \"pay the power bill\" is up",
            reminderSpeech(ReminderKind.Due, "pay the power bill"),
        )
    }

    @Test
    fun `overdue speaks pending duration`() {
        assertEquals(
            "\"pay the power bill\" has been pending for 5 min",
            reminderSpeech(ReminderKind.Overdue, "pay the power bill", 5 * 60_000L),
        )
        assertEquals(
            "\"pay the power bill\" has been pending for 2 hr 5 min",
            reminderSpeech(ReminderKind.Overdue, "pay the power bill", 125 * 60_000L),
        )
    }

    @Test
    fun `overdue without elapsed degrades`() {
        assertEquals(
            "\"pay the power bill\" is overdue",
            reminderSpeech(ReminderKind.Overdue, "pay the power bill", null),
        )
    }

    @Test
    fun `speak name preferred over list name`() {
        assertEquals(
            "You need to start \"pay the power bill\"",
            reminderSpeech(ReminderKind.Start, "Power Bill", "pay the power bill"),
        )
    }

    @Test
    fun `blank speak name falls back to name`() {
        assertEquals(
            "Time to \"Power Bill\" is up",
            reminderSpeech(ReminderKind.Due, "Power Bill", ""),
        )
    }

    @Test
    fun `empty name falls back`() {
        assertEquals(
            "Time to \"Task due\" is up",
            reminderSpeech(ReminderKind.Due, ""),
        )
    }
}
