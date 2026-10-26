package com.vishnu.agento

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Lost-reply adoption (#374): the pure decision layer — which run matches
 * a dangling tail, what a terminal match does, and the record parse both
 * ride on. The ViewModel flow itself needs a Context (plain JUnit cannot
 * build it); every branch it takes is decided here.
 */
class AdoptTerminalTest {

    private fun run(
        id: String,
        status: String,
        updatedAt: Long,
        prompt: String = "move sweep house",
        output: String = "",
        conversation: String = "conv-1",
        error: String = "",
        seq: Long = 0L,
    ) = AgentRun(
        id = id, profile = "chat", status = status, conversationId = conversation,
        prompt = prompt, output = output, error = error, updatedAt = updatedAt,
        seq = seq,
    )

    private val tail = ChatMessage("user", "move sweep house", 1L)

    // ── Match selection ──────────────────────────────────────────────────

    @Test
    fun `live matches ignore terminal runs and prefer seq on ties`() {
        val live = run("l", "running", 100L, seq = 5L)
        val done = run("d", "completed", 200L, output = "x")
        assertEquals("l", liveMatchFor(tail, "conv-1", listOf(live, done))?.id)
        assertNull(liveMatchFor(tail, "conv-1", listOf(done)))
        // Same millisecond: sequence decides, not list order.
        val a = run("a", "running", 100L, seq = 1L)
        val b = run("b", "running", 100L, seq = 2L)
        assertEquals("b", liveMatchFor(tail, "conv-1", listOf(a, b))?.id)
        assertEquals("b", liveMatchFor(tail, "conv-1", listOf(b, a))?.id)
    }

    @Test
    fun `terminal match wins newest first`() {
        val runs = listOf(
            run("old", "completed", 100L, output = "first"),
            run("new", "completed", 200L, output = "second"),
            run("live", "running", 300L),
        )
        // Live runs never match here: the resume path owns them, and the
        // adopt path must not see them.
        val match = terminalMatchFor(tail, "conv-1", runs)
        assertEquals("new", match?.id)
    }

    @Test
    fun `live-only and other-thread runs do not match`() {
        assertNull(terminalMatchFor(tail, "conv-1", listOf(run("l", "running", 1L))))
        assertNull(terminalMatchFor(tail, "other", listOf(run("c", "completed", 1L, output = "x"))))
        assertNull(terminalMatchFor(tail, "conv-1", emptyList()))
    }

    @Test
    fun `matching uses the preview comparison`() {
        // Truncated preview: the tail starts with the stored core.
        val preview = run("t", "completed", 1L, prompt = "move sweep… (truncated)", output = "x")
        assertEquals("t", terminalMatchFor(tail, "conv-1", listOf(preview))?.id)
        // Different prompt: no match, the tail stays alone.
        val other = run("o", "completed", 1L, prompt = "something else", output = "x")
        assertNull(terminalMatchFor(tail, "conv-1", listOf(other)))
    }

    @Test
    fun `image-only blank-prompt tails match blank-prompt runs`() {
        val photoTail = ChatMessage(
            "user", "", 1L,
            attachments = listOf(ChatAttachment(base64 = "aGk=")),
        )
        val blank = run("b", "completed", 1L, prompt = "", output = "described")
        assertEquals("b", terminalMatchFor(photoTail, "conv-1", listOf(blank))?.id)
        // …but a text tail never matches a blank prompt.
        assertNull(terminalMatchFor(tail, "conv-1", listOf(blank)))
    }

    // ── Outcome ──────────────────────────────────────────────────────────

    @Test
    fun `completed with output appends`() {
        assertEquals(
            AdoptOutcome.APPEND,
            adoptDecision(run("c", "completed", 1L, output = "done")),
        )
    }

    @Test
    fun `completed with blank output leaves the tail`() {
        assertEquals(AdoptOutcome.LEAVE, adoptDecision(run("c", "completed", 1L)))
        assertEquals(AdoptOutcome.LEAVE, adoptDecision(run("c", "completed", 1L, output = "  ")))
    }

    @Test
    fun `failed cancelled interrupted are labelled never adopted`() {
        assertEquals(
            AdoptOutcome.LABEL_FAILED,
            adoptDecision(run("f", "failed", 1L, error = "boom")),
        )
        assertEquals(AdoptOutcome.LABEL_FAILED, adoptDecision(run("x", "cancelled", 1L)))
        assertEquals(AdoptOutcome.LABEL_FAILED, adoptDecision(run("i", "interrupted", 1L)))
    }

    // ── Record parse ─────────────────────────────────────────────────────

    @Test
    fun `detail shape parses usage tools and error`() {
        val o = JSONObject(
            """{"id":"r","profile":"chat","status":"completed","conversation_id":"c",""" +
                """"prompt":"hi","output":"hello","reasoning":"r","model":"m","tools":["bash"],""" +
                """"seq":3,"created_at":1,"updated_at":2,"finished_at":3,""" +
                """"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}""",
        )
        val run = parseAgentRun(o)!!
        assertEquals("r", run.id)
        assertEquals("completed", run.status)
        assertEquals("hello", run.output)
        assertEquals(listOf("bash"), run.tools)
        assertEquals(10L, run.usage?.prompt)
        assertEquals(5L, run.usage?.completion)
        assertEquals(15L, run.usage?.total)
        assertTrue(run.isTerminal())
    }

    @Test
    fun `id-less rows do not parse`() {
        assertNull(parseAgentRun(JSONObject("""{"status":"completed"}""")))
        assertTrue(parseAgentRunList("""{"runs":[]}""").isEmpty())
        assertTrue(parseAgentRunList("not json").isEmpty())
        assertTrue(parseAgentRunList("""{"nope":[]}""").isEmpty())
    }

    @Test
    fun `list parse keeps rows and drops id-less ones`() {
        val rows = parseAgentRunList(
            """{"runs":[{"id":"a","status":"running"},"x",null,{"status":"x"},{"id":"b","status":"failed","error":"e"}]}""",
        )
        assertEquals(listOf("a", "b"), rows.map { it.id })
        assertFalse(rows[0].isTerminal())
        assertEquals("e", rows[1].error)
    }

    @Test
    fun `prompt preview comparison pins truncation`() {
        assertTrue(promptMatches("move sweep house", "move sweep house"))
        assertFalse(promptMatches("move sweep house", ""))
        assertFalse(promptMatches("move sweep house", "something else"))
        assertTrue(promptMatches("move sweep house right now", "move sweep house… (truncated)"))
        // A literal trailing marker in a short prompt is not a prefix: the
        // full text must actually exceed the stored core.
        assertFalse(promptMatches("move sweep", "move sweep… (truncated)"))
        assertTrue(promptMatches("  move sweep house  ", "move sweep house"))
    }

    @Test
    fun `long prompts adopt through the truncated preview`() {
        // The store keeps the bounded preview (MaxPrompt), so detail and
        // list return the same truncated string — and the tail, longer
        // than the cap, still matches by prefix. A trim-exact check here
        // would break exactly these prompts; record identity (same stored
        // string in both reads) is the guard instead.
        val longTail = "move sweep house right now please " + "x".repeat(5000)
        val preview = longTail.take(4000) + "… (truncated)"
        val tail = ChatMessage("user", longTail, 1L)
        val row = run("r", "completed", 1L, prompt = preview, output = "done")
        assertEquals("r", terminalMatchFor(tail, "conv-1", listOf(row))?.id)
        assertEquals(AdoptOutcome.APPEND, adoptDecision(row))
    }
}
