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
        assertEquals(
            "2 providers could not be parsed",
            friendlyError(catalogDropMessage(2)).title,
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
}
