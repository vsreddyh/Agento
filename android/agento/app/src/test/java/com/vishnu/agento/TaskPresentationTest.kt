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
        days: List<String> = emptyList(),
        custom: Boolean = false,
        rule: String = "",
    ) = ServerTaskDraft(
        id = "", name = "n", description = "d",
        dueDate = "2026-08-08", dueTime = "09:00", estimatedMinutes = "30",
        repeatEvery = every, repeatUnit = unit, repeatDays = days,
        repeatCustom = custom, repeatRule = rule, parallelable = false, revision = 0,
    )

    @Test
    fun `an empty cadence is a valid one-shot`() {
        assertEquals(RepeatDraft(), draft().repeatOrNull())
    }

    @Test
    fun `a structured cadence parses`() {
        assertEquals(RepeatDraft(every = 3, unit = "weeks"), draft(every = "3", unit = "weeks").repeatOrNull())
    }

    @Test
    fun `a custom rule parses and carries its words`() {
        val t = draft(custom = true, rule = "every 3rd friday").repeatOrNull()
        assertEquals(RepeatDraft(custom = true, text = "every 3rd friday"), t)
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
        assertEquals(RepeatDraft(every = 1, unit = "days"), draft(every = "1").repeatOrNull())
        assertEquals(RepeatDraft(every = 28, unit = "days"), draft(every = "28").repeatOrNull())
    }

    @Test
    fun `an unknown unit does not parse`() {
        assertNull(draft(every = "2", unit = "fortnights").repeatOrNull())
        assertFalse(REPEAT_UNITS.contains("fortnights"))
    }

    @Test
    fun `surrounding whitespace is tolerated in the count`() {
        assertEquals(RepeatDraft(every = 2, unit = "days"), draft(every = "  2  ").repeatOrNull())
    }

    // ── selected days (#304) ─────────────────────────────────────────────────

    @Test
    fun `a selection parses canonicalised`() {
        // The order and case the chips produced do not matter: the server
        // canonicalises, so the app must send what the server would store or its
        // round-trip check reports a mismatch on every write.
        val t = draft(days = listOf("fri", "Mon", "mon")).repeatOrNull()
        assertEquals(RepeatDraft(days = listOf("mon", "fri")), t)
    }

    @Test
    fun `a selection beats a leftover count, and never carries both`() {
        // The dialog clears the count when a day is picked, but the draft is the
        // last word: two cadences in one task is what the server refuses, so the
        // app must not be able to send it even if a stale state says otherwise.
        val t = draft(every = "3", unit = "weeks", days = listOf("mon")).repeatOrNull()
        assertEquals(RepeatDraft(days = listOf("mon")), t)
    }

    @Test
    fun `a custom condition beats a leftover selection, and never carries both`() {
        val t = draft(custom = true, rule = "every 3rd friday", days = listOf("mon")).repeatOrNull()
        assertEquals(RepeatDraft(custom = true, text = "every 3rd friday"), t)
    }

    @Test
    fun `a selection with no known day does not parse`() {
        // Unknown values are dropped rather than sent: the chips cannot produce
        // them, so this is a hand-edited response, and sending it back is a 422.
        // Dropping every one of them leaves nothing to repeat, which is not a
        // valid "days" draft — Save stays disabled rather than silently saving a
        // one-shot.
        assertNull(draft(days = listOf("funday")).repeatOrNull())
        assertNull(draft(days = listOf("  ")).repeatOrNull())
    }

    @Test
    fun `the due date must be one of the selected days`() {
        // 2026-08-08 is a Saturday.
        assertTrue(dueDateIsSelectedDay("2026-08-08", listOf("sat")))
        assertTrue(dueDateIsSelectedDay("2026-08-08", listOf("mon", "sat")))
        assertFalse(dueDateIsSelectedDay("2026-08-08", listOf("mon", "wed", "fri")))
        // With no selection the rule does not apply at all — not "fails".
        assertTrue(dueDateIsSelectedDay("2026-08-08", emptyList()))
    }

    @Test
    fun `a date that will not parse fails the selected-days rule rather than throwing`() {
        // A half-picked form (no date yet) is an ordinary state, and Save is
        // already disabled on the blank-date rule; this must not be the thing
        // that throws out of a composable.
        assertFalse(dueDateIsSelectedDay("", listOf("sat")))
        assertFalse(dueDateIsSelectedDay("garbage", listOf("sat")))
        assertFalse(dueDateIsSelectedDay("2026-13-45", listOf("sat")))
    }

    @Test
    fun `the weekday vocabulary matches the server's, Monday first`() {
        // The server sorts and stores a selection in this order (RepeatDays),
        // and the chips offer exactly these values, so the two sides cannot
        // disagree about what "mon" means or where it sorts.
        assertEquals(listOf("mon", "tue", "wed", "thu", "fri", "sat", "sun"), REPEAT_DAYS)
        assertEquals("Mon", dayLabel("mon"))
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

    @Test
    fun `hasRepeat is true for a selected-days cadence`() {
        // The mode is structured, so a completion rolls it over server-side and
        // must NOT open the recreate draft a custom rule needs — but it must
        // still read as repeating, or the row would show no repeat at all.
        assertTrue(ServerTask(id = "1", name = "n", repeatDays = listOf("mon")).hasRepeat)
    }

    // ── repeatLabel ──────────────────────────────────────────────────────────

    @Test
    fun `a selection renders as the days, matching the server`() {
        // The server's Repeat.String for the same selection: "Mon, Wed, Fri".
        assertEquals(
            "Mon, Wed, Fri",
            repeatLabel(0, "", listOf("mon", "wed", "fri"), false, ""),
        )
    }

    @Test
    fun `the other modes still render as they did`() {
        assertEquals("Every 3 days", repeatLabel(3, "days", emptyList(), false, ""))
        assertEquals("Every day", repeatLabel(1, "days", emptyList(), false, ""))
        assertEquals("every 3rd friday", repeatLabel(0, "", emptyList(), true, "every 3rd friday"))
        assertEquals("", repeatLabel(0, "", emptyList(), false, ""))
    }
}
