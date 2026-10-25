package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Pure policy of the automatic update checks (#366): when a startup check
 * is due, when a notification fires, and what the App updates section
 * prints. Notification posting itself needs a Context and stays with the
 * platform path; the gate it consults is pinned here.
 */
class UpdateCheckTest {

    @Test
    fun `startup check is debounced to six hours`() {
        val now = 10_000_000_000L
        val sixH = UpdateCheck.STARTUP_DEBOUNCE_MS
        // Enabled and never checked: due.
        assertTrue(UpdateCheck.startupDue(0L, now, true))
        // Enabled and checked five hours ago: not due.
        assertFalse(UpdateCheck.startupDue(now - 5 * 3_600_000L, now, true))
        // Enabled and checked exactly at the debounce: due.
        assertTrue(UpdateCheck.startupDue(now - sixH, now, true))
        // Disabled: never due, however stale.
        assertFalse(UpdateCheck.startupDue(0L, now, false))
        assertFalse(UpdateCheck.startupDue(now - 30L * 24L * 3_600_000L, now, false))
    }

    @Test
    fun `notification gate needs a new release and a quiet user`() {
        // Everything aligned: notify.
        assertTrue(
            UpdateCheck.shouldPostNotification(
                newer = true, enabled = true, canPost = true,
                inForeground = false, alreadyPostedTag = "", tag = "agento-v5.14.0",
            ),
        )
        // Up to date, disabled, unpermitted, foreground: stay silent.
        assertFalse(
            UpdateCheck.shouldPostNotification(
                newer = false, enabled = true, canPost = true,
                inForeground = false, alreadyPostedTag = "", tag = "agento-v5.14.0",
            ),
        )
        assertFalse(
            UpdateCheck.shouldPostNotification(
                newer = true, enabled = false, canPost = true,
                inForeground = false, alreadyPostedTag = "", tag = "agento-v5.14.0",
            ),
        )
        assertFalse(
            UpdateCheck.shouldPostNotification(
                newer = true, enabled = true, canPost = false,
                inForeground = false, alreadyPostedTag = "", tag = "agento-v5.14.0",
            ),
        )
        assertFalse(
            UpdateCheck.shouldPostNotification(
                newer = true, enabled = true, canPost = true,
                inForeground = true, alreadyPostedTag = "", tag = "agento-v5.14.0",
            ),
        )
        // No repeat: the same tag posted once stays silent on the next tick.
        assertFalse(
            UpdateCheck.shouldPostNotification(
                newer = true, enabled = true, canPost = true,
                inForeground = false,
                alreadyPostedTag = "agento-v5.14.0", tag = "agento-v5.14.0",
            ),
        )
        // …but a NEWER tag still fires.
        assertTrue(
            UpdateCheck.shouldPostNotification(
                newer = true, enabled = true, canPost = true,
                inForeground = false,
                alreadyPostedTag = "agento-v5.14.0", tag = "agento-v5.15.0",
            ),
        )
        // A blank tag is never a release.
        assertFalse(
            UpdateCheck.shouldPostNotification(
                newer = true, enabled = true, canPost = true,
                inForeground = false, alreadyPostedTag = "", tag = "",
            ),
        )
    }

    @Test
    fun `status line narrates the last check`() {
        val now = 10_000_000_000L
        // Never checked.
        assertEquals(
            "Not checked yet",
            UpdateCheck.statusLine(UpdateCheck.Snapshot(0L, "", "", "", 0L, "", ""), now, false),
        )
        // A failure shows the error, never the stale pending state.
        assertEquals(
            "Last check failed: GitHub API HTTP 403",
            UpdateCheck.statusLine(
                UpdateCheck.Snapshot(now, "agento-v5.14.0", "5.14.0", "u", 1L, "", "GitHub API HTTP 403"),
                now, true,
            ),
        )
        // Fresh check, up to date.
        assertEquals(
            "Up to date · checked just now",
            UpdateCheck.statusLine(
                UpdateCheck.Snapshot(now, "agento-v5.13.2", "5.13.2", "u", 1L, "", ""),
                now, false,
            ),
        )
        // Fresh check with a pending release names it.
        assertEquals(
            "Update available: 5.14.0 · checked just now",
            UpdateCheck.statusLine(
                UpdateCheck.Snapshot(now, "agento-v5.14.0", "5.14.0", "u", 1L, "", ""),
                now, true,
            ),
        )
        // Age buckets: minutes, hours, days — and the "(8.0.0)" fallback
        // when the release has no display name.
        assertEquals(
            "Up to date · checked 5 min ago",
            UpdateCheck.statusLine(
                UpdateCheck.Snapshot(now - 5 * 60_000L, "t", "", "u", 1L, "", ""),
                now, false,
            ),
        )
        assertEquals(
            "Up to date · checked 3 h ago",
            UpdateCheck.statusLine(
                UpdateCheck.Snapshot(now - 3 * 3_600_000L, "t", "", "u", 1L, "", ""),
                now, false,
            ),
        )
        assertEquals(
            "Update available: agento-v5.14.0 · checked 2 d ago",
            UpdateCheck.statusLine(
                UpdateCheck.Snapshot(now - 2 * 86_400_000L, "agento-v5.14.0", "", "u", 1L, "", ""),
                now, true,
            ),
        )
    }
}
