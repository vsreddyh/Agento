package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import java.time.LocalDate

/**
 * The start-time rule: one definition, two consumers with different answers
 * (#164). [startMomentMillis] is the rule; [ServerTask.startMillisOrNull] falls
 * back to the deadline when the task has no start of its own; the reminder
 * engine must NOT, or it arms a "Start now" alert at the due moment.
 *
 * These are the cases that were bugs in review and are therefore the ones worth
 * a permanent test: the zero-estimate fallback, and the two callers disagreeing.
 */
class StartTimeRuleTest {

    private val today = LocalDate.of(2026, 8, 8)
    private val epochDay = today.toEpochDay() * 86_400_000L
    private val nineAm = epochDay + 9 * 3_600_000L

    @Test
    fun `estimate moves the start earlier`() {
        assertEquals(nineAm - 60 * 60_000L, startMomentMillis(nineAm, 60))
    }

    @Test
    fun `zero estimate has no start of its own`() {
        // Not `nineAm`, not zero, and specifically NOT null-by-accident: this
        // null is what stops "Start now" firing at the due moment.
        assertNull(startMomentMillis(nineAm, 0))
    }

    @Test
    fun `negative estimate is treated as no start`() {
        assertNull(startMomentMillis(nineAm, -30))
    }

    @Test
    fun `a task with no estimate starts when it is due`() {
        val t = task(dueDate = today.toString(), dueTime = "09:00", estimatedMinutes = 0)
        assertEquals(nineAm, t.startMillisOrNull())
    }

    @Test
    fun `a task with an estimate starts before it is due`() {
        val t = task(dueDate = today.toString(), dueTime = "09:00", estimatedMinutes = 60)
        assertEquals(nineAm - 3_600_000L, t.startMillisOrNull())
    }

    @Test
    fun `no usable due time means no start`() {
        assertNull(task(dueDate = "", dueTime = "").startMillisOrNull())
        assertNull(task(dueDate = "not-a-date", dueTime = "09:00").startMillisOrNull())
    }

    @Test
    fun `startToDueLine shows start and deadline`() {
        val t = task(dueDate = today.toString(), dueTime = "09:00", estimatedMinutes = 120)
        assertEquals("Today, 07:00 → 09:00", t.startToDueLine(today))
    }

    @Test
    fun `a zero-estimate task does not print a start`() {
        // Start == due, so an arrow would read "Today, 09:00 → 09:00", which
        // claims a window that does not exist.
        val t = task(dueDate = today.toString(), dueTime = "09:00", estimatedMinutes = 0)
        assertEquals("Today, 09:00", t.startToDueLine(today))
    }

    @Test
    fun `a task crossing midnight is anchored on its start day`() {
        // Due 00:30 tomorrow with a 60m estimate starts 23:30 today. The line must
        // not restate the deadline's own day, or it contradicts the group header
        // the row sits under.
        val tomorrow = today.plusDays(1).toString()
        val t = task(dueDate = tomorrow, dueTime = "00:30", estimatedMinutes = 60)
        assertEquals("Today, 23:30 → 00:30", t.startToDueLine(today))
    }

    @Test
    fun `both sides of the arrow are zero padded`() {
        val t = task(dueDate = today.toString(), dueTime = "09:05", estimatedMinutes = 95)
        assertEquals("Today, 07:30 → 09:05", t.startToDueLine(today))
    }

    @Test
    fun `an unparseable task falls back to the plain due line`() {
        val t = task(dueDate = "nope", dueTime = "09:00")
        assertEquals("", t.startToDueLine(today))
    }

    private fun task(dueDate: String, dueTime: String, estimatedMinutes: Int = 0) =
        ServerTask(id = "t1", name = "n", dueDate = dueDate, dueTime = dueTime,
            estimatedMinutes = estimatedMinutes)

    @Test
    fun `due millis is null for an invalid date rather than epoch`() {
        // The dangerous failure is silently returning 0, which is 1970 and sorts
        // every broken task to the top of the list as maximally overdue.
        assertNull(dueMillisOrNull("garbage", "09:00"))
        assertNull(dueMillisOrNull("2026-13-45", "09:00"))
        assertNull(dueMillisOrNull("", ""))
        assertTrue(dueMillisOrNull(today.toString(), "09:00")!! > 0)
    }

    @Test
    fun `a bare hour is accepted and padded`() {
        val a = dueMillisOrNull(today.toString(), "9:00")
        val b = dueMillisOrNull(today.toString(), "09:00")
        assertEquals(b, a)
    }
}