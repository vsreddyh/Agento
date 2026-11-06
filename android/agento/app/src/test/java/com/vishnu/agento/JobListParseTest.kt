package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

/**
 * Issue #430: an unrecognised job-list envelope must fail loudly instead
 * of rendering "No scheduled jobs", and unparseable rows must be counted
 * instead of shrinking the list silently. These tests pin [parseJobList].
 */
class JobListParseTest {

    @Test
    fun `jobs envelope parses rows`() {
        val page = parseJobList(
            """{"jobs":[{"id":"j1","name":"Morning"},{"id":"j2"}]}""",
        )
        assertEquals(listOf("j1", "j2"), page.jobs.map { it.id })
        assertEquals("Morning", page.jobs[0].name)
        assertEquals(0, page.dropped)
    }

    @Test
    fun `retired data and items envelopes fail loudly`() {
        // #433: one canonical envelope. An older shape must not read as a
        // clean empty job list.
        for (body in listOf("""{"data":[{"id":"a"}]}""", """{"items":[{"id":"a"}]}""")) {
            try {
                parseJobList(body)
                throw AssertionError("must throw, not read as no scheduled jobs")
            } catch (e: RuntimeException) {
                assertEquals(JOBS_SHAPE_ERROR, e.message)
            }
        }
    }

    @Test
    fun `retired job_id and row aliases are not read`() {
        // #433: the canonical row carries `id`, `schedule`, `paused`,
        // `delivery` and snake_case run timestamps. A row that only speaks
        // the retired aliases is unparseable, not silently bridged.
        val page = parseJobList(
            """{"jobs":[
                {"job_id":"k","name":"K"},
                {"id":"a","cron":"* * * * *","enabled":false,"deliver":"local"},
                {"name":"no id"},
                42
            ]}""",
        )
        assertEquals(listOf("a"), page.jobs.map { it.id })
        assertEquals("", page.jobs.single().schedule)
        assertEquals(null, page.jobs.single().paused)
        assertEquals("", page.jobs.single().delivery)
        assertEquals(3, page.dropped)
    }

    @Test
    fun `explicit null tri-state reads as unknown`() {
        // hasValue (#430): bare has+optBoolean would coerce an explicit
        // null to false — a positive Active signal instead of unknown.
        val page = parseJobList(
            """{"jobs":[
                {"id":"a","paused":null},
                {"id":"b","paused":true}
            ]}""",
        )
        assertNull(page.jobs[0].paused)
        assertEquals(true, page.jobs[1].paused)
    }

    @Test(expected = RuntimeException::class)
    fun `unknown envelope is a parse failure`() {
        parseJobList("""{"runs":[]}""")
    }

    @Test(expected = RuntimeException::class)
    fun `empty object envelope is a parse failure`() {
        parseJobList("""{}""")
    }

    @Test(expected = RuntimeException::class)
    fun `garbage body is a parse failure`() {
        parseJobList("not json at all")
    }

    @Test
    fun `explicit JSON nulls read as missing`() {
        // Same optJsonStr guard as tasks: a bare optString would coerce
        // a null id to the literal string "null" — a phantom row.
        val page = parseJobList(
            """{"jobs":[
                {"id":null,"name":"x"},
                {"id":"a","name":null,"prompt":null}
            ]}""",
        )
        assertEquals(listOf("a"), page.jobs.map { it.id })
        assertEquals("", page.jobs.single().name)
        assertEquals(1, page.dropped)
    }

    @Test
    fun `envelope failure names the shape`() {
        try {
            parseJobList("""{"runs":[]}""")
            throw AssertionError("must throw, not read as no scheduled jobs")
        } catch (e: RuntimeException) {
            assertEquals(JOBS_SHAPE_ERROR, e.message)
        }
    }

    @Test
    fun `drop message names the count`() {
        assertEquals(
            "1 job could not be parsed — the server sent a row this app cannot read.",
            jobDropMessage(1),
        )
        assertEquals(
            "3 jobs could not be parsed — the server sent rows this app cannot read.",
            jobDropMessage(3),
        )
    }
}
