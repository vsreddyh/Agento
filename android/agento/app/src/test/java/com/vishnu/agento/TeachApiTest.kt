package com.vishnu.agento

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Taught skills (#314): skill + list parsing.
 *
 * Lenient by contract — missing keys read blank, id-less rows are dropped
 * by the caller (`parseTaughtSkill` returns null), unparseable bodies fail
 * the list instead of wiping the screen.
 */
class TeachApiTest {

    @Test
    fun `parseTaughtSkill reads full record`() {
        val o = JSONObject(
            """{"id":"s1","profile":"chat","name":"weekly",""" +
                """"description":"Sends it","body":"# Weekly","status":"draft",""" +
                """"source":"thread-1","tested_at":0,"test_text":"",""" +
                """"created_at":7,"updated_at":9}""",
        )
        val skill = parseTaughtSkill(o)!!
        assertEquals("s1", skill.id)
        assertEquals("weekly", skill.name)
        assertTrue(skill.isDraft())
        assertFalse(skill.isActive())
        assertFalse(skill.isTested())
    }

    @Test
    fun `parseTaughtSkill reads description`() {
        val o = JSONObject("""{"id":"x","description":"Send it via prompt."}""")
        val described = parseTaughtSkill(o)!!
        assertEquals("Send it via prompt.", described.description)
    }

    @Test
    fun `parseTaughtSkill drops id-less rows and blanks`() {
        assertNull(parseTaughtSkill(JSONObject("""{"status":"draft"}""")))
        val skill = parseTaughtSkill(JSONObject("""{"id":"x"}"""))!!
        assertEquals("", skill.status)
        assertFalse(skill.isDraft())
    }

    @Test
    fun `parseSkillList pins leniency`() {
        val body = """{"skills":[
            {"id":"s1","name":"weekly","status":"draft"},
            {"status":"draft"},
            {"id":42,"name":"coerced"},
            {"id":"s2","name":"report","status":"active"}
            ]}"""
        val list = parseSkillList(body)!!
        assertEquals(2, list.size)
        assertEquals("s1", list[0].id)
        assertEquals("s2", list[1].id)
        assertEquals("active", list[1].status)
    }

    @Test
    fun `parseSkillList fails unparseable instead of wiping`() {
        assertNull(parseSkillList("not json"))
        val empty = parseSkillList("""{"skills":[]}""")!!
        assertTrue(empty.isEmpty())
    }

    @Test
    fun `parseSkillList fails missing key instead of wiping`() {
        assertNull(parseSkillList("""{"nope":[]}"""))
    }

    @Test
    fun `mapTestOutput fails missing output`() {
        assertEquals("ran", mapTestOutput(JSONObject("""{"output":"ran"}""")))
        assertEquals("", mapTestOutput(JSONObject("""{"output":""}""")))
        try {
            mapTestOutput(JSONObject("""{"nope":1}"""))
            throw AssertionError("missing output accepted")
        } catch (e: RuntimeException) {
            assertEquals("response had no output", e.message)
        }
    }

    @Test
    fun `mapTruncated passes the flag through`() {
        assertTrue(mapTruncated(JSONObject("""{"truncated":true}""")))
        assertFalse(mapTruncated(JSONObject("""{}""")))
    }
}
