package com.vishnu.agento

import okio.Buffer
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * #214: a dropped connection must not be reported as a completed turn.
 *
 * The bug was that the SSE read loop exited on EOF exactly as it did on `[DONE]`, and
 * both paths fell through to one `ChatEvent.Done`. A turn cut off mid-flight — after
 * its tool calls had already run server-side — therefore arrived at the UI as a
 * finished reply, and the user resent it, duplicating work that had already happened.
 *
 * These tests feed raw stream bytes to the extracted reader, so they measure the parse
 * that actually decides the outcome rather than a helper beside it. The truncated case
 * is the regression: it is byte-for-byte a real stream minus its last two lines.
 */
class SseStreamEndTest {

    private fun delta(content: String) =
        """data: {"choices":[{"delta":{"content":"$content"}}]}"""

    /** A well-formed stream: content frames, a usage frame, then the terminal `[DONE]`. */
    private fun completeStream() = """
        : keepalive
        event: ping
        ${delta("Hello")}
        ${delta(" world")}
        data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}
        data: [DONE]

    """.trimIndent() + "\n"

    /** The same stream with the terminal frame and the trailing newline removed. */
    private fun truncatedStream() = completeStream()
        .removeSuffix("data: [DONE]\n")
        .removeSuffix("\n")

    private class Result(
        val end: SseEnd,
        val text: String,
        val events: List<ChatEvent>,
        val usage: TokenUsage?,
    )

    private fun read(raw: String): Result {
        val full = StringBuilder()
        val reasoned = StringBuilder()
        val events = mutableListOf<ChatEvent>()
        var usage: TokenUsage? = null
        val end = readSseStream(
            source = Buffer().writeUtf8(raw),
            full = full,
            reasoned = reasoned,
            onUsage = { usage = it },
            emit = { events += it },
        )
        return Result(end, full.toString(), events, usage)
    }

    /**
     * Guards the fixture itself, which is the layer that decides whether any of these
     * tests mean anything.
     *
     * The first run of this file failed 5 of 8 tests and the cause was NOT the parser:
     * `org.json` is stubbed to throw "not mocked" on the unit-test classpath, and
     * `readSseStream` wraps each frame in `runCatching`, so every `JSONObject` call
     * failed silently. `[DONE]` still resolved correctly because it is a plain string
     * compare that touches no JSON — so the failures had the shape of a parser bug
     * (`[DONE]` found, text empty) rather than a missing dependency.
     *
     * A JSON-parsing test that can pass while the JSON parser is a stub is not testing
     * anything, so assert that the parser is real before trusting the rest.
     */
    /**
     * The truncated fixture must genuinely lack the terminal frame.
     *
     * It failed this on the first run too: `trimIndent()` left the trailing blank line
     * in, the appended newline produced `[DONE]\n\n`, and the first `removeSuffix`
     * stripped only one of the two newlines — so the "truncated" stream still ended
     * with `[DONE]` and the regression test quietly tested nothing.
     */
    @Test
    fun `the truncated fixture really is truncated`() {
        assertTrue(
            "the truncated fixture still contains [DONE], so the regression test is vacuous",
            !truncatedStream().contains("[DONE]"),
        )
        assertTrue(
            "the truncated fixture must keep the content it did receive",
            truncatedStream().contains("Hello"),
        )
    }

    @Test
    fun `org.json is a real implementation, not an android stub`() {
        val o = JSONObject("""{"choices":[{"delta":{"content":"hi"}}]}""")
        assertEquals(
            "org.json is stubbed to throw or return defaults on the unit-test classpath; " +
                "every other test in this file silently passes no frames through it",
            "hi",
            o.optJSONArray("choices")!!.optJSONObject(0)!!
                .optJSONObject("delta")!!.optString("content"),
        )
    }

    @Test
    fun `a stream ending in DONE is terminal and not interrupted`() {
        val r = read(completeStream())
        assertEquals(SseEnd.Terminal, r.end)
        assertEquals("Hello world", r.text)
        assertEquals(15L, r.usage?.total)
    }

    @Test
    fun `a stream cut off before DONE is reported as end of stream, not terminal`() {
        val r = read(truncatedStream())
        assertEquals(
            "EOF with no [DONE] must be EndOfStream, not Terminal — that distinction " +
                "is the entire fix for #214",
            SseEnd.EndOfStream,
            r.end,
        )
    }

    @Test
    fun `an interrupted turn keeps the text it did receive`() {
        // The partial reply is real output. Discarding it would throw away work the
        // user can see; the flag is what tells the UI to label it as cut short.
        val r = read(truncatedStream())
        assertEquals("Hello world", r.text)
        assertEquals(2, r.events.count { it is ChatEvent.Delta })
    }

    @Test
    fun `an empty stream is end of stream, never a completed empty turn`() {
        val r = read("")
        assertEquals(SseEnd.EndOfStream, r.end)
        assertEquals("", r.text)
    }

    @Test
    fun `a stream that is only a keepalive is end of stream`() {
        val r = read(": keepalive\n\n")
        assertEquals(SseEnd.EndOfStream, r.end)
    }

    @Test
    fun `a gateway error frame is terminal and carries its message`() {
        val raw = """
            ${delta("partial")}
            data: {"error":"SSE client disconnected"}
        """.trimIndent() + "\n"
        val r = read(raw)
        assertTrue(
            "an error frame must win over the EOF that follows it, or the user sees " +
                "a bogus empty reply for a failed turn",
            r.end is SseEnd.ErrorFrame,
        )
        assertEquals("SSE client disconnected", (r.end as SseEnd.ErrorFrame).message)
        // Content before the error is still delivered.
        assertEquals("partial", r.text)
    }

    @Test
    fun `tool progress frames survive a truncation`() {
        // The real failure in #214 happened on a turn that had already run tool calls,
        // so the partial turn carried tool frames. They must not be dropped on the way
        // to reporting the interruption.
        val raw = """
            data: {"tool":"bash","status":"start"}
            ${delta("running")}
        """.trimIndent() + "\n"
        val r = read(raw)
        assertEquals(SseEnd.EndOfStream, r.end)
        assertEquals(1, r.events.count { it is ChatEvent.ToolProgress })
    }

    @Test
    fun `DONE after content is terminal regardless of trailing whitespace`() {
        val r = read("${delta("hi")}\ndata: [DONE]\n\n\n")
        assertEquals(SseEnd.Terminal, r.end)
    }
}

/**
 * The call-site mapping — the line #214 was actually broken at.
 *
 * `SseStreamEndTest` above locks `readSseStream`, which was never the bug: it correctly
 * distinguished `EndOfStream` from `Terminal`, and the producer then emitted an
 * unqualified `ChatEvent.Done` and dropped the distinction on the floor. A suite that
 * tests only the parser looks thorough and leaves the real defect uncovered, which is
 * how the original shipped.
 */
class DoneMappingTest {

    private val usage = TokenUsage(prompt = 10, completion = 5, total = 15)

    @Test
    fun `end of stream maps to an interrupted Done`() {
        val e = doneFor(SseEnd.EndOfStream, "partial", usage) as ChatEvent.Done
        assertTrue("a dropped stream must not report a finished turn", e.interrupted)
    }

    @Test
    fun `terminal maps to a non-interrupted Done`() {
        val e = doneFor(SseEnd.Terminal, "complete", usage) as ChatEvent.Done
        assertTrue("a finished turn must not be flagged interrupted", !e.interrupted)
    }

    @Test
    fun `the text and usage survive the mapping`() {
        // The flag must not cost the user anything: the partial reply and the reported
        // counts are still real, and dropping them here would discard the turn.
        val e = doneFor(SseEnd.EndOfStream, "partial", usage) as ChatEvent.Done
        assertEquals("partial", e.fullText)
        assertEquals(usage, e.usage)
    }

    @Test
    fun `an error frame has no Done mapping`() {
        // It yields ChatEvent.Error at the call site instead, so reaching doneFor with
        // one is a programming error rather than something to paper over.
        val threw = runCatching { doneFor(SseEnd.ErrorFrame("boom"), "", null) }
        assertTrue("an error frame must not be mappable to Done", threw.isFailure)
    }

    @Test
    fun `the two ends print as names, not identity hashes`() {
        // data object, so a log line or an assertion failure reads
        // `expected:<EndOfStream> but was:<Terminal>` instead of two @1a2b3c values —
        // which is what made the first CI failure of this PR hard to read.
        assertEquals("Terminal", SseEnd.Terminal.toString())
        assertEquals("EndOfStream", SseEnd.EndOfStream.toString())
    }
}
