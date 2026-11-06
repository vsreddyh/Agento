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

    @Test
    fun `non-finite doubles reset and report instead of reading clean`() {
        // toDoubleOrNull accepts these, but no task can hold them: NaN
        // reads as a clean 0 and infinities as clean extremes without this
        // guard (#480 review).
        assertEquals(CoercedInt(0, "\"NaN\""), optTaskInt(obj("NaN"), "k"))
        assertEquals(0, optTaskInt(obj("NaN"), "k").value)
        assertTrue(optTaskInt(obj("Infinity"), "k").raw!!.contains("Infinity"))
        assertTrue(optTaskInt(obj("-Infinity"), "k").raw!!.contains("Infinity"))
    }

    @Test
    fun `out-of-range numbers reset and report instead of wrapping`() {
        // A bare toInt() wraps huge Longs negative and saturates huge
        // Doubles — both silent rewrites to values that are neither the
        // raw nor the default (#480 review).
        val bigString = optTaskInt(obj("9999999999"), "k")
        assertEquals(0, bigString.value)
        assertTrue(bigString.raw!!.contains("9999999999"))
        val bigLong = optTaskInt(obj(5000000000L), "k")
        assertEquals(0, bigLong.value)
        assertTrue(bigLong.raw!!.contains("5000000000"))
        val bigDouble = optTaskInt(obj(1e100), "k")
        assertEquals(0, bigDouble.value)
        assertTrue(bigDouble.raw!!.isNotEmpty())
        // Boundary values still read clean.
        assertEquals(CoercedInt(Int.MAX_VALUE, null), optTaskInt(obj(Int.MAX_VALUE), "k"))
        assertEquals(CoercedInt(Int.MIN_VALUE, null), optTaskInt(obj(Int.MIN_VALUE), "k"))
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
    fun `a negative estimate clamps to zero and counts as coerced`() {
        // Well-typed but still a reset: the clamp rewrites -5 to 0, so it
        // notifies like any other reset rather than passing as a default.
        val page = parseTaskListPage(
            """{"tasks":[{"id":"a","name":"A","estimated_minutes":-5}]}""",
        )
        assertEquals(0, page.tasks.single().estimatedMinutes)
        assertEquals(1, page.coerced)
    }

    @Test
    fun `page notes ride the page and the per-fetch record`() {
        val page = parseTaskListPage(
            """{"tasks":[{"id":"a","name":"A","estimated_minutes":"abc"}]}""",
            "open",
            "manager",
        )
        assertEquals(1, page.coerced)
        assertEquals(1, page.coercedNotes.size)
        assertTrue(page.coercedNotes.single().contains("estimated_minutes"))
        // Same slice the log prints: one channel, not two subsets (#480).
        val notes = TasksApi.lastCoercedNotes["manager:open"].orEmpty()
        assertEquals(page.coercedNotes, notes)
        try {
            // Another writer's fetch neither clobbers nor clears this one —
            // even on the same state at a different limit.
            parseTaskListPage("""{"tasks":[{"id":"b","name":"B"}]}""", "open", "widget")
            assertEquals(1, TasksApi.lastCoercedNotes["manager:open"]?.size)
            assertTrue("widget:open" !in TasksApi.lastCoercedNotes)
            // A clean page clears only its own fetch's entry.
            parseTaskListPage("""{"tasks":[{"id":"a","name":"A"}]}""", "open", "manager")
            assertTrue("manager:open" !in TasksApi.lastCoercedNotes)
            // Parallel writers under one source all survive: a second
            // state fetch must not prune the first (#480 review).
            parseTaskListPage(
                """{"tasks":[{"id":"c","name":"C","revision":"bogus"}]}""",
                "done",
                "manager",
            )
            parseTaskListPage(
                """{"tasks":[{"id":"a","name":"A","estimated_minutes":"abc"}]}""",
                "open",
                "manager",
            )
            assertEquals(1, TasksApi.lastCoercedNotes["manager:open"]?.size)
            assertEquals(1, TasksApi.lastCoercedNotes["manager:done"]?.size)
            // ...while the one screen that switches states prunes its
            // stale siblings explicitly on each landed page.
            TasksApi.clearCoercedNotesExcept("manager", "done")
            assertTrue("manager:open" !in TasksApi.lastCoercedNotes)
            assertEquals(1, TasksApi.lastCoercedNotes["manager:done"]?.size)
        } finally {
            TasksApi.noteCoercedNotes("manager", "open", emptyList())
            TasksApi.noteCoercedNotes("manager", "done", emptyList())
            TasksApi.noteCoercedNotes("widget", "open", emptyList())
        }
    }

    @Test
    fun `page notes cap at the log slice`() {
        val rows = (1..8).joinToString(",") {
            """{"id":"t$it","name":"N","estimated_minutes":"bogus$it"}"""
        }
        val page = parseTaskListPage("""{"tasks":[$rows]}""", "open", "manager")
        try {
            assertEquals(8, page.coerced)
            assertEquals(3, page.coercedNotes.size)
            assertEquals(page.coercedNotes, TasksApi.lastCoercedNotes["manager:open"])
        } finally {
            TasksApi.noteCoercedNotes("manager", "open", emptyList())
        }
    }

    @Test
    fun `negative repeat and revision clamp to zero and notify`() {
        // Only the estimate used to clamp: a negative repeat_every
        // rendered exactly like a one-shot, and a negative revision passed
        // clean — both silent rewrites (#480 review).
        val page = parseTaskListPage(
            """{"tasks":[{"id":"a","name":"A","repeat_every":-5,"revision":-1}]}""",
            "open",
            "manager",
        )
        try {
            assertEquals(0, page.tasks.single().repeatEvery)
            assertEquals(0, page.tasks.single().revision)
            assertEquals(1, page.coerced)
            val notes = TasksApi.lastCoercedNotes["manager:open"].orEmpty()
            assertTrue("clamp must name repeat_every: $notes", notes.any { it.contains("repeat_every") })
            assertTrue("clamp must name revision: $notes", notes.any { it.contains("revision") })
        } finally {
            TasksApi.noteCoercedNotes("manager", "open", emptyList())
        }
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

    @Test
    fun `long corrupt values truncate like the widget path`() {
        val long = "X".repeat(100)
        val f = filterCoercionMessage(long)!!
        assertTrue(f.contains("X".repeat(40)))
        assertTrue(!f.contains("X".repeat(41)))
        val s = sortCoercionMessage(long)!!
        assertTrue(s.contains("X".repeat(40)))
        assertTrue(!s.contains("X".repeat(41)))
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

    @Test
    fun `a fixed pref clears its diagnostics entry without re-logging`() {
        // #480 review: diagnostics must reflect the current reset state,
        // not history — while the log still fires once per distinct value.
        val key = "task_widget_view_9901"
        try {
            TaskWidget.noteCoercion(key, "\"Snoozed\"->Open")
            assertEquals("\"Snoozed\"->Open", TaskWidget.lastCoercions[key])
            TaskWidget.noteCoercion(key, null)
            assertTrue(key !in TaskWidget.lastCoercions)
            // Re-corruption updates diagnostics again (log stays quiet —
            // the line is already in the log from the first report).
            TaskWidget.noteCoercion(key, "\"Snoozed\"->Open")
            assertEquals("\"Snoozed\"->Open", TaskWidget.lastCoercions[key])
        } finally {
            TaskWidget.noteCoercion(key, null)
            synchronized(TaskWidget.coercionLock) {
                TaskWidget.coercionSeen.remove("$key|\"Snoozed\"->Open")
            }
        }
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
