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

    // ── Background refresh (#308) ────────────────────────────────────────────

    @Test
    fun `a row mid-exit survives the refresh that lands during its animation`() {
        // The server drops a completed task the moment it is done, so the
        // fresh page no longer lists `a` — but its leave animation is still
        // running and it has to stay on screen for it.
        val merged = mergeRefresh(
            fresh = listOf(ServerTask(id = "b")),
            previous = listOf(ServerTask(id = "a"), ServerTask(id = "b")),
            exiting = setOf("a"),
        )
        assertEquals(listOf("a", "b"), merged.map { it.id })
    }

    @Test
    fun `a carried row keeps its place, not the end of the list`() {
        // `b` is the one leaving, mid-list: it must not jump to the bottom for
        // the ~220ms it has left (nor reorder neighbours whose sort keys tie).
        val merged = mergeRefresh(
            fresh = listOf(ServerTask(id = "a"), ServerTask(id = "c")),
            previous = listOf(ServerTask(id = "a"), ServerTask(id = "b"), ServerTask(id = "c")),
            exiting = setOf("b"),
        )
        assertEquals(listOf("a", "b", "c"), merged.map { it.id })
    }

    @Test
    fun `several carried rows stay in their original order`() {
        val merged = mergeRefresh(
            fresh = listOf(ServerTask(id = "c")),
            previous = listOf(ServerTask(id = "a"), ServerTask(id = "b"), ServerTask(id = "c")),
            exiting = setOf("a", "b"),
        )
        assertEquals(listOf("a", "b", "c"), merged.map { it.id })
    }

    @Test
    fun `a carried index past the end of the fresh page is clamped`() {
        // `c` sat at index 2 but the fresh page is only one row long — it goes
        // at the end rather than out of range.
        val merged = mergeRefresh(
            fresh = listOf(ServerTask(id = "a")),
            previous = listOf(ServerTask(id = "a"), ServerTask(id = "b"), ServerTask(id = "c")),
            exiting = setOf("c"),
        )
        assertEquals(listOf("a", "c"), merged.map { it.id })
    }

    @Test
    fun `a row whose exit already finished is not resurrected`() {
        // exiting set here as it is when the response lands, after the row
        // was dropped: an empty set means nothing is carried over.
        val merged = mergeRefresh(
            fresh = listOf(ServerTask(id = "b")),
            previous = listOf(ServerTask(id = "a"), ServerTask(id = "b")),
            exiting = emptySet(),
        )
        assertEquals(listOf("b"), merged.map { it.id })
    }

    @Test
    fun `a refresh does not duplicate a row the server still lists`() {
        val merged = mergeRefresh(
            fresh = listOf(ServerTask(id = "a")),
            previous = listOf(ServerTask(id = "a")),
            exiting = setOf("a"),
        )
        assertEquals(listOf("a"), merged.map { it.id })
    }

    @Test
    fun `a plain refresh is just the fresh page`() {
        val merged = mergeRefresh(
            fresh = listOf(ServerTask(id = "x")),
            previous = listOf(ServerTask(id = "y")),
            exiting = emptySet(),
        )
        assertEquals(listOf("x"), merged.map { it.id })
    }
}
