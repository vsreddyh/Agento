package com.vishnu.agento

import android.app.NotificationManager
import android.content.Context
import android.media.AudioManager
import android.speech.tts.TextToSpeech
import android.speech.tts.UtteranceProgressListener
import android.util.Log
import java.util.Locale
import java.util.UUID

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
    private var speaking = false
    private var speakStartedAt = 0L

    private val progressListener = object : UtteranceProgressListener() {
        override fun onStart(utteranceId: String) = Unit
        override fun onDone(utteranceId: String) {
            synchronized(lock) { speaking = false }
            pump()
        }
        @Deprecated("Deprecated in Java")
        override fun onError(utteranceId: String) {
            synchronized(lock) { speaking = false }
            pump()
        }
        override fun onError(utteranceId: String, errorCode: Int) {
            synchronized(lock) { speaking = false }
            pump()
        }
    }

    internal fun speak(appCtx: Context, kind: ReminderKind, task: ServerTask, overdueElapsed: Long? = null) {
        if (isSilent(appCtx)) return
        // Quotes delimit the name in text but some engines verbalize "quote",
        // so they are stripped only on the speech path (reminderSpeech keeps them).
        val text = ChatTts.cleanForSpeech(reminderSpeech(kind, task.name, task.speakName, overdueElapsed))
            .replace("\"", "").replace(Regex("[“”]"), "")
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
        if (audio != null && audio.ringerMode != AudioManager.RINGER_MODE_NORMAL) return true
        // No SDK guard: notification channels need 26 and minSdk is 28.
        val nm = appCtx.getSystemService(Context.NOTIFICATION_SERVICE) as? NotificationManager
        if (nm != null && nm.currentInterruptionFilter != NotificationManager.INTERRUPTION_FILTER_ALL) return true
        return false
    }

    private fun ensureEngine(appCtx: Context) {
        synchronized(lock) {
            if (inited) return
            inited = true
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
        tts.setOnUtteranceProgressListener(progressListener)
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
                if (picked != null) tts.language = picked
            }
            pump()
        } else {
            val dead = engine
            synchronized(lock) {
                queue.clear()
                // Retry, don't seal: leaving inited=true with ready=false
                // would queue-but-never-speak forever after one failure.
                inited = false
                engine = null
            }
            runCatching { dead?.shutdown() }
            Log.w("ReminderTts", "engine init failed, dropping queued speech; will retry")
        }
    }

    private fun pump() {
        while (true) {
            // The engine snapshot leaves the lock non-null: reading `engine`
            // into a local first keeps the null check and the dequeue ordered
            // (a synchronous-init callback's pump() must see null and wait for
            // the outer pump(), never dequeue-then-drop).
            val eng: TextToSpeech = synchronized(lock) {
                // Watchdog: an utterance the engine accepted but never completes
                // (no onDone/onError) must not wedge the queue until process death.
                // Utterances are seconds long; a minute means stuck.
                if (speaking && System.currentTimeMillis() - speakStartedAt > SPEAK_TIMEOUT_MS) {
                    Log.w("ReminderTts", "speech timed out, advancing queue")
                    speaking = false
                }
                if (!ready || speaking) return
                engine ?: return
            }
            val next: Pair<String, String>? = synchronized(lock) {
                val n = queue.removeFirstOrNull()
                if (n != null) {
                    speaking = true
                    speakStartedAt = System.currentTimeMillis()
                }
                n
            }
            if (next == null) return
            // A throw (broken OEM binder state) counts like ERROR: clear and
            // advance. Uncaught here it would escape from onDone/onError's thread.
            val rc = runCatching {
                eng.speak(next.second, TextToSpeech.QUEUE_ADD, null, next.first)
            }.getOrElse {
                Log.w("ReminderTts", "speak threw, advancing queue", it)
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
