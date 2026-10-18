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
    private val lock = Any()
    private val queue = ArrayDeque<Pair<String, String>>()
    private var engine: TextToSpeech? = null
    private var ready = false
    private var inited = false
    private var speaking = false

    internal fun speak(appCtx: Context, kind: ReminderKind, task: ServerTask, overdueElapsed: Long? = null) {
        if (isSilent(appCtx)) return
        // Quotes delimit the name in text but some engines verbalize "quote",
        // so they are stripped only on the speech path (reminderSpeech keeps them).
        val text = ChatTts.cleanForSpeech(reminderSpeech(kind, task.name, task.speakName, overdueElapsed))
            .replace("\"", "")
        val id = "${kind.key}-${UUID.randomUUID()}"
        synchronized(lock) {
            // Bounded: an OEM that never fires the init callback would
            // otherwise grow this forever (ready stays false, nothing drains).
            while (queue.size >= MAX_QUEUED) queue.removeFirst()
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
        engine = TextToSpeech(appCtx) { status ->
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
        engine?.setOnUtteranceProgressListener(object : UtteranceProgressListener() {
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
        })
    }

    private fun pump() {
        while (true) {
            val next: Pair<String, String>?
            synchronized(lock) {
                if (!ready || speaking) return
                next = queue.removeFirstOrNull()
                if (next != null) speaking = true
            }
            if (next == null) return
            val eng = engine
            if (eng == null) {
                // Unreachable today (engine is nulled only with ready=false),
                // but a null engine with speaking=true would wedge the queue.
                synchronized(lock) { speaking = false }
                return
            }
            val rc = eng.speak(next.second, TextToSpeech.QUEUE_ADD, null, next.first)
            if (rc != TextToSpeech.ERROR) return
            // A rejected chunk never fires onDone — clear speaking and take
            // the next one in this loop, not via recursion: a broken engine
            // with N queued items would otherwise recurse N deep.
            Log.w("ReminderTts", "speak rejected, advancing queue")
            synchronized(lock) { speaking = false }
        }
    }
}
