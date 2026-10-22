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
    /** Engine generation: a late callback from a dead engine must not clear the new one's state. */
    private var generation = 0
    /** Watchdog generation: stale delayed wakeups no-op instead of freeing a live utterance. */
    private var watchdogGen = 0
    // Lazy: object load must not require a main looper (unit tests have none).
    private val mainHandler: Handler by lazy { Handler(Looper.getMainLooper()) }

    private fun progressListener(gen: Int) = object : UtteranceProgressListener() {
        override fun onStart(utteranceId: String) = Unit
        private fun finished() {
            // Only the current generation owns `speaking`: a stale onDone from
            // the shut-down engine clears nothing, and pump() always runs so a
            // stale completion still advances whatever is queued.
            synchronized(lock) {
                if (gen == generation) speaking = false
            }
            pump()
        }
        override fun onDone(utteranceId: String) = finished()
        @Deprecated("Deprecated in Java")
        override fun onError(utteranceId: String) = finished()
        override fun onError(utteranceId: String, errorCode: Int) = finished()
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
            while (queue.size >= MAX_QUEUED) {
                val dropped = queue.removeFirst()
                Log.w("ReminderTts", "queue full, dropping oldest speech ${dropped.first}")
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
        // Listener attached before the init callback can pump: a fast callback
        // speaking before setOnUtteranceProgressListener would leave `speaking`
        // true forever (no onDone ever fires).
        //
        // Synchronous-init OEMs call the callback inside the constructor, before
        // `engine` is assigned: the callback then stashes its status and the tail
        // below applies it after assignment — so the language pick runs and a
        // synchronous failure shuts down the real object instead of leaking it.
        var earlyStatus: Int? = null
        val tts = TextToSpeech(appCtx) { status ->
            val assigned = synchronized(lock) { engine }
            if (assigned == null) {
                synchronized(lock) { earlyStatus = status }
            } else {
                onEngineInit(status)
            }
        }
        tts.setOnUtteranceProgressListener(progressListener(gen))
        engine = tts
        val es = synchronized(lock) { earlyStatus }
        if (es != null) onEngineInit(es)
    }

    private fun onEngineInit(status: Int) {
        synchronized(lock) { ready = status == TextToSpeech.SUCCESS }
        if (status == TextToSpeech.SUCCESS) {
            val tts = engine
            if (tts != null) {
                // Prefer the device default, fall back to US English: the
                // first available language wins, not the last one tried.
                val candidates = listOf(Locale.getDefault(), Locale.US)
                val picked = candidates.firstOrNull { loc ->
                    runCatching {
                        val r = tts.isLanguageAvailable(loc)
                        r == TextToSpeech.LANG_AVAILABLE ||
                            r == TextToSpeech.LANG_COUNTRY_AVAILABLE ||
                            r == TextToSpeech.LANG_COUNTRY_VAR_AVAILABLE
                    }.getOrDefault(false)
                }
                // The setter throws on some OEMs (missing data, illegal locale)
                // on the binder thread — fall back to the default voice.
                if (picked != null) runCatching { tts.language = picked }
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
        }
    }

    /** Delayed watchdog wakeup: frees a stuck utterance even with no further alarms. */
    private fun watchdogFired(wgen: Int) {
        synchronized(lock) {
            if (wgen != watchdogGen || !speaking) return
            Log.w("ReminderTts", "speech timed out with no callback, advancing queue")
            speaking = false
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
                }
                if (!ready || speaking) return
                val eng = engine ?: return
                val n = queue.removeFirstOrNull()
                if (n != null) {
                    speaking = true
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
                    }
                    return
                }
            }
            if (next == null) return
            // Stored voice/rate/pitch (#355); skipped at defaults so
            // untouched installs speak exactly like before.
            appCtxRef?.let { applyStoredTtsSettings(eng, it) }
            // A throw (broken OEM binder state) counts like ERROR: clear and
            // advance. Uncaught here it would escape from onDone/onError's thread.
            val rc = runCatching {
                eng.speak(next.second, TextToSpeech.QUEUE_ADD, null, next.first)
            }.getOrNull()
            if (rc == null) {
                Log.w("ReminderTts", "speak threw, advancing queue")
                synchronized(lock) { speaking = false }
                continue
            }
            if (rc != TextToSpeech.ERROR) return
            // A rejected chunk never fires onDone — clear speaking and take
            // the next one in this loop, not via recursion: a broken engine
            // with N queued items would otherwise recurse N deep.
            Log.w("ReminderTts", "speak rejected, advancing queue")
            synchronized(lock) { speaking = false }
        }
    }
}
