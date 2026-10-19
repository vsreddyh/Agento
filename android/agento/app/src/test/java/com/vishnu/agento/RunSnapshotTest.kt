package com.vishnu.agento

import okio.Buffer
import org.json.JSONArray
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Resumable runs (#311): the `agento_run` snapshot frame.
 *
 * Every stream opens with exactly one `{"agento_run": {...}}` frame carrying
 * the run's accumulated output, status and sequence. A reattaching client
 * replaces its buffer with the snapshot and follows live deltas; a terminal
 * snapshot's replay deltas are skipped here, centrally, so neither the
 * emitted events nor the accumulated `Done` text double-count them.
 *
 * Fixture style mirrors [SseStreamEndTest]: payloads are bare JSON, `frame`
 * is the only place the `data: ` prefix is added.
 */
class RunSnapshotTest {

    private fun snapshot(
        id: String = "run-1",
        status: String = "running",
        output: String = "",
        reasoning: String = "",
        tools: List<String> = emptyList(),
        usage: JSONObject? = null,
    ): String {
        val o = JSONObject().put("id", id).put("status", status)
        if (output.isNotEmpty()) o.put("output", output)
        if (reasoning.isNotEmpty()) o.put("reasoning", reasoning)
        if (tools.isNotEmpty()) o.put("tools", JSONArray(tools))
        if (usage != null) o.put("usage", usage)
        return JSONObject().put("agento_run", o).toString()
    }

    private fun usageCounts(prompt: Long, completion: Long, total: Long) =
        JSONObject()
            .put("prompt_tokens", prompt)
            .put("completion_tokens", completion)
            .put("total_tokens", total)

    private fun delta(content: String) = JSONObject()
        .put("choices", JSONArray().put(JSONObject().put("delta", JSONObject().put("content", content))))
        .toString()

    private fun frame(payload: String) = "data: $payload"

    private fun streamOf(lines: List<String>) = lines.joinToString("\n", postfix = "\n")

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
    fun `snapshot emits RunState with parsed fields and no text`() {
        val raw = streamOf(
            listOf(
                frame(
                    snapshot(
                        output = "head",
                        tools = listOf("bash", "git"),
                        usage = usageCounts(10, 2, 12),
                    ),
                ),
                frame(delta(" tail")),
                "data: [DONE]",
            ),
        )
        val r = read(raw)
        val states = r.events.filterIsInstance<ChatEvent.RunState>()
        assertEquals(1, states.size)
        val snap = states.single().snapshot
        assertEquals("run-1", snap.id)
        assertEquals("running", snap.status)
        assertEquals("head", snap.output)
        assertEquals(listOf("bash", "git"), snap.tools)
        assertFalse(snap.isTerminal())
        // The head renders once: seeded from the snapshot, then the live delta.
        assertEquals("head tail", r.text)
        assertEquals(TokenUsage(prompt = 10, completion = 2, total = 12, cached = 0), r.usage)
    }

    @Test
    fun `terminal snapshot replay is skipped, not doubled`() {
        val raw = streamOf(
            listOf(
                frame(snapshot(status = "completed", output = "whole")),
                // Compatibility replay for older clients: the full output as
                // an ordinary delta. This client must not count it twice.
                frame(delta("whole")),
                "data: [DONE]",
            ),
        )
        val r = read(raw)
        assertTrue(r.events.any { it is ChatEvent.RunState })
        assertTrue(r.events.none { it is ChatEvent.Delta })
        assertEquals("whole", r.text)
    }

    @Test
    fun `second snapshot does not reseed the accumulators`() {
        val raw = streamOf(
            listOf(
                frame(snapshot(output = "head")),
                frame(delta(" live")),
                // Explicit cancelled status frame: status only, no content.
                frame(snapshot(status = "cancelled")),
                // A delta after the terminal frame stays skipped.
                frame(delta(" late")),
                "data: [DONE]",
            ),
        )
        val r = read(raw)
        assertEquals(2, r.events.filterIsInstance<ChatEvent.RunState>().size)
        assertEquals("head live", r.text)
        val last = r.events.filterIsInstance<ChatEvent.RunState>().last().snapshot
        assertEquals("cancelled", last.status)
        assertTrue(last.isTerminal())
    }

    @Test
    fun `runSnapshot parses leniently and JSONObject round-trips`() {
        val o = JSONObject("""{"id":"x","status":"running"}""")
        val snap = RunSnapshot.parse(o)
        assertEquals("x", snap.id)
        assertEquals("", snap.model)
        assertTrue(snap.tools.isEmpty())
        assertEquals(null, snap.usage)
    }
}
