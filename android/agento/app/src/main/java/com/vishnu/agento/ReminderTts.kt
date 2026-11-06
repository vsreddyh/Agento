package com.vishnu.agento

import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.media.AudioManager
import android.net.Uri
import android.os.Handler
import android.os.Looper
import android.os.SystemClock
import android.speech.tts.TextToSpeech
import android.speech.tts.UtteranceProgressListener
import android.util.Log
import android.widget.Toast
import androidx.core.app.NotificationCompat
import java.util.Locale
import java.util.UUID

private val QUOTE_CHARS = Regex("[“”]")

/** Why a reminder's spoken alert never played (#434). One line per reason. */
internal enum class TtsFailure {
    ENGINE_INIT,
    SPEAK_REJECTED,
    SPEAK_THREW,
    UTTERANCE_ERROR,
    TIMED_OUT,
    QUEUE_DROPPED,
    LOCALE_UNAVAILABLE,
    VOICE_MISSING,
}

/** User-visible line for a speech failure: distinct per reason, never blank. */
internal fun speechFailureLine(failure: TtsFailure): String = when (failure) {
    TtsFailure.ENGINE_INIT -> "Spoken alert couldn't play — speech engine failed to start."
    TtsFailure.SPEAK_REJECTED -> "Spoken alert couldn't play — the engine rejected it."
    TtsFailure.SPEAK_THREW -> "Spoken alert couldn't play — the engine threw an error."
    TtsFailure.UTTERANCE_ERROR -> "Spoken alert stopped early — the engine reported an error."
    TtsFailure.TIMED_OUT -> "Spoken alert timed out — the engine never finished."
    TtsFailure.QUEUE_DROPPED -> "Spoken alert was dropped — too many reminders at once."
    TtsFailure.LOCALE_UNAVAILABLE -> "Spoken alert skipped — your language isn't available in the speech engine."
    TtsFailure.VOICE_MISSING -> "Spoken alert used the default voice — your saved voice is gone."
}

/** Appends a failure line to expanded notification text, once (retries re-report). */
internal fun withSpeechFailure(big: String, line: String): String =
    if (big.lines().any { it.trim() == line.trim() }) big
    else if (big.isEmpty()) line else "$big\n$line"

/**
 * Whether the reminder may speak: the device default — and only the device
 * default — must be available. The old US-English fallback is deliberately
 * NOT consulted (error beats wrong-language speech, #434): even when the
 * fallback engine data exists, an unavailable default fails loudly instead
 * of speaking a language the user didn't configure.
 */
internal fun shouldSpeakWithDefault(defaultAvailable: Boolean, fallbackAvailable: Boolean): Boolean =
    defaultAvailable

/** True when a stored voice pick names nothing this engine offers. */
internal fun isStoredVoiceMissing(availableNames: Collection<String>, want: String): Boolean {
    val w = want.trim()
    if (w.isEmpty()) return false
    return availableNames.none { it == w }
}

/**
 * App-scoped TTS for task reminders (#306). One engine, one FIFO queue:
 * simultaneous alarms `QUEUE_ADD` behind whatever is speaking instead of
 * flushing/cutting each other. DND or non-normal ringer mode stays silent
 * (visual notification only).
 *
 * Every failure that drops an utterance reports loudly (#434): a toast plus
 * a persistent "spoken alert couldn't play" notification on the same
 * channel, never Log-only. The visual reminder notification always posts
 * first (see [TaskAlarmReceiver]), so silence is never mistaken for success.
 */
object ReminderTts {
    private const val MAX_QUEUED = 10
    /** Watchdog: an utterance that never completes frees the queue after this. */
    private const val SPEAK_TIMEOUT_MS = 60_000L
    private val lock = Any()

    internal data class QueuedSpeech(
        val id: String,
        val text: String,
        val taskId: String,
        val kindKey: String,
        val taskName: String,
    )

    private val queue = ArrayDeque<QueuedSpeech>()
    /** Utterance id → queued speech, for async callbacks that only know the id. */
    private val owners = HashMap<String, QueuedSpeech>()
    @Volatile private var engine: TextToSpeech? = null
    private var ready = false
    private var inited = false
    /** Application context for reading stored TTS prefs in pump(). Set on
     * every speak(); the object never holds an Activity. Volatile like the
     * engine: written on the caller thread, read off the TTS callback. */
    @Volatile private var appCtxRef: Context? = null
    private var speaking = false
    private var speakingId: String? = null
    private var speakStartedAt = 0L
    /** Engine generation: a late callback from a dead engine must not clear the new one's state. */
    private var generation = 0
    /** Watchdog generation: stale delayed wakeups no-op instead of freeing a live utterance. */
    private var watchdogGen = 0
    // Lazy: object load must not require a main looper (unit tests have none).
    private val mainHandler: Handler by lazy { Handler(Looper.getMainLooper()) }

    private fun progressListener(gen: Int) = object : UtteranceProgressListener() {
        override fun onStart(utteranceId: String) = Unit
        private fun finished(utteranceId: String, failed: Boolean) {
            var owner: QueuedSpeech? = null
            // Only the current generation owns `speaking`: a stale onDone from
            // the shut-down engine clears nothing, and pump() always runs so a
            // stale completion still advances whatever is queued.
            synchronized(lock) {
                if (gen == generation) {
                    speaking = false
                    speakingId = null
                }
                owner = owners.remove(utteranceId)
            }
            if (failed && owner != null) {
                reportFailure(owner!!.taskId, owner!!.kindKey, owner!!.taskName, TtsFailure.UTTERANCE_ERROR)
            }
            pump()
        }
        override fun onDone(utteranceId: String) = finished(utteranceId, false)
        @Deprecated("Deprecated in Java")
        override fun onError(utteranceId: String) = finished(utteranceId, true)
        override fun onError(utteranceId: String, errorCode: Int) = finished(utteranceId, true)
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
        val item = QueuedSpeech(id, text, task.id, kind.key, task.name)
        synchronized(lock) {
            // Bounded, drop-oldest: for a stuck engine nothing speaks anyway,
            // and for a transient burst the freshest alarm is the most relevant
            // one to keep — the visual notification still posts for every alarm.
            // The dropped one reports loudly instead of vanishing (#434).
            while (queue.size >= MAX_QUEUED) {
                val dropped = queue.removeFirst()
                owners.remove(dropped.id)
                Log.w("ReminderTts", "queue full, dropping oldest speech ${dropped.id}")
                reportFailure(dropped.taskId, dropped.kindKey, dropped.taskName, TtsFailure.QUEUE_DROPPED)
            }
            queue.addLast(item)
            owners[item.id] = item
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
        // A constructor that throws (missing/broken engine) must not wedge
        // the queue with inited=true and no engine: reset and report loudly,
        // keeping the queue like the init-failure path below.
        val tts = runCatching {
            TextToSpeech(appCtx) { status ->
                val assigned = synchronized(lock) { engine }
                if (assigned == null) {
                    synchronized(lock) { earlyStatus = status }
                } else {
                    onEngineInit(status)
                }
            }
        }.getOrNull()
        if (tts == null) {
            val queued: List<QueuedSpeech> = synchronized(lock) {
                inited = false
                queue.toList()
            }
            Log.w("ReminderTts", "engine constructor failed, keeping queued speech for retry")
            queued.forEach {
                reportFailure(it.taskId, it.kindKey, it.taskName, TtsFailure.ENGINE_INIT)
            }
            return
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
                // Device default only (#434): falling back to US English when
                // the default is unavailable speaks the wrong language with
                // no signal. An unavailable default fails loudly instead —
                // the queued speech is dropped with a toast + notification,
                // never spoken in a language the user didn't configure.
                val defaultAvailable = runCatching {
                    val r = tts.isLanguageAvailable(Locale.getDefault())
                    r == TextToSpeech.LANG_AVAILABLE ||
                        r == TextToSpeech.LANG_COUNTRY_AVAILABLE ||
                        r == TextToSpeech.LANG_COUNTRY_VAR_AVAILABLE
                }.getOrDefault(false)
                // Second arg documents the retired fallback: available or
                // not, it must never rescue an unavailable default.
                val fallbackAvailable = runCatching {
                    val r = tts.isLanguageAvailable(Locale.US)
                    r == TextToSpeech.LANG_AVAILABLE ||
                        r == TextToSpeech.LANG_COUNTRY_AVAILABLE ||
                        r == TextToSpeech.LANG_COUNTRY_VAR_AVAILABLE
                }.getOrDefault(false)
                if (!shouldSpeakWithDefault(defaultAvailable, fallbackAvailable)) {
                    val dropped: List<QueuedSpeech> = synchronized(lock) {
                        val all = queue.toList()
                        queue.clear()
                        all.forEach { owners.remove(it.id) }
                        speaking = false
                        speakingId = null
                        inited = false
                        engine = null
                        ready = false
                        all
                    }
                    runCatching { tts.shutdown() }
                    Log.w("ReminderTts", "device locale unavailable, dropping ${dropped.size} queued speech")
                    dropped.forEach {
                        reportFailure(it.taskId, it.kindKey, it.taskName, TtsFailure.LOCALE_UNAVAILABLE)
                    }
                    return
                }
                // The setter throws on some OEMs (missing data, illegal locale)
                // on the binder thread — fall back to the default voice.
                runCatching { tts.language = Locale.getDefault() }
            }
            pump()
        } else {
            // Report loudly AND keep the queue for retry: the toast +
            // notification say this alarm didn't speak, while the next
            // speak() re-inits the engine and replays what stayed queued.
            val queued: List<QueuedSpeech> = synchronized(lock) { queue.toList() }
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
            queued.forEach {
                reportFailure(it.taskId, it.kindKey, it.taskName, TtsFailure.ENGINE_INIT)
            }
        }
    }

    /** Delayed watchdog wakeup: frees a stuck utterance even with no further alarms. */
    private fun watchdogFired(wgen: Int) {
        var owner: QueuedSpeech? = null
        synchronized(lock) {
            if (wgen != watchdogGen || !speaking) return
            Log.w("ReminderTts", "speech timed out with no callback, advancing queue")
            speaking = false
            owner = speakingId?.let { owners.remove(it) }
            speakingId = null
        }
        if (owner != null) {
            reportFailure(owner!!.taskId, owner!!.kindKey, owner!!.taskName, TtsFailure.TIMED_OUT)
        }
        pump()
    }

    /**
     * Loud failure: a toast for whoever is looking at the phone plus a
     * persistent notification on the reminders channel, so a missed spoken
     * alert is never indistinguishable from a played one (#434). Never
     * throws: binder/main-looper failures in a BroadcastReceiver path must
     * not take down the queue.
     */
    private fun reportFailure(taskId: String, kindKey: String, taskName: String, failure: TtsFailure) {
        val appCtx = appCtxRef ?: return
        val line = speechFailureLine(failure)
        Log.w("ReminderTts", "$line task=$taskId kind=$kindKey")
        runCatching {
            mainHandler.post {
                runCatching {
                    Toast.makeText(appCtx, line, Toast.LENGTH_LONG).show()
                }
            }
        }
        runCatching {
            val mgr = appCtx.getSystemService(Context.NOTIFICATION_SERVICE) as? NotificationManager
                ?: return
            mgr.createNotificationChannel(
                NotificationChannel(
                    TaskReminders.CHANNEL_ID,
                    appCtx.getString(R.string.task_reminder_channel),
                    NotificationManager.IMPORTANCE_HIGH,
                ).apply {
                    this.description = appCtx.getString(R.string.task_reminder_channel_desc)
                },
            )
            val title = taskName.ifBlank { FALLBACK_TASK_NAME }
            val tap = PendingIntent.getActivity(
                appCtx, ("tts-failed:$taskId#$kindKey").hashCode(),
                Intent(appCtx, MainActivity::class.java)
                    .setAction(TaskWidget.ACTION_TASKS)
                    .putExtra(TaskWidget.EXTRA_TASK_ID, taskId)
                    .setData(Uri.parse("agento://reminder/${Uri.encode(taskId)}")),
                PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
            )
            val notif = NotificationCompat.Builder(appCtx, TaskReminders.CHANNEL_ID)
                .setSmallIcon(android.R.drawable.ic_menu_agenda)
                .setContentTitle(title)
                .setContentText(line)
                .setStyle(NotificationCompat.BigTextStyle().bigText(withSpeechFailure("", line)))
                .setPriority(NotificationCompat.PRIORITY_HIGH)
                .setAutoCancel(true)
                .setContentIntent(tap)
                .build()
            // Distinct tag per reminder point: the failure note sits next to
            // the original alert instead of overwriting it.
            mgr.notify("tts-failed:$taskId#$kindKey", ALARM_NOTIF_ID, notif)
        }
    }

    private fun pump() {
        while (true) {
            var timedOut: QueuedSpeech? = null
            // Null step means "nothing to do": the single critical section
            // below decided to exit, reported after the lock.
            var step: Triple<TextToSpeech, QueuedSpeech?, Int>? = null
            synchronized(lock) {
                // Watchdog on a MONOTONIC clock: wall-clock jumps must neither
                // trip it early nor stall it. Utterances are seconds long;
                // a minute means stuck.
                if (speaking && SystemClock.uptimeMillis() - speakStartedAt > SPEAK_TIMEOUT_MS) {
                    Log.w("ReminderTts", "speech timed out, advancing queue")
                    speaking = false
                    timedOut = speakingId?.let { owners.remove(it) }
                    speakingId = null
                }
                if (!ready || speaking) {
                    // Exit: reported below, outside the lock.
                } else {
                    val eng = engine
                    if (eng != null) {
                        val n = queue.removeFirstOrNull()
                        if (n != null) {
                            speaking = true
                            speakingId = n.id
                            speakStartedAt = SystemClock.uptimeMillis()
                            watchdogGen += 1
                        }
                        step = Triple(eng, n, watchdogGen)
                    }
                }
            }
            timedOut?.let { reportFailure(it.taskId, it.kindKey, it.taskName, TtsFailure.TIMED_OUT) }
            val (eng, next, wgen) = step ?: return
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
            // Stored voice/rate/pitch (#355); skipped at defaults so
            // untouched installs speak exactly like before.
            appCtxRef?.let {
                applyStoredTtsSettings(eng, it)
                runCatching {
                    val names = eng.voices?.map { v -> v.name }.orEmpty()
                    if (isStoredVoiceMissing(names, BehaviourSettings.ttsVoice(it))) {
                        reportFailure(next.taskId, next.kindKey, next.taskName, TtsFailure.VOICE_MISSING)
                    }
                }
            }
            // A throw (broken OEM binder state) counts like ERROR: clear and
            // advance. Uncaught here it would escape from onDone/onError's thread.
            val rc = runCatching {
                eng.speak(next.text, TextToSpeech.QUEUE_ADD, null, next.id)
            }.getOrNull()
            if (rc == null) {
                Log.w("ReminderTts", "speak threw, advancing queue")
                synchronized(lock) {
                    speaking = false
                    speakingId = null
                    owners.remove(next.id)
                }
                reportFailure(next.taskId, next.kindKey, next.taskName, TtsFailure.SPEAK_THREW)
                continue
            }
            if (rc != TextToSpeech.ERROR) return
            // A rejected chunk never fires onDone — clear speaking and take
            // the next one in this loop, not via recursion: a broken engine
            // with N queued items would otherwise recurse N deep.
            Log.w("ReminderTts", "speak rejected, advancing queue")
            synchronized(lock) {
                speaking = false
                speakingId = null
                owners.remove(next.id)
            }
            reportFailure(next.taskId, next.kindKey, next.taskName, TtsFailure.SPEAK_REJECTED)
        }
    }
}
