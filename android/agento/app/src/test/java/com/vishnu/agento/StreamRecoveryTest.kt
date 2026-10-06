package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * #214, second half: a dropped turn must be recoverable, and a resend must
 * never replay the fragment.
 *
 * The first half (4.14.6 + the lenient read) made a drop VISIBLE — flagged,
 * parked, never reported as finished. Visible is not enough: the orphaned
 * turn keeps running server-side, so its completed text is retrievable, and
 * resending the fragment as context primes a new turn with truncated output
 * while the orphan may still be executing. These tests pin the pure
 * decisions behind both: what leaves the history on resend, what counts as
 * recovered text, and when recovered text counts as settled.
 */
class StripTrailingFragmentTest {

    private fun user(text: String) = ChatMessage("user", text, 1L)

    private fun assistant(
        text: String,
        interrupted: Boolean = false,
        unreported: Boolean = false,
    ) = ChatMessage(
        "assistant", text, 2L,
        interrupted = interrupted,
        interruptedBy = if (interrupted) InterruptedBy.DROP else InterruptedBy.USER,
        unreported = unreported,
    )

    @Test
    fun `an interrupted trailing fragment is stripped for resend`() {
        val history = listOf(user("hi"), assistant("half an ans", interrupted = true))
        assertEquals(listOf(user("hi")), stripTrailingFragmentForResend(history))
    }

    @Test
    fun `an unreported error fragment is stripped too`() {
        // An Error carrying partial text is a cut-off turn by the same rule
        // (asInterruptedDrop) — resending it replays the fragment either way.
        val history = listOf(user("hi"), assistant("half an ans", unreported = true))
        assertEquals(listOf(user("hi")), stripTrailingFragmentForResend(history))
    }

    @Test
    fun `a clean trailing reply is kept`() {
        val history = listOf(user("hi"), assistant("full answer"))
        assertEquals(history, stripTrailingFragmentForResend(history))
    }

    @Test
    fun `a trailing user message is kept`() {
        val history = listOf(user("hi"), assistant("ok"), user("again"))
        assertEquals(history, stripTrailingFragmentForResend(history))
    }

    @Test
    fun `an empty history stays empty`() {
        assertEquals(emptyList<ChatMessage>(), stripTrailingFragmentForResend(emptyList()))
    }
}

/** An Error carrying content is a cut-off turn, not a clean failure. */
class AsInterruptedDropTest {

    @Test
    fun `a partial is flagged without losing anything`() {
        val kept = ChatMessage("assistant", "half", 2L, tools = listOf("read"), unreported = true)
            .asInterruptedDrop()
        assertTrue(kept.interrupted)
        assertEquals(InterruptedBy.DROP, kept.interruptedBy)
        assertEquals("half", kept.content)
        assertEquals(listOf("read"), kept.tools)
        assertTrue(kept.unreported)
    }
}

/** The transcript slice recovery reads back. */
class ParseLastAssistantTextTest {

    private fun row(role: String, content: String) =
        """{"role":"$role","content":"$content"}"""

    @Test
    fun `the latest assistant text after the last user row wins`() {
        val body = """{"messages":[
            ${row("user", "first")},
            ${row("assistant", "old answer")},
            ${row("user", "second")},
            ${row("assistant", "new answer")}
        ]}"""
        assertEquals("new answer", parseLastAssistantText(body))
    }

    @Test
    fun `tool rows do not reset the slice`() {
        val body = """{"messages":[
            ${row("user", "do it")},
            ${row("assistant", "working")},
            {"role":"tool","tool_name":"read","content":"file text"}
        ]}"""
        assertEquals("working", parseLastAssistantText(body))
    }

    @Test
    fun `no assistant row after the last user reads as absent`() {
        val body = """{"messages":[${row("user", "do it")}]}"""
        assertNull(parseLastAssistantText(body))
    }

    @Test
    fun `a blank assistant row reads as absent`() {
        val body = """{"messages":[
            ${row("user", "do it")},
            ${row("assistant", "  ")}
        ]}"""
        assertNull(parseLastAssistantText(body))
    }

    @Test
    fun `envelope variants and content blocks parse`() {
        val wrapped = """{"data":[{"role":"assistant","content":"deep"}]}"""
        assertEquals("deep", parseLastAssistantText(wrapped))
        val bare = """[${row("assistant", "bare")}]"""
        assertEquals("bare", parseLastAssistantText(bare))
        val blocks = """{"messages":[
            {"role":"user","content":"go"},
            {"role":"assistant","content":[{"type":"text","text":"blocked"}]}
        ]}"""
        assertEquals("blocked", parseLastAssistantText(blocks))
    }

    @Test(expected = RuntimeException::class)
    fun `an unreadable body throws like the other transcript readers`() {
        parseLastAssistantText("not json at all {{{")
    }
}

/** Adopt-longer, settle-when-stable: the two recovery decisions. */
class RecoveryDecisionTest {

    @Test
    fun `a longer recovered text is adoptable`() {
        assertTrue(recoveryAdoptable("half", "half an answer and more"))
    }

    @Test
    fun `equal or shorter text is not worth adopting`() {
        assertFalse(recoveryAdoptable("half", "half"))
        assertFalse(recoveryAdoptable("half an answer", "half"))
    }

    @Test
    fun `blank recovery adopts nothing, blank fragment adopts anything real`() {
        assertFalse(recoveryAdoptable("half", null))
        assertFalse(recoveryAdoptable("half", "   "))
        assertTrue(recoveryAdoptable("", "orphan finished this"))
    }

    @Test
    fun `identical polls mean settled, growth means still running`() {
        assertTrue(recoveryTextSettled("same text", "same text"))
        assertFalse(recoveryTextSettled("half", "half and more"))
        assertFalse(recoveryTextSettled(null, "half"))
        assertFalse(recoveryTextSettled("half", null))
    }
}
