package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Test

/**
 * Issue #430: catalog rows the gateway sent but the picker cannot read
 * must be counted, not silently shrink the live list. These tests pin
 * [parseCatalog]: the `providers` envelope parses, unparseable rows are
 * counted in [ProviderCatalog.dropped], and an empty result fails
 * instead of backfilling the offline list.
 */
class ProviderCatalogTest {

    @Test
    fun `providers envelope parses rows`() {
        val page = parseCatalog(
            """{"providers":[
                {"slug":"a","name":"A","models":["m1", 42, ""]},
                {"slug":"b"}
            ]}""",
        )
        assertEquals(listOf("a", "b"), page.providers.map { it.slug })
        assertEquals("A", page.providers[0].label)
        assertEquals(listOf("m1"), page.providers[0].models)
        assertEquals("b", page.providers[1].label)
        assertEquals(0, page.dropped)
    }

    @Test
    fun `unparseable rows are counted not dropped silently`() {
        val page = parseCatalog(
            """{"providers":[
                {"slug":"ok","name":"Fine"},
                {"name":"no slug"},
                {"slug":null,"name":"null slug"},
                {"slug":"","name":"blank slug"},
                42
            ]}""",
        )
        assertEquals(listOf("ok"), page.providers.map { it.slug })
        assertEquals(4, page.dropped)
    }

    @Test(expected = RuntimeException::class)
    fun `empty providers fail instead of backfilling offline`() {
        parseCatalog("""{"providers":[]}""")
    }

    @Test(expected = RuntimeException::class)
    fun `missing envelope fails`() {
        parseCatalog("""{"other":[]}""")
    }

    @Test(expected = RuntimeException::class)
    fun `garbage body fails with the shape error`() {
        parseCatalog("not json at all")
    }

    @Test
    fun `empty names the catalog and garbage the shape`() {
        for ((body, message) in listOf(
            """{"providers":[]}""" to "No providers in catalog",
            "not json at all" to "Unexpected response shape",
            """{"other":[]}""" to "No providers in catalog",
        )) {
            try {
                parseCatalog(body)
                throw AssertionError("must throw for $body")
            } catch (e: RuntimeException) {
                assertEquals(message, e.message)
            }
        }
    }

    @Test
    fun `drop message names the count`() {
        assertEquals(
            "1 provider could not be read — the server sent a row this app cannot parse.",
            catalogDropMessage(1),
        )
        assertEquals(
            "3 providers could not be read — the server sent rows this app cannot parse.",
            catalogDropMessage(3),
        )
    }
}
