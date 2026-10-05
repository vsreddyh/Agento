package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import java.time.LocalDate
import java.time.LocalDateTime
import java.time.ZoneId

/**
 * The reminder ladder: which alerts a task fires, and — the part that matters —
 * which ones it must NOT.
 *
 * Every case here corresponds to a defect that reached review at least once:
 * the zero-estimate "Start now" at the due moment, the collapse that has to drop
 * the EARLIER reminder because the deadline is the one that must not be missed,
 * the gap measured in minutes rather than millis, and the `last` bookkeeping that
 * drops the first reminder of every task when it uses a sentinel.
 */
class ReminderPointsTest {

    private val zone = ZoneId.of("Asia/Kolkata")
    private val today = LocalDate.of(2026, 8, 8)

    private fun at(date: LocalDate, hour: Int, minute: Int = 0): Long =
        LocalDateTime.of(date, java.time.LocalTime.of(hour, minute))
            .atZone(zone).toInstant().toEpochMilli()

    private val nineAm = at(today, 9)

    @Test
    fun `an overdue task fires only the overdue reminder`() {
        val now = at(today, 11)
        val pts = reminderPoints(today.toString(), "09:00", 60, now)
        assertEquals(setOf(ReminderKind.Overdue), pts.keys)
        // At the DUE moment, not at `now` — the notification says how late it is.
        assertEquals(nineAm, pts[ReminderKind.Overdue])
    }

    @Test
    fun `a task due exactly now is overdue, not upcoming`() {
        val pts = reminderPoints(today.toString(), "09:00", 60, nineAm)
        assertEquals(setOf(ReminderKind.Overdue), pts.keys)
    }

    @Test
    fun `a zero-estimate task gets the due alert alone`() {
        // The bug this guards: with no start of its own, "Start now" would fire
        // AT the due moment saying "Start now" — one alert too many, for a moment
        // that is already the deadline.
        val now = at(today, 6)
        val pts = reminderPoints(today.toString(), "09:00", 0, now)
        assertEquals(setOf(ReminderKind.Due), pts.keys)
        assertEquals(nineAm, pts[ReminderKind.Due])
    }

    @Test
    fun `an estimated task gets all three points`() {
        val now = at(today, 6)
        val pts = reminderPoints(today.toString(), "09:00", 60, now)
        assertEquals(
            setOf(ReminderKind.BeforeStart, ReminderKind.Start, ReminderKind.Due),
            pts.keys,
        )
        assertEquals(nineAm - 3_600_000L, pts[ReminderKind.Start])
        assertEquals(
            nineAm - 3_600_000L - TaskReminders.BEFORE_START_MINUTES * 60_000L,
            pts[ReminderKind.BeforeStart],
        )
    }

    @Test
    fun `a point landing exactly on now is dropped`() {
        // The boundary: `now` IS the start moment, and addIfFuture keeps a point only
        // when it is STRICTLY in the future. So Start is dropped and only Due survives.
        // This is the case my first version got wrong twice — it asserted Start was
        // present AND compared it to the due time.
        val now = at(today, 7) // 2h before a 09:00 due with a 2h estimate
        val pts = reminderPoints(today.toString(), "09:00", 120, now)
        assertEquals(setOf(ReminderKind.Due), pts.keys)
        assertEquals(nineAm, pts[ReminderKind.Due])
    }

    @Test
    fun `points already in the past are dropped`() {
        // One hour out with a two-hour estimate: the start moment is 08:00, already
        // past, so neither Start nor the 5-minute warning may be armed retroactively.
        val now = at(today, 8)
        val pts = reminderPoints(today.toString(), "09:00", 120, now)
        assertEquals(setOf(ReminderKind.Due), pts.keys)
        assertFalse(pts.containsKey(ReminderKind.Start))
        assertFalse(pts.containsKey(ReminderKind.BeforeStart))
    }

    @Test
    fun `a one-minute estimate collapses start into due, keeping due`() {
        // Start and Due are one minute apart, under MIN_GAP_MINUTES. Exactly one
        // collapse may fire, and the LATER reminder has to be the survivor: losing
        // "Due now" leaves a task that has just come due silent.
        val now = at(today, 8, 58)
        val pts = reminderPoints(today.toString(), "09:00", 1, now)
        assertTrue(pts.containsKey(ReminderKind.Due))
        assertFalse(pts.containsKey(ReminderKind.Start))
    }

    @Test
    fun `a task whose only point is due keeps it`() {
        // The regression guard for the nullable-last bookkeeping: a Long sentinel
        // made `at - Long.MIN_VALUE` overflow negative and drop this reminder.
        val now = at(today, 8, 59)
        val pts = reminderPoints(today.toString(), "09:00", 0, now)
        assertEquals(setOf(ReminderKind.Due), pts.keys)
    }

    @Test
    fun `a task with no usable due time has no reminders`() {
        assertTrue(reminderPoints("", "09:00", 60, nineAm).isEmpty())
        assertTrue(reminderPoints("nonsense", "09:00", 60, nineAm).isEmpty())
        assertTrue(reminderPoints(today.toString(), "9am", 60, nineAm).isEmpty())
        assertTrue(reminderPoints(today.toString(), "", 60, nineAm).isEmpty())
    }

    @Test
    fun `an out-of-range time is rejected rather than wrapped`() {
        // 25:00 must not silently become 01:00 tomorrow.
        assertTrue(reminderPoints(today.toString(), "25:00", 60, nineAm).isEmpty())
        assertTrue(reminderPoints(today.toString(), "09:99", 60, nineAm).isEmpty())
    }

    @Test
    fun `reminders land in the order the kinds are declared`() {
        val now = at(today, 5)
        val pts = reminderPoints(today.toString(), "12:00", 60, now)
        assertEquals(
            listOf(ReminderKind.BeforeStart, ReminderKind.Start, ReminderKind.Due),
            pts.keys.toList(),
        )
        // And their timestamps ascend, which is what the alarm budget assumes.
        assertEquals(pts.values.sorted(), pts.values.toList())
    }

    @Test
    fun `every reminder point is in the future for a future task`() {
        val now = at(today, 5)
        val pts = reminderPoints(today.toString(), "12:00", 60, now)
        assertTrue(pts.values.all { it > now })
    }

    @Test
    fun `an isodate check rejects what a lenient parse would accept`() {
        assertTrue("2026-08-08".isIsoDate())
        assertFalse("2026-8-8".isIsoDate())
        assertFalse("".isIsoDate())
        assertFalse("2026-02-30".isIsoDate()) // parses as text, not as a real day
    }
}
