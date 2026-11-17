package com.vishnu.agento

import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Issue #430: background readers (widget, reminders)
 * never see drop counts — their only trace is the parse `Log.w`. Those
 * lines must survive the diagnostics [TaskWidget.widgetLine] filter, or
 * the "logged to diagnostics" claim is silently false.
 */
class WidgetLogFilterTest {

    @Test
    fun `parse drop logs survive the diagnostics filter`() {
        assertTrue(TaskWidget.widgetLine("W/TasksApi: dropped 2 unparseable task row(s) in list response"))
        assertTrue(TaskWidget.widgetLine("W/TaskSyncWorker: task surface refresh failed"))
        assertTrue(TaskWidget.widgetLine("W/SyncRunner: sync failed"))
        // Inline widget complete/skip (#532) shares the reminder receiver,
        // so its failure line logs under TaskNotif and must survive too.
        assertTrue(TaskWidget.widgetLine("W/TaskNotif: action SKIP for 68b0 failed; row/alert kept"))
    }

    @Test
    fun `unrelated lines still filtered out`() {
        assertFalse(TaskWidget.widgetLine("W/SomeOtherApp: widget widget widget"))
        assertTrue(TaskWidget.widgetLine("W/TaskWidget: updateAppWidget failed"))
    }
}
