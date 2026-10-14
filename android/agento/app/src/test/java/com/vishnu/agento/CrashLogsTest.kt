package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import java.time.LocalDateTime

/**
 * Crash-log file shape and TTL (#290). The handler itself needs Android;
 * the name/expiry rules are pure and pinned here.
 */
class CrashLogsTest {

    @Test
    fun `file name matches the specified shape`() {
        val name = CrashLogs.fileName(LocalDateTime.of(2026, 10, 14, 3, 4, 5), 1234)
        assertEquals("crash-20261014-030405-1234.log", name)
    }

    @Test
    fun `ttl is five days`() {
        assertEquals(5L * 24 * 60 * 60 * 1000, CrashLogs.TTL_MS)
    }

    @Test
    fun `expired only past the ttl`() {
        val now = 10_000_000L
        assertFalse(CrashLogs.isExpired(now, now))
        assertFalse(CrashLogs.isExpired(now - CrashLogs.TTL_MS, now))
        assertTrue(CrashLogs.isExpired(now - CrashLogs.TTL_MS - 1, now))
    }

    @Test
    fun `future timestamps never count as expired`() {
        assertFalse(CrashLogs.isExpired(20_000_000L, 10_000_000L))
    }
}
