package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Issue #430: all-dropped pages render the drop message through
 * [ErrorCard], which headlines [friendlyError]. The count must lead —
 * hiding it behind Details reintroduces the silent shrink.
 */
class FriendlyErrorTest {

    @Test
    fun `shape errors headline the mismatch`() {
        assertEquals(
            "The server sent a response this app doesn't understand",
            friendlyError("Unexpected response shape").title,
        )
        assertTrue(friendlyError("Unexpected response shape").nextStep.contains("App updates"))
    }

    @Test
    fun `drop counts headline the raw count`() {
        assertEquals(
            "3 tasks could not be parsed",
            friendlyError(taskDropMessage(3)).title,
        )
    }

    @Test
    fun `drop next step points at app updates`() {
        assertTrue(friendlyError(taskDropMessage(2)).nextStep.contains("App updates"))
    }

    @Test
    fun `unrelated failures still read generic`() {
        assertEquals(
            "Something went wrong",
            friendlyError("weird opaque failure").title,
        )
    }

    @Test
    fun `gone paths read as gone, not as an outdated app`() {
        val gone = friendlyError("HTTP 404: not found")
        assertEquals("That is no longer on the server", gone.title)
        assertTrue(gone.nextStep.contains("Refresh"))
    }

    @Test
    fun `broken parent chain reads as gone too`() {
        val gone = friendlyError("HTTP 400: invalid path")
        assertEquals("That is no longer on the server", gone.title)
        assertTrue(gone.nextStep.contains("Refresh"))
    }

    @Test
    fun `unknown routes still point at app updates`() {
        val route = friendlyError("HTTP 404: 404 page not found")
        assertTrue(route.nextStep.contains("App updates"))
    }

    @Test
    fun `folder delete refusal keeps its VPS guidance`() {
        val folder = friendlyError("HTTP 400: not a file: folders are cleared on the VPS, not here")
        assertEquals("Folders can't be deleted here", folder.title)
        assertTrue(folder.nextStep.contains("VPS"))
    }
}
