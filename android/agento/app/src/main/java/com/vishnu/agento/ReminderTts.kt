package com.vishnu.agento

import android.app.NotificationManager
import android.content.Context
import android.media.AudioManager
import android.os.Handler
import android.os.Looper
import android.os.SystemClock
import android.speech.tts.TextToSpeech
import android.speech.tts.UtteranceProgressListener
import android.util.Log
import java.util.Locale
import java.util.UUID

private val QUOTE_CHARS = Regex("[“”]")

/**
 * App-scoped TTS for task reminders (#306). One engine, one FIFO queue:
 * simultaneous alarms `QUEUE_ADD` behind whatever is speaking instead of
 * flushing/cutting each other. DND or non-normal ringer mode stays silent
 * (visual notification only).
 */
object ReminderTts {
    private const val MAX_QUEUED = 10
    /** Watchdog: an utterance that never completes frees the queue after this. */
    private const val SPEAK_TIMEOUT_MS = 60_000L
    /** Hung-init bound (#450 review): construction succeeded but the
     * callback never arrives. Re-arms the retry and reports what actually
     * happened instead of filling toward a misleading "queue was full". */
    private const val INIT_TIMEOUT_MS = 30_000L
    private val lock = Any()
    private val queue = ArrayDeque<Pair<String, String>>()
    @Volatile private var engine: TextToSpeech? = null
    private var ready = false
    private var inited = false
    /** Application context for reading stored TTS prefs in pump(). Set on
     * every speak(); the object never holds an Activity. Volatile like the
     * engine: written on the caller thread, read off the TTS callback. */
    @Volatile private var appCtxRef: Context? = null
    private var speaking = false
    private var speakStartedAt = 0L
    /** Id of the utterance on the engine, null when none (#450 review):
     * timeouts clear it so a late callback for the timed-out id matches
     * nothing — no double toast, no clearing the successor's flag. */
    private var speakingId: String? = null
    /** Voices snapshot the note was decided under (#450 review): a voice
     * install/uninstall changes what the probe means even when the stored
     * voice and locale did not. Compared by name set — cheap next to the
     * speak call it guards. */
    @Volatile private var noteVoices: Set<String> = emptySet()
    @Volatile private var localeNote: String? = null
    /** True when [localeNote] claims a fallback readout rather than engine
     * state (#450 review): the fallback flushes only at onStart; state
     * notes flush even on throw/reject. */
    @Volatile private var localeNoteFallback = false
    /** Stored-voice value the note was decided under; null = never decided. */
    @Volatile private var noteVoice: String? = null
    /** System locale the note was decided under (#450 review): same re-probe
     * as the stored voice — a language switch changes the probe meaning. */
    @Volatile private var noteLocale: java.util.Locale? = null
    /** Engine generation: a late callback from a dead engine must not clear the new one's state. */
    private var generation = 0
    /** Watchdog generation: stale delayed wakeups no-op instead of freeing a live utterance. */
    private var watchdogGen = 0
    // Lazy: object load must not require a main looper (unit tests have none).
    private val mainHandler: Handler by lazy { Handler(Looper.getMainLooper()) }

    private fun progressListener(gen: Int) = object : UtteranceProgressListener() {
        override fun onStart(utteranceId: String) {
            // Speech demonstrably started (#450 review): flush the fallback
            // note now, not on accept — a later timeout/error toasts on top
            // of a fallback that actually happened. First onStart wins.
            // Gated like onError/onDone: a late onStart for a stopped or
            // timed-out utterance must neither toast for unheard speech nor
            // consume the note a newer utterance still needs.
            val current = synchronized(lock) { gen == generation && utteranceId == speakingId }
            if (!current) return
            localeNote?.let { TtsFailures.report(it); localeNote = null }
        }
        private fun finished(utteranceId: String) {
            // Only the current utterance owns `speaking` (#450 review): a
            // late onDone for a timed-out utterance must not clear the
            // successor's flag (and pump() always runs so a stale
            // completion still advances whatever is queued). The engine
            // generation still guards dead-engine callbacks. Clearing the
            // id too: a duplicate/late onError for the heard id must find
            // nothing to match, not double-report.
            synchronized(lock) {
                if (gen == generation && utteranceId == speakingId) {
                    speaking = false
                    speakingId = null
                }
            }
            pump()
        }
        override fun onDone(utteranceId: String) = finished(utteranceId)
        @Deprecated("Deprecated in Java")
        override fun onError(utteranceId: String) {
            // Same surface as the 2-arg overload (#450 review): the legacy
            // shape is not the live one on current SDKs, but leaving it
            // silent would contradict the "every onError reports" claim.
            // Utterance-gated: a timeout already toasted for this id, so a
            // late error must neither double-report nor touch the successor.
            val current = synchronized(lock) { gen == generation && utteranceId == speakingId }
            if (current) {
                Log.w("ReminderTts", "utterance error (legacy), advancing queue")
                TtsFailures.report(
                    "A reminder could not be read aloud — the speech engine reported an error.",
                )
            }
            finished(utteranceId)
        }
        override fun onError(utteranceId: String, errorCode: Int) {
            // Queue-advancing is right (a dropped alarm line is worse than an
            // ugly one), but a log line is not an error surface (#434): a
            // reminder the user never heard now says so on the next open.
            // Utterance-gated like the legacy overload: timeouts already
            // toasted for their id.
            val current = synchronized(lock) { gen == generation && utteranceId == speakingId }
            if (current) {
                Log.w("ReminderTts", "utterance error $errorCode, advancing queue")
                TtsFailures.report(
                    "A reminder could not be read aloud — the speech engine reported an error.",
                )
            }
            finished(utteranceId)
        }
    }

    internal fun speak(appCtx: Context, kind: ReminderKind, task: ServerTask, overdueElapsed: Long? = null) {
        if (isSilent(appCtx)) return
        appCtxRef = appCtx.applicationContext
        // Quotes delimit the name in text but some engines verbalize "quote",
        // so they are stripped only on the speech path (reminderSpeech keeps them).
        val leadMinutes = BehaviourSettings.reminderLeadMinutes(appCtx)
        val text = ChatTts.cleanForSpeech(reminderSpeech(kind, task.name, task.speakName, overdueElapsed, leadMinutes))
            .replace("\"", "").replace(QUOTE_CHARS, "")
        val id = "${kind.key}-${UUID.randomUUID()}"
        synchronized(lock) {
            // Bounded, drop-oldest: for a stuck engine nothing speaks anyway,
            // and for a transient burst the freshest alarm is the most relevant
            // one to keep — the visual notification still posts for every alarm.
            var droppedSpeech = 0
            while (queue.size >= MAX_QUEUED) {
                val dropped = queue.removeFirst()
                Log.w("ReminderTts", "queue full, dropping oldest speech ${dropped.first}")
                droppedSpeech++
            }
            // A dropped speech is never heard — say so once per call (#450
            // review), like the throw/reject paths below.
            if (droppedSpeech > 0) {
                TtsFailures.report(
                    "A reminder could not be read aloud — the speech queue was full.",
                )
            }
            queue.addLast(id to text)
        }
        ensureEngine(appCtx.applicationContext)
        pump()
    }

    internal fun isSilent(appCtx: Context): Boolean {
        val audio = appCtx.getSystemService(Context.AUDIO_SERVICE) as? AudioManager
        // Vibrate counts as silent on purpose: TTS would play on the media
        // stream regardless, and vibrate means "stay quiet" (#306: DND/silent
        // stays silent, visual notification only).
        if (audio != null && audio.ringerMode != AudioManager.RINGER_MODE_NORMAL) return true
        // No SDK guard: notification channels need 26 and minSdk is 28.
        val nm = appCtx.getSystemService(Context.NOTIFICATION_SERVICE) as? NotificationManager
        if (nm != null && nm.currentInterruptionFilter != NotificationManager.INTERRUPTION_FILTER_ALL) return true
        return false
    }

    private fun ensureEngine(appCtx: Context) {
        val gen: Int = synchronized(lock) {
            if (inited) return
            inited = true
            generation += 1
            generation
        }
        // inited is claimed above so concurrent speaks still construct once;
        // a throwing constructor releases it below so the next speak()
        // retries instead of sealing the queue forever (#450 review).
        // Listener attached before the init callback can pump: a fast callback
        // speaking before setOnUtteranceProgressListener would leave `speaking`
        // true forever (no onDone ever fires).
        //
        // Synchronous-init OEMs call the callback inside the constructor, before
        // `engine` is assigned: the callback then stashes its status and the tail
        // below applies it after assignment — so the language pick runs and a
        // synchronous failure shuts down the real object instead of leaking it.
        var earlyStatus: Int? = null
        var earlyGen = -1
        val tts = runCatching {
            TextToSpeech(appCtx) { status ->
                val assigned = synchronized(lock) { engine }
                if (assigned == null) {
                    synchronized(lock) {
                        earlyStatus = status
                        earlyGen = gen
                    }
                } else if (gen == synchronized(lock) { generation }) {
                    // Generation-guarded like progressListener (#450
                    // review): the hung-init watchdog retires without
                    // bumping generation, so a late callback from the
                    // retired engine must not act on its replacement
                    // (old FAILURE would shut down the new working engine).
                    onEngineInit(status)
                }
                // else: retired engine's late callback — dropped.
            }
        }.getOrNull()
        if (tts == null) {
            // Constructor threw (missing/broken engine): report like the
            // init-failure path and release the latch — the queue is kept,
            // the next speak() tries again.
            synchronized(lock) { inited = false }
            Log.w("ReminderTts", "engine construction failed, keeping queued speech for retry")
            TtsFailures.report(
                "Reminder speech is unavailable — the text-to-speech engine failed to start. Kept for retry.",
            )
            return
        }
        tts.setOnUtteranceProgressListener(progressListener(gen))
        engine = tts
        // Hung init never completes the callback (#450 review): without a
        // bound, every later speak() hits `if (inited) return` and never
        // retries while the queue fills toward a misleading "queue was
        // full". Retire unconditionally — even with an empty queue, or a
        // silence-drained backlog leaves the latch stuck and the next alarm
        // fills toward that same misleading toast. Generation + state
        // checked atomically under the lock:
        // a late success between the check and the unlatch can't strand a
        // working engine — the section sees a single state.
        // No-main-looper environments (unit tests) skip the post — the
        // runCatching mirrors the utterance watchdog below.
        val initGen = gen
        runCatching {
            mainHandler.postDelayed({
                var hung = false
                var hungHasQueue = false
                var hungEngine: TextToSpeech? = null
                synchronized(lock) {
                    if (initGen == generation && inited && !ready) {
                        inited = false
                        hung = true
                        hungHasQueue = queue.isNotEmpty()
                        hungEngine = engine
                        engine = null
                    }
                }
                hungEngine?.let { runCatching { it.shutdown() } }
                // Report only when speech is actually waiting; the unlatch
                // itself is silent.
                if (hung && hungHasQueue) {
                    Log.w("ReminderTts", "engine init timed out, keeping queued speech for retry")
                    TtsFailures.report(
                        "Reminder speech is unavailable — the text-to-speech engine timed out starting. Kept for retry.",
                    )
                }
            }, INIT_TIMEOUT_MS)
        }
        val (es, esGen) = synchronized(lock) { earlyStatus to earlyGen }
        // Stash applies only to this attempt's synchronous callback (#450
        // review): a retired engine's late callback stashed during the
        // replacement's construction must not init the new engine.
        if (es != null && esGen == gen) onEngineInit(es)
    }

    private fun onEngineInit(status: Int) {
        synchronized(lock) { ready = status == TextToSpeech.SUCCESS }
        if (status == TextToSpeech.SUCCESS) {
            val tts = engine
            if (tts != null) {
                decideReminderNote(tts)
            }
            pump()
        } else {
            val dead = engine
            synchronized(lock) {
                // Kept, not cleared: an alarm enqueued while this init was in
                // flight arrived after the failure was decided — wiping it
                // would drop a voice whose notification just posted. The queue
                // is bounded anyway, and the retry speaks it.
                // Retry, don't seal: leaving inited=true with ready=false
                // would queue-but-never-speak forever after one failure.
                inited = false
                engine = null
            }
            runCatching { dead?.shutdown() }
            Log.w("ReminderTts", "engine init failed, keeping queued speech for retry")
            // Queued, not dead (#450 review): the next speak() retries the
            // init, so this must not read as permanent — only as unavailable
            // with a retry coming.
            TtsFailures.report(
                "Reminder speech is unavailable — the text-to-speech engine failed to start. Kept for retry.",
            )
        }
    }

    /** Fresh probe + set + stashed note, in one place (#450 review).
     * Reusing init facts after a locale switch invents fallback notes for
     * healthy locales, so every decision re-reads availability and re-sets.
     * Wrong-language speech is worse than an error (#434), but a stored
     * voice that resolves wins over the probe — the English note would be
     * false for readouts pump() voices per utterance. pump() re-runs this
     * when the stored voice, locale, or installed voices change; the note
     * flushes once per decision at the first onStart. */
    private fun decideReminderNote(tts: TextToSpeech) {
        val default = Locale.getDefault()
        val stored = appCtxRef?.let { BehaviourSettings.ttsVoice(it) }.orEmpty()
        noteVoice = stored
        noteLocale = default
        noteVoices = voiceNames(tts)
        // Prefer the device default, fall back to US English. A throwing
        // setter tries the next candidate: giving up would skip a working
        // US fallback for a broken default setter and misreport voices as
        // missing. The English note fires only when a set actually landed:
        // claiming English after a thrown setter asserts a fallback that
        // never happened.
        val candidates = listOf(default, Locale.US)
        var picked: Locale? = null
        var setLoc: Locale? = null
        var setterFailed = false
        for (loc in candidates) {
            if (!isLanguageAvailable(tts, loc)) continue
            if (picked == null) picked = loc
            if (runCatching { tts.language = loc }.isSuccess) {
                setLoc = loc
                break
            }
            setterFailed = true
        }
        val storedResolves = stored.isNotEmpty() && runCatching {
            tts.voices?.any { it.name == stored } == true
        }.getOrDefault(false)
        localeNote = when {
            storedResolves -> null
            picked == null -> "No usable text-to-speech voice is installed."
            // Same setter-failure surface as ChatTts (#450 review): an
            // available voice whose set threw proceeds in whatever language
            // the engine kept — silently is the wrong-language silence #434
            // fixed elsewhere. Only a landed non-default set earns the
            // English-fallback note (flushed at onStart, superseded by a
            // same-utterance failure like every other note). A prior throw
            // keeps the couldn't-set cause instead of misattributing the
            // fallback to a missing voice.
            setLoc == null -> "Couldn't set the reminder voice — reminders may read in the wrong language."
            setterFailed -> "Couldn't set the reminder voice — reminders may read in the wrong language."
            setLoc != default -> "No voice for ${default.displayName} — reminders are read in English."
            else -> null
        }
        // Only the fallback note is readout-gated (#450 review): state notes
        // stay true even when an utterance is rejected before speaking.
        localeNoteFallback = localeNote?.startsWith("No voice for ") == true
    }

    /** Voice names snapshot for the re-probe key (#450 review): a voice
     * install/uninstall changes the probe meaning with no stored or locale
     * change. One binder read per utterance — same order as the speak call
     * it guards. */
    private fun voiceNames(tts: TextToSpeech): Set<String> =
        runCatching { tts.voices?.mapNotNull { it.name }.orEmpty().toSet() }
            .getOrDefault(emptySet())

    /** isLanguageAvailable wrapper so the probe loop reads flat. */
    private fun isLanguageAvailable(tts: TextToSpeech, loc: Locale): Boolean =
        runCatching {
            val r = tts.isLanguageAvailable(loc)
            r == TextToSpeech.LANG_AVAILABLE ||
                r == TextToSpeech.LANG_COUNTRY_AVAILABLE ||
                r == TextToSpeech.LANG_COUNTRY_VAR_AVAILABLE
        }.getOrDefault(false)

    /** Delayed watchdog wakeup: frees a stuck utterance even with no further alarms. */
    private fun watchdogFired(wgen: Int) {
        synchronized(lock) {
            if (wgen != watchdogGen || !speaking) return
            Log.w("ReminderTts", "speech timed out with no callback, advancing queue")
            speaking = false
            speakingId = null
            // State notes stay true even though nothing more will be heard
            // (#450 review); the fallback note does not — speech never
            // demonstrably started, so only onStart may flush it.
            val note = localeNote
            if (note != null && !localeNoteFallback) {
                TtsFailures.report(note)
                localeNote = null
            }
            // Same silence class as the fixed paths (#450 review): a stuck
            // utterance is never heard, so the next open must say so.
            TtsFailures.report(
                "A reminder could not be read aloud — the speech engine timed out.",
            )
        }
        pump()
    }

    private fun pump() {
        while (true) {
            // ONE critical section for check + dequeue + throttle: two separate
            // blocks let concurrent speak()/onDone pass the guard together and
            // fire two concurrent eng.speak calls. The engine snapshot leaves
            // the lock non-null, so a synchronous-init callback's pump() still
            // sees null and waits for the outer pump() instead of dropping.
            val step: Triple<TextToSpeech, Pair<String, String>?, Int> = synchronized(lock) {
                // Watchdog on a MONOTONIC clock: wall-clock jumps must neither
                // trip it early nor stall it. Utterances are seconds long;
                // a minute means stuck.
                if (speaking && SystemClock.uptimeMillis() - speakStartedAt > SPEAK_TIMEOUT_MS) {
                    Log.w("ReminderTts", "speech timed out, advancing queue")
                    speaking = false
                    speakingId = null
                    // State notes flush like the watchdog above (#450
                    // review); the fallback note waits for onStart.
                    val note = localeNote
                    if (note != null && !localeNoteFallback) {
                        TtsFailures.report(note)
                        localeNote = null
                    }
                    // Same surface as the watchdog above (#450 review).
                    TtsFailures.report(
                        "A reminder could not be read aloud — the speech engine timed out.",
                    )
                }
                if (!ready || speaking) return
                val eng = engine ?: return
                val n = queue.removeFirstOrNull()
                if (n != null) {
                    speaking = true
                    speakingId = n.first
                    speakStartedAt = SystemClock.uptimeMillis()
                    watchdogGen += 1
                }
                Triple(eng, n, watchdogGen)
            }
            val (eng, next, wgen) = step
            // Posted outside the lock: postDelayed only enqueues, and the
            // critical section should end before it. Guarded: a no-main-looper
            // environment (local unit test) requeues and bails — falling
            // through would speak a duplicate AND lose the throttle.
            if (next != null) {
                val posted = runCatching {
                    mainHandler.postDelayed({ watchdogFired(wgen) }, SPEAK_TIMEOUT_MS)
                }.getOrDefault(false)
                if (!posted) {
                    synchronized(lock) {
                        queue.addFirst(next)
                        speaking = false
                        speakingId = null
                    }
                    return
                }
            }
            if (next == null) return
            // Re-check silence per utterance (#450 review): speak() drops at
            // entry, but DND/vibrate can flip mid-flight while items are
            // already queued — playing them aloud would break the "stays
            // silent" contract. Intentional silence is never a failure: no
            // report, and the item is dropped like the entry path drops it.
            val silenceCtx = appCtxRef
            if (silenceCtx != null && isSilent(silenceCtx)) {
                synchronized(lock) { speaking = false; speakingId = null }
                continue
            }
            // Locale note, re-armed on stored-voice/locale/voices change
            // (#450 review): the probe ran once at init. Decided every
            // utterance; the flush happens at onStart, so a
            // rejected/thrown attempt never claims a fallback readout that
            // never happened.
            val nowStored = appCtxRef?.let { BehaviourSettings.ttsVoice(it) }.orEmpty()
            val nowLocale = Locale.getDefault()
            val nowVoices = engine?.let { voiceNames(it) }.orEmpty()
            if (nowStored != noteVoice || nowLocale != noteLocale || nowVoices != noteVoices) {
                engine?.let { decideReminderNote(it) }
            }
            // Stored voice/rate/pitch (#355); skipped at defaults so
            // untouched installs speak exactly like before. Existence is not
            // success (#450 review): a stored voice that resolves but won't
            // set gets its own report — a throwing voice setter is the
            // wrong-voice silence, and the decide() suppression above keys
            // on existence, so this is the only surface for it.
            val storedRequested = appCtxRef?.let { BehaviourSettings.ttsVoice(it) }.orEmpty().isNotEmpty()
            val storedOk = appCtxRef?.let { applyStoredTtsSettings(eng, it) } ?: true
            if (storedRequested && !storedOk) {
                TtsFailures.report("Couldn't use the stored voice — speech may use a different voice.")
            }
            // A throw (broken OEM binder state) counts like ERROR: clear and
            // advance. Uncaught here it would escape from onDone/onError's thread.
            val rc = runCatching {
                eng.speak(next.second, TextToSpeech.QUEUE_ADD, null, next.first)
            }.getOrNull()
            if (rc == null) {
                Log.w("ReminderTts", "speak threw, advancing queue")
                // State notes stay true even though nothing was spoken
                // (#450 review); the fallback note does not — it flushes
                // only at onStart.
                val note = localeNote
                if (note != null && !localeNoteFallback) {
                    TtsFailures.report(note)
                    localeNote = null
                }
                TtsFailures.report(
                    "A reminder could not be read aloud — the speech engine failed.",
                )
                // Clear the id like finished() does (#450 review): a late
                // onError for this id must not match the successor.
                synchronized(lock) { speaking = false; speakingId = null }
                continue
            }
            if (rc != TextToSpeech.ERROR) return
            // A rejected chunk never fires onDone — clear speaking and take
            // the next one in this loop, not via recursion: a broken engine
            // with N queued items would otherwise recurse N deep.
            Log.w("ReminderTts", "speak rejected, advancing queue")
            val note = localeNote
            if (note != null && !localeNoteFallback) {
                TtsFailures.report(note)
                localeNote = null
            }
            TtsFailures.report(
                "A reminder could not be read aloud — the speech engine rejected the text.",
            )
            synchronized(lock) { speaking = false; speakingId = null }
        }
    }
}
