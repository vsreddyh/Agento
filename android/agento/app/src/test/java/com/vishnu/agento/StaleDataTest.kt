package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
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

    private fun millis(year: Int, month: Int, day: Int, hour: Int, minute: Int): Long =
        java.util.Calendar.getInstance().apply {
            set(java.util.Calendar.YEAR, year)
            set(java.util.Calendar.MONTH, month)
            set(java.util.Calendar.DAY_OF_MONTH, day)
            set(java.util.Calendar.HOUR_OF_DAY, hour)
            set(java.util.Calendar.MINUTE, minute)
            set(java.util.Calendar.SECOND, 0)
            set(java.util.Calendar.MILLISECOND, 0)
        }.timeInMillis

    @Test
    fun `same calendar day matches across hours`() {
        assertTrue(
            isSameDay(
                millis(2026, java.util.Calendar.OCTOBER, 5, 9, 14),
                millis(2026, java.util.Calendar.OCTOBER, 5, 23, 59),
            ),
        )
    }

    @Test
    fun `adjacent days do not match`() {
        assertFalse(
            isSameDay(
                millis(2026, java.util.Calendar.OCTOBER, 5, 23, 59),
                millis(2026, java.util.Calendar.OCTOBER, 6, 0, 1),
            ),
        )
    }

    @Test
    fun `same day-of-year in different years does not match`() {
        // DAY_OF_YEAR alone aliases across years; the badge must not call
        // last year's fetch "today".
        assertFalse(
            isSameDay(
                millis(2025, java.util.Calendar.OCTOBER, 5, 9, 14),
                millis(2026, java.util.Calendar.OCTOBER, 5, 9, 14),
            ),
        )
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

    @Test
    fun `transient client failure reads as retry not a device state`() {
        // SDK AVAILABLE but the client failed to build: the settings
        // section offers a retry for the same state (never a permission
        // grant), so the sync card must not claim "unavailable on this
        // device".
        assertTrue(HEALTH_CONNECT_TRANSIENT_MESSAGE.isNotBlank())
        assertNotEquals(
            HEALTH_CONNECT_UNAVAILABLE_MESSAGE,
            HEALTH_CONNECT_TRANSIENT_MESSAGE,
        )
        assertTrue(HEALTH_CONNECT_TRANSIENT_MESSAGE.contains("try again", ignoreCase = true))
    }

    // ── Settings update-required ordering ─────────────────────────────

    @Test
    fun `update required outranks not installed`() {
        // The dead-branch class from review: availability-first ordering
        // made update-required devices read "not installed".
        assertEquals(
            "Health Connect needs an update",
            healthStatusLine(healthAvailable = false, healthUpdateRequired = true),
        )
        assertEquals(
            "Health Connect not installed",
            healthStatusLine(healthAvailable = false, healthUpdateRequired = false),
        )
        assertEquals(
            "Health Connect ready",
            healthStatusLine(healthAvailable = true, healthUpdateRequired = false),
        )
    }

    @Test
    fun `a transiently dead client never reads ready`() {
        // #471 review: the card headline claimed "ready" while the hub
        // row, the sync message and the retry section all said
        // not-responding for the same SDK state.
        assertEquals(
            "Health Connect isn't responding",
            healthStatusLine(healthAvailable = true, healthUpdateRequired = false, healthTransient = true),
        )
        // Same order as the hub row: update-required outranks transient.
        assertEquals(
            "Health Connect needs an update",
            healthStatusLine(healthAvailable = true, healthUpdateRequired = true, healthTransient = true),
        )
    }

    @Test
    fun `hub health row leads with update needed`() {
        assertEquals(
            "Update needed",
            hubHealthStatus(false, true, false, false, false, ""),
        )
        assertEquals(
            "Not responding",
            hubHealthStatus(true, false, true, false, false, ""),
        )
        assertEquals(
            "Not installed",
            hubHealthStatus(false, false, false, false, false, ""),
        )
        assertEquals(
            "Setup needed",
            hubHealthStatus(true, false, false, false, false, ""),
        )
        assertEquals("Syncing…", hubHealthStatus(true, false, false, true, true, ""))
        assertEquals("Synced", hubHealthStatus(true, false, false, true, false, "t"))
        assertEquals("Ready", hubHealthStatus(true, false, false, true, false, ""))
    }

    @Test
    fun `transient perm line reads unknown not ungranted`() {
        // A dead client forces an empty grant set; "not granted yet"
        // would imply a grant that cannot fix anything.
        assertEquals(
            "Permissions unknown — the client isn't responding",
            healthPermLine(permissionsGranted = false, healthTransient = true),
        )
        assertEquals(
            "Permissions granted",
            healthPermLine(permissionsGranted = true, healthTransient = false),
        )
        assertEquals(
            "Permissions not granted yet",
            healthPermLine(permissionsGranted = false, healthTransient = false),
        )
    }
}
