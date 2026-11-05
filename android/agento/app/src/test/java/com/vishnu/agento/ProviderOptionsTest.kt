package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Issue #430: a failed catalog fetch must leave the picker in an error
 * state, never the offline list masquerading as live data. These tests
 * pin [providerOptionsFor]'s offline gate.
 */
class ProviderOptionsTest {

    private fun live() = listOf(
        ProviderOption("opencode-go", "OpenCode Go", listOf("m1", "m2")),
        ProviderOption("other", "Other", listOf("x")),
    )

    @Test
    fun `live catalog passes through`() {
        assertEquals(live(), providerOptionsFor(live(), ""))
    }

    @Test
    fun `empty catalog falls back offline by default`() {
        val options = providerOptionsFor(emptyList(), "")
        assertTrue(options.any { it.slug == "opencode-go" })
        // Offline entries carry no models: the picker cannot offer what
        // it has never seen.
        assertTrue(options.all { it.models.isEmpty() })
    }

    @Test
    fun `failed fetch suppresses the offline list`() {
        assertTrue(providerOptionsFor(emptyList(), "", allowOffline = false).isEmpty())
    }

    @Test
    fun `saved value survives even when offline is suppressed`() {
        val options = providerOptionsFor(emptyList(), "legacy", allowOffline = false)
        assertEquals(listOf("legacy"), options.map { it.slug })
    }

    @Test
    fun `saved value is kept alongside the base`() {
        val slugs = providerOptionsFor(live(), "legacy").map { it.slug }
        assertEquals(listOf("legacy", "opencode-go", "other"), slugs)
        // Already-listed saves are not duplicated.
        assertEquals(2, providerOptionsFor(live(), "other").size)
    }

    @Test
    fun `model options read only the selected provider`() {
        assertEquals(listOf("m1", "m2"), modelOptionsFor(live(), "opencode-go"))
        assertTrue(modelOptionsFor(live(), "").isEmpty())
        assertTrue(modelOptionsFor(live(), "missing").isEmpty())
    }
}
