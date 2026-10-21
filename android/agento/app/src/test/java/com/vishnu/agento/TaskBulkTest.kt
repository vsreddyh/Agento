package com.vishnu.agento

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
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
    fun `bulk bar is live only with a selection and nothing busy`() {
        assertFalse(bulkActionsLive(emptySet(), emptySet()))
        assertTrue(bulkActionsLive(setOf("a"), emptySet()))
        assertFalse(bulkActionsLive(setOf("a"), setOf("a")))
        assertTrue(bulkActionsLive(setOf("a", "b"), setOf("c")))
        assertFalse(bulkActionsLive(setOf("a", "b"), setOf("b")))
    }

    @Test
    fun `toggle trims ids`() {
        assertEquals(setOf("a"), toggleBulkId(emptySet(), "  a  "))
        assertEquals(emptySet<String>(), toggleBulkId(setOf("a"), "a "))
    }

    @Test
    fun `toggle refuses blank ids`() {
        assertEquals(setOf("a"), toggleBulkId(setOf("a"), "   "))
        assertEquals(emptySet<String>(), toggleBulkId(emptySet(), ""))
    }

    @Test(expected = IllegalArgumentException::class)
    fun `bulk op rejects reason on non-skip`() {
        BulkOp("complete", "a", "busy")
    }

    @Test(expected = IllegalArgumentException::class)
    fun `bulk op rejects fields on non-update`() {
        BulkOp("complete", "a", null, JSONObject().put("due_date", "2026-12-01"))
    }

    @Test
    fun `bulk op allows blank extras like the server does`() {
        BulkOp("complete", "a", "   ")
        BulkOp("complete", "a", null, JSONObject())
        BulkOp("complete", "a", null, JSONObject().put("due_date", JSONObject.NULL))
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
    fun `bulk body trims reason and omits blanks`() {
        val body = bulkOpsBody(listOf(BulkOp("skip", "a", "  busy  ")))
        assertEquals("busy", body.getJSONArray("ops").getJSONObject(0).getString("reason"))
        val blank = bulkOpsBody(listOf(BulkOp("skip", "a", "   ")))
        assertFalse(blank.getJSONArray("ops").getJSONObject(0).has("reason"))
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
    fun `bulk update sent reads present fields only`() {
        val sent = bulkUpdateSent(
            JSONObject().put("due_date", "2026-12-01").put("expected_revision", 3),
        )
        assertEquals("2026-12-01", sent.dueDate)
        assertNull(sent.name)
        assertNull(sent.repeatCustom)
    }

    @Test
    fun `bulk unknown fields names only real extras`() {
        val fields = JSONObject().put("due_date", "2026-12-01")
            .put("expected_revision", 3)
            .put("due_time", JSONObject.NULL)
            .put("someday_field", "x")
        assertEquals(listOf("someday_field"), bulkUnknownFields(fields))
        assertTrue(bulkUnknownFields(JSONObject()).isEmpty())
    }

    @Test
    fun `bulk update sent canonicalises days and reads numbers`() {
        val sent = bulkUpdateSent(
            JSONObject().put("repeat_days", org.json.JSONArray(listOf("Wed", "mon")))
                .put("estimated_minutes", 15)
                .put("repeat_custom", true),
        )
        assertEquals(listOf("mon", "wed"), sent.repeatDays)
        assertEquals(15, sent.estimatedMinutes)
        assertEquals(true, sent.repeatCustom)
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
