package com.vishnu.agento

import org.junit.Assert.assertEquals
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
}
