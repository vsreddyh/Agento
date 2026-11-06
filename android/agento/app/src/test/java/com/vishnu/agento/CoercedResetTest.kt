package com.vishnu.agento

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Issue #437: any reset of a stored value to a default must notify — what
 * key held what raw value and what it became. Defaults themselves are
 * fine; silent rewriting is not.
 *
 * These tests pin the pure layer: the lenient int read, the filter/sort
 * restore helpers, the widget display-setting helpers, and the list-page
 * coercion count. The notify channels themselves (toast, Log.w,
 * diagnostics) are covered by the call sites' existing tests.
 */
class OptTaskIntTest {

    private fun obj(value: Any?): JSONObject {
        val o = JSONObject()
        if (value != null) o.put("k", value)
        return o
    }

    @Test
    fun `numbers read as their int value with no coercion`() {
        assertEquals(CoercedInt(5, null), optTaskInt(obj(5), "k"))
        assertEquals(CoercedInt(5, null), optTaskInt(obj(5L), "k"))
        assertEquals(CoercedInt(5, null), optTaskInt(obj(5.9), "k"))
    }

    @Test
    fun `numeric strings read as their number with no coercion`() {
        assertEquals(CoercedInt(90, null), optTaskInt(obj("90"), "k"))
        assertEquals(CoercedInt(7, null), optTaskInt(obj("  7  "), "k"))
        // Double-encoded numbers get the same truncation Numbers do.
        assertEquals(CoercedInt(1, null), optTaskInt(obj("1.0"), "k"))
    }

    @Test
    fun `absent and null read the default with no coercion`() {
        assertEquals(CoercedInt(0, null), optTaskInt(JSONObject(), "k"))
        assertEquals(CoercedInt(3, null), optTaskInt(JSONObject(), "k", 3))
        assertEquals(CoercedInt(0, null), optTaskInt(obj(JSONObject.NULL), "k"))
    }

    @Test
    fun `non-numeric strings reset to the default and name the raw value`() {
        val r = optTaskInt(obj("abc"), "k")
        assertEquals(0, r.value)
        assertEquals("\"abc\"", r.raw)
    }

    @Test
    fun `wrong types reset to the default and name the raw value`() {
        assertEquals(0, optTaskInt(obj(true), "k").value)
        assertTrue(optTaskInt(obj(true), "k").raw!!.isNotEmpty())
        assertEquals(9, optTaskInt(obj(false), "k", 9).value)
    }
}

class TaskNumericCoercionTest {

    @Test
    fun `a mistyped estimate resets to zero and counts the row once`() {
        val page = parseTaskListPage(
            """{"tasks":[{"id":"a","name":"A","estimated_minutes":"abc","repeat_every":"lots","revision":"bogus"}]}""",
        )
        assertEquals(1, page.tasks.size)
        assertEquals(0, page.tasks.single().estimatedMinutes)
        assertEquals(0, page.tasks.single().repeatEvery)
        assertEquals(0, page.tasks.single().revision)
        // One row counted once, however many of its fields reset.
        assertEquals(1, page.coerced)
        assertEquals(0, page.dropped)
    }

    @Test
    fun `clean rows count no coercion`() {
        val page = parseTaskListPage(
            """{"tasks":[{"id":"a","name":"A","estimated_minutes":90,"repeat_every":"2","revision":4}]}""",
        )
        assertEquals(90, page.tasks.single().estimatedMinutes)
        assertEquals(2, page.tasks.single().repeatEvery)
        assertEquals(4, page.tasks.single().revision)
        assertEquals(0, page.coerced)
    }

    @Test
    fun `missing numerics are defaults, not coercions`() {
        val page = parseTaskListPage("""{"tasks":[{"id":"a","name":"A"}]}""")
        assertEquals(0, page.coerced)
    }

    @Test
    fun `a single-row parse reports through its own log line`() {
        // coerced == null: the single-row paths (get/create/update) log
        // directly — one row notifies once by construction. This just pins
        // that the value still resets rather than throwing.
        val task = parseTask(JSONObject("""{"id":"a","name":"A","estimated_minutes":"abc"}"""))
        assertEquals(0, task!!.estimatedMinutes)
    }

    @Test
    fun `coercion notes name the key raw and reset values`() {
        val notes = mutableListOf<String>()
        parseTask(
            JSONObject("""{"id":"abc123","name":"A","estimated_minutes":"abc"}"""),
            notes,
        )
        assertEquals(1, notes.size)
        assertTrue("note must name key, raw and became: $notes", notes.single().contains("estimated_minutes"))
        assertTrue("note must name key, raw and became: $notes", notes.single().contains("\"abc\""))
        assertTrue("note must name key, raw and became: $notes", notes.single().contains("->0"))
    }
}

class FilterSortCoercionTest {

    @Test
    fun `known filter and sort names parse with no message`() {
        assertEquals(ServerTaskFilter.Done, filterForName("Done"))
        assertNull(filterCoercionMessage("Done"))
        assertEquals(ServerTaskSort.Name, sortForName("Name"))
        assertNull(sortCoercionMessage("Name"))
    }

    @Test
    fun `unknown filter resets to Open and names raw and became`() {
        assertEquals(ServerTaskFilter.Open, filterForName("Archived"))
        val msg = filterCoercionMessage("Archived")!!
        assertTrue(msg.contains("Archived"))
        assertTrue(msg.contains("Open"))
    }

    @Test
    fun `unknown sort resets to Start and names raw and became`() {
        assertEquals(ServerTaskSort.Start, sortForName("Priority"))
        val msg = sortCoercionMessage("Priority")!!
        assertTrue(msg.contains("Priority"))
        assertTrue(msg.contains("Start"))
    }
}

class WidgetCoercionTest {

    @Test
    fun `known widget names parse with no coercion`() {
        assertEquals(TaskWidgetView.Done, widgetViewForName("Done"))
        assertNull(widgetViewCoercion("Done"))
        assertEquals(TaskWidgetDensity.Compact, widgetDensityForName("Compact"))
        assertNull(widgetDensityCoercion("Compact"))
        assertEquals(TaskWidgetSort.Name, widgetSortForName("Name"))
        assertNull(widgetSortCoercion("Name"))
    }

    @Test
    fun `absent widget prefs are defaults with no coercion`() {
        assertEquals(TaskWidgetView.Open, widgetViewForName(null))
        assertNull(widgetViewCoercion(null))
        assertEquals(TaskWidgetDensity.Comfortable, widgetDensityForName(null))
        assertNull(widgetDensityCoercion(null))
        assertEquals(TaskWidgetSort.Server, widgetSortForName(null))
        assertNull(widgetSortCoercion(null))
    }

    @Test
    fun `unknown widget names reset and name raw and became`() {
        assertEquals(TaskWidgetView.Open, widgetViewForName("Snoozed"))
        assertEquals("\"Snoozed\"->Open", widgetViewCoercion("Snoozed"))
        assertEquals(TaskWidgetDensity.Comfortable, widgetDensityForName("Roomy"))
        assertEquals("\"Roomy\"->Comfortable", widgetDensityCoercion("Roomy"))
        assertEquals(TaskWidgetSort.Server, widgetSortForName("Priority"))
        assertEquals("\"Priority\"->Server", widgetSortCoercion("Priority"))
    }
}

class TaskCoercedMessageTest {

    @Test
    fun `singular and plural read as sentences`() {
        assertEquals(
            "1 task value was reset to its default — see Settings → About diagnostics for what changed.",
            taskCoercedMessage(1),
        )
        assertEquals(
            "3 task values were reset to their defaults — see Settings → About diagnostics for what changed.",
            taskCoercedMessage(3),
        )
    }
}
