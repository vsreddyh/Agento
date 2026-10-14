package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import java.time.LocalDateTime

/**
 * Cron discovery rules (#289): unseen computation and the run timestamp.
 */
class CronCheckTest {

    private fun run(at: Long, text: String = "t") = CronRun(at = at, text = text)

    @Test
    fun `first check records without notifying`() {
        assertTrue(CronCheck.unseenRuns(listOf(run(100), run(200)), null).isEmpty())
    }

    @Test
    fun `only strictly newer runs are unseen`() {
        val runs = listOf(run(100), run(200), run(300))
        assertEquals(listOf(run(300)), CronCheck.unseenRuns(runs, 200L))
    }

    @Test
    fun `runs without an instant are never new`() {
        assertTrue(CronCheck.unseenRuns(listOf(run(0)), 0L).isEmpty())
    }

    @Test
    fun `run timestamp is IST day time`() {
        val at = LocalDateTime.of(2026, 10, 14, 9, 0).atZone(IST).toInstant().toEpochMilli()
        assertEquals("14 Oct, 09:00", formatCronRunAt(at))
    }
}
