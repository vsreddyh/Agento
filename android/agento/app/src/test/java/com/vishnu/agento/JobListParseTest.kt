package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Test

/**
 * Issue #430: an unrecognised job-list envelope must fail loudly instead
 * of rendering "No scheduled jobs". These tests pin [parseJobList]: the
 * known envelopes parse, and anything else throws.
 */
class JobListParseTest {

    @Test
    fun `jobs envelope parses rows`() {
        val jobs = parseJobList(
            """{"jobs":[{"id":"j1","name":"Morning"},{"id":"j2"}]}""",
        )
        assertEquals(listOf("j1", "j2"), jobs.map { it.id })
        assertEquals("Morning", jobs[0].name)
    }

    @Test
    fun `data and items envelopes parse too`() {
        assertEquals(1, parseJobList("""{"data":[{"id":"a"}]}""").size)
        assertEquals(1, parseJobList("""{"items":[{"id":"a"}]}""").size)
    }

    @Test
    fun `job_id fallback works and id-less rows are skipped`() {
        val jobs = parseJobList(
            """{"jobs":[{"job_id":"k","name":"K"},{"name":"no id"}]}""",
        )
        assertEquals(listOf("k"), jobs.map { it.id })
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
