package com.vishnu.agento

import okio.Buffer
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * ask_user relay (#373): the `agento_question` frame parses into a
 * [ChatEvent.Question] that carries no text, no usage and no output —
 * the card is not a transcript row. The attach gate pins which thread a
 * frame belongs to, because the ViewModel itself needs a Context and
 * cannot run here: the pure rule is what the tests can hold.
 */
class QuestionFrameTest {

    private fun frame(payload: String) = "data: $payload"

    private fun questionPayload(
        id: String = "q-1",
        conversation: String = "conv-1",
        method: String = "select",
        title: String = "Which account?",
        options: String = """"options":["Joint","Personal"]""",
    ) = """{"agento_question":{"id":"$id","conversation_id":"$conversation","method":"$method","title":"$title",$options,"created_at":1730000000000}}"""

    private fun read(raw: String): Pair<String, List<ChatEvent>> {
        val full = StringBuilder()
        val reasoned = StringBuilder()
        val events = mutableListOf<ChatEvent>()
        readSseStream(
            source = Buffer().writeUtf8(raw),
            full = full,
            reasoned = reasoned,
            onUsage = {},
            emit = { events += it },
        )
        return full.toString() to events
    }

    @Test
    fun `question frame emits a Question event with all fields`() {
        val (text, events) = read(frame(questionPayload()) + "\n")
        val questions = events.filterIsInstance<ChatEvent.Question>()
        assertEquals(1, questions.size)
        val q = questions[0].question
        assertEquals("q-1", q.id)
        assertEquals("conv-1", q.conversationId)
        assertEquals("select", q.method)
        assertEquals("Which account?", q.title)
        assertEquals(listOf("Joint", "Personal"), q.options)
        assertTrue(q.hasOptions())
        // And nothing else: no text accumulated, no delta emitted.
        assertEquals("", text)
        assertTrue(events.none { it is ChatEvent.Delta })
    }

    @Test
    fun `input method parses without options`() {
        val payload = questionPayload(method = "input", options = """"placeholder":"Type here"""")
        val (_, events) = read(frame(payload) + "\n")
        val q = events.filterIsInstance<ChatEvent.Question>().single().question
        assertEquals("input", q.method)
        assertTrue(q.options.isEmpty())
        assertFalse(q.hasOptions())
        assertEquals("Type here", q.placeholder)
    }

    @Test
    fun `id-less frames are dropped`() {
        val (text, events) = read(frame(questionPayload(id = "")) + "\n")
        assertTrue(events.none { it is ChatEvent.Question })
        assertEquals("", text)
    }

    @Test
    fun `question survives around text and snapshot frames`() {
        val lines = listOf(
            frame("""{"choices":[{"delta":{"content":"Let me ask"}}]}"""),
            frame(questionPayload()),
            frame("""{"agento_run":{"id":"r","status":"running","seq":3,"tools":[]}}"""),
            "data: [DONE]",
        ).joinToString("\n", postfix = "\n")
        val (text, events) = read(lines)
        assertEquals("Let me ask", text)
        assertEquals(1, events.filterIsInstance<ChatEvent.Question>().size)
        assertEquals(1, events.filterIsInstance<ChatEvent.RunState>().size)
    }

    @Test
    fun `attach gate keys on id and conversation`() {
        val q = PendingQuestion(id = "q", conversationId = "c", method = "select", title = "t")
        assertTrue(questionAttaches(q, "c"))
        assertFalse(questionAttaches(q, "other"))
        assertFalse(questionAttaches(q, ""))
        assertFalse(questionAttaches(q.copy(conversationId = ""), "c"))
        assertFalse(questionAttaches(q.copy(id = ""), "c"))
    }

    @Test
    fun `parse is lenient and drops blank ids`() {
        assertNull(PendingQuestion.parse(JSONObject("""{"method":"select"}""")))
        val q = PendingQuestion.parse(
            JSONObject("""{"id":"q","conversation_id":"c","method":"input","title":"T"}"""),
        )!!
        assertEquals("q", q.id)
        assertEquals("c", q.conversationId)
        assertEquals("input", q.method)
        assertTrue(q.options.isEmpty())
        assertFalse(q.hasOptions())
        // Blank option entries are filtered, not rendered as empty buttons.
        val withBlanks = PendingQuestion.parse(
            JSONObject("""{"id":"q","options":["a","","b"]}"""),
        )!!
        assertEquals(listOf("a", "b"), withBlanks.options)
    }
}
