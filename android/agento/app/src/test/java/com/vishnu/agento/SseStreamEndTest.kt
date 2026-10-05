package com.vishnu.agento

import okio.Buffer
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
