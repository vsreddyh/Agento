package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Task Manager UX helpers (#288): the list-level last-synced stamp and
 * the revision-race detector.
 */
class TaskManagerUxTest {

    @Test
    fun `no stamp before the first successful load`() {
        assertNull(formatLastSynced(0L, 1_000_000L))
        assertNull(formatLastSynced(-5L, 1_000_000L))
    }

    @Test
    fun `just synced reads just now`() {
        assertEquals("just now", formatLastSynced(1_000_000L, 1_000_000L))
        assertEquals("just now", formatLastSynced(1_000_000L, 1_059_000L))
    }

    @Test
    fun `minutes and hours`() {
        assertEquals("1m ago", formatLastSynced(0L + 1, 60_001L))
        assertEquals("2m ago", formatLastSynced(1_000_000L, 1_000_000L + 150_000L))
        assertEquals("59m ago", formatLastSynced(1_000_000L, 1_000_000L + 59 * 60_000L))
        assertEquals("1h ago", formatLastSynced(1_000_000L, 1_000_000L + 60 * 60_000L))
        assertEquals("26h ago", formatLastSynced(1_000_000L, 1_000_000L + 26 * 3_600_000L))
    }

    @Test
    fun `a future stamp never goes negative`() {
        assertEquals("just now", formatLastSynced(2_000_000L, 1_000_000L))
    }

    @Test
    fun `conflict is exactly the 409 shape the server sends`() {
        assertTrue(isConflict(RuntimeException("HTTP 409: revision mismatch")))
        assertFalse(isConflict(RuntimeException("HTTP 500: boom")))
        assertFalse(isConflict(RuntimeException("Server URL not configured")))
        assertFalse(isConflict(RuntimeException()))
    }
}
