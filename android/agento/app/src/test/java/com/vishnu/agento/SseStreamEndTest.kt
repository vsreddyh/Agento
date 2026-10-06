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

    /**
     * A content frame's JSON PAYLOAD, with no `data: ` prefix.
     *
     * Deliberately a bare payload: this originally returned the whole `data: {...}` line
     * and `frame()` prepends `data: `, so every content line came out as
     * `data: data: {...}`. The parser stripped one prefix, handed `data: {...}` to
     * `JSONObject`, and `runCatching` swallowed the failure — so the text assertions
     * read `expected:<Hello world> but was:<>` while the truncation tests, which assert
     * only the SseEnd, passed happily.
     *
     * That is the same shape as the trimIndent bug: a malformed fixture that the
     * assertions happened not to reach. `frame` is the ONLY place a prefix is added.
     */
    private fun delta(content: String) =
        """{"choices":[{"delta":{"content":"$content"}}]}"""

    /**
     * The stream, as a list of lines.
     *
     * A LIST rather than a raw string with `trimIndent()`, because that is what made this
     * fixture wrong twice. `trimIndent()` on a raw string with a blank line before the
     * closing quotes keeps a trailing newline, so `... + "\n"` produced
     * `data: [DONE]\n\n` and a single `removeSuffix("data: [DONE]\n")` stripped one
     * newline — leaving the "truncated" stream ending in `[DONE]`. The central
     * regression test then asserted nothing and still looked like it was testing the
     * fix. `dropLast(1)` cannot be wrong that way.
     */
    private val completeLines = listOf(
        ": keepalive",
        "event: ping",
        frame(delta("Hello")),
        frame(delta(" world")),
        frame("""{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}"""),
        "data: [DONE]",
    )

    /** Prefixes a JSON payload as one SSE data line. The only place this happens. */
    private fun frame(payload: String) = "data: $payload"

    private fun streamOf(lines: List<String>) = lines.joinToString("\n", postfix = "\n")

    /** A well-formed stream: content frames, a usage frame, then the terminal `[DONE]`. */
    private fun completeStream() = streamOf(completeLines)

    /** The same stream with the terminal frame removed. */
    private fun truncatedStream() = streamOf(completeLines.dropLast(1))

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
    /**
     * Every data line must carry exactly ONE `data: ` prefix.
     *
     * The fixture was briefly emitting `data: data: {...}` — `delta()` returned the whole
     * SSE line and `frame()` added the prefix again. The parser stripped one, handed
     * `data: {...}` to `JSONObject`, and `runCatching` swallowed the throw, so the text
     * assertions failed with an empty string while every test that asserts only the
     * SseEnd passed. A malformed fixture that the assertions happen not to reach is the
     * same failure twice in a row (the `trimIndent` one before it), so it gets its own
     * assertion rather than being left to whichever test happens to notice.
     */
    @Test
    fun `no fixture line has a doubled data prefix`() {
        val lines = completeLines +
            listOf(frame(delta("x")), frame("""{"tool":"bash"}"""), ": keepalive", "event: ping")
        lines.forEach { line ->
            assertTrue(
                "doubled data: prefix in fixture line: $line",
                !line.startsWith("data: data:"),
            )
        }
    }

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
    fun `the org json parser is a real implementation, not an android stub`() {
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
        val r = read(
            streamOf(listOf(frame(delta("partial")), frame("""{"error":"SSE client disconnected"}"""))),
        )
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
        val r = read(
            streamOf(listOf(frame("""{"tool":"bash","status":"start"}"""), frame(delta("running")))),
        )
        assertEquals(SseEnd.EndOfStream, r.end)
        assertEquals(1, r.events.count { it is ChatEvent.ToolProgress })
    }

    @Test
    fun `DONE after content is terminal regardless of trailing whitespace`() {
        val r = read(streamOf(listOf(frame(delta("hi")), "data: [DONE]")) + "\n\n")
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
        val e = doneFor(SseEnd.EndOfStream, "partial", usage)
        assertTrue("a dropped stream must not report a finished turn", e.interrupted)
    }

    @Test
    fun `terminal maps to a non-interrupted Done`() {
        val e = doneFor(SseEnd.Terminal, "complete", usage)
        assertTrue("a finished turn must not be flagged interrupted", !e.interrupted)
    }

    @Test
    fun `the text and usage survive the mapping`() {
        // The flag must not cost the user anything: the partial reply and the reported
        // counts are still real, and dropping them here would discard the turn.
        val e = doneFor(SseEnd.EndOfStream, "partial", usage)
        assertEquals("partial", e.fullText)
        assertEquals(usage, e.usage)
    }

    /**
     * An empty body is still an interrupted turn, not a completed empty one.
     *
     * pi-gateway sends `[DONE]` for every turn that completes, so a body with no `[DONE]`
     * is one that never finished — including the empty-body case, where the connection
     * died before a single frame arrived. Reporting that as a completed empty turn gives
     * a silent blank reply with no indication anything went wrong, which is the exact
     * failure #214 is about. "Connection lost" with no text is more honest than silence.
     *
     * Pinned deliberately: it is a judgement call, and a judgement call that is not
     * pinned becomes an accident the first time someone tidies the EOF path.
     */
    @Test
    fun `an empty body maps to interrupted, not to a completed empty turn`() {
        val e = doneFor(SseEnd.EndOfStream, "", null)
        assertTrue("an unfinished turn must not read as finished", e.interrupted)
        assertEquals("", e.fullText)
        assertEquals(null, e.usage)
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

/**
 * The `InterruptedBy` wire format must not move.
 *
 * The constants are SCREAMING_CASE for Kotlin convention, but saved threads carry the
 * lowercase key. If the storage format ever changed, every existing thread would silently
 * fall back to the default and a dropped turn would be labelled as a user stop — a
 * wrong label on real history, with nothing to indicate it.
 */
class InterruptedByStorageTest {

    @Test
    fun `the wire keys are unchanged`() {
        assertEquals("user", InterruptedBy.USER.storageKey)
        assertEquals("drop", InterruptedBy.DROP.storageKey)
    }

    @Test
    fun `every constant has a distinct key`() {
        val keys = InterruptedBy.entries.map { it.storageKey }
        assertEquals(keys.size, keys.toSet().size)
    }

    @Test
    fun `a stored key round-trips to its constant and back`() {
        for (by in InterruptedBy.entries) {
            val stored = by.storageKey
            val parsed = InterruptedBy.entries.firstOrNull { it.storageKey == stored }
            assertEquals(by, parsed)
            assertEquals(stored, parsed!!.storageKey)
        }
    }

    @Test
    fun `an unrecognised stored key is not silently accepted as a real value`() {
        // Any key outside the known set has no constant, which is what makes the
        // defensive fallback in ChatThreads reachable at all.
        assertEquals(null, InterruptedBy.entries.firstOrNull { it.storageKey == "cancelled" })
    }
}

/**
 * A mid-body `IOException` is a DROP, not a failed request.
 *
 * The `ChatEvent.Error` branch keeps partial text but never sets `interrupted`, so routing
 * a socket reset there persists the fragment as a COMPLETE assistant reply — #214's defect
 * on the path likelier to occur in the field than a clean EOF.
 */
class MidStreamIoFailureTest {

    /** A source that yields [lines] and then throws, as a reset socket does. */
    private class ExplodingSource(lines: List<String>) : okio.ForwardingSource(
        Buffer().writeUtf8(lines.joinToString("\n", postfix = "\n")),
    ) {
        private var served = false
        override fun read(sink: Buffer, byteCount: Long): Long {
            if (!served) {
                served = true
                return super.read(sink, byteCount)
            }
            throw IOException("connection reset")
        }
    }

    private val partial = listOf(
        "data: {\"choices\":[{\"delta\":{\"content\":\"Half a \"}}]}",
        "data: {\"choices\":[{\"delta\":{\"content\":\"thought\"}}]}",
    )

    @Test
    fun `a mid-body IOException reports EndOfStream not an exception`() {
        // The uncaught case is the point: without the narrow catch this throws out of the
        // reader and the test fails here rather than at the assert.
        val full = StringBuilder()
        val end = readSseStreamLenient(
            source = okio.buffer(ExplodingSource(partial)),
            full = full,
            reasoned = StringBuilder(),
            onUsage = {},
            emit = {},
        )
        assertEquals(SseEnd.EndOfStream, end)
    }

    @Test
    fun `the text received before the reset is kept`() {
        // The fragment is real output. Losing it is the other half of the bug — the user
        // would have to guess what the agent had already said.
        val full = StringBuilder()
        readSseStreamLenient(
            source = okio.buffer(ExplodingSource(partial)),
            full = full,
            reasoned = StringBuilder(),
            onUsage = {},
            emit = {},
        )
        assertEquals("Half a thought", full.toString())
    }

    @Test
    fun `EndOfStream maps to an interrupted Done`() {
        // End-to-end over the two halves, because the defect lived in the SEAM: the reader
        // returning the right thing and the producer reporting it wrong.
        val done = doneFor(SseEnd.EndOfStream, "Half a thought", null) as ChatEvent.Done
        assertTrue("the fragment must not be reported as finished", done.interrupted)
    }
}
