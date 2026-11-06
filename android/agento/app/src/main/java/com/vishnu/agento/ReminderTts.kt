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
internal fun speechFailureLine(failure: TtsFailure): String = when (failure) {    TtsFailure.ENGINE_INIT -> "Spoken alert couldn't play — speech engine failed to start."
    TtsFailure.SPEAK_REJECTED -> "Spoken alert couldn't play — the engine rejected it."
    TtsFailure.SPEAK_THREW -> "Spoken alert couldn't play — the engine threw an error."
    TtsFailure.UTTERANCE_ERROR -> "Spoken alert stopped early — the engine reported an error."
    TtsFailure.TIMED_OUT -> "Spoken alert timed out — the engine never finished."
    TtsFailure.QUEUE_DROPPED -> "Spoken alert was dropped — too many reminders at once."
    TtsFailure.LOCALE_UNAVAILABLE -> "Spoken alert skipped — your language isn't available in the speech engine."
    TtsFailure.VOICE_MISSING -> "Spoken alert used the default voice — your saved voice is gone."
}

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
 * What [purgeTaskSpeech] removed: queued items dropped, whether an
 * in-flight utterance was suppressed, and which reminder points lost
 * items ([taskId] + [kindKey] — their failure notices go too).
 */
internal data class SpeechPurge(
    val dropped: Int,
    val inFlight: Boolean,
    val points: Set<Pair<String, String>>,
)

/**
 * Pure core of [ReminderTts.dropTask]/[ReminderTts.dropAll] (#470
 * review): removes every queued utterance for [taskId] — or every queued
 * utterance when [taskId] is null — and every owner entry for it, plus
 * the reported flags of exactly the removed items.
 *
 * The speaking item is suppressed, not stopped, at this layer: its owner
 * entry goes (so the completion callback can neither report nor retract),
 * but [speakingId] itself — and the audio — is the caller's to handle:
 * [ReminderTts.dropTask] stops it, [ReminderTts.dropAll] stops it, and in
 * both cases the silenced callback finds nothing to report. Its notice is
 * cancelled by the caller instead, so suppressing it here cannot orphan
 * an ENGINE_INIT tag. Pure and JVM-pinned: the engine needs Android, but
 * this decision does not.
 */
internal fun purgeTaskSpeech(
    queue: ArrayDeque<ReminderTts.QueuedSpeech>,
    owners: MutableMap<String, ReminderTts.QueuedSpeech>,
    reported: MutableSet<String>,
    speakingId: String?,
    taskId: String?,
): SpeechPurge {
    val points = mutableSetOf<Pair<String, String>>()
    var dropped = 0
    val it = queue.iterator()
    while (it.hasNext()) {
        val q = it.next()
        if (taskId == null || q.taskId == taskId) {
            it.remove()
            reported.remove(q.id)
            points.add(q.taskId to q.kindKey)
            dropped++
        }
    }
    val speakingOwner = speakingId?.let { owners[it] }
    val inFlight = speakingOwner != null && (taskId == null || speakingOwner.taskId == taskId)
    if (inFlight) {
        reported.remove(speakingOwner!!.id)
        points.add(speakingOwner.taskId to speakingOwner.kindKey)
    }
    // Owner entries for the purge go regardless: the queued ones above,
    // the suppressed in-flight one here, and any drift leftover (the map
    // only ever holds queued-or-speaking items).
    owners.entries.removeIf { e -> taskId == null || e.value.taskId == taskId }
    return SpeechPurge(dropped, inFlight, points)
}

/**
 * App-scoped TTS for task reminders (#306). One engine, one FIFO queue:
 * simultaneous alarms `QUEUE_ADD` behind whatever is speaking instead of
 * flushing/cutting each other. DND or non-normal ringer mode stays silent
 * (visual notification only).
 *
 * Every failure that drops an utterance reports loudly (#434): a toast plus
 * a companion "spoken alert couldn't play" notice on the same channel,
 * never Log-only. The visual reminder notification always posts first
 * (see [TaskAlarmReceiver]), so silence is never mistaken for success.
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
    /**
     * Utterance ids already reported as ENGINE_INIT while kept for retry.
     * A dead engine fires one init callback per ensureEngine, but the queue
     * survives across them — without this, every re-init would re-toast the
     * same unsaid utterances. Removed when the utterance completes (its
     * failure notice is then cancelled as retracted) or drops terminally
     * (its tag is overwritten with the terminal reason).
     */
    private val initReported = HashSet<String>()
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
            var retractInit = false
            // Only the current generation owns `speaking`: a stale onDone from
            // the shut-down engine clears nothing, and pump() always runs so a
            // stale completion still advances whatever is queued.
            synchronized(lock) {
                if (gen == generation) {
                    speaking = false
                    speakingId = null
                }
                owner = owners.remove(utteranceId)
                // A kept-for-retry utterance that now played: retract the
                // earlier ENGINE_INIT notice instead of orphaning it — but
                // only when no other reported utterance for the same point is
                // still outstanding. Two queued items can share one point
                // tag (stuck engine + nag re-fire); the first onDone must not
                // delete the second's evidence. A terminal error overwrites
                // the same tag below, so the set entry just goes away.
                val wasReported = initReported.remove(utteranceId)
                retractInit = !failed && wasReported && owners.values.none {
                    it.taskId == owner?.taskId && it.kindKey == owner?.kindKey &&
                        initReported.contains(it.id)
                }
            }
            if (failed && owner != null) {
                reportFailure(owner!!.taskId, owner!!.kindKey, owner!!.taskName, TtsFailure.UTTERANCE_ERROR)
            } else if (retractInit && owner != null) {
                cancelFailure(owner!!.taskId, owner!!.kindKey)
            }
            pump()
        }
        override fun onDone(utteranceId: String) = finished(utteranceId, false)
        @Deprecated("Deprecated in Java")
        override fun onError(utteranceId: String) = finished(utteranceId, true)
        override fun onError(utteranceId: String, errorCode: Int) = finished(utteranceId, true)
    }

    internal fun speak(appCtx: Context, kind: ReminderKind, task: ServerTask, overdueElapsed: Long? = null) {
        // Off means off: refuse new utterances while disabled. The
        // enqueue-time check spans monitors with cancelAll's purge, so an
        // alarm already in flight can still land behind it — pump()
        // re-checks under its own lock and reportFailure() stays silent
        // while disabled, so nothing queued can speak or notify (#470
        // review).
        if (!TaskReminders.isEnabled(appCtx)) return
        if (isSilent(appCtx)) return
        appCtxRef = appCtx.applicationContext
        // Quotes delimit the name in text but some engines verbalize "quote",
        // so they are stripped only on the speech path (reminderSpeech keeps them).
        val leadMinutes = BehaviourSettings.reminderLeadMinutes(appCtx)
        val text = ChatTts.cleanForSpeech(reminderSpeech(kind, task.name, task.speakName, overdueElapsed, leadMinutes))
            .replace("\"", "").replace(QUOTE_CHARS, "")
        val id = "${kind.key}-${UUID.randomUUID()}"
        val item = QueuedSpeech(id, text, task.id, kind.key, task.name)
        // Snapshot the evicted under lock, report outside it: reportFailure
        // does binder IPC (NotificationManager), which must never run while
        // holding `lock` that pump()/onDone/onError need.
        val dropped: List<QueuedSpeech> = synchronized(lock) {
            // Bounded, drop-oldest: for a stuck engine nothing speaks anyway,
            // and for a transient burst the freshest alarm is the most relevant
            // one to keep — the visual notification still posts for every alarm.
            // The dropped ones report loudly instead of vanishing (#434).
            val out = mutableListOf<QueuedSpeech>()
            while (queue.size >= MAX_QUEUED) {
                val d = queue.removeFirst()
                owners.remove(d.id)
                initReported.remove(d.id)
                out.add(d)
            }
            queue.addLast(item)
            owners[item.id] = item
            out
        }
        dropped.forEach {
            Log.w("ReminderTts", "queue full, dropping oldest speech ${it.id}")
            reportFailure(it.taskId, it.kindKey, it.taskName, TtsFailure.QUEUE_DROPPED)
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
            // Newly-queued only: utterances already reported stay reported —
            // re-toasting the whole old queue on every re-init is spam, and
            // their notices are retracted individually if they later play.
            val queued: List<QueuedSpeech> = synchronized(lock) {
                inited = false
                queue.filter { initReported.add(it.id) }
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
                        all.forEach {
                            owners.remove(it.id)
                            initReported.remove(it.id)
                        }
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
            // notice say this alarm hasn't spoken yet, while the next
            // speak() re-inits the engine and replays what stayed queued.
            // Already-reported utterances stay silent here (their notices
            // persist until the utterance plays or drops terminally).
            val queued: List<QueuedSpeech> = synchronized(lock) {
                queue.filter { initReported.add(it.id) }
            }
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
            owner = speakingId?.let {
                initReported.remove(it)
                owners.remove(it)
            }
            speakingId = null
        }
        if (owner != null) {
            reportFailure(owner!!.taskId, owner!!.kindKey, owner!!.taskName, TtsFailure.TIMED_OUT)
        }
        pump()
    }

    /**
     * Loud failure: a toast for whoever is looking at the phone plus a
     * companion failure notice on the reminders channel (auto-cancel, like
     * the reminder alert itself), so a missed spoken alert is never
     * indistinguishable from a played one (#434). Never throws:
     * binder/main-looper failures in a BroadcastReceiver path must not
     * take down the queue.
     */
    private fun reportFailure(taskId: String, kindKey: String, taskName: String, failure: TtsFailure) {
        val appCtx = appCtxRef ?: return
        val line = speechFailureLine(failure)
        runCatching {
            Log.w("ReminderTts", "$line task=$taskId kind=$kindKey")
        }
        // Off means off: never toast or post for a disabled reminder set,
        // however the failure raced the toggle (#470 review). The log line
        // above still fires — it is diagnostics, not user-visible.
        if (!TaskReminders.isEnabled(appCtx)) return
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
                .setStyle(NotificationCompat.BigTextStyle().bigText(line))
                .setPriority(NotificationCompat.PRIORITY_HIGH)
                .setAutoCancel(true)
                .setContentIntent(tap)
                .build()
            // Distinct tag per reminder point: the failure note sits next to
            // the original alert instead of overwriting it.
            mgr.notify(failureTag(taskId, kindKey), ALARM_NOTIF_ID, notif)
        }
    }

    /** Notification tag for one reminder point's speech-failure notice. */
    private fun failureTag(taskId: String, kindKey: String) = "tts-failed:$taskId#$kindKey"

    /** Retracts one point's failure notice (its utterance later played). */
    private fun cancelFailure(taskId: String, kindKey: String) {
        val appCtx = appCtxRef ?: return
        runCatching {
            val mgr = appCtx.getSystemService(Context.NOTIFICATION_SERVICE) as? NotificationManager
                ?: return
            mgr.cancel(failureTag(taskId, kindKey), ALARM_NOTIF_ID)
        }
    }

    /**
     * Drops every failure notice for one task (all kinds). Called when posted
     * alerts are dismissed ([TaskReminders.cancelPosted]), so a
     * completed/skipped/dismissed-via-action task leaves no orphaned failure
     * notice.
     */
    internal fun cancelFailures(appCtx: Context, taskId: String) {
        runCatching {
            val mgr = appCtx.getSystemService(Context.NOTIFICATION_SERVICE) as? NotificationManager
                ?: return
            for (kind in ReminderKind.entries) {
                runCatching { mgr.cancel(failureTag(taskId, kind.key), ALARM_NOTIF_ID) }
            }
        }
    }

    /**
     * Drops one reminder point's failure notice. Called when that point
     * posts again: a retried alarm that now plays must not sit next to its
     * own earlier failure — but other points' notices stay, since one
     * point firing says nothing about another's speech (#434 review).
     */
    internal fun cancelFailureFor(appCtx: Context, taskId: String, kindKey: String) {
        runCatching {
            val mgr = appCtx.getSystemService(Context.NOTIFICATION_SERVICE) as? NotificationManager
                ?: return
            mgr.cancel(failureTag(taskId, kindKey), ALARM_NOTIF_ID)
        }
    }

    /**
     * Purges one task's queued speech (all kinds). Called wherever the
     * task's alerts go away ([TaskReminders.cancelPosted], the gone and
     * rescheduled early-exits in `post`): cancelling the visual + failure
     * notices is not enough — a surviving queue entry would later speak a
     * completed task, or fail and re-post the tag just cancelled (#470
     * review). An in-flight utterance is stopped too, like the disable
     * path: completing a task mid-utterance must not keep speaking a done
     * task audibly. Its callback finds no owner entry and stays silent,
     * and its notice is cancelled here since the success path can no
     * longer retract it.
     */
    internal fun dropTask(taskId: String) {
        var eng: TextToSpeech? = null
        val purged = synchronized(lock) {
            purgeTaskSpeech(queue, owners, initReported, speakingId, taskId).also {
                // Capture under the lock, stop outside it: binder IPC must
                // never run while holding the queue lock. Only when
                // suppressing — stopping audio for any other reason would
                // be vandalism.
                if (it.inFlight) eng = engine
            }
        }
        runCatching { eng?.stop() }
        for ((t, kindKey) in purged.points) cancelFailure(t, kindKey)
        if (purged.dropped > 0 || purged.inFlight) {
            runCatching {
                Log.w("ReminderTts", "dropped ${purged.dropped} queued speech for task=$taskId" +
                    if (purged.inFlight) " (in-flight stopped)" else "")
            }
        }
    }
    internal fun dropTask(taskId: String) {
        val purged = synchronized(lock) {
            purgeTaskSpeech(queue, owners, initReported, speakingId, taskId)
        }
        for ((t, kindKey) in purged.points) cancelFailure(t, kindKey)
        if (purged.dropped > 0 || purged.inFlight) {
            runCatching {
                Log.w("ReminderTts", "dropped ${purged.dropped} queued speech for task=$taskId" +
                    if (purged.inFlight) " (in-flight finishes silently)" else "")
            }
        }
    }

    /**
     * Drops ALL queued speech (reminders disabled). Like [dropTask] for
     * every task at once, plus the engine itself is stopped: off means
     * off, so an utterance already playing must not finish audibly
     * either. Called from [TaskReminders.cancelAll]. Residual races land
     * on the pump()/reportFailure() guards: an enqueue behind this purge
     * is silently dropped before speaking, never reported.
     */
    internal fun dropAll() {
        val eng: TextToSpeech?
        val points: Set<Pair<String, String>>
        var hadSpeech = false
        synchronized(lock) {
            val purged = purgeTaskSpeech(queue, owners, initReported, speakingId, null)
            hadSpeech = purged.dropped > 0 || purged.inFlight
            // Unconditional: assigning false/null when idle is harmless,
            // and a stale generation must never resume a cleared queue.
            speaking = false
            speakingId = null
            eng = engine
            points = purged.points
        }
        runCatching { eng?.stop() }
        for ((taskId, kindKey) in points) cancelFailure(taskId, kindKey)
        if (hadSpeech) {
            runCatching { Log.w("ReminderTts", "reminders disabled, dropped all queued speech") }
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
                    timedOut = speakingId?.let {
                        initReported.remove(it)
                        owners.remove(it)
                    }
                    speakingId = null
                }
                // Disabled mid-flight (#470 review): an alarm already in
                // flight when the toggle flips can enqueue behind cancelAll's
                // purge (check-then-enqueue spans two monitors), so the
                // speak decision re-checks here, under this lock, instead of
                // trusting the enqueue-time check. Purge silently, not
                // loudly: an expected shutdown is not a failure, and the
                // disable path already cancelled the notices.
                val ctx = appCtxRef
                if (ctx != null && !TaskReminders.isEnabled(ctx)) {
                    queue.clear()
                    owners.clear()
                    initReported.clear()
                    return
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
                    // Unknown (null) is not empty: a binder hiccup answering
                    // null must not report "your saved voice is gone" for a
                    // stored pick — the same null-vs-empty distinction the
                    // voice picker draws (#434). Only a known voice list
                    // that lacks the pick reports.
                    val names = eng.voices?.map { v -> v.name } ?: return@runCatching
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
                    initReported.remove(next.id)
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
                initReported.remove(next.id)
                owners.remove(next.id)
            }
            reportFailure(next.taskId, next.kindKey, next.taskName, TtsFailure.SPEAK_REJECTED)
        }
    }
}
