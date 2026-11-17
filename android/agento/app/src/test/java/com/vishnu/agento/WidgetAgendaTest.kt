package com.vishnu.agento

import java.time.LocalDate
import java.time.LocalDateTime
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Widget agenda contract (#532): the widget's sections, its title rule, the
 * compact threshold and the secondary line. The bucket order and the
 * dueBucket grouping live in the Task Manager's own code, so these pin the
 * half the widget adds — that it groups in that order, counts each group,
 * stays flat in Done, honors "show due dates" without losing the repeat
 * condition, and caps rows without leaving an orphan header behind.
 */
class WidgetAgendaTest {

    private val today = LocalDate.of(2026, 8, 9)
    private val now = LocalDateTime.of(2026, 8, 9, 10, 0)

    private fun task(
        id: String,
        dueDate: String = "",
        dueTime: String = "",
        estimatedMinutes: Int = 0,
        repeatEvery: Int = 0,
        repeatUnit: String = "",
        parallelable: Boolean = false,
        completedAt: String = "",
    ) = ServerTask(
        id = id,
        name = "task $id",
        dueDate = dueDate,
        dueTime = dueTime,
        estimatedMinutes = estimatedMinutes,
        repeatEvery = repeatEvery,
        repeatUnit = repeatUnit,
        parallelable = parallelable,
        completedAt = completedAt,
    )

    @Test
    fun `sections follow the task manager bucket order with counts`() {
        val overdue = task("overdue", dueDate = "2026-08-08", dueTime = "09:00")
        // Due 12:00 with a three-hour estimate starts at 09:00: under way,
        // not yet late, so this is Current.
        val current = task(
            "current", dueDate = "2026-08-09", dueTime = "12:00", estimatedMinutes = 180)
        val later = task("later", dueDate = "2026-09-30", dueTime = "09:00")
        val undated = task("undated")
        val second = task("second", dueDate = "2026-08-08", dueTime = "09:00")

        val agenda = widgetAgenda(
            listOf(overdue, current, later, undated, second),
            grouped = true,
            today = today,
            now = now,
        )

        assertEquals(
            listOf(
                WidgetItem.Header(DueBucket.Overdue, 2),
                WidgetItem.Row(overdue),
                WidgetItem.Row(second),
                WidgetItem.Header(DueBucket.Current, 1),
                WidgetItem.Row(current),
                WidgetItem.Header(DueBucket.Later, 1),
                WidgetItem.Row(later),
                WidgetItem.Header(DueBucket.NoDate, 1),
                WidgetItem.Row(undated),
            ),
            agenda,
        )
    }

    @Test
    fun `done view stays flat`() {
        val a = task("a", completedAt = "2026-08-08T09:00:00")
        val b = task("b", completedAt = "2026-08-07T09:00:00")

        val agenda = widgetAgenda(listOf(a, b), grouped = false, today = today, now = now)

        // No headers and no reordering: a bucket for finished work says
        // nothing, and the placement's sort already chose the order.
        assertEquals(listOf(WidgetItem.Row(a), WidgetItem.Row(b)), agenda)
    }

    @Test
    fun `empty agenda has no header`() {
        assertTrue(widgetAgenda(emptyList(), grouped = true, today = today, now = now).isEmpty())
    }

    @Test
    fun `non-scrolling cap counts rows and keeps their headers`() {
        val agenda = widgetAgenda(
            listOf(
                task("o1", dueDate = "2026-08-08", dueTime = "09:00"),
                task("o2", dueDate = "2026-08-08", dueTime = "09:00"),
                task("t1", dueDate = "2026-08-09", dueTime = "23:00"),
                task("n1"),
            ),
            grouped = true,
            today = today,
            now = now,
        )
        // Overdue header + 2 rows + Later-today header + its row: four
        // shown rows fit the cap, and every header shown still has a row.
        val capped = agenda.takeAgendaRows(3)
        assertEquals(
            listOf(
                WidgetItem.Header(DueBucket.Overdue, 2),
                agenda[1],
                agenda[2],
                WidgetItem.Header(DueBucket.LaterToday, 1),
                agenda[4],
            ),
            capped,
        )
        assertEquals(3, capped.count { it is WidgetItem.Row })
        // A header whose only rows fell outside the cap is dropped, not
        // left dangling over nothing.
        assertTrue(capped.none { it is WidgetItem.Header && it.bucket == DueBucket.NoDate })
    }

    @Test
    fun `a cap of zero shows nothing and a big cap shows everything`() {
        val agenda = widgetAgenda(
            listOf(task("a", dueDate = "2026-08-08", dueTime = "09:00")),
            grouped = true,
            today = today,
            now = now,
        )
        assertTrue(agenda.takeAgendaRows(0).isEmpty())
        assertEquals(agenda, agenda.takeAgendaRows(99))
    }

    @Test
    fun `trailing header without rows is dropped`() {
        val rows = listOf(
            WidgetItem.Header(DueBucket.Overdue, 1),
            WidgetItem.Row(task("a")),
            WidgetItem.Header(DueBucket.NoDate, 0),
        )
        assertEquals(rows.take(2), rows.takeAgendaRows(5))
    }

    @Test
    fun `title trims caps and falls back when blank`() {
        assertEquals("Groceries", widgetTitle("  Groceries  ", "Open"))
        assertEquals("Open", widgetTitle(null, "Open"))
        assertEquals("Open", widgetTitle("", "Open"))
        assertEquals("Open", widgetTitle("   ", "Open"))
        // A long title is cut, not wrapped into the count line.
        val long = "x".repeat(WIDGET_TITLE_MAX + 20)
        assertEquals(long.take(WIDGET_TITLE_MAX), widgetTitle(long, "Open"))
        assertEquals(WIDGET_TITLE_MAX, widgetTitle(long, "Open").length)
    }

    @Test
    fun `compact under the provider minimums`() {
        // Full size is the provider's own minimums: 180dp wide, 110dp
        // tall. Anything smaller goes compact, either dimension.
        assertFalse(widgetIsCompact(COMPACT_MAX_WIDTH_DP, COMPACT_MIN_HEIGHT_DP))
        assertFalse(widgetIsCompact(320, 220))
        // Two cells wide (or one row tall) shrinks.
        assertTrue(widgetIsCompact(110, COMPACT_MIN_HEIGHT_DP))
        assertTrue(widgetIsCompact(COMPACT_MAX_WIDTH_DP - 1, COMPACT_MIN_HEIGHT_DP))
        // A short-but-wide placement has no room for the header line.
        assertTrue(widgetIsCompact(320, COMPACT_MIN_HEIGHT_DP - 1))
        // A launcher that will not say (0) keeps the full list rather than
        // surprising the user with a downgrade.
        assertFalse(widgetIsCompact(0, 0))
        // ...but a known-small width still shrinks even with a missing
        // height.
        assertTrue(widgetIsCompact(110, 0))
    }

    @Test
    fun `widget rows post the notification actions`() {
        // The ring and the skip control (#532) reuse the reminder actions,
        // so one receiver route (and one failure path) serves both. Pure
        // because the intents themselves need a Context.
        assertEquals(
            TaskNotifActionReceiver.ACTION_COMPLETE,
            widgetMutationAction(NotifAction.COMPLETE),
        )
        assertEquals(
            TaskNotifActionReceiver.ACTION_SKIP,
            widgetMutationAction(NotifAction.SKIP),
        )
    }

    @Test
    fun `secondary line ignores due dates but keeps the repeat condition`() {
        val repeating = task(
            "a", dueDate = "2026-08-09", dueTime = "23:00", estimatedMinutes = 60,
            repeatEvery = 3, repeatUnit = "days", parallelable = true)

        val withDue = widgetSecondaryLine(repeating, today, showDue = true)
        assertEquals("Today, 22:00 → 23:00 · Every 3 days · parallel", withDue)

        val withoutDue = widgetSecondaryLine(repeating, today, showDue = false)
        assertEquals("Every 3 days · parallel", withoutDue)
    }

    @Test
    fun `secondary line is empty for a plain one-shot task with due dates off`() {
        assertEquals("", widgetSecondaryLine(task("a", dueDate = "2026-08-09"), today, false))
    }
}
