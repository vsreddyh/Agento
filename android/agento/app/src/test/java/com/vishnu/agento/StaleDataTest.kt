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
 * [staleBadgeFor] decides the badge, it does not format it.
 */
class StaleDataTest {

    private fun task(id: String) = ServerTask(id = id, name = id)

    // ── Widget stale rule ─────────────────────────────────────────────

    @Test
    fun `failed fetch over cache badges with the fetch time`() {
        val cached = listOf(task("a"))
        assertEquals(StaleBadge(1234L), staleBadgeFor(null, cached, 1234L))
    }

    @Test
    fun `live rows never badge`() {
        val fresh = listOf(task("a"))
        val cached = listOf(task("a"))
        assertNull(staleBadgeFor(fresh, cached, 1234L))
    }

    @Test
    fun `no cache means error rendering not a stale badge`() {
        // tasks == null with no cache renders the error line (#435 keeps
        // that path); a badge there would claim rows that are not shown.
        assertNull(staleBadgeFor(null, null, 1234L))
    }

    @Test
    fun `unknown fetch time still badges timelessly`() {
        // cachedAt is written with every successful fetch, but the two
        // cache maps are separate Volatile writes — a torn read can observe
        // a cached entry with a missing stamp, and that must never render
        // as live data. The badge then carries no time (see
        // task_widget_stale_unknown); it does not vanish.
        assertEquals(StaleBadge(null), staleBadgeFor(null, listOf(task("a")), null))
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
    fun `save error is per tab`() {
        val a = "stale-data-tab-a"
        val b = "stale-data-tab-b"
        ChatThreads.clearSaveError(a)
        ChatThreads.clearSaveError(b)
        ChatThreads.recordSaveError(a)
        assertEquals(ChatThreads.SAVE_FAILED_MESSAGE, ChatThreads.saveError(a))
        assertEquals("", ChatThreads.saveError(b))
        ChatThreads.clearSaveError(a)
    }

    @Test
    fun `save-error marker key is per tab`() {
        // The prefs marker survives the restart the flag is about; a shared
        // key would banner every tab for one tab's failure, or clear another
        // tab's marker on an unrelated success.
        assertNotEquals(
            ChatThreads.saveErrorMarkerKey("chat"),
            ChatThreads.saveErrorMarkerKey("code"),
        )
        assertTrue(ChatThreads.saveErrorMarkerKey("chat").isNotBlank())
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

    @Test
    fun `update required names its own fix`() {
        // An outdated provider is not a missing platform: sharing the
        // unavailable line would disagree with the settings card, which
        // gates on the SDK status. The sync card names the update instead.
        assertTrue(HEALTH_CONNECT_UPDATE_REQUIRED_MESSAGE.isNotBlank())
        assertNotEquals(
            HEALTH_CONNECT_UNAVAILABLE_MESSAGE,
            HEALTH_CONNECT_UPDATE_REQUIRED_MESSAGE,
        )
        assertNotEquals(
            "Health Connect permissions not granted",
            HEALTH_CONNECT_UPDATE_REQUIRED_MESSAGE,
        )
    }
}
