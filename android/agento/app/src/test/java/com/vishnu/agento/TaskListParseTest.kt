package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Issue #430: gateway data failures must surface, never read as a clean
 * empty. These tests pin [parseTaskListPage]: the known envelopes parse,
 * unparseable rows are counted in [TaskList.dropped], and an unknown
 * envelope is a parse failure rather than "no tasks".
 */
class TaskListParseTest {

    @Test
    fun `tasks envelope parses rows`() {
        val page = parseTaskListPage(
            """{"tasks":[{"id":"a","name":"Alpha"},{"id":"b","name":"Beta"}]}""",
        )
        assertEquals(listOf("a", "b"), page.tasks.map { it.id })
        assertEquals(0, page.dropped)
    }

    @Test
    fun `data and items envelopes parse too`() {
        val data = parseTaskListPage("""{"data":[{"id":"a","name":"A"}]}""")
        assertEquals(1, data.tasks.size)
        val items = parseTaskListPage("""{"items":[{"id":"a","name":"A"}]}""")
        assertEquals(1, items.tasks.size)
    }

    @Test
    fun `id fallback to _id works`() {
        val page = parseTaskListPage("""{"tasks":[{"_id":"z","name":"Zed"}]}""")
        assertEquals("z", page.tasks.single().id)
        assertEquals(0, page.dropped)
    }

    @Test
    fun `unparseable rows are counted not dropped silently`() {
        val page = parseTaskListPage(
            """{"tasks":[
                {"id":"ok","name":"Fine"},
                {"name":"no id"},
                {"id":"no-name"},
                {"id":"","name":"blank id"},
                {"id":"  ","name":"whitespace id"},
                42,
                {"_id":"","id":"","name":""}
            ]}""",
        )
        assertEquals(listOf("ok"), page.tasks.map { it.id })
        assertEquals(6, page.dropped)
    }

    @Test
    fun `truncated flag survives`() {
        val page = parseTaskListPage("""{"tasks":[],"truncated":true}""")
        assertTrue(page.truncated)
        assertEquals(0, page.dropped)
    }

    @Test(expected = RuntimeException::class)
    fun `unknown envelope is a parse failure`() {
        parseTaskListPage("""{"other":[]}""")
    }

    @Test(expected = RuntimeException::class)
    fun `empty object envelope is a parse failure`() {
        parseTaskListPage("""{}""")
    }

    @Test(expected = RuntimeException::class)
    fun `garbage body is a parse failure`() {
        parseTaskListPage("not json at all")
    }

    @Test
    fun `envelope failure names the shape`() {
        try {
            parseTaskListPage("""{"other":[]}""")
            throw AssertionError("must throw, not read as no tasks")
        } catch (e: RuntimeException) {
            assertEquals("Unexpected response shape", e.message)
        }
    }

    @Test
    fun `drop message names the count`() {
        assertEquals(
            "1 task could not be parsed — the server sent a row this app cannot read.",
            taskDropMessage(1),
        )
        assertEquals(
            "3 tasks could not be parsed — the server sent rows this app cannot read.",
            taskDropMessage(3),
        )
    }
}
