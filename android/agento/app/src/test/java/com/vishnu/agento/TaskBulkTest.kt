package com.vishnu.agento

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

/**
 * Bulk selection and batch plumbing (#345): the selection toggle with its
 * cap, the batch request body, and the failing-op envelope the batch
 * endpoint answers with.
 */
class TaskBulkTest {

    @Test
    fun `toggle adds a missing id and removes a present one`() {
        assertEquals(setOf("a"), toggleBulkId(emptySet(), "a"))
        assertEquals(setOf("a", "b"), toggleBulkId(setOf("a"), "b"))
        assertEquals(emptySet<String>(), toggleBulkId(setOf("a"), "a"))
    }

    @Test
    fun `toggle refuses past the cap but still removes`() {
        val full = (0 until MAX_BULK_SELECT).map { "id$it" }.toSet()
        assertNull(toggleBulkId(full, "one-more"))
        assertEquals(full - "id0", toggleBulkId(full, "id0"))
    }

    @Test
    fun `toggle refuses blank ids`() {
        assertEquals(setOf("a"), toggleBulkId(setOf("a"), "   "))
        assertEquals(emptySet<String>(), toggleBulkId(emptySet(), ""))
    }

    @Test
    fun `bulk body carries ops verbatim`() {
        val body = bulkOpsBody(
            listOf(
                BulkOp("complete", "a"),
                BulkOp("skip", "b", "busy"),
            ),
        )
        val ops = body.getJSONArray("ops")
        assertEquals(2, ops.length())
        assertEquals("complete", ops.getJSONObject(0).getString("op"))
        assertEquals("a", ops.getJSONObject(0).getString("id"))
        assertEquals("busy", ops.getJSONObject(1).getString("reason"))
    }

    @Test
    fun `bulk body carries update fields`() {
        val body = bulkOpsBody(
            listOf(BulkOp("update", "a", null, JSONObject().put("due_date", "2026-12-01"))),
        )
        val fields = body.getJSONArray("ops").getJSONObject(0).getJSONObject("fields")
        assertEquals("2026-12-01", fields.getString("due_date"))
    }

    @Test
    fun `batch failure parses the envelope`() {
        val f = parseBatchError(
            """HTTP 409: {"detail": "conflict: task changed", "index": 2, "id": "abc", "op": "update"}""",
        )
        assertEquals("conflict: task changed", f?.detail)
        assertEquals(2, f?.index)
        assertEquals("abc", f?.id)
        assertEquals("update", f?.op)
    }

    @Test
    fun `batch failure is null without an envelope`() {
        assertNull(parseBatchError("HTTP 503: upstream connect error"))
        assertNull(parseBatchError("""HTTP 422: {"detail": ""}"""))
        assertNull(parseBatchError("HTTP 404: not found"))
    }
}
