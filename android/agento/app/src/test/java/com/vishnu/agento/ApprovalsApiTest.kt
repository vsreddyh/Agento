package com.vishnu.agento

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Approval gates (#313): approval + audit parsing.
 *
 * Lenient by contract — missing keys read blank, id-less rows are dropped
 * by the caller (`parseApproval` returns null) — so these tests pin the
 * shapes the sheet renders rather than the network layer.
 */
class ApprovalsApiTest {

    @Test
    fun `parseApproval reads full record`() {
        val o = JSONObject(
            """{"id":"a1","server":"notes","tool":"delete_note",""" +
                """"args":{"id":"abc"},"status":"pending","created_at":7}""",
        )
        val a = parseApproval(o)!!
        assertEquals("a1", a.id)
        assertEquals("notes", a.server)
        assertEquals("delete_note", a.tool)
        assertEquals(mapOf("id" to "abc"), a.args)
        assertEquals("pending", a.status)
        assertTrue(a.isPending())
        assertEquals("id=abc", a.argsSummary())
    }

    @Test
    fun `parseApproval drops id-less rows and blanks`() {
        assertNull(parseApproval(JSONObject("""{"status":"pending"}""")))
        val a = parseApproval(JSONObject("""{"id":"x"}"""))!!
        assertEquals("", a.status)
        assertEquals("", a.argsSummary())
        assertTrue(a.args.isEmpty())
    }

    @Test
    fun `parseAuditRow reads dispositions`() {
        val o = JSONObject(
            """{"id":"e1","server":"miser-money","tool":"log_transaction",""" +
                """"disposition":"denied","error":"denied by user","at":9}""",
        )
        val row = parseAuditRow(o)!!
        assertEquals("denied", row.disposition)
        assertEquals("denied by user", row.error)
        assertEquals(9L, row.at)
        assertNull(parseAuditRow(JSONObject("""{"tool":"x"}""")))
    }

    @Test
    fun `argsSummary sorts keys and caps at 200`() {
        val a = GateApproval(
            id = "x",
            args = mapOf("z" to "1", "a" to "2", "m" to "x".repeat(300)),
        )
        val summary = a.argsSummary()
        assertTrue(summary.startsWith("a=2, m="))
        assertTrue(summary.length <= 200)
    }

    @Test
    fun `relativeAgo and waitingLabel name ages and clamp skew`() {
        val now = 1_000_000_000L
        assertEquals("", relativeAgo(0, now))
        assertEquals("just now", relativeAgo(now + 5_000, now))
        assertEquals("2m ago", relativeAgo(now - 120_000, now))
        assertEquals("2h ago", relativeAgo(now - 2 * 3_600_000, now))
        assertEquals("3d ago", relativeAgo(now - 3 * 86_400_000, now))
        assertEquals("just now", waitingLabel(now, now))
        assertEquals("2m waiting", waitingLabel(now - 120_000, now))
        assertEquals("", waitingLabel(0, now))
    }

    @Test
    fun `encode keeps ids exact and httpCode names the code`() {
        assertEquals("a%20b", ApprovalsApi.encode("a b"))
        assertEquals("abc", ApprovalsApi.encode("abc"))
        assertEquals("409", ApprovalsApi.httpCode("HTTP 409: conflict"))
        assertEquals("500", ApprovalsApi.httpCode("HTTP 500: boom"))
        assertEquals(null, ApprovalsApi.httpCode("weird"))
        assertEquals(null, ApprovalsApi.httpCode(null))
    }

    @Test
    fun `parseApprovalList carries degraded flag`() {
        val degraded = parseApprovalList(
            """{"approvals":[],"degraded":"store is not shared"}""",
        )
        assertTrue(degraded.approvals.isEmpty())
        assertTrue(degraded.degraded)
        val healthy = parseApprovalList("""{"approvals":[]}""")
        assertFalse(healthy.degraded)
    }
}
