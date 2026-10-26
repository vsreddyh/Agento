package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertSame
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Widget row order (#369): `Server` keeps the fetch order; every other
 * mode is the Task Manager's own [sortedByMode], so a placement can
 * mirror the in-app list. Titles must stay one-for-one with
 * [ServerTaskSort], or the "same order as the app" promise silently
 * drifts.
 */
class TaskWidgetSortTest {

    private val tasks = listOf(
        ServerTask(id = "b", name = "banana", dueDate = "2026-08-10", dueTime = "09:00",
            estimatedMinutes = 30, createdAt = "2026-08-01T10:00:00"),
        ServerTask(id = "a", name = "Apple", dueDate = "2026-08-09", dueTime = "09:00",
            estimatedMinutes = 0, createdAt = "2026-08-03T10:00:00"),
        ServerTask(id = "c", name = "cherry", dueDate = "2026-08-11", dueTime = "09:00",
            estimatedMinutes = 10, createdAt = "2026-08-02T10:00:00"),
    )

    @Test
    fun `server order keeps the fetch order untouched`() {
        // Identity, not just equality: no copy, no reorder, no drop.
        assertSame(tasks, tasks.sortedForWidget(TaskWidgetSort.Server))
    }

    @Test
    fun `every other mode is the task manager sort`() {
        assertEquals(
            tasks.sortedByMode(ServerTaskSort.Start),
            tasks.sortedForWidget(TaskWidgetSort.Start),
        )
        assertEquals(
            tasks.sortedByMode(ServerTaskSort.Name),
            tasks.sortedForWidget(TaskWidgetSort.Name),
        )
        assertEquals(
            tasks.sortedByMode(ServerTaskSort.Created),
            tasks.sortedForWidget(TaskWidgetSort.Created),
        )
        assertEquals(
            tasks.sortedByMode(ServerTaskSort.Estimate),
            tasks.sortedForWidget(TaskWidgetSort.Estimate),
        )
    }

    @Test
    fun `name order is case-insensitive`() {
        val names = tasks.sortedForWidget(TaskWidgetSort.Name).map { it.id }
        assertEquals(listOf("a", "b", "c"), names)
    }

    @Test
    fun `titles mirror the task manager sort menu`() {
        assertEquals("Start time", TaskWidgetSort.Start.title)
        assertEquals("Name", TaskWidgetSort.Name.title)
        assertEquals("Newest", TaskWidgetSort.Created.title)
        assertEquals("Estimate", TaskWidgetSort.Estimate.title)
        // …which is exactly what the manager shows, pinned by reference
        // so a retitle on either side breaks loudly instead of drifting.
        assertEquals(ServerTaskSort.Start.title, TaskWidgetSort.Start.title)
        assertEquals(ServerTaskSort.Name.title, TaskWidgetSort.Name.title)
        assertEquals(ServerTaskSort.Created.title, TaskWidgetSort.Created.title)
        assertEquals(ServerTaskSort.Estimate.title, TaskWidgetSort.Estimate.title)
    }

    @Test
    fun `sort key round-trips through backups`() {
        assertTrue(SettingsBackup.isWidgetDisplayKey("task_widget_sort_12"))
        assertTrue(SettingsBackup.isStringWidgetKey("task_widget_sort_12"))
    }
}
