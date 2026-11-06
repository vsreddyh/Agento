package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Reminder TTS failures fail loudly (#434): every drop reports a distinct
 * user-visible line, the device locale never falls back to US English
 * silently, and a dead engine reads differently from "no voices".
 *
 * The engine itself needs Android (not under JVM test); what is pinned here
 * is the pure surface behind it — the lines, the locale rule, the
 * stored-voice match, and the voice-list hint.
 */
class ReminderTtsFailureTest {

    @Test
    fun `every failure has a distinct non-blank line`() {
        val lines = TtsFailure.entries.map { speechFailureLine(it) }
        assertTrue(lines.all { it.isNotBlank() })
        assertEquals(lines.size, lines.toSet().size)
        // All loud, none Log-only shaped: each names the spoken alert.
        assertTrue(lines.all { it.startsWith("Spoken alert") })
    }

    @Test
    fun `failure lines say what happened`() {
        assertTrue(speechFailureLine(TtsFailure.ENGINE_INIT).contains("failed to start"))
        assertTrue(speechFailureLine(TtsFailure.LOCALE_UNAVAILABLE).contains("language"))
        assertTrue(speechFailureLine(TtsFailure.VOICE_MISSING).contains("default voice"))
    }

    @Test
    fun `unavailable default never falls back silently`() {
        // The whole point (#434): error beats wrong-language speech.
        assertTrue(shouldSpeakWithDefault(defaultAvailable = true, fallbackAvailable = true))
        assertTrue(shouldSpeakWithDefault(defaultAvailable = true, fallbackAvailable = false))
        assertFalse(shouldSpeakWithDefault(defaultAvailable = false, fallbackAvailable = true))
        assertFalse(shouldSpeakWithDefault(defaultAvailable = false, fallbackAvailable = false))
    }

    @Test
    fun `stored voice match is exact and trims the want`() {
        val names = listOf("en-US-voice", "de-DE-voice")
        assertFalse(isStoredVoiceMissing(names, ""))
        assertFalse(isStoredVoiceMissing(names, "en-US-voice"))
        // BehaviourSettings.ttsVoice trims on read; a padded want still matches.
        assertFalse(isStoredVoiceMissing(names, "  en-US-voice  "))
        assertTrue(isStoredVoiceMissing(names, "fr-FR-voice"))
        assertTrue(isStoredVoiceMissing(emptyList(), "en-US-voice"))
        assertFalse(isStoredVoiceMissing(emptyList(), ""))
    }

    @Test
    fun `voice hint distinguishes dead engine from no voices`() {
        // Loading: the button already reads "Loading…", no hint line.
        assertNull(voiceListHint(listing = true, voices = null, savedVoice = "v"))
        // Dead engine: failure state with a retry. With Engine default there
        // is no pick to keep, so the line says so instead.
        assertEquals(
            "The speech engine isn't answering — voice choices are unavailable until it responds.",
            voiceListHint(listing = false, voices = null, savedVoice = ""),
        )
        assertEquals(
            "The speech engine isn't answering — the pick is kept, and Reload voices retries it.",
            voiceListHint(listing = false, voices = null, savedVoice = "v"),
        )
        // Genuinely empty: a different state, not an engine failure.
        assertEquals(
            "This engine offers no voices — install a voice pack, or leave Engine default.",
            voiceListHint(listing = false, voices = emptyList(), savedVoice = ""),
        )
        assertEquals(
            "This engine offers no voices — the pick is kept, and Reload voices retries it.",
            voiceListHint(listing = false, voices = emptyList(), savedVoice = "v"),
        )
        // Stored pick the engine no longer offers: kept, loudly.
        val voices = listOf("a" to "A", "b" to "B")
        assertEquals(
            "Kept from an import: this engine no longer offers that voice, but the pick is kept.",
            voiceListHint(listing = false, voices = voices, savedVoice = "gone"),
        )
        // Happy paths: no hint.
        assertNull(voiceListHint(listing = false, voices = voices, savedVoice = ""))
        assertNull(voiceListHint(listing = false, voices = voices, savedVoice = "a"))
    }
}
