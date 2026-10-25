package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

/**
 * Widget tap contract (#370): the ring completes inline, a row opens its
 * detail sheet, and everything else opens the Task Manager. The ring/row
 * half rides the collection template (platform-tested, untestable here);
 * this pins the header/background half — every chrome id maps to its
 * destination, and row ids map to nothing so render() can never steal
 * them from the template.
 */
class WidgetTapTest {

    @Test
    fun `background and header open the task manager`() {
        for (id in listOf(
            R.id.task_widget_body,
            R.id.task_widget_title,
            R.id.task_widget_count,
            R.id.task_widget_view,
            R.id.task_widget_empty,
        )) {
            assertEquals(
                "view id $id must open the Task Manager",
                WidgetTapTarget.OPEN_TASKS,
                widgetTapTarget(id),
            )
        }
    }

    @Test
    fun `refresh keeps its own job`() {
        assertEquals(
            WidgetTapTarget.REFRESH,
            widgetTapTarget(R.id.task_widget_refresh),
        )
    }

    @Test
    fun `rows are not header targets`() {
        // Ring, name and due lines, the static container and the list
        // itself: the template/fill-ins or the body fallback own these,
        // and the table must not claim them.
        assertNull(widgetTapTarget(R.id.task_widget_row))
        assertNull(widgetTapTarget(R.id.task_widget_row_check))
        assertNull(widgetTapTarget(R.id.task_widget_row_name))
        assertNull(widgetTapTarget(R.id.task_widget_row_due))
        assertNull(widgetTapTarget(R.id.task_widget_static_list))
        assertNull(widgetTapTarget(R.id.task_widget_list_view))
    }
}
