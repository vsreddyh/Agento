package com.vishnu.agento

import android.content.Context
import android.os.Handler
import android.os.Looper
import android.os.SystemClock
import android.speech.tts.TextToSpeech
import android.speech.tts.UtteranceProgressListener
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import java.util.Locale

/**
 * Built-in Android text-to-speech for the three chats (no permission,
 * no extra dependency — the framework binds the system engine, so no
 * manifest entry is needed either).
 *
 * One instance is hoisted at the activity level and shared by
 * the Chat tab; [ChatScreen] stops it when the tab
 * is left or a new message is sent.
 */
class ChatTts(context: Context) {

    private val appCtx = context.applicationContext

    /** Key of the message currently being spoken, null when idle. */
    private val _speakingKey = MutableStateFlow<String?>(null)
    val speakingKey: StateFlow<String?> = _speakingKey

    // TTS callbacks can arrive off the Main thread (utterance progress
    // always does), so everything the callbacks share with Main is
    // volatile-backed: readiness as a StateFlow (which also lets Compose
    // re-fire keyed effects on init), the rest as @Volatile plain fields.
    private val _ready = MutableStateFlow(false)
    /** True once the engine initialized successfully. Milestone cues key on
     * this so a late init still cues instead of losing the tool set that
     * arrived while TTS was down. */
    val readyFlow: StateFlow<Boolean> = _ready
    @Volatile private var failed = false
    @Volatile private var engine: TextToSpeech? = null
    /** Guards [pending] take-and-clear (#450 review): the init callback
     * (binder thread) and a toggle landing in the completion instant
     * (Main) race read-clear-speak vs write. Both sides drain through
     * [drainPending], so the utterance is spoken exactly once instead of
     * being overwritten-then-cleared. A leaf lock: neither side holds it
     * across a speak call. */
    private val pendingLock = Any()
    @Volatile private var pending: Pair<String, String>? = null
    /** Utterance ID of the last queued chunk; only its completion clears state. */
    @Volatile private var lastUtteranceId = ""
    /** IDs of the currently queued readout (#450 review): a delayed
     * onError/onDone from a flushed, stopped, or re-spoken readout must not
     * kill or clear the new one. IDs embed the arm generation (`key@gN`),
     * so even a same-key stop→re-speak — whose bare IDs would be identical
     * across generations — arms distinct strings; a stale callback simply
     * finds its ID absent. This covers finished() too: lastUtteranceId
     * holds the tagged last ID, so an old onDone never matches the new
     * readout. Replaced wholesale on arm/stop, never mutated, so the
     * volatile read is consistent. */
    @Volatile private var liveIds: Set<String> = emptySet()
    /** Arm generation: makes each readout's utterance IDs unique. */
    private val speechGen = java.util.concurrent.atomic.AtomicLong(0L)
    /** Wrong-locale/no-voice note stashed by [pickLanguage], reported on
     * the first actual speak (#450 review): init (and therefore the
     * picker) runs on every launch, so reporting there toasts on launches
     * where nobody asked for speech. */
    @Volatile private var localeNote: String? = null
    /** True when [localeNote] claims a fallback readout ("speaking
     * English") rather than engine state (#450 review): the fallback is
     * only true once speech starts (flushed at onStart), while state notes
     * ("no usable voice", "couldn't set") stay true even when a chunk is
     * rejected. */
    @Volatile private var localeNoteFallback = false
    /** Stored-voice value the note was decided under (#450 review): the
     * note is one-shot, so a later Settings change (stored set or cleared)
     * re-probes instead of trusting a stale decision. Null = never probed. */
    @Volatile private var noteVoice: String? = null
    /** System locale the note was decided under (#450 review): a language
     * switch changes what the probe means even when the stored voice did
     * not — same re-probe, no extra mechanism. */
    @Volatile private var noteLocale: java.util.Locale? = null
    /** Installed voice names the note was decided under (#450 review): a
     * voice install/uninstall changes the probe with no stored or locale
     * change. One binder read per readout — same order as the speak call. */
    @Volatile private var noteVoices: Set<String> = emptySet()

    init {
        // Guarded: a missing/broken engine throws out of the constructor
        // (#450 review) — an escaping throw would crash composition instead
        // of reaching the init-failure path. No report here (no user action
        // yet — the construction runs on every launch); the failure
        // surfaces via toggle()'s false return and the caller's hint, or via
        // the pending path, when speech is actually attempted.
        // Synchronous-init OEMs run the callback inside the constructor,
        // before the engine field is assigned (finding 1): only record the
        // status here — pickLanguage/speakInternal need the field. Async
        // callbacks see the assigned field and handle directly.
        var earlyStatus: Int? = null
        val tts = runCatching {
            TextToSpeech(appCtx) { status ->
                if (engine != null) {
                    handleInitStatus(status)
                } else {
                    earlyStatus = status
                }
            }
        }.getOrNull()
        // Attached BEFORE the field is published below (#450 review): an
        // async callback landing between publish and attach would drain
        // pending and queue speak() calls with no listener ever attached.
        tts?.setOnUtteranceProgressListener(object : UtteranceProgressListener() {
            // Flush the fallback note when speech demonstrably starts
            // (#450 review): accept is not heard — a later timeout/error
            // toasting on top of an unheard "speaking English" would pair a
            // false claim with the real failure. First onStart wins; later
            // chunks find nothing left. State notes flush the same way:
            // they stay true regardless, so heard-or-not doesn't matter.
            override fun onStart(utteranceId: String) {
                // Gated like onError/onDone (#450 review): a late onStart
                // for a stopped or timed-out readout must neither toast a
                // fallback for unheard speech nor consume the note a newer
                // readout still needs.
                if (utteranceId !in liveIds) return
                localeNote?.let { TtsFailures.report(it); localeNote = null }
            }
            // Only the last chunk's completion clears state — earlier
            // chunks finishing must not flip the UI back mid-speech.
            // Clearing liveIds too (#450 review): a delayed onError for an
            // already-finished chunk must find its ID gone, not toast
            // failure for completed speech (same for an OEM firing both
            // onDone and onError for one utterance).
            private fun finished(utteranceId: String) {
                if (utteranceId == lastUtteranceId) {
                    lastUtteranceId = ""
                    liveIds = emptySet()
                    _speakingKey.value = null
                }
            }
            /** A mid-queue failure (#434): the readout just stopped, which
             * looked exactly like finishing. Stop and say so — but only
             * for a live ID (#450 review): the IDs embed the arm
             * generation, so a delayed onError from a flushed, stopped, or
             * re-spoken readout finds its ID absent and can neither kill
             * the new readout nor toast for it. */
            private fun failed(utteranceId: String, code: String) {
                runCatching { android.util.Log.w("ChatTts", "utterance failed: $code") }
                if (utteranceId !in liveIds) return
                stop()
                TtsFailures.report("Read-aloud stopped — the speech engine reported an error.")
            }
            override fun onDone(utteranceId: String) = finished(utteranceId)
            @Deprecated("Deprecated in Java")
            override fun onError(utteranceId: String) = failed(utteranceId, "unknown")
            override fun onError(utteranceId: String, errorCode: Int) =
                failed(utteranceId, errorCode.toString())
        })
        // Published after the listener is attached (see above): any callback
        // that sees a non-null engine is guaranteed its listener.
        engine = tts
        if (tts == null) {
            // A thrown constructor is an init failure with no callback:
            // mark it so toggle()'s false path (and the caller's hint)
            // fires instead of queueing into a dead engine.
            failed = true
            _ready.value = false
        } else if (earlyStatus != null) {
            // Deferred synchronous init: the field is assigned now, so the
            // stashed status can run the same path as an async callback —
            // no silent drop of the queued utterance (#450 review).
            val es = earlyStatus
            earlyStatus = null
            if (es != null) handleInitStatus(es)
        }
    }

    /** Applies one init completion: readiness, language, then at most one
     * queued utterance. Called from the async callback and from the init
     * tail for the synchronous case — one shape, no silent drop (#450
     * review). */
    private fun handleInitStatus(status: Int) {
        _ready.value = status == TextToSpeech.SUCCESS
        failed = !_ready.value
        if (_ready.value) {
            pickLanguage()
            drainPending()
        } else {
            // Report only if speech was actually attempted (#450
            // review): a bare init failure fires on every launch
            // (rotation included) even when nobody asked for speech.
            // The toggle-false path already surfaces via the caller's
            // hint; this covers the utterance queued during init.
            val attempted = synchronized(pendingLock) {
                val v = pending
                pending = null
                v
            } != null
            if (attempted) {
                TtsFailures.report(
                    "Text-to-speech engine unavailable — read-aloud is off until it initializes.",
                )
            }
        }
    }

    /** Speaks at most one queued utterance, exactly once (#450 review):
     * take-and-clear under [pendingLock] so a toggle landing in the
     * init-completion instant can't be overwritten-then-cleared. */
    private fun drainPending() {
        val p = synchronized(pendingLock) {
            val v = pending
            pending = null
            v
        }
        if (p != null) speakInternal(p.first, p.second)
    }

    /** Main-thread handler, lazy: construction must not require a looper
     * (the reminder side documents the same constraint for unit tests). */
    private val mainHandler: Handler by lazy { Handler(Looper.getMainLooper()) }

    /** Arms a hung-init watchdog with the queue (#450 review): construction
     * succeeded but the callback may never arrive, parking the tap silently
     * forever. Fires only while an attempted readout is still waiting;
     * marks the engine failed (future taps get the caller hint) and reports
     * the queued utterance instead of stranding it. */
    private fun postInitWatchdog() {
        runCatching {
            mainHandler.postDelayed({
                if (!_ready.value && !failed && synchronized(pendingLock) { pending } != null) {
                    failed = true
                    val p = synchronized(pendingLock) {
                        val v = pending
                        pending = null
                        v
                    }
                    if (p != null) {
                        TtsFailures.report(
                            "Text-to-speech engine unavailable — read-aloud is off until it initializes.",
                        )
                    }
                }
            }, INIT_TIMEOUT_MS)
        }
    }

    /** Device default first, US English fallback; keeps whatever works. */
    private fun pickLanguage() {
        val tts = engine ?: return
        val default = Locale.getDefault()
        val candidates = listOf(default, Locale.US)
        var anyAvailable = false
        var setterFailed = false
        for (loc in candidates) {
            // Availability and setter are separate facts (#450 review): the
            // voice can exist while the setter throws (OEM data issues),
            // and a combined check looping on would misreport which failed.
            val available = runCatching {
                val r = tts.isLanguageAvailable(loc)
                r == TextToSpeech.LANG_AVAILABLE ||
                    r == TextToSpeech.LANG_COUNTRY_AVAILABLE ||
                    r == TextToSpeech.LANG_COUNTRY_VAR_AVAILABLE
            }.getOrDefault(false)
            if (!available) continue
            anyAvailable = true
            // A throwing setter tries the next candidate (#450 review):
            // giving up would skip a working US fallback for a broken
            // default setter and misreport voices as missing.
            if (runCatching { tts.language = loc }.isSuccess) {
                // Clean success clears any stashed note (#450 review): a voice
                // installed (or a stored voice set) since the last probe must
                // not leave a stale fallback claim behind.
                localeNote = null
                localeNoteFallback = false
                // Speaking the wrong language silently is a failure the user
                // cannot hear as one (#434) — but only worth saying when
                // speech is actually attempted (#450 review), so stash it
                // for speakInternal to flush.
                if (loc != default) {
                    // A prior setter throw means the fallback cause is the
                    // setter, not a missing voice (#450 review): there WAS a
                    // default voice, it just wouldn't set — saying "no
                    // voice" would send the user to install what exists.
                    localeNote = if (setterFailed) {
                        "Couldn't set the voice for ${default.displayName} — read-aloud may use the wrong language."
                    } else {
                        "No voice for ${default.displayName} — speaking English instead."
                    }
                    localeNoteFallback = true
                }
                return
            }
            setterFailed = true
            localeNote =
                "Couldn't set the voice for ${loc.displayName} — read-aloud may use the wrong language."
            localeNoteFallback = false
        }
        // Nothing available at all: the engine is unusable for speech. (If
        // voices existed but every setter threw, the last couldn't-set note
        // above stands.) Pick nothing rather than pretend a language was set.
        if (!anyAvailable) {
            localeNote = "No usable text-to-speech voice is installed."
            localeNoteFallback = false
        }
    }

    /**
     * Speaks [text] under [key]; tapping the same key again stops.
     * Returns false when the engine failed to init (caller shows a hint).
     */
    fun toggle(key: String, text: String): Boolean {
        if (_speakingKey.value == key) {
            stop()
            return true
        }
        // Queued same key → cancel the locked read (#450 review): without
        // this the re-tap re-queues and init speaks anyway, so a stop tap
        // during init does nothing. Read under the lock like the write, so
        // a concurrent drain can't flip cancel into speak or vice versa.
        if (synchronized(pendingLock) { pending?.first } == key) {
            stop()
            return true
        }
        val clean = cleanForSpeech(text)
        if (clean.isEmpty()) {
            // Nothing speakable: don't cut live speech for it (#450
            // review) — an empty tap while another message plays used to
            // stop() first and go silent. Take-and-clear a stranded queued
            // utterance instead and say so; an idle empty tap says the
            // plain fact, since tapping read-aloud and getting nothing with
            // no feedback reads as broken.
            val stranded = synchronized(pendingLock) {
                val v = pending
                pending = null
                v
            }
            TtsFailures.report(
                if (stranded != null) {
                    "Earlier read-aloud request dropped — nothing to speak."
                } else {
                    "Nothing to speak."
                },
                ToastKind.Info,
            )
            return true
        }
        // Captured before stop() clears it: a queued different key is
        // superseded below.
        val superseded = synchronized(pendingLock) { pending != null }
        stop()
        if (!_ready.value) {
            // Engine still initing: queue one utterance. A failed engine
            // reports false so the caller can show a hint instead.
            if (failed) return false
            // Single slot: a different key supersedes (#450 review) — say
            // so (Info: the user's own tap, not an error) instead of
            // silently dropping. Worded conditionally: init may still fail
            // after this tap, in which case the latest never speaks.
            if (superseded) {
                TtsFailures.report(
                    "Earlier read-aloud request dropped — will speak the latest one if the engine starts.",
                    ToastKind.Info,
                )
            }
            synchronized(pendingLock) { pending = key to clean }
            // Init may have completed between the first readiness read and
            // the write — drain here instead of stranding a queued
            // utterance nobody drains (#450 review). Exactly-once via
            // drainPending: a concurrent callback drain takes it first.
            if (_ready.value) drainPending()
            // Hung init never completes the callback — bound the wait with
            // a watchdog instead of parking the tap silently forever.
            postInitWatchdog()
            return true
        }
        speakInternal(key, clean)
        return true
    }

    private fun speakInternal(key: String, text: String) {
        val tts = engine ?: return
        // Arm first (#450 review): everything below does binder reads during
        // which a stop can land, and the loop's liveness re-check only
        // protects what comes after the arm. chunk() is pure CPU, so arming
        // needs no engine call. A stop tap in the window below now stops
        // (speakingKey is set next) instead of falling into the pending
        // path and double-arming.
        val tag = speechGen.incrementAndGet()
        val chunks = chunk(text)
        val ids = chunks.mapIndexed { i, _ -> if (i == 0) "$key@$tag" else "$key#$i@$tag" }
        liveIds = ids.toSet()
        // Stuck-utterance watchdog (#450 review): an accepted chunk that
        // never calls back sticks the stop icon with no report — the
        // reminder-side timeout's chat equivalent. Scaled to the readout
        // length so unhurried speech never trips it. Reference-checked: a
        // newer arm, a stop, or a finishing onDone replaces/clears the set
        // and no-ops this run.
        val armed = liveIds
        val stuckAfterMs = chunks.sumOf { it.length } * 1000L / MIN_TTS_CPS + STUCK_TIMEOUT_MS
        runCatching {
            mainHandler.postDelayed({
                if (liveIds === armed && armed.isNotEmpty()) {
                    stop()
                    val note = localeNote
                    if (note != null && !localeNoteFallback) {
                        TtsFailures.report(note)
                        localeNote = null
                    }
                    TtsFailures.report("Read-aloud timed out — the speech engine stopped responding.")
                }
            }, stuckAfterMs)
        }
        _speakingKey.value = key
        // Probe (or re-probe) when the stored voice, system locale, or
        // installed voices changed since the note was decided (#450
        // review): a one-shot note goes stale otherwise. The probe is a few
        // cached binder reads; the flush happens once per decision at the
        // first onStart, so a persistent fallback still toasts once, not per
        // readout.
        val stored = BehaviourSettings.ttsVoice(appCtx)
        val curLocale = Locale.getDefault()
        val voiceSig = runCatching {
            tts.voices?.mapNotNull { it.name }.orEmpty().toSet()
        }.getOrDefault(emptySet())
        if (stored != noteVoice || curLocale != noteLocale || voiceSig != noteVoices) {
            pickLanguage()
            noteVoice = stored
            noteLocale = curLocale
            noteVoices = voiceSig
        }
        // Stored voice/rate/pitch (#355); skipped at defaults so untouched
        // installs speak exactly like before. Existence is not success: a
        // stored voice that resolves but won't set must not clear the probe
        // note, and gets its own report — a throwing voice setter is the
        // wrong-voice silence #434 fixes (#450 review).
        val storedOk = applyStoredTtsSettings(tts, appCtx)
        if (stored.isNotEmpty()) {
            if (storedOk) {
                localeNote = null
            } else if (ids.first() in liveIds) {
                // Gated on liveness like the loop below (#450 review): a
                // stop landing in the probe/apply window cancels this arm,
                // and a cancelled arm must stay silent rather than toast
                // failure for speech nobody asked to hear anymore.
                TtsFailures.report("Couldn't use the stored voice — speech may use a different voice.")
            }
        }
        // ~3500-char chunks at sentence boundaries (engine limit is ~4000).
        // Only the last chunk's ID is tracked: its completion is what
        // clears the speaking state.
        chunks.forEachIndexed { i, part ->
            val id = ids[i]
            // Bail if a concurrent stop superseded this arm (#450 review):
            // the loop queues ahead while a binder-thread onError can stop
            // mid-queue, and queueing past the stop resurrects speech with
            // no stop icon for it.
            if (id !in liveIds) return
            if (i == chunks.lastIndex) lastUtteranceId = id
            val rc = runCatching {
                tts.speak(
                    part,
                    if (i == 0) TextToSpeech.QUEUE_FLUSH else TextToSpeech.QUEUE_ADD,
                    null,
                    id,
                )
            }.getOrNull()
            if (rc == null) {
                // A throw (broken OEM binder state) counts like ERROR
                // (#450 review): the Reminder path already treats it that
                // way, and an escaping throw would leave the stop icon
                // stuck with no report.
                stop()
                // State notes stay true even though nothing was spoken
                // (#450 review); the fallback note does not — it flushes
                // only at onStart. Local copy: volatile reads don't
                // smart-cast.
                val note = localeNote
                if (note != null && !localeNoteFallback) {
                    TtsFailures.report(note)
                    localeNote = null
                }
                TtsFailures.report("Read-aloud failed — the speech engine failed.")
                return
            }
            if (rc == TextToSpeech.ERROR) {
                // A rejected chunk never fires onDone — clear state now
                // instead of sticking on the stop icon.
                stop()
                // And say it (#434): clearing the icon alone reads as "the
                // readout finished". State notes flush like the throw path.
                val note = localeNote
                if (note != null && !localeNoteFallback) {
                    TtsFailures.report(note)
                    localeNote = null
                }
                TtsFailures.report("Read-aloud failed — the speech engine rejected the text.")
                return
            }
        }
    }

    fun stop() {
        // Under the lock like every other pending side (#450 review): a
        // binder-thread stop racing toggle's locked write + drain check
        // must serialize, not silently drop the queued utterance mid-handoff.
        synchronized(pendingLock) { pending = null }
        lastUtteranceId = ""
        // Cleared: stale callbacks find their IDs absent, whichever
        // generation armed them.
        liveIds = emptySet()
        runCatching { engine?.stop() }
        _speakingKey.value = null
    }

    fun shutdown() {
        // A queued-but-unspoken utterance dies with the instance (#450
        // review): rotation during init strands the tap — the new instance
        // starts with pending=null and arms no watchdog for it. Say so
        // instead of dropping it silently. stop() would clear it quietly.
        val stranded = synchronized(pendingLock) {
            val v = pending
            pending = null
            v
        }
        stop()
        if (stranded != null) {
            TtsFailures.report(
                "Queued read-aloud request was dropped.",
                ToastKind.Info,
            )
        }
        runCatching { engine?.shutdown() }
        engine = null
        _ready.value = false
        failed = true
    }

    companion object {
        private const val MAX_CHUNK = 3500
        /** Hung init bound (#450 review): construction succeeded but the
         * callback never arrives. Init is typically sub-second; 30s is
         * past any honest stall, far short of silent forever. */
        private const val INIT_TIMEOUT_MS = 30_000L
        /** Slowest plausible speech for the stuck-utterance bound below
         * (chars/sec): the timeout scales with readout length so unhurried
         * speech never trips it, plus STUCK_TIMEOUT_MS of margin. */
        private const val MIN_TTS_CPS = 7
        /** Margin past the length estimate after which silence from the
         * engine means stuck, not slow (#450 review). */
        private const val STUCK_TIMEOUT_MS = 60_000L

        /** Splits on sentence ends so chunks don't cut mid-thought. */
        fun chunk(text: String): List<String> {
            if (text.length <= MAX_CHUNK) return listOf(text)
            val out = mutableListOf<String>()
            var start = 0
            while (start < text.length) {
                var end = minOf(start + MAX_CHUNK, text.length)
                if (end < text.length) {
                    val cut = text.lastIndexOfAny(
                        charArrayOf('.', '!', '?', '\n'),
                        end - 1,
                    )
                    if (cut > start) end = cut + 1
                }
                out.add(text.substring(start, end))
                start = end
            }
            return out
        }

        /** Strips Markdown/HTML formatting so it isn't read aloud literally. */
        fun cleanForSpeech(raw: String): String {
            var s = raw
            s = s.replace(Regex("(?s)```.*?```"), " code ")
            s = s.replace(Regex("`([^`]*)`"), "$1")
            s = s.replace(Regex("!\\[([^\\]]*)]\\([^)]*\\)"), "$1")
            s = s.replace(Regex("\\[([^\\]]*)]\\([^)]*\\)"), "$1")
            s = s.replace(Regex("(?m)^\\s{0,3}#{1,6}\\s+"), "")
            s = s.replace(Regex("[*_~]{1,3}"), "")
            s = s.replace(Regex("(?m)^\\s*>\\s?"), "")
            s = s.replace("|", " ")
            // Decode (not drop) entities so words survive; strip real HTML
            // tags — the leading-letter guard keeps "a < b" prose intact.
            s = s.replace("&amp;", "&")
                .replace("&lt;", "<")
                .replace("&gt;", ">")
                .replace("&quot;", "\"")
                .replace("&#39;", "'")
            s = s.replace(Regex("</?[A-Za-z][^>]*>"), " ")
            s = s.replace(Regex("(?m)^\\s*[-*+]\\s+"), "")
            s = s.replace(Regex("(?m)^\\s*\\d+[.)]\\s+"), "")
            s = s.replace(Regex("\\s+"), " ").trim()
            return s
        }
    }
}
