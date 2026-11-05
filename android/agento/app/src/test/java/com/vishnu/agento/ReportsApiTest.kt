package com.vishnu.agento

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * ∃-Mail inbox parsing (#365).
 *
 * Lenient by contract — missing keys read blank, id-less rows are dropped
 * by the caller — so these tests pin the shapes the screen renders rather
 * than the network layer.
 */
class ReportsApiTest {

    @Test
    fun `parseReportSummary reads full record`() {
        val s = parseReportSummary(
            JSONObject(
                """{"id":"r1","title":"Q3 findings","createdAt":"2026-11-04T10:00:00Z",""" +
                    """"read":false,"starred":true,"preview":"First line"}""",
            ),
        )!!
        assertEquals("r1", s.id)
        assertEquals("Q3 findings", s.title)
        assertEquals("2026-11-04T10:00:00Z", s.createdAt)
        assertEquals(false, s.read)
        assertEquals(true, s.starred)
        assertEquals("First line", s.preview)
    }

    @Test
    fun `parseReportSummary drops id-less rows and blanks`() {
        assertNull(parseReportSummary(JSONObject("""{"title":"x"}""")))
        val s = parseReportSummary(JSONObject("""{"id":"r1"}"""))!!
        assertEquals("", s.title)
        assertEquals(false, s.read)
        assertEquals(false, s.starred)
    }

    @Test
    fun `parseReportSegment keeps text and math shapes`() {
        val text = parseReportSegment(
            JSONObject("""{"kind":"text","text":"Hello **world**"}"""),
        )!!
        assertEquals("text", text.kind)
        assertEquals("Hello **world**", text.text)
        assertEquals("", text.html)

        val math = parseReportSegment(
            JSONObject(
                """{"kind":"math","tex":"x^2","html":"<span class=\"katex\">x</span>","display":true}""",
            ),
        )!!
        assertEquals("math", math.kind)
        assertEquals("x^2", math.tex)
        assertTrue(math.display)
        assertTrue(math.html.contains("katex"))
    }

    @Test
    fun `parseReportSegment drops unknown kinds`() {
        assertNull(parseReportSegment(JSONObject("""{"kind":"video","text":"x"}""")))
        assertNull(parseReportSegment(JSONObject("""{"text":"x"}""")))
    }

    @Test
    fun `parseReportDetail reads segments and engine`() {
        val d = parseReportDetail(
            JSONObject(
                """{"id":"r1","title":"T","body_md":"a ${'$'}x${'$'} b","math_engine":"katex 0.16.21",""" +
                    """"segments":[{"kind":"text","text":"a "},""" +
                    """{"kind":"math","tex":"x","html":"<span>k</span>"}]}""",
            ),
        )!!
        assertEquals("r1", d.id)
        assertEquals("a ${'$'}x${'$'} b", d.bodyMd)
        assertEquals("katex 0.16.21", d.mathEngine)
        assertEquals(2, d.segments.size)
        assertEquals("text", d.segments[0].kind)
        assertEquals("math", d.segments[1].kind)
    }

    @Test
    fun `parseReportDetail tolerates missing segments`() {
        val d = parseReportDetail(JSONObject("""{"id":"r1","body_md":"plain"}"""))!!
        assertTrue(d.segments.isEmpty())
        assertEquals("plain", d.bodyMd)
        assertNull(parseReportDetail(JSONObject("""{"title":"no id"}""")))
    }

    @Test
    fun `formatReportTime reads server millis`() {
        // 1762345678901 = 5 Nov 2026 17:57 IST. The server emits BSON
        // DateTime as millis; an earlier test fed ISO only, which parses but
        // never appears on the wire — production showed the raw number.
        assertEquals("5 Nov 17:57", formatReportTime("1762345678901"))
    }

    @Test
    fun `formatReportTime falls back to ISO then raw`() {
        assertEquals("4 Nov 15:30", formatReportTime("2026-11-04T10:00:00Z"))
        assertEquals("", formatReportTime("  "))
        assertEquals("not-a-date", formatReportTime("not-a-date"))
    }
}
