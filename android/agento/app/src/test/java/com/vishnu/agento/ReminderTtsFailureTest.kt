package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Reminder TTS failures fail loudly (#434): every drop reports a distinct
 * user-visible line, and the device locale never falls back to US English
 * silently.
 *
 * The engine itself needs Android (not under JVM test); what is pinned here
 * is the pure surface behind it — the lines, the locale rule, and the
 * stored-voice match. (The voice-picker hint went with the Speech card in
 * #596; reminder speech runs on engine defaults now.)
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
}

/**
 * Queued speech purges with its task (#470 review): completing a task
 * cancels the visual + failure notices but must also dequeue its
 * utterances, or a stale entry later speaks a done task or re-posts the
 * tag just cancelled. The engine needs Android; the purge decision is
 * pure and pinned here.
 */
class TtsQueuePurgeTest {

    private fun item(id: String, task: String, kind: String = "due") =
        ReminderTts.QueuedSpeech(id, "say $id", task, kind, "Task $task")

    private fun state(vararg items: ReminderTts.QueuedSpeech) = Triple(
        ArrayDeque(items.toList()),
        items.associate { it.id to it }.toMutableMap(),
        mutableSetOf<String>(),
    )

    @Test
    fun `queued items for the task go, others stay`() {
        val (queue, owners, reported) = state(
            item("a1", "A"), item("a2", "A", "start"), item("b1", "B"),
        )
        reported.add("a1")
        val purged = purgeTaskSpeech(queue, owners, reported, null, "A")
        assertEquals(2, purged.dropped)
        assertEquals(false, purged.inFlight)
        assertEquals(listOf("b1"), queue.map { it.id })
        assertEquals(setOf("b1"), owners.keys)
        assertTrue(reported.isEmpty())
    }

    @Test
    fun `an in-flight utterance is suppressed, not stopped`() {
        // Production shape: pump() dequeues before speaking, so the
        // speaking item is in owners but no longer in the queue. Purge
        // drops its bookkeeping; dropTask stops the audio on top.
        val queue = ArrayDeque(listOf(item("b1", "B")))
        val owners = mutableMapOf("a1" to item("a1", "A"), "b1" to item("b1", "B"))
        val reported = mutableSetOf("a1")
        // The speaking id is untouched (the audio finishes); only its
        // owner/reported entries go, so the callback can no longer report
        // — and the caller cancels its notice instead of orphaning it.
        val purged = purgeTaskSpeech(queue, owners, reported, "a1", "A")
        assertEquals(0, purged.dropped)
        assertEquals(true, purged.inFlight)
        assertEquals(listOf("b1"), queue.map { it.id })
        assertEquals(setOf("b1"), owners.keys)
        assertTrue(reported.isEmpty())
    }

    @Test
    fun `another task speaking is left alone`() {
        val (queue, owners, reported) = state(item("a1", "A"), item("b1", "B"))
        val purged = purgeTaskSpeech(queue, owners, reported, "b1", "A")
        assertEquals(1, purged.dropped)
        assertEquals(false, purged.inFlight)
        assertEquals(listOf("b1"), queue.map { it.id })
        assertEquals(setOf("b1"), owners.keys)
    }

    @Test
    fun `a drift leftover owner entry goes too`() {
        val (queue, owners, reported) = state(item("b1", "B"))
        owners["ghost"] = item("ghost", "A")
        val purged = purgeTaskSpeech(queue, owners, reported, null, "A")
        assertEquals(0, purged.dropped)
        assertEquals(false, purged.inFlight)
        assertEquals(setOf("b1"), owners.keys)
    }

    @Test
    fun `no match purges nothing`() {
        val (queue, owners, reported) = state(item("b1", "B"))
        val purged = purgeTaskSpeech(queue, owners, reported, null, "A")
        assertEquals(SpeechPurge(0, false), purged)
        assertEquals(1, queue.size)
    }

    @Test
    fun `a null task purges everything for the disable path`() {        // dropAll's core: every task's queue goes, every owner entry goes,
        // and a speaking item of any task suppresses.
        val queue = ArrayDeque(listOf(item("a1", "A"), item("b1", "B", "start")))
        val owners = mutableMapOf(
            "a1" to item("a1", "A"),
            "b1" to item("b1", "B", "start"),
            "s1" to item("s1", "C"),
        )
        val reported = mutableSetOf("a1", "s1")
        val purged = purgeTaskSpeech(queue, owners, reported, "s1", null)
        assertEquals(2, purged.dropped)
        assertEquals(true, purged.inFlight)
        assertTrue(queue.isEmpty())
        assertTrue(owners.isEmpty())
        assertTrue(reported.isEmpty())
    }
}

/**
 * Kind-scoped purges (#470 review): a lead change moves only BeforeStart
 * points, so other kinds' queued speech and owner entries must survive.
 */
class TtsPointPurgeTest {

    private fun item(id: String, task: String, kind: String) =
        ReminderTts.QueuedSpeech(id, "say $id", task, kind, "Task $task")

    @Test
    fun `a kind filter keeps other kinds`() {
        val queue = ArrayDeque(
            listOf(item("a1", "A", "before"), item("a2", "A", "due"), item("b1", "B", "before")),
        )
        val owners = mutableMapOf(
            "a1" to item("a1", "A", "before"),
            "a2" to item("a2", "A", "due"),
            "b1" to item("b1", "B", "before"),
        )
        val reported = mutableSetOf("a1", "a2")
        val purged = purgeTaskSpeech(queue, owners, reported, null, "A", "before")
        assertEquals(1, purged.dropped)
        assertEquals(false, purged.inFlight)
        assertEquals(listOf("a2", "b1"), queue.map { it.id })
        assertEquals(setOf("a2", "b1"), owners.keys)
        assertEquals(setOf("a2"), reported)
    }

    @Test
    fun `an in-flight utterance of another kind is not suppressed`() {
        val queue = ArrayDeque(listOf(item("b1", "B", "due")))
        val owners = mutableMapOf(
            "a1" to item("a1", "A", "due"),
            "b1" to item("b1", "B", "due"),
        )
        val purged = purgeTaskSpeech(queue, owners, mutableSetOf(), "a1", "A", "before")
        assertEquals(0, purged.dropped)
        assertEquals(false, purged.inFlight)
        assertEquals(setOf("a1", "b1"), owners.keys)
    }
}
