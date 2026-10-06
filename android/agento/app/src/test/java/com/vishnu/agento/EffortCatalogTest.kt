package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * #251: the effort picker must offer what the running model speaks.
 *
 * The gateway validates `reasoning_effort` per request against the APPLIED
 * model's advertised levels and 400s anything else — so a catalog row that
 * disagrees is a refused turn, not a silent downgrade. These tests pin the
 * rows that were measured live and the invariants every row must hold.
 */
class EffortCatalogTest {

    @Test
    fun `mimo flash offers the live-probed graded ladder`() {
        // Measured 2026-10-04: off/minimal/low/medium/high ("none" is what
        // the app sends for off). The old toggle row came from Hermes-era
        // files that no longer exist.
        assertEquals(
            listOf("none", "minimal", "low", "medium", "high"),
            EffortCatalog.optionsFor("mimo-v2.6-flash"),
        )
    }

    @Test
    fun `muse-spark offers none`() {
        // The default model must be able to turn reasoning off. Unverified on
        // the real model, but consistent with every graded row — and a wrong
        // offering 400s naming the real levels rather than running wrong.
        assertTrue(EffortCatalog.optionsFor("muse-spark-1.3-contributor").contains("none"))
    }

    @Test
    fun `sibling mimo rows follow the probed ladder`() {
        // Only v2.6-flash was live-probed; the siblings extrapolate it (see
        // the KDoc). If a sibling diverges, its 400 names its real levels and
        // the row gets corrected — this test makes that correction land here.
        for (m in listOf("mimo-v2-5", "mimo-v2-6-pro", "mimo-v2-pro", "mimo-v2-omni")) {
            assertEquals(EffortCatalog.MIMO_GRADED, EffortCatalog.optionsFor(m))
        }
    }

    @Test
    fun `no row offers ultra`() {
        // "ultra" is not a recognised level name on any model — offering it
        // 400s the turn. It used to sit in FALLBACK and the mimo rows.
        assertFalse(EffortCatalog.FALLBACK.contains("ultra"))
        assertFalse(EffortCatalog.optionsFor("mimo-v2.6-flash").contains("ultra"))
        assertFalse(EffortCatalog.optionsFor("muse-spark-1.3-contributor").contains("ultra"))
        assertFalse(EffortCatalog.optionsFor("some-unknown-model").contains("ultra"))
    }

    @Test
    fun `provider prefixes do not change the lookup`() {
        assertEquals(
            EffortCatalog.optionsFor("muse-spark-1.3-contributor"),
            EffortCatalog.optionsFor("opencode-go/muse-spark-1.3-contributor"),
        )
    }

    @Test
    fun `unknown models fall back without ultra`() {
        val opts = EffortCatalog.optionsFor("a-model-from-the-future")
        assertEquals(EffortCatalog.FALLBACK, opts)
        assertTrue(opts.contains("medium"))
    }
}
