package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Stale and failed data surfaces instead of rendering as fresh (#435).
 *
 * What a JVM test can pin is the pure surface behind the three fixes —
 * the stale-render rule, the save-error record/clear transitions, and the
 * unavailable-platform wording. The rendering itself (RemoteViews text,
 * the sheet banner, the sync call) needs Android and is reviewed by eye:
 * [staleAtFor] names the fetch time, it does not format it.
 */
class StaleDataTest {

    private fun task(id: String) = ServerTask(id = id, name = id)

    // ── Widget stale rule ─────────────────────────────────────────────

    @Test
    fun `failed fetch over cache names the fetch time`() {
        val cached = listOf(task("a"))
        assertEquals(1234L, staleAtFor(null, cached, 1234L))
    }

    @Test
    fun `live rows never badge`() {
        val fresh = listOf(task("a"))
        val cached = listOf(task("a"))
        assertNull(staleAtFor(fresh, cached, 1234L))
    }

    @Test
    fun `no cache means error rendering not a stale badge`() {
        // tasks == null with no cache renders the error line (#435 keeps
        // that path); a badge there would claim rows that are not shown.
        assertNull(staleAtFor(null, null, 1234L))
    }

    @Test
    fun `badge needs a known fetch time`() {
        // cachedAt is written with every successful fetch, so an unknown
        // stamp cannot happen in practice; when it does, the row renders
        // unbadged rather than with a blank time.
        assertNull(staleAtFor(null, listOf(task("a")), null))
    }

    // ── Thread save-error state ───────────────────────────────────────

    @Test
    fun `save error defaults to empty`() {
        assertEquals("", ChatThreads.saveError("stale-data-never-written"))
    }

    @Test
    fun `record and clear round-trip`() {
        val tab = "stale-data-roundtrip"
        ChatThreads.clearSaveError(tab)
        assertEquals("", ChatThreads.saveError(tab))
        ChatThreads.recordSaveError(tab)
        assertEquals(ChatThreads.SAVE_FAILED_MESSAGE, ChatThreads.saveError(tab))
        ChatThreads.clearSaveError(tab)
        assertEquals("", ChatThreads.saveError(tab))
    }

    @Test
    fun `save failure names storage`() {
        // The banner's whole point: memory is ahead of disk, and the next
        // cold start shows the older conversation. "Storage" tells the user
        // what to free; a generic "failed" would not.
        assertTrue(ChatThreads.SAVE_FAILED_MESSAGE.contains("storage", ignoreCase = true))
    }

    // ── Health Connect unavailable wording ────────────────────────────

    @Test
    fun `unavailable reads as a device state not a permission or data state`() {
        assertTrue(HEALTH_CONNECT_UNAVAILABLE_MESSAGE.isNotBlank())
        assertNotEquals(
            "Health Connect permissions not granted",
            HEALTH_CONNECT_UNAVAILABLE_MESSAGE,
        )
        assertTrue(HEALTH_CONNECT_UNAVAILABLE_MESSAGE.contains("unavailable", ignoreCase = true))
    }
}
