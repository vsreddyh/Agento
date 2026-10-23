package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Chat header overflow (#354): eight icon buttons (drawer + seven actions)
 * starved the title — "Chat" clipped to "C" with "…" where the model name
 * belongs. The bar keeps [chatHeaderVisibleActions] on screen and folds
 * [chatHeaderOverflowActions] behind "More".
 */
class ChatHeaderTest {

    @Test
    fun `every header action is assigned exactly once`() {
        assertEquals(
            ChatHeaderAction.entries.sorted(),
            chatHeaderAllActions.sorted(),
        )
        assertEquals(
            ChatHeaderAction.entries.size,
            (chatHeaderVisibleActions + chatHeaderOverflowActions).toSet().size,
        )
    }

    @Test
    fun `visible set is the small always-visible set`() {
        assertEquals(
            listOf(
                ChatHeaderAction.MODEL,
                ChatHeaderAction.CONVERSATIONS,
                ChatHeaderAction.AUTO_READ,
                ChatHeaderAction.NEW,
            ),
            chatHeaderVisibleActions,
        )
    }

    @Test
    fun `overflow holds runs approvals and memory`() {
        assertEquals(
            listOf(
                ChatHeaderAction.RUNS,
                ChatHeaderAction.APPROVALS,
                ChatHeaderAction.MEMORY,
            ),
            chatHeaderOverflowActions,
        )
    }

    @Test
    fun `visible plus overflow fits a 360dp phone with room for the title`() {
        // Drawer + visible actions + one "More" icon at 48dp each. Six
        // slots = 288dp, leaving ~72dp for the title on a 360dp window —
        // enough for "Chat" in full at 1.0 and 1.3 font scale. Seven
        // actions (384dp) left the title ~1 character wide.
        val barSlots = 1 + chatHeaderVisibleActions.size + 1
        assertTrue("bar carries $barSlots slots, want <= 6", barSlots <= 6)
    }

    @Test
    fun `no single action can silently re-stuff the bar`() {
        // A future action added to the enum without a slot assignment
        // breaks the exhaustive split above; one added to visible past
        // the budget breaks the slot budget.
        assertEquals(7, ChatHeaderAction.entries.size)
        assertTrue(chatHeaderVisibleActions.size <= 4)
    }

    @Test
    fun `overflow badge surfaces either pending count`() {
        assertEquals(0, chatHeaderOverflowBadgeCount(0, 0))
        assertEquals(3, chatHeaderOverflowBadgeCount(3, 0))
        assertEquals(2, chatHeaderOverflowBadgeCount(0, 2))
        assertEquals(5, chatHeaderOverflowBadgeCount(3, 2))
    }

    @Test
    fun `overflow badge never goes negative`() {
        assertEquals(0, chatHeaderOverflowBadgeCount(-1, -2))
        assertEquals(2, chatHeaderOverflowBadgeCount(-1, 2))
    }
}
