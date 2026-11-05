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
    fun `empty names the catalog and wrong shapes name the shape`() {
        for ((body, message) in listOf(
            """{"providers":[]}""" to "No providers in catalog",
            "not json at all" to "Unexpected response shape",
            """{"other":[]}""" to "Unexpected response shape",
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
    fun `all dropped names the count instead of the empty gate`() {
        // Zero usable rows out of unparseable ones is a data failure,
        // not an empty inventory: the picker headlines the count.
        try {
            parseCatalog("""{"providers":[{"name":"x"},42]}""")
            throw AssertionError("must throw, not read as no providers")
        } catch (e: RuntimeException) {
            assertEquals(
                "2 providers could not be parsed — the server sent rows this app cannot read.",
                e.message,
            )
        }
    }

    @Test
    fun `drop message names the count`() {
        assertEquals(
            "1 provider could not be parsed — the server sent a row this app cannot read.",
            catalogDropMessage(1),
        )
        assertEquals(
            "3 providers could not be parsed — the server sent rows this app cannot read.",
            catalogDropMessage(3),
        )
    }
}
