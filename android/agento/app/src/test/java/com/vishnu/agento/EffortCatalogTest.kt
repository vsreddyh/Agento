package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * #251, #360: the effort picker must offer what the running model speaks.
 *
 * The gateway validates `reasoning_effort` per request against the APPLIED
 * model's advertised levels and 400s anything else — so a catalog row that
 * disagrees is a refused turn, not a silent downgrade. These tests pin the
 * rows that were measured live and the invariants every row must hold. #361
 * adds the other half: an unset pick must still resolve to a level on the
 * wire, because a missing `model_options` is refused just as hard.
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
    fun `deepseek v4_1 flash offers only the levels Pi advertises`() {
        // Live-probed 2026-10-23 with the model applied (#360): Pi reports
        // low/high/max — there is no off, minimal, medium or xhigh. The old
        // row offered none and xhigh, and either pick 400s the turn.
        assertEquals(
            listOf("low", "high", "max"),
            EffortCatalog.optionsFor("opencode-go/deepseek-v4.1-flash"),
        )
        assertEquals(
            EffortCatalog.optionsFor("deepseek-v4.1-flash"),
            EffortCatalog.optionsFor("opencode/deepseek-v4.1-flash-free"),
        )
    }

    @Test
    fun `sibling deepseek rows match their measured ladders`() {
        // Live-probed 2026-10-23, same method. These rows used to extrapolate
        // a medium/xhigh ladder that neither provider copy speaks, and the
        // opencode-go copies offer off while the opencode copies do not — so
        // each row lists only what BOTH copies accept (object KDoc).
        assertEquals(
            listOf("low", "high", "max"),
            EffortCatalog.optionsFor("deepseek-v4-flash"),
        )
        assertEquals(
            listOf("low", "high", "max"),
            EffortCatalog.optionsFor("deepseek-v4-flash-vision-exp"),
        )
        assertEquals(
            listOf("high", "max"),
            EffortCatalog.optionsFor("deepseek-v4-pro"),
        )
        // The provider disagreement is why "none" is withheld: valid on the
        // opencode-go copy, refused by the opencode copy.
        for (m in listOf("deepseek-v4-flash", "deepseek-v4-flash-vision-exp", "deepseek-v4-pro")) {
            assertFalse(m, EffortCatalog.optionsFor(m).contains("none"))
            assertFalse(m, EffortCatalog.optionsFor(m).contains("medium"))
            assertFalse(m, EffortCatalog.optionsFor(m).contains("xhigh"))
        }
    }

    @Test
    fun `an unset effort still carries reasoning_effort on the wire`() {
        // #361: a fresh tab has no stored effort, and the gateway refuses a
        // turn without model_options.reasoning_effort. The request builder
        // resolves the blank to what the picker shows, never sends blank.
        for (model in listOf(
            "opencode-go/deepseek-v4.1-flash",
            "mimo-v2.6-flash",
            "muse-spark-1.3-contributor",
            "a-model-from-the-future",
        )) {
            val effort = chatModelOptions(model, "").getString("reasoning_effort")
            assertTrue(effort.isNotBlank())
            assertTrue(effort in EffortCatalog.optionsFor(model))
        }
    }

    @Test
    fun `a stored effort is the one sent`() {
        assertEquals(
            "max",
            chatModelOptions("opencode-go/deepseek-v4.1-flash", "max")
                .getString("reasoning_effort"),
        )
        // Case is folded once, in effectiveFor, with Locale.ROOT.
        assertEquals(
            "high",
            chatModelOptions("deepseek-v4-pro", " HIGH ").getString("reasoning_effort"),
        )
    }

    @Test
    fun `a stale or invalid stored effort falls back instead of 400ing`() {
        // effortFor already clamps, but the wire boundary must not depend on
        // every caller having done so: an explicit level this model does not
        // offer (including the two #360 phantoms) resolves to its default.
        for (stale in listOf("none", "xhigh", "medium", "ultra")) {
            val sent = chatModelOptions("opencode-go/deepseek-v4.1-flash", stale)
                .getString("reasoning_effort")
            assertEquals("high", sent)
            assertTrue(sent in EffortCatalog.optionsFor("opencode-go/deepseek-v4.1-flash"))
        }
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
