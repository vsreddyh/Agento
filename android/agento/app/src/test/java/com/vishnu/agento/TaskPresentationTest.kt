package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import java.time.LocalDate

/**
 * The two rules the UI trusts to describe a task correctly: how a due date reads
 * to a human, and when a repeat is safe to send.
 *
 * [ServerTaskDraft.repeatOrNull] mirrors the server's validation. When the two
 * drift, the app enables Save and the server rejects the write — the user gets a
 * failure on a form that looked valid. These bounds are the contract.
 */
class TaskPresentationTest {

    private val today = LocalDate.of(2026, 8, 8)

    // ── friendlyDue ──────────────────────────────────────────────────────────

    @Test
    fun `relative days read as words`() {
        // 2026-08-08 is a Saturday, so +2 days is Monday. The weekday is also
        // asserted against the real calendar, so a wrong expectation is
        // distinguishable from a wrong test fixture.
        assertEquals("Today, 09:00", friendlyDue(today.toString(), "09:00", today))
        assertEquals("Tomorrow, 09:00", friendlyDue(today.plusDays(1).toString(), "09:00", today))
        assertEquals("Monday, 09:00", friendlyDue(today.plusDays(2).toString(), "09:00", today))
        assertEquals("Monday", today.plusDays(2).dayOfWeek
            .getDisplayName(java.time.format.TextStyle.FULL, java.util.Locale.ENGLISH))
    }

    @Test
    fun `beyond a week it falls back to a short date`() {
        assertEquals("20 Aug, 09:00", friendlyDue(today.plusDays(12).toString(), "09:00", today))
    }

    /**
     * The week boundary is INCLUSIVE: exactly +7 days still reads as a weekday
     * name, and +8 is the first day that falls back to a date. Both sides are
     * asserted, because a change from `<=` to `<` moves that boundary by a day
     * and only one of these two lines would fail — the symptom would be a task
     * that quietly stopped naming the day exactly a week out.
     *
     * 2026-08-08 is a Saturday, so +7 is Saturday and +8 is Sunday.
     */
    @Test
    fun `the weekday window ends inclusively at seven days`() {
        assertEquals("Saturday, 09:00", friendlyDue(today.plusDays(7).toString(), "09:00", today))
        assertEquals("16 Aug, 09:00", friendlyDue(today.plusDays(8).toString(), "09:00", today))
    }

    /**
     * An overdue task reads as an ordinary short date — there is no "Overdue"
     * marker, because the `when` has no branch for it and `due.isAfter(today)`
     * excludes it from the weekday window.
     *
     * Pinned deliberately rather than as an endorsement. A task whose due date has
     * passed is the case where a human most wants to be told so, and the current
     * rendering says nothing; if that is a bug rather than a decision, this
     * assertion is the thing to change, and having it written down makes the
     * change a one-line diff instead of a redesign. A past date is also the branch
     * most likely to be broken by a future edit to the weekday window, since it is
     * the only input that reaches `short` while being in the past.
     */
    @Test
    fun `an overdue task reads as a plain short date, not a weekday`() {
        assertEquals("7 Aug, 09:00", friendlyDue(today.minusDays(1).toString(), "09:00", today))
        assertEquals("1 Aug, 09:00", friendlyDue(today.minusDays(7).toString(), "09:00", today))
    }

    @Test
    fun `a date in another year keeps the year`() {
        assertEquals("8 Aug 2027, 09:00", friendlyDue("2027-08-08", "09:00", today))
    }

    @Test
    fun `a missing time leaves just the day`() {
        assertEquals("Today", friendlyDue(today.toString(), "", today))
        assertEquals("Today", friendlyDue(today.toString(), "   ", today))
    }

    @Test
    fun `an unparseable date reads as nothing rather than throwing`() {
        assertEquals("", friendlyDue("", "09:00", today))
        assertEquals("", friendlyDue("garbage", "09:00", today))
        assertEquals("", friendlyDue("2026-13-45", "09:00", today))
    }

    // ── repeatOrNull ─────────────────────────────────────────────────────────

    private fun draft(
        every: String = "",
        unit: String = "days",
        custom: Boolean = false,
        rule: String = "",
    ) = ServerTaskDraft(
        id = "", name = "n", description = "d",
        dueDate = "2026-08-08", dueTime = "09:00", estimatedMinutes = "30",
        repeatEvery = every, repeatUnit = unit, repeatCustom = custom,
        repeatRule = rule, parallelable = false, revision = 0,
    )

    @Test
    fun `an empty cadence is a valid one-shot`() {
        assertEquals(Triple(0, "", ""), draft().repeatOrNull())
    }

    @Test
    fun `a structured cadence parses`() {
        assertEquals(Triple(3, "weeks", ""), draft(every = "3", unit = "weeks").repeatOrNull())
    }

    @Test
    fun `a custom rule parses and carries its words`() {
        val t = draft(custom = true, rule = "every 3rd friday").repeatOrNull()
        assertEquals(Triple(0, "", "every 3rd friday"), t)
    }

    @Test
    fun `a custom rule with no words does not parse`() {
        // Save must stay disabled: the server rejects an empty repeat_rule.
        assertNull(draft(custom = true, rule = "   ").repeatOrNull())
    }

    @Test
    fun `a half-typed number does not parse`() {
        assertNull(draft(every = "3x").repeatOrNull())
        assertNull(draft(every = "-1").repeatOrNull())
        assertNull(draft(every = "1.5").repeatOrNull())
    }

    @Test
    fun `the count bounds match the server`() {
        assertNull(draft(every = "0").repeatOrNull())
        assertNull(draft(every = "29").repeatOrNull())
        assertEquals(Triple(1, "days", ""), draft(every = "1").repeatOrNull())
        assertEquals(Triple(28, "days", ""), draft(every = "28").repeatOrNull())
    }

    @Test
    fun `an unknown unit does not parse`() {
        assertNull(draft(every = "2", unit = "fortnights").repeatOrNull())
        assertFalse(REPEAT_UNITS.contains("fortnights"))
    }

    @Test
    fun `surrounding whitespace is tolerated in the count`() {
        assertEquals(Triple(2, "days", ""), draft(every = "  2  ").repeatOrNull())
    }

    // ── hasRepeat ────────────────────────────────────────────────────────────

    @Test
    fun `hasRepeat is false for a one-shot in any spelling`() {
        assertFalse(ServerTask(id = "1", name = "n").hasRepeat)
        assertFalse(ServerTask(id = "1", name = "n", repeatEvery = 0, repeatRule = "").hasRepeat)
    }

    @Test
    fun `hasRepeat is true for a cadence or a custom rule`() {
        assertTrue(ServerTask(id = "1", name = "n", repeatEvery = 2).hasRepeat)
        assertTrue(ServerTask(id = "1", name = "n", repeatRule = "monthly").hasRepeat)
        assertTrue(ServerTask(id = "1", name = "n", repeatCustom = true).hasRepeat)
    }
}
