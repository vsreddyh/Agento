package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Slash skill menu (#372): the trigger predicate, the chip parse, the
 * insert rewrite, the taught-row exclusion, and the payload parse that
 * carries `scope`/`status`. All pure — the picker, the renderer and the
 * fetch share these, so a drift in any one breaks loudly here.
 */
class SkillCommandTest {

    // ── Trigger ──────────────────────────────────────────────────────────

    @Test
    fun `bare slash and partial tokens trigger`() {
        assertEquals("", slashQuery("/"))
        assertEquals("no", slashQuery("/no"))
        assertEquals("notes", slashQuery("/notes"))
        assertEquals("notes", slashQuery("/notes "))
        assertEquals("notes", slashQuery("/notes summarise today"))
        // Case flows through to the case-insensitive matcher unlowered.
        assertEquals("Notes", slashQuery("/Notes"))
    }

    @Test
    fun `empty query lists everything`() {
        // "/" followed by whitespace: the first token is empty, which is
        // the empty query — backspacing to "/" restores the full list.
        assertEquals("", slashQuery("/ "))
    }

    @Test
    fun `mid-sentence slash never triggers`() {
        assertNull(slashQuery("run /notes today"))
        assertNull(slashQuery(" /notes"))
        assertNull(slashQuery(""))
        assertNull(slashQuery("notes"))
    }

    @Test
    fun `complete skill commands do not trigger`() {
        // Typed by hand or just inserted by the picker (trailing space):
        // the picker must not reopen over its own insertion.
        assertNull(slashQuery("/skill:notes"))
        assertNull(slashQuery("/skill:notes "))
        assertNull(slashQuery("/skill:notes summarise today"))
        assertNull(slashQuery("/skill:NOTES"))
    }

    @Test
    fun `explicit skill prefix still filters`() {
        // "/skill:" with no name is not a command — show everything, so a
        // user discovering the prefix sees what it takes.
        assertEquals("", slashQuery("/skill:"))
        // …but a complete `/skill:<name>` token never reopens the picker:
        // after a tap-insert (`/skill:notes `) it must stay closed while
        // arguments are typed, and a hand-typed command sends as-is.
        assertNull(slashQuery("/skill:not"))
        assertNull(slashQuery("/skill:not "))
    }

    // ── Chip parse ───────────────────────────────────────────────────────

    @Test
    fun `chip parses name and args`() {
        assertEquals(SkillInvocation("notes", ""), parseSkillCommand("/skill:notes"))
        assertEquals(
            SkillInvocation("notes", "summarise today"),
            parseSkillCommand("/skill:notes summarise today"),
        )
        assertEquals(
            SkillInvocation("my-skill_2", "x"),
            parseSkillCommand("/skill:my-skill_2 x"),
        )
    }

    @Test
    fun `chip rejects non-commands`() {
        // No name: plain text, never a chip.
        assertNull(parseSkillCommand("/skill:"))
        // Mid-sentence: plain text.
        assertNull(parseSkillCommand("run /skill:notes today"))
        // Bad characters: plain text, stays visible as typed. (A space
        // after a valid name is the args separator, not a bad character —
        // `/skill:no tes` invokes skill `no` with args `tes`.)
        assertNull(parseSkillCommand("/skill:na!me x"))
        assertNull(parseSkillCommand("/skill:notes!"))
        // Other slash forms: not skills, untouched.
        assertNull(parseSkillCommand("/notes"))
        assertNull(parseSkillCommand(""))
    }

    // ── Insert ───────────────────────────────────────────────────────────

    @Test
    fun `insert replaces the slash token`() {
        assertEquals("/skill:notes ", insertSkillCommand("/", "notes"))
        assertEquals("/skill:notes ", insertSkillCommand("/no", "notes"))
        assertEquals("/skill:notes ", insertSkillCommand("/notes ", "notes"))
    }

    @Test
    fun `insert keeps already-typed arguments`() {
        assertEquals(
            "/skill:notes summarise today",
            insertSkillCommand("/no summarise today", "notes"),
        )
    }

    // ── Taught exclusion ─────────────────────────────────────────────────

    @Test
    fun `taught rows are not invocable`() {
        val skills = listOf(
            SkillInfo(name = "notes", description = "Summarise"),
            SkillInfo(name = "draft", scope = "taught", status = "draft"),
            SkillInfo(name = "active", scope = "taught", status = "active"),
            SkillInfo(name = "off", enabled = false),
            SkillInfo(name = "unknown", enabled = null),
        )
        // A definitely-disabled row would burn a turn like a taught one;
        // an unknown toggle (older gateways) keeps the row.
        assertEquals(
            listOf(skills[0], skills[4]),
            invocableSkills(skills),
        )
    }

    // ── Payload parse ────────────────────────────────────────────────────

    @Test
    fun `skill list parses scope and status`() {
        val body = """
            [
              {"name": "notes", "description": "Summarise", "enabled": true},
              {"name": "scratch", "scope": "taught", "status": "draft"},
              "bare"
            ]
        """.trimIndent()
        val skills = parseServerSkillList(body)
        assertEquals(3, skills.size)
        // Sorted by name: bare, notes, scratch.
        assertEquals("bare", skills[0].name)
        assertEquals("", skills[0].scope)
        assertEquals("notes", skills[1].name)
        assertEquals("", skills[1].scope)
        assertEquals(true, skills[1].enabled)
        assertEquals("scratch", skills[2].name)
        assertEquals("taught", skills[2].scope)
        assertEquals("draft", skills[2].status)
        // The picker never offers the taught row.
        assertEquals(listOf(skills[0], skills[1]), invocableSkills(skills))
    }

    @Test
    fun `skill list accepts wrapped and bare shapes`() {
        assertEquals(
            listOf("b", "a").sorted(),
            parseServerSkillList("""{"skills": [{"name": "b"}, {"name": "a"}]}""").map { it.name },
        )
        assertEquals(
            listOf("x"),
            parseServerSkillList("""["x", " "]""").map { it.name },
        )
    }

    @Test
    fun `skill list matches name and description`() {
        val s = SkillInfo(name = "notes", description = "Summarise the day")
        assertTrue(s.matches(""))
        assertTrue(s.matches("note"))
        assertTrue(s.matches("DAY"))
        assertTrue(s.matches("summarise"))
    }
}
