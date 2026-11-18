package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Issue #433: project-list shapes must surface, never read as a clean
 * empty. These tests pin [parseProjectList]: the canonical envelope parses,
 * corrupt rows are skipped, and retired envelopes or `_id`-only rows are
 * parse failures rather than "no projects".
 */
class ProjectsApiTest {

    @Test
    fun `projects envelope parses rows`() {
        val out = parseProjectList(
            """{"projects":[{"id":"a","name":"Alpha"},{"id":"b","name":"Beta"}]}""",
        )
        assertEquals(listOf("a", "b"), out.map { it.id })
    }

    @Test
    fun `retired data and items envelopes fail loudly`() {
        // #433: one canonical envelope. The old shapes must not read as a
        // clean empty board.
        for (body in listOf(
            """{"data":[{"id":"a","name":"A"}]}""",
            """{"items":[{"id":"a","name":"A"}]}""",
            """{"other":[]}""",
            """{}""",
            "not json at all",
        )) {
            try {
                parseProjectList(body)
                throw AssertionError("must throw, not read as no projects")
            } catch (e: RuntimeException) {
                assertEquals(PROJECTS_SHAPE_ERROR, e.message)
            }
        }
    }

    @Test
    fun `retired _id rows fail loudly`() {
        // #433: the canonical row carries `id`; an `_id`-only row is the old
        // health-api shape, not a corrupt row — it throws instead of reading
        // as an empty board.
        try {
            parseProjectList("""{"projects":[{"_id":"z","name":"Zed"}]}""")
            throw AssertionError("must throw, not read as no projects")
        } catch (e: RuntimeException) {
            assertEquals(PROJECTS_SHAPE_ERROR, e.message)
        }
    }

    @Test
    fun `corrupt rows are skipped`() {
        val out = parseProjectList(
            """{"projects":[
                {"id":"ok","name":"Fine"},
                {"name":"no id"},
                {"id":"no-name"},
                42
            ]}""",
        )
        assertEquals(listOf("ok"), out.map { it.id })
    }

    @Test
    fun `explicit JSON nulls read as missing`() {
        // optJsonStr (#130): a bare optString would coerce these to the
        // literal string "null" — a phantom id that passes the isEmpty gate.
        val out = parseProjectList(
            """{"projects":[
                {"id":null,"name":"x"},
                {"id":"a","name":null},
                {"id":"b","name":"B","note":null}
            ]}""",
        )
        assertEquals(listOf("b"), out.map { it.id })
        assertEquals("", out.single().note)
    }

    // #519: the manual board order is status rank, then stored position,
    // then newest-updated. Missing/invalid positions read as null so a
    // legacy row can never masquerade as the top of the board.
    @Test
    fun `position parses from the row and reads null when absent`() {
        val out = parseProjectList(
            """{"projects":[
                {"id":"a","name":"A","position":2000},
                {"id":"b","name":"B"},
                {"id":"c","name":"C","position":null},
                {"id":"d","name":"D","position":"oops"}
            ]}""",
        )
        assertEquals(listOf(2000L, null, null, null), out.map { it.position })
    }

    @Test
    fun `manual order groups by status rank then position then recency`() {
        val rows = listOf(
            project("todo-legacy", "Todo", null, "2026-01-03T00:00:00Z"),
            project("todo-second", "Todo", 2000, "2026-01-01T00:00:00Z"),
            project("todo-first", "Todo", 1000, "2026-01-01T00:00:00Z"),
            project("ongoing", "Ongoing", null, "2026-01-01T00:00:00Z"),
            project("done", "Done", null, "2026-01-05T00:00:00Z"),
        )
        assertEquals(
            listOf("ongoing", "todo-first", "todo-second", "todo-legacy", "done"),
            manualProjectOrder(rows),
        )
    }

    @Test
    fun `manual sort keeps rows missing from the order last`() {
        val rows = listOf(
            project("known", "Todo", null, "2026-01-01T00:00:00Z"),
            project("fresh", "Todo", null, "2026-01-02T00:00:00Z"),
        )
        assertEquals(
            listOf("known", "fresh"),
            sortProjectsManually(rows, listOf("known")).map { it.id },
        )
    }

    @Test
    fun `snooze reads from the row and defaults blank`() {
        val out = parseProjectList(
            """{"projects":[
                {"id":"a","name":"A","status":"Paused","snoozeAt":"2026-11-19T12:30:00Z"},
                {"id":"b","name":"B","status":"Ongoing","snoozeAt":null}
            ]}""",
        )
        assertEquals("2026-11-19T12:30:00Z", out[0].snoozeAt)
        assertEquals("", out[1].snoozeAt)
    }

    @Test
    fun `only a ticker wake notifies, not a manual move`() {
        val armed = "2026-11-19T12:30:00Z"
        // The server stamps the marker with the snooze instant it fired.
        val woken = ServerProject(
            id = "a", name = "A", status = "Ongoing",
            snoozeAt = "", snoozeFiredAt = armed,
        )
        assertTrue(ProjectSnooze.isWake(woken, armed))
        // Manual move: the server clears the snooze without a marker.
        assertFalse(ProjectSnooze.isWake(woken.copy(snoozeFiredAt = ""), armed))
        // An old marker from a previous wake cannot match a later snooze.
        assertFalse(ProjectSnooze.isWake(woken.copy(snoozeFiredAt = "2026-11-18T12:30:00Z"), armed))
        // A row snoozed again is not a wake for the cached instant.
        assertFalse(
            ProjectSnooze.isWake(
                woken.copy(snoozeAt = "2026-11-20T12:30:00Z"),
                armed,
            )
        )
        assertFalse(ProjectSnooze.isWake(woken.copy(status = "Paused"), armed))
    }

    private fun project(id: String, status: String, position: Long?, updatedAt: String) =
        ServerProject(id = id, name = id, status = status, updatedAt = updatedAt, position = position)
}
