package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Issue #433: toolsets/skills/session lists have one canonical shape each.
 * These tests pin the pure parsers: the canonical shapes parse, and the
 * retired `data`/`items` wrappers are failures naming the fix, never a
 * clean empty.
 */
class ServerApiShapeTest {

    @Test
    fun `skills canonical shapes parse`() {
        assertEquals(
            listOf("a"),
            parseServerSkillList("""{"skills":[{"name":"a"}]}""").map { it.name },
        )
        assertEquals(
            listOf("b"),
            parseServerSkillList("""[{"name":"b"}]""").map { it.name },
        )
    }

    @Test
    fun `skills retired envelopes fail loudly`() {
        for (body in listOf(
            """{"data":[{"name":"a"}]}""",
            """{"items":[{"name":"a"}]}""",
        )) {
            try {
                parseServerSkillList(body)
                throw AssertionError("must throw, not read as no skills")
            } catch (e: RuntimeException) {
                assertEquals(SKILLS_SHAPE_ERROR, e.message)
            }
        }
    }

    @Test
    fun `session transcript retired envelopes fail loudly`() {
        for (body in listOf(
            """{"data":[{"role":"assistant","content":"deep"}]}""",
            """{"items":[{"role":"assistant","content":"deep"}]}""",
        )) {
            try {
                parseLastAssistantText(body)
                throw AssertionError("must throw, not read as no transcript")
            } catch (e: RuntimeException) {
                assertEquals(SESSION_MESSAGES_SHAPE_ERROR, e.message)
            }
        }
    }

    @Test
    fun `explicit JSON nulls read as missing`() {
        // optJsonStr (#130): a bare optString would coerce a null name to
        // the literal string "null", defeating the id/slug fallback and
        // rendering a phantom row.
        assertEquals(
            listOf("real"),
            parseServerSkillList("""[{"name":null,"id":"real"}]""").map { it.name },
        )
        assertTrue(parseServerSkillList("""{"skills":[{"name":null}]}""").isEmpty())
    }
}
