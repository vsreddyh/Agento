package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import java.time.Instant
import java.time.LocalDate
import java.time.LocalTime

/**
 * Project snooze value rules, pinned without Android: an instant needs BOTH
 * a date and a time, the default is 6 PM tomorrow IST, and anything within
 * the server's one-minute lead is rejected before it can be saved.
 */
class ProjectSnoozeTest {

    @Test
    fun `date and time together make the instant, IST pinned`() {
        val iso = ProjectSnooze.iso("2026-11-19", "18:00")
        assertNotNull(iso)
        assertEquals("2026-11-19T12:30:00Z", iso)
        assertEquals("19 Nov, 6:00 PM", ProjectSnooze.label(iso!!))
    }

    @Test
    fun `a bare date or a bare time is not a snooze`() {
        assertNull(ProjectSnooze.iso("2026-11-19", ""))
        assertNull(ProjectSnooze.iso("", "18:00"))
        assertNull(ProjectSnooze.iso("not-a-date", "18:00"))
        assertNull(ProjectSnooze.iso("2026-11-19", "25:00"))
    }

    @Test
    fun `unusable stored values never crash the row`() {
        assertEquals("", ProjectSnooze.label(""))
        assertEquals("", ProjectSnooze.label("nonsense"))
        assertNull(ProjectSnooze.localDateTime(""))
        // Fallbacks seed the pickers: tomorrow, 6 PM.
        assertEquals(LocalDate.now(IST).plusDays(1).toString(), ProjectSnooze.datePart(""))
        assertEquals("18:00", ProjectSnooze.timePart(""))
    }

    @Test
    fun `the default is tomorrow at six in the evening`() {
        val expected = LocalDate.now(IST).plusDays(1).atTime(18, 0)
            .atZone(IST).toInstant().toString()
        assertEquals(
            expected,
            ProjectSnooze.iso(
                ProjectSnooze.datePart(""),
                ProjectSnooze.timePart(""),
            ),
        )
        assertEquals(expected, ProjectSnooze.defaultSnooze())
        val default = Instant.parse(ProjectSnooze.defaultSnooze()).atZone(IST).toLocalDateTime()
        assertEquals(LocalTime.of(18, 0), default.toLocalTime())
        assertEquals(LocalDate.now(IST).plusDays(1), default.toLocalDate())
    }

    @Test
    fun `the server lead is enforced before saving`() {
        val now = 1_800_000_000_000L
        // 30 seconds ahead: the ticker would flip it before the save landed.
        assertFalse(ProjectSnooze.isFuture(Instant.ofEpochMilli(now + 30_000).toString(), now))
        // Exactly a minute is still not enough; a minute and a second is.
        assertFalse(ProjectSnooze.isFuture(Instant.ofEpochMilli(now + 60_000).toString(), now))
        assertTrue(ProjectSnooze.isFuture(Instant.ofEpochMilli(now + 61_000).toString(), now))
        assertFalse(ProjectSnooze.isFuture("nonsense", now))
    }

    @Test
    fun `missing ids age out on a short page but survive a full one`() {
        val armed = mapOf("gone" to "2026-11-19T12:30:00Z")
        // Short page: the whole board fit, so "gone" is deleted, not hidden.
        val (firedShort, stillShort) = ProjectSnooze.splitArmed(armed, emptyList())
        assertTrue(firedShort.isEmpty())
        assertTrue(stillShort.isEmpty())
        // Full page: possibly truncated, so "gone" stays armed.
        val full = List(ProjectSnooze.LIST_LIMIT) { i ->
            ServerProject(
                id = "p$i", name = "p$i", status = "Paused",
                snoozeAt = "2026-11-20T12:30:00Z",
            )
        }
        val (firedFull, stillFull) = ProjectSnooze.splitArmed(armed, full)
        assertTrue(firedFull.isEmpty())
        assertEquals("2026-11-19T12:30:00Z", stillFull["gone"])
        // ...but a real wake on a full page still notifies and clears.
        val woken = ServerProject(
            id = "w", name = "W", status = "Ongoing",
            snoozeAt = "", snoozeFiredAt = "2026-11-19T12:30:00Z",
        )
        val (fired, still) = ProjectSnooze.splitArmed(
            mapOf("w" to "2026-11-19T12:30:00Z"), full + woken,
        )
        assertEquals(listOf(woken), fired)
        assertFalse(still.containsKey("w"))
    }
}
