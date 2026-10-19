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
}
