package com.vishnu.agento

import org.junit.Assert.assertEquals
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
    fun `data and items envelopes parse too`() {
        assertEquals(1, parseJobList("""{"data":[{"id":"a"}]}""").jobs.size)
        assertEquals(1, parseJobList("""{"items":[{"id":"a"}]}""").jobs.size)
    }

    @Test
    fun `job_id fallback works and unparseable rows are counted`() {
        val page = parseJobList(
            """{"jobs":[{"job_id":"k","name":"K"},{"name":"no id"},42]}""",
        )
        assertEquals(listOf("k"), page.jobs.map { it.id })
        assertEquals(2, page.dropped)
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
    fun `envelope failure names the shape`() {
        try {
            parseJobList("""{"runs":[]}""")
            throw AssertionError("must throw, not read as no scheduled jobs")
        } catch (e: RuntimeException) {
            assertEquals("Unexpected response shape", e.message)
        }
    }
}
