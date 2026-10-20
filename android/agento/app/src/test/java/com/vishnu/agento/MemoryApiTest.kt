package com.vishnu.agento

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Per-profile memory (#315): item + list parsing.
 *
 * Lenient by contract — missing keys read blank, id-less rows are dropped
 * by the caller (`parseMemoryItem` returns null) — so these tests pin the
 * shapes the sheet renders rather than the network layer.
 */
class MemoryApiTest {

    @Test
    fun `parseMemoryItem reads full record`() {
        val o = JSONObject(
            """{"id":"m1","profile":"chat","topic":"work-style",""" +
                """"text":"Prefers tables.","source":"agent","status":"pending",""" +
                """"created_at":7,"updated_at":9}""",
        )
        val item = parseMemoryItem(o)!!
        assertEquals("m1", item.id)
        assertEquals("work-style", item.topic)
        assertEquals("Prefers tables.", item.text)
        assertEquals("agent", item.source)
        assertTrue(item.isPending())
        assertFalse(item.isConfirmed())
    }

    @Test
    fun `parseMemoryItem drops id-less rows and blanks`() {
        assertNull(parseMemoryItem(JSONObject("""{"status":"pending"}""")))
        val item = parseMemoryItem(JSONObject("""{"id":"x"}"""))!!
        assertEquals("", item.status)
        assertFalse(item.isPending())
        assertFalse(item.isConfirmed())
    }

    @Test
    fun `encode keeps ids exact`() {
        assertEquals("a%20b", MemoryApi.encode("a b"))
        assertEquals("a%2Fb", MemoryApi.encode("a/b"))
    }
}
