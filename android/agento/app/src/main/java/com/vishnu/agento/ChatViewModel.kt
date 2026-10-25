package com.vishnu.agento

import android.app.Application
import android.util.Log
import androidx.compose.runtime.State
import androidx.compose.runtime.mutableStateOf
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.collect
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

/** Lightweight row for the conversation switcher (no message bodies). */
data class ThreadSummary(
    val id: String,
    val title: String,
    val updatedAt: Long,
    val count: Int,
)

/** Fallback title for threads without a custom title. */
private const val UNTITLED = "New conversation"
/** First recovery poll after a drop: the orphaned turn needs a moment to
 * produce text worth adopting (#214). */
private const val RECOVERY_FIRST_DELAY_MS = 6_000L
/** Second poll: two polls returning the same text means the orphan stopped
 * producing, the only completion signal without a server resume primitive. */
private const val RECOVERY_SECOND_DELAY_MS = 15_000L

/** One toast-host notice: text + styling + re-fire identity (identical texts
 * can sit in the queue as distinct entries). */
data class ToastNotice(
    val text: String,
    val kind: ToastKind = ToastKind.Error,
    val seq: Long = 0L,
)

data class ChatUiState(
    val provider: String = "",
    val model: String = "",
    val effort: String = "",
    val path: String = "",
    val messages: List<ChatMessage> = emptyList(),
    val streaming: Boolean = false,
    val pending: String = "",
    /** Photos staged in the composer. Cleared on send; parked untouched while streaming.
     * Staged payloads are never blank: staged read raw, wire sites count wiredPhotos(). */
    val pendingAttachments: List<ChatAttachment> = emptyList(),
    /** Queued notices for the toast host, head-first so two rapid notifies surface. */
    val notices: List<ToastNotice> = emptyList(),
    val error: String = "",
    /** True once the persisted conversations finish loading; sends wait for it. */
    val ready: Boolean = false,
    /** Conversations in this tab, newest first. */
    val threads: List<ThreadSummary> = emptyList(),
    val threadId: String = "",
    /** Reachability of the server; null = not checked yet. */
    val online: Boolean? = null,
    /** Distinct tool names seen live this turn (pi-gateway's progress frames). */
    val activeTools: List<String> = emptyList(),
    /** Label of the latest live tool frame, e.g. what the tool is doing. */
    val activeToolLabel: String = "",
    /** Server-side totals for the active thread's conversation (#121);
     * null = unknown — which is also the steady state today, since pi-gateway
     * serves no `api/sessions/{id}`; see [SessionTotals]. */
    val serverTokens: SessionTotals? = null,
    /** Text queued while streaming (#122): auto-sends when the reply
     * finishes cleanly, restored to the composer on failure/stop. */
    val queued: String = "",
    /** Id of the server-side run backing the in-flight turn (#311), from
     * the `X-Agento-Run-Id` response header. Powers stop→cancel, steer and
     * the runs sheet; blank when the turn predates runs or already ended. */
    val activeRunId: String = "",
    /** Run named by a 429 refusal: the profile is busy with a turn this tab
     * did not start. Kept for the runs sheet (watch/cancel) after the text
     * is parked back in the composer. */
    val busyRunId: String = "",
    /** Runs sheet visibility + its rows for the active thread (#311). */
    val showRuns: Boolean = false,
    val runs: List<AgentRun> = emptyList(),
    val runsLoading: Boolean = false,
    /** Sheet-scoped error: rendered inside the runs sheet, never in the
     * chat error banner (whose Retry resends a turn — a runs-list outage
     * must not offer that). */
    val runsError: String = "",
    /** Approvals sheet for held tool calls (#313): pending cards with
     * approve/deny, plus the audit trail below. */
    val showApprovals: Boolean = false,
    val approvals: List<GateApproval> = emptyList(),
    val approvalsLoading: Boolean = false,
    val approvalsError: String = "",
    val auditRows: List<AuditRow> = emptyList(),
    /** Pending approvals right now: badges the approvals entry and raises
     * the in-turn banner while streaming. */
    val pendingApprovalCount: Int = 0,
    /** Split-brain signal from the server: decisions here cannot reach held
     * calls. The sheet banners it instead of showing an undecidable queue. */
    val approvalsDegraded: Boolean = false,
    /** Approval ids with a decision in flight: cards disable their buttons
     * so a double tap cannot fire two decisions. */
    val decidingApprovals: Set<String> = emptySet(),
    /** Memory sheet for learned facts (#315): pending proposals to confirm,
     * confirmed rows to edit/delete, topic pause toggles, and a composer. */
    val showMemory: Boolean = false,
    val memoryItems: List<MemoryItem> = emptyList(),
    val memoryPaused: List<String> = emptyList(),
    val memoryTopics: List<String> = emptyList(),
    val memoryWarnings: List<String> = emptyList(),
    val memoryLoading: Boolean = false,
    val memoryError: String = "",
    /** Pending proposals right now: badges the memory entry. */
    val pendingMemoryCount: Int = 0,
    /** Memory ids with a write in flight: their buttons disable so a double
     * tap cannot fire two writes. */
    val actingMemory: Set<String> = emptySet(),
    /** Slash skill picker (#372): query + invocable list for the composer.
     * Visibility is gated by the text itself (ChatScreen shows it only
     * while `slashQuery(pending)` is non-null), so thread switches, sends
     * and queue parks hide it without touching every pending reset. */
    val slash: SlashPickerState = SlashPickerState(),
)

/** Slash `/` skill picker state (#372). */
data class SlashPickerState(
    val query: String = "",
    val skills: List<SkillInfo> = emptyList(),
    val loading: Boolean = false,
    val error: String = "",
)

/** Device-observed usage for one conversation thread, summed from
 * per-message reports (#121). Pure for testability. */
data class ThreadUsage(
    /** Sum of reported turn totals (device-observed). */
    val total: Long = 0L,
    /** Of-prompt cached subset sum. */
    val cached: Long = 0L,
    /** Last reported prompt size = current context size (full history resent). */
    val lastPrompt: Long = 0L,
    /** First reported prompt size = baseline overhead hint (SOUL + skills +
     * tools, plus the first user message). */
    val firstPrompt: Long = 0L,
    /** Turns with usable server reports. */
    val counted: Int = 0,
    /** Assistant replies with no usable report (failed/interrupted/legacy). */
    val unreported: Int = 0,
)

/** Sums per-message usage across one thread's messages. */
fun threadUsageOf(messages: List<ChatMessage>): ThreadUsage {
    var total = 0L
    var cached = 0L
    var lastPrompt = 0L
    var firstPrompt = 0L
    var counted = 0
    var unreported = 0
    for (m in messages) {
        if (m.role == "user") continue
        // Blank assistant rows are transient (the in-flight streaming
        // placeholder), not turns — never count them either way.
        if (m.content.isBlank()) continue
        if (m.total > 0 || m.prompt > 0 || m.completion > 0) {
            // A turn without a reported total still contributes its
            // prompt + completion (completion-only/prompt-only reports,
            // or future shapes without `total`) instead of adding 0.
            total += if (m.total > 0) m.total else m.prompt + m.completion
            cached += m.cached
            if (firstPrompt == 0L && m.prompt > 0) firstPrompt = m.prompt
            if (m.prompt > 0) lastPrompt = m.prompt
            counted++
        } else {
            unreported++
        }
    }
    return ThreadUsage(
        total = total, cached = cached, lastPrompt = lastPrompt,
        firstPrompt = firstPrompt, counted = counted, unreported = unreported,
    )
}

/**
 * The Chat tab's ViewModel. Conversations
 * persist to disk (`ChatThreads`, up to 20 threads) so history survives
 * restarts; the switcher picks the active one and + starts a new thread
 * without deleting the rest. The full active history is sent per request.
 */
class ChatViewModel(app: Application, val tab: String) : AndroidViewModel(app) {

    private val api = ChatApi(app)
    private val runsApi = RunsApi(app)
    private val approvalsApi = ApprovalsApi(app)
    private val memoryApi = MemoryApi(app)
    private val teachApi = TeachApi(app)
    private val serverApi = ServerApi(app)
    private val appCtx: Application = app

    /** Decoded-thumbnail memory, VM-held so it survives rotation; evicted on
     * clear. Lazy (#339): constructing android.util.LruCache touches the
     * android.jar stubs, which would make a plain-JUnit ViewModel test
     * impossible — the cache is only needed once a photo renders. */
    private val thumbCacheDelegate = lazy { ThumbCache() }
    internal val thumbCache by thumbCacheDelegate

    /** Evict the thumbnails IF the cache exists (#339): the cache is lazy,
     * and a trim or a clear must not construct it just to empty it. */
    private fun evictThumbs() {
        if (thumbCacheDelegate.isInitialized()) thumbCache.evictAll()
    }

    /** The same guard for the per-key evictions (#339): constructing the
     * cache touches android.util.LruCache, whose android.jar stub throws
     * — a plain-JUnit ViewModel test must never build it. */
    private fun removeThumbKey(key: ThumbKey) {
        if (thumbCacheDelegate.isInitialized()) thumbCache.remove(key)
    }

    /** The same guard for a whole thread's thumbs (#339). */
    private fun removeThreadThumbs(threadKey: String) {
        if (thumbCacheDelegate.isInitialized()) thumbCache.removeThread(threadKey)
    }

    /** Memory-trim hook (#339): the cache is byte-bounded (~8 MB) but that
     * is still a lot to hold while the app sits in the background on a
     * 1 GB device. The UI-hidden trim is the signal that the process may
     * be killed; the bitmaps re-decode on the way back, so dropping them
     * costs time, not data. */
    private val trimCallback = object : android.content.ComponentCallbacks2 {
        override fun onConfigurationChanged(newConfig: android.content.res.Configuration) = Unit
        override fun onLowMemory() {
            // The legacy low-memory signal: free the bitmaps here too (#339).
            evictThumbs()
        }
        override fun onTrimMemory(level: Int) {
            if (level >= android.content.ComponentCallbacks2.TRIM_MEMORY_UI_HIDDEN) {
                evictThumbs()
            }
        }
    }

    init {
        appCtx.registerComponentCallbacks(trimCallback)
    }

    override fun onCleared() {
        super.onCleared()
        appCtx.unregisterComponentCallbacks(trimCallback)
        evictThumbs()
    }

    private val _state = mutableStateOf(
        ChatUiState(
            provider = api.providerFor(tab),
            model = api.modelFor(tab),
            effort = api.effortFor(tab, api.modelFor(tab)),
            path = api.pathFor(tab),
        )
    )
    val state: State<ChatUiState> = _state

    private var streamJob: Job? = null
    /** A post-drop recovery poll in flight (null otherwise). Cancelled by a
     * new send or a stop: recovery must never resurrect text behind a turn
     * the user has already moved past. */
    private var recoveryJob: Job? = null

    /** One runs-sheet load at a time (same shape as serverTotalsJob): a
     * newer open or a thread change cancels the older one, so stale rows
     * can never land on another thread. */
    private var runsJob: Job? = null

    /** Approval watch while a turn streams: a held call stalls the stream
     * with no frames, which otherwise reads as thinking. Polls pending
     * approvals so the banner can point at the approvals sheet. */
    private var approvalWatch: Job? = null

    /** Drops a pending recovery, if any. Called on every active-thread
     * change (switch/new/delete/fork), send, and stop — recovery belongs
     * to one tail of one thread, and anything that moves past it ends it
     * rather than letting an IO coroutine linger ~21s per drop. */
    private fun cancelRecovery() {
        recoveryJob?.cancel()
        recoveryJob = null
    }
    /** Send generation: incremented on every doSend/stop so a late
     * buffered Done/Error/Delta (callbackFlow is UNLIMITED) from a
     * cancelled turn can never overwrite a newer turn's state. */
    private var sendGen = 0
    /** Last `agento_run` snapshot status this turn (#311): null before the
     * first snapshot. Drives the cancelled-Done mapping and the replay
     * ignore below; reset per turn in doSend. */
    private var lastRunStatus: String? = null
    /** Threads already offered auto-resume: fires once per thread so a
     * failed resend never loops. */
    private var resumedThreads = mutableSetOf<String>()
    private var threadId: String = ""
    private var threads: List<ChatThread> = emptyList()
    /** Bumped by threads-mutating user ops so init's async publish can tell a
     * stale load from the live list. Bumped on the SUCCESS path of such an op
     * only — an op that returns early (not ready, wrong thread, blocked by a
     * live turn) changes nothing, so the in-flight load may still publish. */
    private val initGen = java.util.concurrent.atomic.AtomicInteger(0)

    /** Marks "the live list is newer than any in-flight load" (#339): every
     * threads-mutating op calls this on its success path, so a new op cannot
     * forget the bump. One helper, one place to look. */
    private fun touchThreads() {
        initGen.incrementAndGet()
    }

    init {
        // Load on IO, publish on Main; publish applies fully only when nothing outranked it.
        viewModelScope.launch(Dispatchers.IO) {
            val loaded = ChatThreads.load(appCtx, tab)
                .filter { it.id.isNotEmpty() }
                .sortedByDescending { it.updatedAt }
            // Empty threads never enter history: drop persisted drafts, keep chats.
            val freshThreads = loaded.filter { it.messages.isNotEmpty() }
            // One thread must exist to open: a fresh empty id when nothing loaded (first() is safe).
            val fresh = if (freshThreads.isEmpty()) {
                ChatThread(id = ChatThreads.newId(), updatedAt = ChatThreads.now())
            } else {
                null
            }
            val threadsFinal = fresh?.let { listOf(it) } ?: freshThreads
            val activeFinal = threadsFinal.first()
            if (fresh != null) {
                // Persist the fresh empty thread on IO (save is IO-gated).
                ChatThreads.save(appCtx, tab, threadsFinal)
            }
            withContext(Dispatchers.Main) {
                if (initGen.get() == 0) {
                    threads = threadsFinal
                    threadId = activeFinal.id
                    _state.value = _state.value.copy(
                        messages = ChatThreads.toUi(activeFinal.messages),
                        threads = summaries(),
                        threadId = threadId,
                        ready = true,
                    )
                } else {
                    // A user op already published newer state — just mark ready so the UI unblocks.
                    _state.value = _state.value.copy(ready = true)
                }
            }
            refreshServerTotals()
            // Pending proposals badge without opening the sheet.
            pollMemoryCount(api.pathFor(tab))
            // App-killed turn recovery (#311): an unanswered tail with a live run resends into it.
            threads.firstOrNull { it.id == threadId }?.let {
                maybeResume(threadId, ChatThreads.toUi(it.messages))
            }
        }
        // Out-of-band rewrite (backup import) → re-read the file (#339):
        // the import writes it directly, so the in-memory list is stale and
        // the next persist would write it back over the restore.
        viewModelScope.launch {
            var seen = ChatThreads.rewrites.value[tab] ?: 0L
            ChatThreads.rewrites.collect { map ->
                val v = map[tab] ?: 0L
                if (v != seen) {
                    seen = v
                    onThreadFileRewritten()
                }
            }
        }
        checkReachability()
    }

    fun refreshConfig() {
        val model = api.modelFor(tab)
        _state.value = _state.value.copy(
            provider = api.providerFor(tab),
            model = model,
            effort = api.effortFor(tab, model),
            path = api.pathFor(tab),
        )
        // The path can change here (profile switch in Settings).
        retargetMemory()
    }

    fun onPending(v: String) {
        _state.value = _state.value.copy(pending = v)
        updateSlash(v)
    }

    /** Slash picker cache per profile path: fetched invocable list + fetch time. */
    private var slashCache = mutableMapOf<String, Pair<Long, List<SkillInfo>>>()
    private var slashJob: Job? = null
    /** Profile path with a fetch in flight: keystrokes while it runs only
     * re-filter, never relaunch (the blocking OkHttp call is not
     * cooperatively cancellable, so relaunches pile up and last-wins). */
    private var slashFetching: String? = null
    /** Composer text at dismiss time: the card stays hidden until the text
     * changes, so Close actually closes instead of repainting an empty
     * card over the same `/` prefix. */
    private var slashDismissedAt: String? = null

    /**
     * Slash picker fetch (#372): first trigger per profile path fetches,
     * then a 60 s cache; a failed fetch keeps the stale list behind the
     * error so hand-typed commands still complete from a visible list.
     * Stale-guarded on the live text and path — a send or a profile
     * switch mid-flight must not land old rows on the new composer.
     */
    private fun updateSlash(text: String) {
        val query = slashQuery(text)
        if (query == null || text == slashDismissedAt) {
            slashJob?.cancel()
            slashJob = null
            slashFetching = null
            if (_state.value.slash != SlashPickerState()) {
                _state.value = _state.value.copy(slash = SlashPickerState())
            }
            return
        }
        // Any live typing clears the dismiss lock: the text moved on.
        slashDismissedAt = null
        val path = api.pathFor(tab)
        val now = System.currentTimeMillis()
        val cached = slashCache[path]
        if (cached != null && now - cached.first < 60_000) {
            _state.value = _state.value.copy(
                slash = SlashPickerState(query = query, skills = cached.second),
            )
            return
        }
        if (slashFetching == path) {
            // A fetch for this path is already in flight: re-filter only.
            // Relaunching per keystroke piles up blocking calls that
            // cancellation cannot abort; the in-flight one lands and the
            // live query filters it.
            _state.value = _state.value.copy(
                slash = _state.value.slash.copy(query = query),
            )
            return
        }
        if (_state.value.slash.query != query || !_state.value.slash.loading) {
            _state.value = _state.value.copy(
                slash = SlashPickerState(
                    query = query,
                    skills = cached?.second.orEmpty(),
                    loading = true,
                ),
            )
        }
        slashFetching = path
        slashJob?.cancel()
        slashJob = viewModelScope.launch {
            val res = serverApi.listSkills(path)
            // Text moved on, or the profile switched, while fetching.
            if (slashQuery(_state.value.pending) == null || api.pathFor(tab) != path) {
                slashFetching = null
                return@launch
            }
            val liveQuery = slashQuery(_state.value.pending) ?: query
            slashFetching = null
            res.fold(
                onSuccess = { list ->
                    val inv = invocableSkills(list)
                    slashCache[path] = System.currentTimeMillis() to inv
                    _state.value = _state.value.copy(
                        slash = SlashPickerState(query = liveQuery, skills = inv),
                    )
                },
                onFailure = {
                    _state.value = _state.value.copy(
                        slash = SlashPickerState(
                            query = liveQuery,
                            skills = cached?.second.orEmpty(),
                            error = "Couldn't load skills",
                        ),
                    )
                },
            )
        }
    }

    /** Picker row tap: the `/` token becomes `/skill:<name> `, the picker
     * closes, and the keyboard stays up with the cursor at the end (a
     * plain-String field value resets the cursor there). */
    fun pickSkill(name: String) {
        val cur = _state.value.pending
        if (!cur.startsWith("/")) return
        slashJob?.cancel()
        slashJob = null
        slashFetching = null
        slashDismissedAt = null
        _state.value = _state.value.copy(
            pending = insertSkillCommand(cur, name),
            slash = SlashPickerState(),
        )
    }

    fun dismissSlash() {
        slashJob?.cancel()
        slashJob = null
        slashFetching = null
        // Locked to the current text: the visibility gate keys on it, so
        // the card stays hidden until the user types something new.
        slashDismissedAt = _state.value.pending
        _state.value = _state.value.copy(slash = SlashPickerState())
    }

    /** Stages picked photos, capped at [MAX_IMAGES_PER_TURN] — excess is dropped
     * with an Info notice (nothing failed, the cap held). Only wired photos
     * occupy cap slots; growth is a before/after diff. */
    fun addPendingAttachments(items: List<ChatAttachment>) {
        val wired = items.wiredPhotos()
        // Silent, not a cap refusal: blanks are not photos, so nothing was dropped.
        if (wired.isEmpty()) return
        val cur = _state.value.pendingAttachments
        val room = MAX_IMAGES_PER_TURN - cur.wiredPhotoCount()
        if (room <= 0) {
            notify(capRefused(room), ToastKind.Info)
            return
        }
        val take = wired.take(room)
        val dropped = wired.size - take.size
        // Stored canonical-filtered: only wired photos occupy slots — the
        // same rule the history store uses (photos.wiredPhotos()). A blank
        // in the staged set is therefore dropped silently; the stage path
        // never produces one (prepareAttachment throws instead), so this
        // is a boundary rule, not a user-visible drop.
        _state.value = _state.value.copy(pendingAttachments = cur.wiredPhotos() + take)
        if (dropped > 0) {
            notify(capRefused(room), ToastKind.Info)
        }
    }

    /** True while the composer has a free photo slot (re-checked mid-batch). Counted wired. */
    fun hasAttachmentRoom(): Boolean =
        _state.value.pendingAttachments.wiredPhotoCount() < MAX_IMAGES_PER_TURN

    /** Whether [id] is still the active thread: batches drop if a switch landed mid-decode. */
    fun isThreadCurrent(id: String): Boolean = id == threadId

    /** Live thread id for batch homing (see [isThreadCurrent]). */
    fun threadIdNow(): String = threadId

    /** Live staged-photo count for tap-time cap math (the UI reads the VM live). Counted wired. */
    fun pendingAttachmentCount(): Int = _state.value.pendingAttachments.wiredPhotoCount()

    /** Appends heard text to the composer: read + write atomic so rapid results
     * can't clobber each other. Preserves user-typed text. */
    fun appendPending(text: String) {
        if (text.isBlank()) return
        val cur = _state.value.pending
        _state.value = _state.value.copy(
            pending = if (cur.isBlank()) text else joinPending(cur, text),
        )
    }

    /** Toast-host notice: appended to the queue, so a repeated identical notice
     * re-fires (distinct sequence) instead of deduping silent. Main-confined. */
    fun notify(msg: String, kind: ToastKind = ToastKind.Error) {
        // Bounded per kind: Errors and Infos cap independently (3 + 2) — Infos never evict an Error.
        val combined = _state.value.notices + ToastNotice(msg, kind, noticeSeq.incrementAndGet())
        val errors = combined.filter { it.kind == ToastKind.Error }.takeLast(3)
        val infos = combined.filter { it.kind != ToastKind.Error }.takeLast(2)
        _state.value = _state.value.copy(
            notices = (errors + infos).sortedBy { it.seq },
        )
    }

    /** Monotonic notice sequence for re-fire identity (see [notify]); atomic. */
    private val noticeSeq = java.util.concurrent.atomic.AtomicLong(0L)

    /** Single spelling for the thread-load dead-tap answer. */
    private fun notifyLoading() =
        notify("Conversations still loading — try again in a moment.", ToastKind.Info)

    /** Single spelling for the thread-change staged-discard notice. */
    private fun stagedDiscardedNotice() =
        notify("Staged photos discarded — they belonged to the previous thread.", ToastKind.Info)

    /** Notices that survive a thread change: Info toasts must not render over the
     * new composer — Errors are failures. */
    private fun retainedNotices(): List<ToastNotice> =
        _state.value.notices.filter { it.kind == ToastKind.Error }

    /** Single pending-joint spelling: one-space gap, both sides ride verbatim. */
    private fun joinPending(first: String, second: String): String =
        if (first.isBlank()) second else "${first.trimEnd()} $second"

    /** Removes a staged composer photo by stable pick id: the strip's index comes
     * from a state snapshot, so removing by position drops the WRONG photo. */
    fun removePendingAttachmentById(id: String) {
        if (id.isEmpty()) return
        val cur = _state.value.pendingAttachments
        if (cur.none { it.id == id }) return
        _state.value = _state.value.copy(
            pendingAttachments = cur.filterNot { it.id == id },
        )
    }

    /** The staged photo with [id] (#339): the remove path resolves the
     * payload from the LIVE list, not a composition snapshot — a batch
     * landing between recomposition and tap shifts the snapshot's index. */
    internal fun pendingAttachmentById(id: String): ChatAttachment? =
        if (id.isEmpty()) null else _state.value.pendingAttachments.firstOrNull { it.id == id }

    /** The staged photo at [index] (#339): the LEGACY remove path (an
     * empty id — every legacy pick shares it) can only be resolved by
     * position, and the LIVE list is the only one that is not a stale
     * composition snapshot. */
    internal fun pendingAttachmentAt(index: Int): ChatAttachment? =
        _state.value.pendingAttachments.getOrNull(index)

    /** Removes the staged photo at [index] (#339): a legacy payload (empty
     * id) cannot be removed by id — every legacy pick shares that empty id,
     * so the id path would drop the wrong photo (or nothing). */
    fun removePendingAttachmentAt(index: Int) {
        val cur = _state.value.pendingAttachments
        if (index !in cur.indices) return
        _state.value = _state.value.copy(
            pendingAttachments = cur.filterIndexed { i, _ -> i != index },
        )
    }

    /** Drops the shown head of [ChatUiState.notices] after the toast host shows it. */
    fun consumeNotice() {
        if (_state.value.notices.isNotEmpty()) {
            _state.value = _state.value.copy(notices = _state.value.notices.drop(1))
        }
    }

    /** Fresh-process detector for the camera-gate reset: true once, on the first sweep ever. */
    private var sweepGateResetDone = false

    fun consumeSweepGateReset(): Boolean {
        if (sweepGateResetDone) return false
        sweepGateResetDone = true
        return true
    }

    /** History rows: only threads with actual chats, newest first. */
    private fun summaries(): List<ThreadSummary> = threads
        .filter { it.messages.isNotEmpty() }
        .map {
            ThreadSummary(
                id = it.id,
                title = it.title.ifBlank { UNTITLED },
                updatedAt = it.updatedAt,
                count = it.messages.size,
            )
        }

    private fun persist() {
        // A pending rewrite (backup import) merges FIRST: the restored file
        // is newer than this list for every thread but the live one (#339),
        // so persisting the list alone would resurrect the pre-import
        // conversations over the restore.
        val merged = consumePendingRewrite()
        // Drafts never hit disk: only threads with chats are worth keeping.
        val snapshot = merged ?: threads.filter { it.messages.isNotEmpty() }
        viewModelScope.launch(Dispatchers.IO) {
            ChatThreads.save(appCtx, tab, snapshot)
        }
    }

    /** The restored threads of a rewrite that landed mid-turn (#339): the
     * live turn's messages are not in the file yet, so the file is adopted
     * at the next persist instead of immediately. */
    private var pendingRestored: List<ChatThread>? = null

    /** Merges a pending rewrite into the in-memory list (#339): the
     * restored file wins for every thread but the live one, whose
     * just-produced messages would otherwise be lost.
     *
     * LIVE WINS, ON PURPOSE: the merge only runs when a turn is in flight
     * (the file predates that turn), so the live thread's messages are the
     * newest thing the user has. A restored file that no longer holds it
     * does not delete it — the turn finishes, and the merge keeps it. */
    private fun consumePendingRewrite(): List<ChatThread>? {
        val restored = pendingRestored ?: return null
        pendingRestored = null
        val live = threads.firstOrNull { it.id == threadId }
        val merged = restored.filter { it.id != threadId } + listOfNotNull(live)
        threads = merged
        _state.value = _state.value.copy(threads = summaries())
        return merged.filter { it.messages.isNotEmpty() }
    }

    /** A thread file was rewritten out-of-band (#339): the backup import
     * writes it directly, so the in-memory list is stale. Re-read it, or
     * the next persist writes the pre-import threads back over the
     * restore. */
    private fun onThreadFileRewritten() {
        viewModelScope.launch(Dispatchers.IO) {
            val restored = ChatThreads.load(appCtx, tab)
                .filter { it.id.isNotEmpty() }
                .sortedByDescending { it.updatedAt }
                .filter { it.messages.isNotEmpty() }
            withContext(Dispatchers.Main) {
                if (_state.value.streaming) {
                    // Mid-turn: the file predates the live turn, so
                    // adopting it now would drop that turn. Merge at the
                    // next persist instead.
                    pendingRestored = restored
                } else {
                    applyRewrite(restored)
                }
            }
        }
    }

    /** Adopts a rewritten thread file (#339): the restored list is
     * authoritative, and a conversation it no longer holds must not stay
     * on screen (its next persist would put it back). */
    private fun applyRewrite(restored: List<ChatThread>) {
        // The composer clears like every other thread change (#339): staged
        // photos belong to the conversation the restore just replaced, and
        // sending them into the restored one is the mis-send class the
        // switch/new/delete sites already close.
        val hadStaged = _state.value.pendingAttachments.wiredPhotoCount() > 0
        // Staged thumbs are keyed by the thread they were staged in (#339).
        val stagedRewrite = _state.value.pendingAttachments
        val stagedRewriteThread = threadId
        val active = restored.firstOrNull { it.id == threadId }
        if (active != null) {
            threads = restored
            _state.value = _state.value.copy(
                messages = ChatThreads.toUi(active.messages),
                threads = summaries(),
                pendingAttachments = emptyList(),
                notices = retainedNotices(),
            )
        } else {
            val fresh = ChatThread(id = ChatThreads.newId(), updatedAt = ChatThreads.now())
            threads = listOf(fresh) + restored
            threadId = fresh.id
            _state.value = _state.value.copy(
                messages = emptyList(),
                threads = summaries(),
                threadId = threadId,
                pendingAttachments = emptyList(),
                notices = retainedNotices(),
            )
        }
        if (hadStaged) {
            stagedDiscardedNotice()
        }
        evictStagedThumbs(stagedRewrite, stagedRewriteThread)
    }

    /** Evicts the thumbnails of a discarded staged set (#339): they live in
     * the cache under the thread they were staged in, and a discard left
     * them there until pressure, a trim, or the thread's deletion. */
    private fun evictStagedThumbs(staged: List<ChatAttachment>, thread: String) {
        staged.forEachIndexed { i, a ->
            removeThumbKey(thumbCacheKey(a.id, i, a.base64, thread))
        }
    }

    private fun upsertActive(messages: List<ChatMessage>) {
        val customTitle = threads.firstOrNull { it.id == threadId }
            ?.title?.takeIf { it.isNotBlank() && it != UNTITLED }
        val title = customTitle ?: ChatThreads.titleFor(messages)
        threads = threads.map {
            if (it.id == threadId) {
                it.copy(
                    title = title,
                    updatedAt = ChatThreads.now(),
                    messages = ChatThreads.toStored(messages),
                )
            } else it
        }
        _state.value = _state.value.copy(threads = summaries())
    }

    /** Switches to another conversation; the draft stays on the old one. */
    fun switchThread(id: String) {
        // Voiced on staged content like newConversation()/deleteThread(): an empty composer stays silent.
        if (!_state.value.ready) {
            if (_state.value.pendingAttachments.wiredPhotoCount() > 0 ||
                _state.value.pending.isNotBlank()
            ) {
                notifyLoading()
            }
            return
        }
        if (id == threadId || _state.value.streaming) {
            // Voiced (#339): a switch during a live turn reads as a dead
            // tap otherwise.
            if (id != threadId && _state.value.streaming) {
                notify("Still answering — switch after this reply finishes.", ToastKind.Info)
            }
            return
        }
        val target = threads.firstOrNull { it.id == id } ?: return
        // Past every no-op return: this tap republishes thread state, so it outranks init's load.
        touchThreads()
        // Staged photos are keyed by the thread they were staged in (#339):
        // capture the set and that thread before the switch, so the
        // discard can evict the exact cache entries.
        val stagedSwitch = _state.value.pendingAttachments
        val stagedThread = threadId
        threadId = id
        cancelRecovery()
        runsJob?.cancel()
        stopApprovalWatch()
        retargetApprovals()
        retargetMemory()
        // Composer clears on switch: carrying typed text into the new thread
        // would fire A's draft into B on the next send.
        // DELIBERATE SPLIT, named so the next edit doesn't "fix" it into
        // uniformity: navigate (switch/open) DISCARDS with voice, fresh-thread
        // (new/delete) PARKS via joinPending — park mis-sends, clear loses words.
        // Both drops voice: the photo notice below, plus the typed-text one.
        val sw = _state.value
        val hadTypedSwitch = sw.pending.isNotBlank() || sw.queued.isNotBlank()
        val hadStaged = _state.value.pendingAttachments.wiredPhotoCount() > 0
        _state.value = _state.value.copy(
            messages = ChatThreads.toUi(target.messages),
            threads = summaries(),
            threadId = id,
            error = "",
            pending = "",
            // The notice queue drops Infos with them (Errors survive).
            pendingAttachments = emptyList(),
            notices = retainedNotices(),
            activeTools = emptyList(),
            activeToolLabel = "",
            serverTokens = null,
            queued = "",
            // Run ids belong to the old thread's turn: a switch must not
            // stop another thread's run.
            activeRunId = "", busyRunId = "",
            showRuns = false, runsLoading = false,
        )
        if (hadStaged) {
            stagedDiscardedNotice()
        }
        evictStagedThumbs(stagedSwitch, stagedThread)
        if (hadTypedSwitch) {
            notify("Typed draft discarded — it belonged to the previous thread.", ToastKind.Info)
        }
        refreshServerTotals()
        maybeResume(id, ChatThreads.toUi(target.messages))
    }

    /**
     * Opens a thread from a notification deep-link (#287): the task
     * instruction's tap-to-open card. With [autosend] the thread's
     * unanswered user message is sent immediately — this is what "starts"
     * the instructed chat. Single-shot and guarded: it fires only when the
     * tail is a user message with no assistant reply yet and nothing is
     * streaming, so a stale tap can neither double-send nor fire into a
     * live turn.
     *
     * [onDone] fires once the open has been attempted (found or not), so
     * the caller consumes the deep-link on confirmed open rather than
     * optimistically: clearing first would orphan the thread as an unsent
     * draft if the ViewModel was not ready yet. Not-ready returns without
     * firing, so the unconsumed target retries when ready flips.
     */
    fun openThread(id: String, autosend: Boolean, onDone: () -> Unit = {}) {
        if (!_state.value.ready) {
            // Voiced like the other not-ready gates (#339): a deep-link tap
            // during thread load must not read as a dead button. Silent when
            // the composer is empty, like switchThread().
            if (_state.value.pendingAttachments.wiredPhotoCount() > 0 ||
                _state.value.pending.isNotBlank()
            ) {
                notifyLoading()
            }
            return
        }
        // Written to disk after this ViewModel loaded: merge disk-only threads in.
        viewModelScope.launch {
            if (threads.none { it.id == id }) {
                val loaded = withContext(Dispatchers.IO) {
                    ChatThreads.load(appCtx, tab)
                        .filter { it.id.isNotEmpty() && it.messages.isNotEmpty() }
                }
                // Newest-first, like init's load (#339): the append path
                // left the switcher out of order, and a persisted empty
                // draft must not enter history.
                threads = (threads + loaded.filter { l -> threads.none { it.id == l.id } })
                    .sortedByDescending { it.updatedAt }
                    // The storage cap, like load()'s (#339): the merged list
                    // must not exceed what the next persist would write.
                    .take(ChatThreads.MAX_THREADS)
            }
            // Consumed only when the open was not blocked on a live turn:
            // a streaming-blocked tap keeps its target and retries on re-tap.
            if (openThreadLoaded(id, autosend)) onDone()
        }
    }

    private fun openThreadLoaded(id: String, autosend: Boolean): Boolean {
        val target = threads.firstOrNull { it.id == id } ?: return true
        if (id != threadId) {
            // Blocked on a live turn: NOT consumed, so the retained target
            // retries on the next tap.
            if (_state.value.streaming) {
                // Voiced (#339), like the switch path above.
                notify("Still answering — open after this reply finishes.", ToastKind.Info)
                return false
            }
            // Real publish past every no-op return, same rule as switchThread.
            touchThreads()
            // Staged thumbs are keyed by the thread they were staged in
            // (#339): capture both before the switch.
            val stagedOpen = _state.value.pendingAttachments
            val stagedOpenThread = threadId
            threadId = id
            cancelRecovery()
            runsJob?.cancel()
            stopApprovalWatch()
            retargetApprovals()
            retargetMemory()
            // Composer clears on open like switchThread (cross-thread mis-send), and voices it.
            val op = _state.value
            val hadTypedOpen = op.pending.isNotBlank() || op.queued.isNotBlank()
            // Same staged-discard voice as switchThread above.
            val hadStagedOpen = _state.value.pendingAttachments.wiredPhotoCount() > 0
            _state.value = _state.value.copy(
                messages = ChatThreads.toUi(target.messages),
                threads = summaries(),
                threadId = id,
                error = "",
                pending = "",
                // Staged photos die with the text draft: a parked photo firing into
                // the wrong thread is a silent mis-send.
                pendingAttachments = emptyList(),
                notices = retainedNotices(),
                activeTools = emptyList(),
                activeToolLabel = "",
                serverTokens = null,
                queued = "",
                activeRunId = "", busyRunId = "",
                showRuns = false, runsLoading = false,
            )
            if (hadStagedOpen) {
                stagedDiscardedNotice()
            }
            evictStagedThumbs(stagedOpen, stagedOpenThread)
            if (hadTypedOpen) {
                notify("Typed draft discarded — it belonged to the previous thread.", ToastKind.Info)
            }
            refreshServerTotals()
            maybeResume(id, ChatThreads.toUi(target.messages))
        }
        if (!autosend || _state.value.streaming) return true
        val msgs = _state.value.messages
        val last = msgs.lastOrNull() ?: return true
        // No assistant reply yet in this thread: anything else means leave it alone.
        if (last.role != "user" || msgs.any { it.role == "assistant" }) return true
        val provider = api.providerFor(tab)
        val model = api.modelFor(tab)
        val effort = api.effortFor(tab, model)
        val path = api.pathFor(tab)
        // Photos sit out an autosend like every other resend.
        noteParkedPhotos("Opened")
        _state.value = _state.value.copy(
            provider = provider, model = model, effort = effort, path = path,
            // Draft and queue stay in the composer: autosend fires the thread's own tail.
            pending = withParkedQueue(_state.value.pending),
            streaming = true, error = "", queued = "",
        )
        doSend(msgs, path, provider, model, effort)
        return true
    }

    /** + starts a new conversation; threads with chats are kept, empty
     * drafts are dropped so they never pile up in history. */
    fun newConversation() {
        // Voiced on staged content like send()/retry(): an empty composer stays silent.
        if (!_state.value.ready) {
            if (_state.value.pendingAttachments.wiredPhotoCount() > 0 ||
                _state.value.pending.isNotBlank()
            ) {
                notifyLoading()
            }
            return
        }
        // Real publish past the not-ready return: only from here may this op outrank init's load.
        touchThreads()
        // Bump the generation like stop(): a Done/Error already buffered in
        // the callbackFlow channel must go stale, or it lands on the fresh
        // thread and persists the old turn's assistant message there.
        sendGen++
        streamJob?.cancel()
        streamJob = null
        serverTotalsJob?.cancel()
        // Stop the server-side run too, not just the local socket: a New that
        // stayed local would leave the agent running invisibly. Fire-and-forget.
        val newRunToCancel = _state.value.activeRunId
        val newCancelPath = _state.value.path
        if (newRunToCancel.isNotBlank()) {
            viewModelScope.launch(Dispatchers.IO) {
                runCatching { runsApi.cancel(newCancelPath, newRunToCancel) }
                    .onFailure { runCatching { Log.w("ChatRuns", "cancel $newRunToCancel failed", it) } }
            }
        }
        val fresh = ChatThread(id = ChatThreads.newId(), updatedAt = ChatThreads.now())
        threads = listOf(fresh) + threads.filter { it.messages.isNotEmpty() }
        // Staged thumbs are keyed by the thread they were staged in (#339).
        val stagedNew = _state.value.pendingAttachments
        val stagedNewThread = threadId
        threadId = fresh.id
        cancelRecovery()
        runsJob?.cancel()
        stopApprovalWatch()
        retargetApprovals()
        retargetMemory()
        // Same queued→pending restore as stop(): killing the stream must not eat
        // queued text.
        val s = _state.value
        val parked = when {
            s.queued.isBlank() -> ""
            s.pending.isBlank() -> s.queued
            else -> joinPending(s.pending, s.queued)
        }
        // Same staged-discard voice as switchThread.
        val hadStagedNew = _state.value.pendingAttachments.wiredPhotoCount() > 0
        _state.value = _state.value.copy(
            messages = emptyList(), error = "", streaming = false, pending = parked,
            // Staged photos die with the draft like every other thread change: a
            // New with a parked photo must not fire it into the fresh thread.
            pendingAttachments = emptyList(),
            notices = retainedNotices(),
            threads = summaries(), threadId = threadId,
            activeTools = emptyList(), activeToolLabel = "",
            serverTokens = null,
            queued = "",
            activeRunId = "", busyRunId = "",
            showRuns = false, runsLoading = false,
        )
        if (hadStagedNew) {
            stagedDiscardedNotice()
        }
        evictStagedThumbs(stagedNew, stagedNewThread)
        // Skip the write when nothing has been chatted yet — a pure-empty
        // list carries no information and is dropped on next launch anyway.
        if (threads.any { it.messages.isNotEmpty() }) persist()
    }

    /** Renames a conversation (auto-titles stop once renamed). */
    fun renameThread(id: String, title: String) {
        val clean = title.trim().ifEmpty { return }
        threads = threads.map { if (it.id == id) it.copy(title = clean) else it }
        _state.value = _state.value.copy(threads = summaries())
        persist()
    }

    /** Deletes a conversation; if it was active, falls back to the newest
     * thread with chats (never a blank draft). */
    fun deleteThread(id: String) {
        // Voiced on staged content like send()/retry(): an empty composer stays silent.
        if (!_state.value.ready) {
            if (_state.value.pendingAttachments.wiredPhotoCount() > 0 ||
                _state.value.pending.isNotBlank()
            ) {
                notifyLoading()
            }
            return
        }
        // Real publish past the not-ready return, same rule as newConversation.
        touchThreads()
        // Bump the generation like stop(): a buffered Done/Error must go
        // stale, or it lands on the fallback thread and persists there.
        sendGen++
        streamJob?.cancel()
        streamJob = null
        // Stop the server-side run when deleting the ACTIVE thread: same orphan class New closes above.
        val deleteRunToCancel = _state.value.activeRunId
        val deleteCancelPath = _state.value.path
        val deletingActive = threadId == id
        if (deletingActive && deleteRunToCancel.isNotBlank()) {
            viewModelScope.launch(Dispatchers.IO) {
                runCatching { runsApi.cancel(deleteCancelPath, deleteRunToCancel) }
                    .onFailure { runCatching { Log.w("ChatRuns", "cancel $deleteRunToCancel failed", it) } }
            }
        }
        threads = threads.filterNot { it.id == id }
        // The deleted thread's thumbs die with it (#339): every key carries
        // its thread, so this frees them now instead of at LRU pressure.
        removeThreadThumbs(id)
        if (threadId == id) {
            // `id` is already filtered out above: the fallback is the newest remaining thread.
            val next = threads.firstOrNull { it.messages.isNotEmpty() }
                ?: threads.firstOrNull()
                ?: ChatThread(id = ChatThreads.newId(), updatedAt = ChatThreads.now())
            if (threads.none { it.id == next.id }) threads = listOf(next) + threads
            threadId = next.id
            cancelRecovery()
            runsJob?.cancel()
            stopApprovalWatch()
            retargetApprovals()
            retargetMemory()
            // Same restore as newConversation above.
            val d = _state.value
            val kept = when {
                d.queued.isBlank() -> ""
                d.pending.isBlank() -> d.queued
                else -> joinPending(d.pending, d.queued)
            }
            // Same staged-discard voice as switchThread.
            val hadStagedDelete = d.pendingAttachments.wiredPhotoCount() > 0
            _state.value = _state.value.copy(
                messages = ChatThreads.toUi(next.messages),
                threadId = next.id, error = "", streaming = false, pending = kept,
                // Same staged-photos-die-with-the-draft rule as the other thread changes.
                pendingAttachments = emptyList(),
                notices = retainedNotices(),
                serverTokens = null, queued = "",
                activeRunId = "", busyRunId = "",
                showRuns = false, runsLoading = false,
            )
            if (hadStagedDelete) {
                stagedDiscardedNotice()
            }
        }
        _state.value = _state.value.copy(threads = summaries())
        persist()
        refreshServerTotals()
    }

    /** Threads matching [query] in title or message text (switcher search). */
    fun searchThreads(query: String): List<ThreadSummary> {
        // Trimmed, NOT lowercased: one folding rule, region matching, covers all three paths.
        val q = query.trim()
        if (q.isEmpty()) return _state.value.threads
        return threads.filter { t ->
            t.messages.isNotEmpty() && (
                // Same untitled fallback as summaries().
                t.title.ifBlank { UNTITLED }.contains(q, ignoreCase = true) ||
                    // One folding rule (region-matching) shared by title, raw text and marker.
                    t.messages.any { messageMatchesQuery(it.role, it.content, it.attachments, q) }
                )
        }.map {
            ThreadSummary(
                id = it.id,
                title = it.title.ifBlank { UNTITLED },
                updatedAt = it.updatedAt,
                count = it.messages.size,
            )
        }
    }

    /** Saves a thread's transcript as a skill draft for review in Skills.
     * Nothing runs from it: drafts are inert until reviewed, tested and
     * activated. [done] receives an error (null on success) and whether the
     * transcript clipped at the server body cap — a long thread stores
     * less than was sent, and the writer must know. */
    fun saveThreadAsSkill(id: String, name: String, description: String, done: (String?, Boolean) -> Unit) {
        // Check messages, not the export: exportMarkdown always emits at
        // least a "# title" header, so a blank check on the body never fires. Gone vs empty.
        val thread = threads.firstOrNull { it.id == id }
        if (thread == null) {
            done("Conversation is gone.", false)
            return
        }
        if (thread.messages.isEmpty()) {
            done("Conversation is empty.", false)
            return
        }
        // Sent whole: the server clips at its body cap and reports
        // truncated, which the toast surfaces. A client-side cap would drift.
        val body = exportMarkdown(id)
        if (body.isBlank()) {
            done("Conversation is empty.", false)
            return
        }
        val path = _state.value.path
        viewModelScope.launch(Dispatchers.IO) {
            // Rechecked, not captured: a profile switch mid-save must not
            // write the old profile's draft. An error, not a silent success.
            if (path != api.pathFor(tab)) {
                withContext(Dispatchers.Main) { done("Profile changed — try again.", false) }
                return@launch
            }
            // Source links the draft to its thread.
            val result = teachApi.createDraft(path, name.trim(), description.trim(), body, source = "thread:$id")
            withContext(Dispatchers.Main) {
                // Rechecked after the write too: a switch during the network
                // call lands the draft on the old profile.
                if (path != api.pathFor(tab)) {
                    done("Profile changed — try again.", false)
                    return@withContext
                }
                // Always answered across a profile switch: a stuck "Saving…" is worse.
                val detail = result.exceptionOrNull()?.message.orEmpty()
                val truncated = result.getOrNull()?.second == true
                // Server messages pass through bounded like loadTaught: never memory text.
                done(
                    if (result.isSuccess) null
                    else detail.take(120).ifBlank {
                        "Couldn't save draft" +
                            (ApprovalsApi.httpCode(detail)?.let { " (HTTP $it)" } ?: "") + "."
                    },
                    truncated,
                )
            }
        }
    }

    /** Renders a conversation as Markdown for the sharesheet export. */
    fun exportMarkdown(id: String): String {
        val t = threads.firstOrNull { it.id == id } ?: return ""
        val title = t.title.ifBlank { UNTITLED }
        val sb = StringBuilder("# $title\n\n")
        for (m in t.messages) {
            val who = if (m.role == "user") "You" else "Assistant"
            // Photos are payloads, not text: the marker keeps the export honest
            // about image turns. Deliberate split from the wire: the wire keeps
            // trailing newlines exactly (verbatim send guarantee), the export
            // matches what the bubble SHOWED. Counted wired, user-only.
            val wiredCount = if (m.role == "user") m.attachments.wiredPhotoCount() else 0
            val marker = if (wiredCount > 0) {
                contentWithMarkerVerbatim(m.content.trimEnd(), wiredCount)
            } else {
                m.content.trim()
            }
            sb.append("**$who:** $marker\n\n")
            if (m.role != "user" && (m.tools.isNotEmpty() || m.skills.isNotEmpty())) {
                // Backticked so a tool/skill name can never break the export's
                // Markdown.
                val used = listOf(
                    m.tools.takeIf { it.isNotEmpty() }
                        ?.let { "tools: " + it.joinToString(", ") { n -> "`${n.replace("`", "'")}`" } },
                    m.skills.takeIf { it.isNotEmpty() }
                        ?.let { "skills: " + it.joinToString(", ") { n -> "`${n.replace("`", "'")}`" } },
                ).filterNotNull().joinToString(" · ")
                sb.append("_Used $used._\n\n")
            }
        }
        return sb.toString().trim()
    }

    /** Cheap reachability probe driving the offline banner. */
    fun checkReachability() {
        viewModelScope.launch {
            val report = withContext(Dispatchers.IO) {
                runCatching { api.testConnection(api.pathFor(tab)).getOrThrow() }.getOrNull()
            }
            _state.value = _state.value.copy(
                online = report != null && report.gatewayOk && report.syncOk
            )
        }
    }

    /** Records the turn's server-reported token counts (null/absent = the
     * stream carried no usable `usage` object, so nothing is recorded). */
    private fun addTokens(usage: TokenUsage?) {
        if (usage == null) return
        viewModelScope.launch(Dispatchers.IO) {
            runCatching { UsageStore.add(appCtx, tab, usage) }
        }
    }

    fun stop() {
        val wasStreaming = _state.value.streaming
        // Bump the generation first: a buffered Done/Error goes stale.
        sendGen++
        streamJob?.cancel()
        streamJob = null
        // A pending recovery belongs to the turn just stopped: it must not
        // resurrect text behind whatever the user does next.
        cancelRecovery()
        stopApprovalWatch()
        // The badge may still name a gate held at stop time: refresh it.
        refreshApprovalCount()
        // Stop the server-side run this tab started, not just the local socket:
        // closing the stream no longer cancels the work. Only the active run —
        // a 429-parked busy run belongs to another turn. Fire and forget.
        val runToCancel = _state.value.activeRunId
        val cancelPath = _state.value.path
        if (runToCancel.isNotBlank()) {
            viewModelScope.launch(Dispatchers.IO) {
                runCatching { runsApi.cancel(cancelPath, runToCancel) }
                    .onFailure { runCatching { Log.w("ChatRuns", "cancel $runToCancel failed", it) } }
            }
        }
        // Mark the in-flight reply as stopped: the partial text keeps an interrupted flag.
        val msgs = _state.value.messages
        val last = msgs.lastOrNull()
        val patched = if (wasStreaming && last?.role == "assistant") {
            if (last.content.isNotBlank() || last.reasoning.isNotBlank()) {
                msgs.dropLast(1) + last.copy(
                    interrupted = true,
                    interruptedBy = InterruptedBy.USER,
                )
            } else {
                // Stopped before anything arrived: drop the empty placeholder.
                msgs.dropLast(1)
            }
        } else {
            msgs
        }
        val queued = _state.value.queued
        val livePending = _state.value.pending
        _state.value = _state.value.copy(
            messages = patched, streaming = false,
            activeTools = emptyList(), activeToolLabel = "",
            queued = "",
            activeRunId = "",
            // A no-op stop (idle tab after a 429) keeps the parked busy run.
            busyRunId = if (wasStreaming) "" else _state.value.busyRunId,
            // Queued text concatenates when the composer is occupied; trimmed at the JOINT only.
            pending = when {
                queued.isBlank() -> livePending
                livePending.isBlank() -> queued
                else -> joinPending(livePending, queued)
            },
        )
        // Persist the stopped partial so the interrupted flag (and the
        // text/trace so far) survives a thread switch or restart.
        if (patched !== msgs && patched.any {
                it.role == "assistant" &&
                    (it.content.isNotBlank() || it.reasoning.isNotBlank())
            }
        ) {
            upsertActive(patched)
            persist()
        }
    }

    /** Queued text merged into a draft, for every path that republishes the
     * composer: every path that clears `queued` parks it instead — no resend
     * path may lose text the user typed mid-stream. */
    private fun withParkedQueue(draft: String): String {
        val queued = _state.value.queued
        return when {
            queued.isBlank() -> draft
            draft.isBlank() -> queued
            else -> joinPending(draft, queued)
        }
    }

    /** Toasts that staged photos sit out this resend: retry/regenerate/continue
     * resend history, never the composer's staging. Info, not Error. */
    private fun noteParkedPhotos(verb: String) {
        if (_state.value.pendingAttachments.wiredPhotoCount() > 0) {
            notify(
                "$verb without the staged photos — they stay in the " +
                    "composer for your next message.",
                ToastKind.Info,
            )
        }
    }

    /** Resends the current history (used after a failed request).
     *
     * A trailing fragment never goes back out (#214): retry used to resend
     * the interrupted reply as part of history, priming a new turn with its
     * own truncated output while the orphaned turn might still be running.
     * Like regenerate, the resend drops it; the visible thread keeps it
     * until the new turn's Done replaces it (doSend rebuilds from history).
     */
    fun retry() {
        // Gated like send(): the !ready tap voices, the streaming tap stays silent.
        if (!_state.value.ready) {
            // Voiced on text-or-wired-photos like send().
            if (_state.value.pendingAttachments.wiredPhotoCount() > 0 || _state.value.pending.isNotBlank()) {
                notifyLoading()
            }
            return
        }
        if (_state.value.streaming) return
        // messageHasContent, not content.isNotBlank: an image-only turn resends.
        val history = stripTrailingFragmentForResend(_state.value.messages)
            .filter { messageHasContent(it) }
        if (history.none { it.role == "user" }) return
        noteParkedPhotos("Retried")
        // The queue parks back into the composer (#339), like every other
        // resend (regenerate/continue/replace/fork): a retry that cleared
        // it would strand the queued text (or fire it twice in the narrow
        // window where the queue is non-empty and nothing is streaming).
        val retryDraft = withParkedQueue(_state.value.pending)
        _state.value = _state.value.copy(pending = retryDraft, queued = "")
        doSend(history)
    }

    fun send() {
        // Raw stored, trimmed checked: the stored content rides exactly as typed.
        val rawText = _state.value.pending
        val text = rawText.trim()
        // Photos send with or without text: an image-only turn is a real turn.
        val photos = _state.value.pendingAttachments
        // Ready-gated with a voice: the IME send action bypasses the disabled button.
        if (!_state.value.ready) {
            if (text.isNotEmpty() || photos.wiredPhotoCount() > 0) {
                notifyLoading()
            }
            return
        }
        if (text.isEmpty() && photos.wiredPhotoCount() == 0) return
        // Queue-or-send: a send mid-stream parks the text and auto-fires on Done.
        // First queued text wins. Photos stay parked throughout: the queued fire
        // is text only, so staged photos never ride an unconfirmed turn.
        if (_state.value.streaming) {
            if (text.isNotEmpty()) {
                if (_state.value.queued.isBlank()) {
                    // Queued verbatim like the direct send: the turn keeps indentation.
                    _state.value = _state.value.copy(pending = "", queued = rawText)
                    // Same narrowing note: the queued fire is text-only, so staged photos park.
                    val parkedQueuedCount = pendingAttachmentCount()
                    if (parkedQueuedCount > 0) {
                        notify(
                            "Queued behind this reply — text sends, " +
                                attachmentMarker(parkedQueuedCount) +
                                " stays staged for the next message.",
                            ToastKind.Info,
                        )
                    }
                } else {
                    // Second tap while a queue is parked: the new text stays in
                    // the composer, but with no toast the tap reads dead.
                    val parkedSecondCount = pendingAttachmentCount()
                    notify(
                        "Already queued — sends when this reply finishes." +
                            if (parkedSecondCount > 0) {
                                " " + attachmentMarker(parkedSecondCount) + " stays staged."
                            } else {
                                ""
                            },
                        ToastKind.Info,
                    )
                }
            } else if (pendingAttachmentCount() > 0) {
                // Image-only tap while streaming: nothing queues, so say the photos wait.
                notify(
                    "Still answering — photos stay staged and " +
                        "send with your next message.",
                    ToastKind.Info,
                )
            }
            return
        }
        // Re-read per-tab config at send time so Settings edits apply instantly.
        val provider = api.providerFor(tab)
        val model = api.modelFor(tab)
        val effort = api.effortFor(tab, model)
        val path = api.pathFor(tab)
        // Queued text survives this send: it was typed for the NEXT turn, so park it back.
        val draftAfterSend = withParkedQueue("")
        _state.value = _state.value.copy(provider = provider, model = model, effort = effort, path = path, queued = "")
        // Stored canonical AND wired: whitespace-only text with photos normalizes to "".
        val stored = if (text.isEmpty()) "" else rawText
        val history = _state.value.messages +
            ChatMessage("user", stored, ChatThreads.now(), attachments = photos.wiredPhotos())
        _state.value = _state.value.copy(
            messages = history, pending = draftAfterSend, pendingAttachments = emptyList(),
            streaming = true, error = "",
        )
        doSend(history, path, provider, model, effort)
    }

    /**
     * Appends the composer text to the running turn as a steering instruction
     * (#311) instead of queueing it behind the turn. Falls back to [send]
     * when nothing is running (which queues while streaming, sends when
     * idle). A settled race (409) also falls back to [send]: the text becomes
     * the next turn rather than vanishing.
     */
    fun steer() {
        val text = _state.value.pending.trim()
        // Raw snapshot for the settle-time strip: it must match the composer.
        val sentRaw = _state.value.pending
        // Ready-gated with a voice like send(): !ready is reachable; ungated dead-taps.
        if (text.isEmpty() || !_state.value.ready) {
            if (!_state.value.ready &&
                (text.isNotEmpty() || _state.value.pendingAttachments.wiredPhotoCount() > 0)
            ) {
                notifyLoading()
            } else if (text.isEmpty() && _state.value.pendingAttachments.wiredPhotoCount() > 0) {
                // Photos alone cannot be steered (#339): the steer turn is
                // text-only, so say that instead of swallowing the tap.
                notify("Steering needs text — photos ride the next message.", ToastKind.Info)
            }
            return
        }
        if (!_state.value.streaming || _state.value.activeRunId.isBlank()) {
            send()
            return
        }
        // Steering carries text only: staged photos park with no part in the
        // steered turn — said out loud, or the steer button drops the staging.
        // Voiced only on the Queued outcome (the Conflict branch calls send()).
        val runId = _state.value.activeRunId
        val path = _state.value.path
        viewModelScope.launch(Dispatchers.IO) {
            val result = runsApi.steer(path, runId, text)
            val outcome = result.getOrNull()
            withContext(Dispatchers.Main) {
                // Stale steer (a stop landed meanwhile): never touch state —
                // the composer text belongs to whatever the user does next.
                if (!_state.value.streaming || _state.value.activeRunId != runId) return@withContext
                when (outcome) {
                    // Strip only what this steer sent: typing that landed
                    // mid-round-trip belongs to the user. Prefix on the RAW
                    // snapshot, not the trimmed instruction. One joint space
                    // dropped, the remainder verbatim.
                    is SteerOutcome.Queued -> {
                        val curPending = _state.value.pending
                        val sentBase = sentRaw.trimEnd()
                        _state.value = _state.value.copy(
                            pending = if (sentBase.isNotEmpty() && curPending.startsWith(sentBase)) {
                                curPending.removePrefix(sentBase).removePrefix(" ")
                            } else {
                                curPending
                            },
                        )
                        // Parked-photos voice lives here, not pre-launch: the
                        // Conflict branch calls send(), which voices its own notice.
                        val liveParked = _state.value.pendingAttachments.wiredPhotoCount()
                        if (liveParked > 0) {
                            notify(
                                "Steered the text — " +
                                    attachmentMarker(liveParked) +
                                    " stays staged for your next message.",
                                ToastKind.Info,
                            )
                        }
                    }
                    // Settled race: the text becomes the next turn. When the queue
                    // was already occupied send() parks nothing, so say the queue is full.
                    is SteerOutcome.Conflict -> {
                        val queuedWasOccupied = _state.value.queued.isNotBlank()
                        send()
                        if (queuedWasOccupied && _state.value.streaming) {
                            _state.value = _state.value.copy(
                                error = "Turn ended — queue is full, text kept in composer.",
                            )
                        }
                    }
                    null -> _state.value = _state.value.copy(
                        error = "Steer failed" +
                            (result.exceptionOrNull()?.message?.take(120)?.let { ": $it" } ?: "") +
                            " — the text stays in the composer.",
                    )
                }
            }
        }
    }

    /** Opens the runs sheet for the active thread and loads its runs (#311). */
    fun openRuns() {
        if (!_state.value.ready || threadId.isBlank()) return
        // Drop the previous thread's rows up front: the first paint is the
        // spinner, never stale rows under a new thread id.
        _state.value = _state.value.copy(showRuns = true, runsError = "", runs = emptyList())
        refreshRuns()
    }

    fun closeRuns() {
        _state.value = _state.value.copy(showRuns = false)
    }

    /** Reloads the runs sheet rows: newest first, in-flight and finished. */
    fun refreshRuns() {
        if (!_state.value.ready || threadId.isBlank()) {
            _state.value = _state.value.copy(runsLoading = false)
            return
        }
        val path = _state.value.path
        val conversationId = threadId
        _state.value = _state.value.copy(runsLoading = true, runsError = "")
        runsJob?.cancel()
        runsJob = viewModelScope.launch(Dispatchers.IO) {
            val result = runsApi.list(path, conversationId = conversationId, limit = 50)
            withContext(Dispatchers.Main) {
                // Stale load (thread switched mid-flight): never write
                // another thread's rows or errors here.
                if (conversationId != threadId) return@withContext
                // A failed load keeps the old rows (never blanked into a
                // lying "no runs yet") and reports inside the sheet: the
                // chat error banner's Retry would resend a turn, which a
                // list outage must never offer.
                result.onFailure {
                    _state.value = _state.value.copy(
                        runsLoading = false,
                        runsError = "Couldn't load runs (${it.message?.take(120) ?: "unknown"}).",
                    )
                }.onSuccess { rows ->
                    _state.value = _state.value.copy(
                        runs = rows, runsLoading = false, runsError = "",
                    )
                }
            }
        }
    }

    /**
     * Cancels a run from the sheet, then reloads. When it is the active
     * turn, its stream ends through the normal cancelled path (explicit
     * status frame + Done), so no local patching happens here. A failed
     * cancel surfaces instead of silently refreshing.
     */
    fun cancelRun(id: String) {
        if (id.isBlank()) return
        val path = _state.value.path
        viewModelScope.launch(Dispatchers.IO) {
            val result = runsApi.cancel(path, id)
            withContext(Dispatchers.Main) {
                // Failed cancel reports inside the sheet and skips the
                // reload, so the refresh error cannot overwrite it.
                result.onFailure {
                    _state.value = _state.value.copy(
                        runsError = "Cancel failed (${it.message?.take(120) ?: "unknown"}) — try again.",
                    )
                }.onSuccess {
                    if (_state.value.showRuns) refreshRuns()
                }
            }
        }
    }

    /** Opens the approvals sheet and loads pending cards + audit (#313). */
    fun openApprovals() {
        if (!_state.value.ready) return
        // Drop stale rows up front: the first paint is the spinner, never
        // the previous session's cards.
        _state.value = _state.value.copy(
            showApprovals = true, approvalsError = "",
            approvals = emptyList(), auditRows = emptyList(),
        )
        refreshApprovals()
    }

    fun closeApprovals() {
        // A load that lands after close only wastes a call and its rows are
        // cleared on the next open; cancel it anyway.
        approvalsJob?.cancel()
        approvalsJob = null
        _state.value = _state.value.copy(showApprovals = false)
    }

    /** Single-flight approvals load: a newer open or manual refresh cancels
     * the older one, so stale rows can never land. */
    private var approvalsJob: Job? = null

    /** Reloads pending approvals and the recent audit trail. Failures keep
     * the old rows and report inside the sheet (never the chat banner). A
     * [notice] survives a successful load (e.g. "already decided"). */
    fun refreshApprovals(notice: String? = null) {
        val path = _state.value.path
        _state.value = _state.value.copy(approvalsLoading = true)
        approvalsJob?.cancel()
        approvalsJob = viewModelScope.launch(Dispatchers.IO) {
            // Same limit as pollApprovalCount: a smaller page here would
            // shrink the badge the moment the sheet opens.
            val pending = approvalsApi.listApprovals(path, status = "pending", limit = 100)
            val audit = approvalsApi.listAudit(path, limit = 50)
            withContext(Dispatchers.Main) {
                // Per-half: a half that loaded replaces its rows; any half
                // that failed reports without wiping the good half. Errors
                // name the HTTP code, never the body. A [notice] (e.g.
                // "already decided") must survive that failure too, so the
                // two are joined rather than one replacing the other.
                val failedCode = ApprovalsApi.httpCode(pending.exceptionOrNull()?.message)
                    ?: ApprovalsApi.httpCode(audit.exceptionOrNull()?.message)
                val loadErr = if (pending.isFailure || audit.isFailure) {
                    "Couldn't load approvals" +
                        (failedCode?.let { " (HTTP $it)" } ?: "") + "."
                } else {
                    ""
                }
                _state.value = _state.value.copy(
                    approvals = pending.getOrNull()?.approvals ?: _state.value.approvals,
                    auditRows = audit.getOrNull() ?: _state.value.auditRows,
                    approvalsLoading = false,
                    approvalsError = listOf(notice.orEmpty(), loadErr)
                        .filter { it.isNotBlank() }
                        .joinToString(" "),
                    pendingApprovalCount = pending.getOrNull()?.approvals?.size
                        ?: _state.value.pendingApprovalCount,
                    // Sticky until a non-degraded load clears it: flapping
                    // the banner on every blip would hide real pendings
                    // behind a warning that comes and goes.
                    approvalsDegraded = pending.getOrNull()?.degraded
                        ?: _state.value.approvalsDegraded,
                )
            }
        }
    }

    /** Decides a held call: approve runs it, deny fails it back to the turn
     * as a tool error. Double-taps collapse into one call; a 409 (someone
     * else decided) reloads with a transient note instead of a retry prompt. */
    private val deciding = mutableSetOf<String>()

    fun decideApproval(id: String, approve: Boolean) {
        // Both the add here and the remove below run on Main (UI taps and
        // the Main-confined completion), so a plain set is safe; moving
        // either side off Main needs a lock.
        if (id.isBlank() || !deciding.add(id)) return
        _state.value = _state.value.copy(decidingApprovals = deciding.toSet())
        val path = _state.value.path
        viewModelScope.launch(Dispatchers.IO) {
            val result = approvalsApi.decide(path, id, approve)
            withContext(Dispatchers.Main) {
                deciding.remove(id)
                _state.value = _state.value.copy(decidingApprovals = deciding.toSet())
                // A 409 means someone else decided first: reload (the row
                // leaves pending) with a transient note, never a retry
                // prompt — retrying a decided id 409s again. Errors name
                // the HTTP code, never the body.
                val code = ApprovalsApi.httpCode(result.exceptionOrNull()?.message)
                if (result.isFailure && code != "409") {
                    _state.value = _state.value.copy(
                        approvalsError = "Decision failed" +
                            (code?.let { " (HTTP $it)" } ?: "") + " — try again.",
                    )
                } else if (_state.value.showApprovals) {
                    refreshApprovals(
                        if (result.isFailure) "Already decided — refreshed." else null,
                    )
                }
                refreshApprovalCount()
            }
        }
    }

    /** Light pending count for the badge/banner, without touching rows. */
    private fun refreshApprovalCount() {
        val path = _state.value.path
        viewModelScope.launch(Dispatchers.IO) {
            pollApprovalCount(path)
        }
    }

    /** Watch cadence while streaming: immediate poll, then every 5s. A gate
     * held at turn start otherwise reads as thinking for a full interval. */
    private fun startApprovalWatch(path: String) {
        approvalWatch?.cancel()
        approvalWatch = viewModelScope.launch(Dispatchers.IO) {
            pollApprovalCount(path)
            while (true) {
                delay(5_000)
                pollApprovalCount(path)
            }
        }
    }

    private suspend fun pollApprovalCount(path: String) {
        // Failures keep the old count: a network blip must not zero the
        // badge into "no pending".
        val page = approvalsApi.listApprovals(path, status = "pending", limit = 100)
            .getOrNull() ?: return
        val n = page.approvals.size
        withContext(Dispatchers.Main) {
            // Stale poll (profile switched mid-flight): the old profile's
            // count must not land on the new one.
            if (_state.value.path != path) return@withContext
            // Assigned, not OR-accumulated: recovery clears via poll, and
            // only a full sheet load sets it otherwise. A transient degraded
            // must not stick past recovery behind a stale banner.
            _state.value = _state.value.copy(
                pendingApprovalCount = n,
                approvalsDegraded = page.degraded,
            )
        }
    }

    private fun stopApprovalWatch() {
        approvalWatch?.cancel()
        approvalWatch = null
    }

    /** A thread/profile change retires any approvals load started under the
     * old path. Approvals are global, so reopen the sheet against the new
     * path rather than leaving a cancelled load's spinner behind. */
    private fun retargetApprovals() {
        approvalsJob?.cancel()
        approvalsJob = null
        if (_state.value.showApprovals) refreshApprovals()
    }

    /** Opens the memory sheet and loads items + pauses + topics (#315). */
    fun openMemory() {
        if (!_state.value.ready) return
        // Drop stale rows up front: the first paint is the spinner, never
        // the previous session's items.
        _state.value = _state.value.copy(
            showMemory = true, memoryError = "", memoryWarnings = emptyList(),
            memoryItems = emptyList(), memoryPaused = emptyList(),
            memoryTopics = emptyList(),
        )
        refreshMemory()
    }

    fun closeMemory() {
        // A load that lands after close only wastes a call and its rows are
        // cleared on the next open; cancel it anyway and drop the spinner so
        // a cancelled load cannot leave a stale true behind.
        memoryJob?.cancel()
        memoryJob = null
        _state.value = _state.value.copy(showMemory = false, memoryLoading = false)
    }

    /** Single-flight memory load: a newer open or manual refresh cancels
     * the older one, so stale rows can never land. */
    private var memoryJob: Job? = null

    /** Reloads the memory sheet. Failures keep the old rows and report
     * inside the sheet (never the chat banner). A [notice] survives a
     * successful load (e.g. "already confirmed"); [stickyNotice] also
     * survives a failed one, for information that holds regardless of the
     * repaint (a clipped save clipped even if the rows did not reload). */
    fun refreshMemory(notice: String? = null, stickyNotice: Boolean = false) {
        val path = _state.value.path
        _state.value = _state.value.copy(memoryLoading = true)
        memoryJob?.cancel()
        memoryJob = viewModelScope.launch(Dispatchers.IO) {
            // One call carries items + pauses + topics: the screen's three
            // views of the same rows, so there is no half to reconcile.
            val list = memoryApi.listMemory(path)
            withContext(Dispatchers.Main) {
                // Stale load (path changed mid-flight): old-profile rows
                // must not land on the new profile. Loading clears first so
                // the guard never leaves a stuck spinner behind.
                _state.value = _state.value.copy(memoryLoading = false)
                if (_state.value.path != path) return@withContext
                val failedCode = ApprovalsApi.httpCode(list.exceptionOrNull()?.message)
                val loadErr = if (list.isFailure) {
                    "Couldn't load memory" +
                        (failedCode?.let { " (HTTP $it)" } ?: "") + "."
                } else {
                    ""
                }
                val rows = list.getOrNull()
                _state.value = _state.value.copy(
                    memoryItems = rows?.items ?: _state.value.memoryItems,
                    memoryPaused = rows?.paused ?: _state.value.memoryPaused,
                    memoryTopics = rows?.topics ?: _state.value.memoryTopics,
                    memoryWarnings = rows?.warnings ?: _state.value.memoryWarnings,
                    memoryError = if (list.isFailure && !stickyNotice) {
                        // The write succeeded; only the reload failed. The
                        // notice must not read as a failed write — unless it
                        // is sticky information about the write itself.
                        loadErr
                    } else {
                        listOf(notice.orEmpty(), loadErr)
                            .filter { it.isNotBlank() }
                            .joinToString(" ")
                    },
                    pendingMemoryCount = rows?.items?.count { it.isPending() }
                        ?: _state.value.pendingMemoryCount,
                )
            }
        }
    }

    /** Runs one memory write with its button disabled, then reloads the
     * sheet when it is still open. Errors name the HTTP code, never the
     * body; failures keep the old rows. */
    private val acting = mutableSetOf<String>()

    private fun actOnMemory(
        /** Namespaced by the caller: "item:\u2026" for rows, "topic:\u2026" for
         * pauses. Namespacing by kind, not by sniffing the id string, so an
         * item literally named "topic:x" still keys as an item and can never
         * collide with a pause key or bare "new"/"forget". */
        key: String,
        id: String,
        action: suspend (String) -> Result<*>,
        /** Runs on Main after a success: collapses editors, clears composers. */
        onSuccess: () -> Unit = {},
        /** Notice carried into the completion's own refresh: passing it here
         * instead of refreshing inside onSuccess avoids a double load whose
         * second, noticeless call cancels the first (single-flight). */
        notice: String? = null,
        /** Whether the notice also survives a failed reload. */
        stickyNotice: Boolean = false,
    ) {
        // Both the add here and the remove below run on Main (UI taps and
        // the Main-confined completion), so a plain set is safe; moving
        // either side off Main needs a lock.
        if (id.isBlank() || !acting.add(key)) return
        _state.value = _state.value.copy(actingMemory = acting.toSet())
        val path = _state.value.path
        viewModelScope.launch(Dispatchers.IO) {
            val result = action(path)
            withContext(Dispatchers.Main) {
                acting.remove(key)
                _state.value = _state.value.copy(actingMemory = acting.toSet())
                // A switch mid-write retires the whole completion: the
                // write belonged to the old profile, so neither its success
                // (repaint/badge) nor its failure (sheet error) may land on
                // the new one.
                if (_state.value.path != path) return@withContext
                val code = ApprovalsApi.httpCode(result.exceptionOrNull()?.message)
                if (result.isFailure) {
                    _state.value = _state.value.copy(
                        memoryError = "Memory update failed" +
                            (code?.let { " (HTTP $it)" } ?: "") + " — try again.",
                    )
                } else {
                    // The badge refreshes even with the sheet closed: a
                    // write that finished after close still changed the
                    // pending count.
                    onSuccess()
                    pollMemoryCount(path)
                    if (_state.value.showMemory) refreshMemory(notice, stickyNotice)
                }
            }
        }
    }

    /** Confirms a pending proposal: it joins the next turn's prompt. */
    fun confirmMemory(id: String) =
        actOnMemory("item:$id", id, action = { path -> memoryApi.updateMemory(path, id, status = "confirmed") })

    /** Deletes one item: it is gone from the next turn. */
    fun deleteMemory(id: String) {
        // Sticky like the truncation notice: the row is gone even if the
        // reload fails, and silence would read as a failed delete.
        actOnMemory("item:$id", id, action = { path -> memoryApi.deleteMemory(path, id) },
            notice = "Deleted.", stickyNotice = true)
    }

    /** Saves an edit to text and/or topic. Trims like create: a
     * whitespace-only edit is a validation error here, not a server 400,
     * and an emptied topic keeps the old one instead of re-categorising to
     * general by silence. The editor collapses via [onSaved]: on failure
     * the edit context stays open alongside the error. */
    fun editMemory(id: String, text: String, topic: String, onSaved: () -> Unit = {}) {
        val clean = text.trim()
        if (clean.isBlank()) {
            _state.value = _state.value.copy(memoryError = "Text is required.")
            return
        }
        val keepTopic = topic.trim()
        actOnMemory(
            key = "item:$id",
            id = id,
            action = { path ->
                memoryApi.updateMemory(
                    path, id, text = clean,
                    topic = keepTopic.ifBlank { null },
                )
            },
            onSuccess = onSaved,
        )
    }

    /** Pauses or resumes one topic. Paused items stay on screen; they are
     * simply not injected into turns. A blank topic has no route (PUT
     * …/topics/ is a 404), so it never fires. */
    fun toggleMemoryPause(topic: String, paused: Boolean) {
        if (topic.isBlank()) return
        actOnMemory("topic:$topic", topic, action = { path -> memoryApi.setTopicPaused(path, topic, paused) })
    }

    /** Writes a user item: confirmed on arrival, no review loop for the
     * person whose memory it is. A clipped save reports it in-sheet. The
     * composer clears via [onSaved]: on failure the typed text stays so the
     * error does not eat it. */
    fun createMemory(topic: String, text: String, onSaved: () -> Unit = {}) {
        if (text.isBlank()) {
            _state.value = _state.value.copy(memoryError = "Text is required.")
            return
        }
        val key = "new"
        if (!acting.add(key)) return
        _state.value = _state.value.copy(actingMemory = acting.toSet())
        val path = _state.value.path
        viewModelScope.launch(Dispatchers.IO) {
            val result = memoryApi.createMemory(path, topic.trim().ifBlank { "general" }, text.trim())
            withContext(Dispatchers.Main) {
                acting.remove(key)
                _state.value = _state.value.copy(actingMemory = acting.toSet())
                // Same stale-path rule as actOnMemory: a switch mid-save
                // must not clear the new profile's composer or repaint it.
                if (_state.value.path != path) return@withContext
                val code = ApprovalsApi.httpCode(result.exceptionOrNull()?.message)
                if (result.isFailure) {
                    _state.value = _state.value.copy(
                        memoryError = "Couldn't save memory" +
                            (code?.let { " (HTTP $it)" } ?: "") + " — try again.",
                    )
                } else {
                    val truncated = result.getOrNull()?.second == true
                    onSaved()
                    pollMemoryCount(path)
                    if (_state.value.showMemory) {
                        refreshMemory(
                            if (truncated) "Saved, but the text clipped at the length cap." else null,
                            stickyNotice = truncated,
                        )
                    }
                }
            }
        }
    }

    /** Forgets every item for the profile. Pauses survive (policy, not
     * memory). Two-tap in the sheet; this is the second tap. */
    fun forgetMemory() {
        val key = "forget"
        if (!acting.add(key)) return
        _state.value = _state.value.copy(actingMemory = acting.toSet())
        val path = _state.value.path
        viewModelScope.launch(Dispatchers.IO) {
            val result = memoryApi.forgetMemory(path)
            withContext(Dispatchers.Main) {
                acting.remove(key)
                _state.value = _state.value.copy(actingMemory = acting.toSet())
                if (_state.value.path != path) return@withContext
                val code = ApprovalsApi.httpCode(result.exceptionOrNull()?.message)
                if (result.isFailure) {
                    _state.value = _state.value.copy(
                        memoryError = "Couldn't forget memory" +
                            (code?.let { " (HTTP $it)" } ?: "") + " — try again.",
                    )
                } else {
                    pollMemoryCount(path)
                    if (_state.value.showMemory) {
                        refreshMemory("Forgot ${result.getOrNull() ?: 0} item(s). Pauses kept.")
                    }
                }
            }
        }
    }

    /** A thread/profile change retires any memory load started under the
     * old path, reopening the sheet against the new one. The badge count
     * refreshes regardless: pending proposals must badge on launch and on
     * switch, not only after the first open. */
    private fun retargetMemory() {
        memoryJob?.cancel()
        memoryJob = null
        if (_state.value.showMemory) refreshMemory()
        pollMemoryCount(_state.value.path)
    }

    /** Light pending-proposal count for the badge, without touching rows.
     * Mirrors pollApprovalCount: failures keep the old count, and a stale
     * poll never lands on a new path. */
    private fun pollMemoryCount(path: String) {
        viewModelScope.launch(Dispatchers.IO) {
            // The sheet's own query, not a pending-only page: with 200+
            // mixed-status rows a filtered window could count pendings the
            // sheet's window excludes (or vice versa). Same rows, same count.
            val n = memoryApi.listMemory(path, limit = 200)
                .getOrNull()?.items?.count { it.isPending() } ?: return@launch
            withContext(Dispatchers.Main) {
                if (_state.value.path != path) return@withContext
                _state.value = _state.value.copy(pendingMemoryCount = n)
            }
        }
    }

    /**
     * Reattaches an app-killed turn (#311): a trailing user message plus a
     * live server run with the same prompt means resending streams the
     * existing run instead of starting a duplicate (the server reattaches
     * same conversation + same prompt). Fires once per thread, only for an
     * unanswered user tail — anything else (assistant tail, failure, stop)
     * stays for the user to drive.
     *
     * Safe beside openThread autosend: autosend sets streaming synchronously
     * before any resume lookup can return, and the Main guard below returns
     * on streaming — so the same tail can never fire as two turns even if
     * both paths trigger.
     */
    private fun maybeResume(threadId: String, messages: List<ChatMessage>) {
        val last = messages.lastOrNull() ?: return
        // Image-only tails count: their text is blank but the photos resend.
        if (last.role != "user" || !messageHasContent(last)) return
        if (threadId in resumedThreads) return
        viewModelScope.launch(Dispatchers.IO) {
            val path = api.pathFor(tab)
            // Query unfiltered by status: a future `cancelling`/`queued`
            // state would be missed by a `status=running` query, and the
            // client-side !isTerminal check below is the real gate. The
            // wider window is bounded (20 rows of one conversation).
            val result = runsApi.list(path, conversationId = threadId, limit = 20)
            result.onFailure {
                runCatching { Log.w("ChatRuns", "resume list failed", it) }
            }
            val runs = result.getOrNull().orEmpty()
            // Belt and braces on the status: the query already filters, but a
            // stale row must never trigger a resend on its own. Anything not
            // terminal is live (running, cancelling, or a future state) —
            // narrowing to `running` would strand exactly the edge cases
            // resume exists for.
            // Image-only tails match on blank prompt: Prompt is the raw text.
            // PHOTO-BLIND MATCH, by necessity (#339): the server records the
            // prompt text only, so a text-identical tail can attach to a
            // different live run. The resend still carries the user's own
            // tail (photos included) and the newest live run wins.
            val imageOnlyTail = last.content.isBlank() &&
                last.attachments.wiredPhotoCount() > 0
            // Newest match wins: two image-only tails can both be live.
            val run = runs.filter {
                !it.isTerminal() && it.conversationId == threadId &&
                    (promptMatches(last.content, it.prompt) ||
                        (imageOnlyTail && it.prompt.isBlank()))
            }.maxByOrNull { it.updatedAt } ?: return@launch
            // Bounded: re-clearing only loses the once-guard for threads long
            // dead, whose resend finds no live run and no-ops.
            withContext(Dispatchers.Main) {
                if (threadId != this@ChatViewModel.threadId || _state.value.streaming || !_state.value.ready) {
                    return@withContext
                }
                val cur = _state.value.messages
                if (cur.lastOrNull()?.role != "user") return@withContext
                // Prompt-identity re-check under Main: re-verify cur.last after the list() window.
                val curLast = cur.lastOrNull() ?: return@withContext
                if (!messageHasContent(curLast)) return@withContext
                val curImageOnly = curLast.content.isBlank() &&
                    curLast.attachments.wiredPhotoCount() > 0
                if (!(promptMatches(curLast.content, run.prompt) ||
                        (curImageOnly && run.prompt.isBlank()))
                ) {
                    return@withContext
                }
                // Re-check under Main: two concurrent opens would otherwise
                // both pass the entry check and resend twice.
                if (threadId in resumedThreads) return@withContext
                // Marked only when the resend actually fires: an earlier mark
                // would strand the thread if the guards above skipped it
                // (switch-away-and-back would never retry).
                if (resumedThreads.size >= 200) resumedThreads.clear()
                resumedThreads.add(threadId)
                // Deliberately fresh provider/model/effort, matched on prompt
                // only: the server reattach ignores model switches by design
                // (same conversation + same prompt reattaches), so this
                // streams the running model's run — no duplicate turn.
                val provider = api.providerFor(tab)
                val model = api.modelFor(tab)
                val effort = api.effortFor(tab, model)
                _state.value = _state.value.copy(
                    provider = provider, model = model, effort = effort, path = path,
                )
                // Photos sit out a resume like every other resend.
                noteParkedPhotos("Resumed")
                doSend(cur, path, provider, model, effort)
            }
        }
    }

    /**
     * The stored prompt is a bounded preview: exact when short, a truncated
     * prefix when marked. Compared trim-tolerant — the server trimmed its
     * copy too, and this check is best-effort anyway (the server reattach
     * compares the full text and 429s on mismatch).
     */
    private fun promptMatches(full: String, preview: String): Boolean {
        if (preview.isEmpty()) return false
        val marker = "… (truncated)"
        val text = full.trim()
        if (!preview.endsWith(marker)) return text == preview.trim()
        // A literal trailing marker in a short prompt must not read as a
        // truncation prefix: the full text has to actually exceed the core.
        val core = preview.dropLast(marker.length)
        return text.length > core.length && text.startsWith(core)
    }

    /** Regenerates the last reply (#122, v1 replace semantics): drops the
     * trailing assistant message and resends the remaining history. */
    fun regenerate() {
        // Same gate split as retry(): voice !ready, silent on streaming.
        if (!_state.value.ready) {
            // Voiced on text-or-wired-photos like send().
            if (_state.value.pendingAttachments.wiredPhotoCount() > 0 || _state.value.pending.isNotBlank()) {
                notifyLoading()
            }
            return
        }
        if (_state.value.streaming) return
        // Same image-only rule as retry().
        val msgs = _state.value.messages.filter { messageHasContent(it) }
        if (msgs.none { it.role == "user" }) return
        val trimmed = if (msgs.lastOrNull()?.role == "assistant") msgs.dropLast(1) else msgs
        if (trimmed.none { it.role == "user" }) return
        noteParkedPhotos("Regenerated")
        val provider = api.providerFor(tab)
        val model = api.modelFor(tab)
        val effort = api.effortFor(tab, model)
        val path = api.pathFor(tab)
        // Queued text parks back with the draft: same rule as send().
        val draftParked = withParkedQueue(_state.value.pending)
        _state.value = _state.value.copy(
            provider = provider, model = model, effort = effort, path = path,
            pending = draftParked, queued = "",
        )
        doSend(trimmed, path, provider, model, effort)
    }

    /** Continues a stopped reply (#122): appends an honest "Continue" turn
     * and sends (the stateless loop resends full history, so continuation
     * is a fresh turn — never presented as server-side resumption). */
    fun continueTurn() {
        // Same gate split as retry().
        if (!_state.value.ready) {
            // Voiced on text-or-wired-photos like send().
            if (_state.value.pendingAttachments.wiredPhotoCount() > 0 || _state.value.pending.isNotBlank()) {
                notifyLoading()
            }
            return
        }
        if (_state.value.streaming) return
        val msgs = _state.value.messages
        val last = msgs.lastOrNull()
        if (last?.role != "assistant" || !last.interrupted) return
        noteParkedPhotos("Continued")
        val provider = api.providerFor(tab)
        val model = api.modelFor(tab)
        val effort = api.effortFor(tab, model)
        val path = api.pathFor(tab)
        // Queued text parks back with the draft, same rule as send().
        val draftParked = withParkedQueue(_state.value.pending)
        _state.value = _state.value.copy(
            provider = provider, model = model, effort = effort, path = path,
            pending = draftParked, queued = "",
        )
        val history = msgs + ChatMessage(
            "user",
            "Continue",
            ChatThreads.now(),
            // Carry the interrupted turn's photos: the "Continue" turn keeps
            // image parts. The LAST photo-carrying user turn, not just the
            // last user turn (#339): the wire keeps image parts on the
            // trailing user turn only, so a last text-only user turn would
            // leave an earlier photo riding as a text marker — the model's
            // photo context lost exactly when it is needed.
            attachments = msgs.lastOrNull {
                it.role == "user" && it.attachments.wiredPhotoCount() > 0
            }?.attachments?.wiredPhotos().orEmpty(),
        )
        // Typed draft survives like retry()/regenerate(): never vaporize a typed draft.
        _state.value = _state.value.copy(messages = history, streaming = true, error = "")
        doSend(history, path, provider, model, effort)
    }

    /** Edits a user message and resends from there (#122 Replace): history
     * after [index] is dropped. Callers confirm first when turns are lost. */
    fun replaceAndResend(index: Int, text: String) {
        // !ready voiced like send()/retry(): an empty composer voices nothing.
        if (!_state.value.ready) {
            if (_state.value.pendingAttachments.wiredPhotoCount() > 0 ||
                _state.value.pending.isNotBlank()
            ) {
                notifyLoading()
            }
            return
        }
        if (_state.value.streaming) return
        val clean = text.trim()
        val msgs = _state.value.messages
        if (index !in msgs.indices || msgs[index].role != "user") return
        // Empty text is allowed only onto photos: that yields the image-only shape send() supports.
        if (clean.isEmpty() && msgs[index].attachments.wiredPhotoCount() == 0) return
        val provider = api.providerFor(tab)
        val model = api.modelFor(tab)
        val effort = api.effortFor(tab, model)
        val path = api.pathFor(tab)
        // Draft AND queue park back, read before the queue is cleared below.
        val draft = withParkedQueue(_state.value.pending)
        _state.value = _state.value.copy(
            provider = provider, model = model, effort = effort, path = path, queued = "",
        )
        // The edited turn keeps its photos: resending the new text without them
        // would silently narrow the turn the user composed. Carry-only by
        // design (the HintLine + confirm clause say the photos resend).
        // Staged composer photos are deliberately NOT cleared here (unlike
        // send(), whose staged photos WERE sent) — wiping the composer's
        // unrelated staging would destroy user input. The typed draft parks back.
        val history = msgs.take(index) +
            ChatMessage(
                "user",
                if (clean.isEmpty()) "" else text,
                ChatThreads.now(),
                attachments = msgs[index].attachments.wiredPhotos(),
            )
        // Staged composer photos sit out like every other resend: voice it via
        // noteParkedPhotos. Replace/Fork carry the EDITED turn's photos only.
        noteParkedPhotos("Replaced")
        _state.value = _state.value.copy(messages = history, pending = draft, streaming = true, error = "")
        doSend(history, path, provider, model, effort)
    }

    /** Forks a thread at an edited user message (#122): the copy (up to and
     * including the replacement) becomes a new conversation and sends. */
    fun forkAndResend(index: Int, text: String) {
        // Same gated voice as Replace above.
        if (!_state.value.ready) {
            if (_state.value.pendingAttachments.wiredPhotoCount() > 0 ||
                _state.value.pending.isNotBlank()
            ) {
                notifyLoading()
            }
            return
        }
        if (_state.value.streaming) return
        val clean = text.trim()
        val msgs = _state.value.messages
        if (index !in msgs.indices || msgs[index].role != "user") return
        // Same empty-text rule as Replace above, counted wired.
        if (clean.isEmpty() && msgs[index].attachments.wiredPhotoCount() == 0) return
        val provider = api.providerFor(tab)
        val model = api.modelFor(tab)
        val effort = api.effortFor(tab, model)
        val path = api.pathFor(tab)
        // Same photo-carry rule as Replace above: the forked turn is the edited
        // turn, photos included. Composer staging does NOT survive here — forking
        // switches to a FRESH thread, and every thread change discards staging.
        val forked = msgs.take(index) +
            ChatMessage(
                "user",
                if (clean.isEmpty()) "" else text,
                ChatThreads.now(),
                attachments = msgs[index].attachments.wiredPhotos(),
            )
        val fresh = ChatThread(
            id = ChatThreads.newId(),
            updatedAt = ChatThreads.now(),
            messages = ChatThreads.toStored(forked),
        )
        // Real publish past every no-op return above: the fork outranks init's load.
        touchThreads()
        threads = listOf(fresh) + threads
        // Staged thumbs are keyed by the thread they were staged in (#339).
        val stagedFork = _state.value.pendingAttachments
        val stagedForkThread = threadId
        threadId = fresh.id
        cancelRecovery()
        runsJob?.cancel()
        stopApprovalWatch()
        retargetApprovals()
        retargetMemory()
        // Typed draft (and any queued text) parks back like Replace above.
        // Staged photos do NOT park — they die with the thread being left.
        val forkDraft = withParkedQueue(_state.value.pending)
        val hadStagedFork = _state.value.pendingAttachments.wiredPhotoCount() > 0
        _state.value = _state.value.copy(
            provider = provider, model = model, effort = effort, path = path,
            messages = forked, threads = summaries(), threadId = threadId,
            error = "", pending = forkDraft, streaming = true, queued = "",
            pendingAttachments = emptyList(),
            notices = retainedNotices(),
            activeTools = emptyList(), activeToolLabel = "",
            serverTokens = null,
            activeRunId = "", busyRunId = "",
            showRuns = false, runsLoading = false,
        )
        if (hadStagedFork) {
            stagedDiscardedNotice()
        }
        evictStagedThumbs(stagedFork, stagedForkThread)
        persist()
        doSend(forked, path, provider, model, effort)
    }

    private fun doSend(
        history: List<ChatMessage>,
        path: String = api.pathFor(tab),
        provider: String = api.providerFor(tab),
        model: String = api.modelFor(tab),
        effort: String = api.effortFor(tab, model),
    ) {
        _state.value = _state.value.copy(
            streaming = true, error = "",
            activeTools = emptyList(), activeToolLabel = "",
            activeRunId = "", busyRunId = "",
        )
        // Placeholder assistant message that deltas append to. Its timestamp
        // and model are fixed up front: rebuilding it per token must not
        // mint a fresh ts (remembers keyed on it would reset mid-stream)
        // nor drop the serving model (the meta line would flicker).
        val placeholderTs = ChatThreads.now()
        _state.value = _state.value.copy(
            messages = history + ChatMessage(
                "assistant", "", placeholderTs, model = model,
            )
        )
        val acc = StringBuilder()
        // Reasoning trace accumulator (bounded by the sender; mirrored here
        // so the placeholder update below can't grow it past the cap).
        val racc = StringBuilder()
        // One stable conversation per app thread (#120): the thread id
        // ships as X-Hermes-Session-Id so turns append to the same server
        // session (titles/costs read per conversation). Captured up front:
        // a thread switch mid-turn must not retarget the in-flight request.
        val stableSessionId = threadId
        val liveTools = mutableListOf<String>()
        streamJob?.cancel()
        // A new turn supersedes any pending recovery of the previous one.
        cancelRecovery()
        // Watch for held approvals while this turn streams: a gate stalls
        // the stream with no frames, and the banner is the only signal.
        stopApprovalWatch()
        startApprovalWatch(path)
        // Fresh generation: events from any older turn still buffered go
        // stale (see stop()). Captured below; every handler checks it.
        sendGen++
        val gen = sendGen
        lastRunStatus = null
        // Model actually serving this turn: the request model until a
        // snapshot names the running one (mid-turn switch). Every render
        // below reads this, never the request model, so the meta line
        // cannot flicker back on the next token.
        var servingModel = model
        streamJob = viewModelScope.launch {
            api.streamChat(path, provider, model, history, stableSessionId, effort).collect { event ->
                // Stale turn (cancelled after this event buffered): never
                // let it touch state — it could resurrect a dropped
                // placeholder or clear a newer turn's interrupted flag.
                if (gen != sendGen) return@collect
                when (event) {
                    is ChatEvent.RunId -> {
                        _state.value = _state.value.copy(activeRunId = event.id)
                    }
                    is ChatEvent.RunState -> {
                        val snap = event.snapshot
                        // A blank status carries no information (lenient parse
                        // of an odd frame): keep the prior one rather than
                        // clearing a known cancelled/running.
                        if (snap.status.isNotBlank()) lastRunStatus = snap.status
                        if (snap.tools.isNotEmpty()) {
                            for (t in snap.tools) {
                                val name = t.trim()
                                if (name.isNotEmpty() && name !in liveTools && liveTools.size < 20) {
                                    liveTools.add(name)
                                }
                            }
                            _state.value = _state.value.copy(activeTools = liveTools.toList())
                        }
                        // Prefer the snapshot's model whenever it names one: a
                        // mid-turn switch means the running model, not the
                        // requested one, is serving this stream.
                        if (snap.model.isNotBlank()) {
                            servingModel = snap.model.trim()
                            patchPlaceholder(
                                content = null, reasoning = null,
                                model = servingModel,
                            )
                        }
                        // Seed the accumulators from the authoritative snapshot
                        // when this stream opened on one (reattach): the server
                        // only relays newer frames, so the head would otherwise
                        // never render. Fresh sends snapshot empty output and
                        // skip this; terminal snapshots seed too, and their
                        // replay deltas never arrive (the parser skips them).
                        if (snap.output.isNotEmpty() && acc.isEmpty()) {
                            acc.append(snap.output)
                            patchPlaceholder(
                                content = acc.toString(), reasoning = null,
                                model = servingModel,
                            )
                        }
                        if (snap.reasoning.isNotEmpty() && racc.isEmpty()) {
                            racc.append(snap.reasoning.take(REASONING_CAP - racc.length))
                            patchPlaceholder(
                                content = null, reasoning = racc.toString(),
                                model = servingModel,
                            )
                        }
                    }
                    is ChatEvent.RateLimited -> {
                        // The profile is busy with a different turn: drop the
                        // placeholder (this prompt was never sent) and remember
                        // the blocking run for the runs sheet. The failed
                        // prompt stays in history for Retry; newer typing
                        // stays in the composer untouched. No auto-retry: a
                        // timer would turn a busy profile into a self-DDoS.
                        val msgs = _state.value.messages
                        val unposted = if (msgs.lastOrNull()?.role == "assistant") {
                            msgs.dropLast(1)
                        } else {
                            msgs
                        }
                        // Newer typing stays in the composer untouched: the
                        // failed prompt lives in history for Retry, and the
                        // follow-up sends when the user taps send. Nothing is
                        // queued — queueing it too would send it twice (once
                        // from the composer, once from the queue).
                        streamJob = null
                        _state.value = _state.value.copy(
                            messages = unposted, streaming = false,
                            error = "Profile busy — a run is already answering" +
                                (if (event.runId.isNotBlank()) " (${event.runId.take(8)}…)" else "") +
                                ". Wait or stop it from Runs.",
                            activeTools = emptyList(), activeToolLabel = "",
                            // The RunId event ahead of this one already set
                            // activeRunId to the blocking run: clear it, or a
                            // later stop() would cancel a turn this tab never
                            // started. Only busyRunId keeps it (sheet cancel).
                            activeRunId = "",
                            busyRunId = event.runId,
                        )
                    }
                    is ChatEvent.ToolProgress -> {
                        val name = event.tool.trim()
                        // Capped during collection: a flood of frames must
                        // not grow this unbounded before Done caps at 20.
                        if (name.isNotEmpty() && name !in liveTools && liveTools.size < 20) {
                            liveTools.add(name)
                        }
                        _state.value = _state.value.copy(
                            activeTools = liveTools.toList(),
                            // Keep the last non-empty label; frames
                            // without one must not blank the indicator.
                            activeToolLabel = event.label.ifEmpty { _state.value.activeToolLabel },
                        )
                    }
                    is ChatEvent.Delta -> {
                        acc.append(event.text)
                        val msgs = _state.value.messages
                        val prior = msgs.lastOrNull()
                        // Preserve the reasoning trace accumulated so far:
                        // rebuilding the placeholder from content alone
                        // would wipe it on every token.
                        val kept = if (prior?.role == "assistant") prior.reasoning else ""
                        _state.value = _state.value.copy(
                            messages = msgs.dropLast(1) + ChatMessage(
                                "assistant", acc.toString(), placeholderTs,
                                reasoning = kept, model = servingModel,
                            ),
                        )
                    }
                    is ChatEvent.Reasoning -> {
                        if (racc.length < REASONING_CAP) {
                            racc.append(event.text.take(REASONING_CAP - racc.length))
                        }
                        val msgs = _state.value.messages
                        val last = msgs.lastOrNull()
                        // copy() preserves the serving model patched by the
                        // RunState handler: safe only because the snapshot is
                        // always frame one. A future frame reorder that puts
                        // reasoning first would reintroduce the flicker.
                        if (last?.role == "assistant") {
                            _state.value = _state.value.copy(
                                messages = msgs.dropLast(1) + last.copy(reasoning = racc.toString()),
                            )
                        }
                    }
                    is ChatEvent.Done -> {
                        val final = event.fullText.ifEmpty { acc.toString() }
                        val finishedAt = ChatThreads.now()
                        // A cancelled turn ends with an explicit `cancelled`
                        // snapshot plus `[DONE]` (#311): the stream is clean,
                        // but the reply is user-stopped, not finished. It must
                        // read like stop(), not like a reply — parked queue,
                        // no recovery (the turn is over, output complete), no
                        // notification, USER attribution (not DROP). Keyed on
                        // "cancelled" only: failed/interrupted runs end via
                        // error frames (the Error path), never clean Done.
                        val cancelled = lastRunStatus == "cancelled"
                        // A dropped turn that produced NOTHING leaves an empty assistant
                        // bubble carrying only the "Connection lost" label. The `Error` and
                        // user-stop paths both drop an empty placeholder instead, so this
                        // one was the odd case out — and an empty persisted message is
                        // worse than no message: it enters history and the next turn's
                        // prompt as a blank assistant turn.
                        //
                        // Only when interrupted: a FINISHED reply that happens to be empty
                        // is a real (if useless) answer, and dropping it would hide that
                        // the server responded at all.
                        val nothingArrived = final.isBlank() && racc.toString().isBlank()
                        val finished = if ((event.interrupted || cancelled) && nothingArrived) {
                            _state.value.messages.dropLast(1)
                        } else msgsDropLastPlusAssistant(
                            final, finishedAt, liveTools.toList(), event.usage,
                            racc.toString(), servingModel,
                            // #214: the stream ended without `[DONE]`, so this
                            // reply is a fragment of a turn that is still
                            // executing server-side. The text is kept — it is
                            // real output — but flagged, because reporting a
                            // dropped turn as a finished one is what made the
                            // user resend and duplicate work that had already
                            // been done by the agent's tool calls.
                            interrupted = event.interrupted || cancelled,
                        )
                        // A cancelled finish is user-stopped: fix the
                        // attribution the helper derives (interrupted reads
                        // as DROP there, which blames the network for a
                        // deliberate stop).
                        val doneMessages = if (cancelled) {
                            val tail = finished.lastOrNull()
                            if (tail?.role == "assistant" && tail.interrupted) {
                                finished.dropLast(1) + tail.copy(
                                    interruptedBy = InterruptedBy.USER,
                                )
                            } else {
                                finished
                            }
                        } else {
                            finished
                        }
                        _state.value = _state.value.copy(
                            messages = doneMessages,
                            streaming = false,
                            online = true,
                            activeTools = emptyList(),
                            activeToolLabel = "",
                            activeRunId = "",
                            busyRunId = "",
                        )
                        addTokens(event.usage)
                        upsertActive(doneMessages)
                        persist()
                        // Skipped when the empty bubble was dropped: there is
                        // no message at finished.size - 1 to attach tools to
                        // (it points at the previous user row), and the ts
                        // lookup would miss anyway.
                        if (!((event.interrupted || cancelled) && nothingArrived)) {
                            backfillUsage(path, stableSessionId, finishedAt, final, doneMessages.size - 1)
                        }
                        refreshServerTotals(path, stableSessionId)
                        if (event.interrupted && !nothingArrived && !cancelled) {
                            // The stream died but the turn may have completed
                            // server-side: go try to return it (#214). Skipped
                            // when the bubble was dropped — there is no
                            // message to attach recovered text to.
                            recoverInterruptedTurn(path, stableSessionId, finishedAt, final)
                        }
                        // #58: ping the user when a reply lands while the app
                        // is backgrounded (gateway has no cronjobs to report).
                        // An empty body that dropped before a single frame yields
                        // Done("", interrupted = true) (#214). Notifying on that puts an
                        // empty notification on the lock screen, which reads as a bug in
                        // the app rather than the connection failing — the user has no
                        // reply to look at, so the ping tells them nothing actionable.
                        //
                        // Replied vs crashed (#287): a clean finish pings as a
                        // reply; a cut-off turn pings as a failure, so the
                        // lock screen never reports a fragment as an answer.
                        // A user-cancelled turn pings as neither: stopping
                        // is deliberate, not news.
                        if (!ForegroundTracker.isForeground && !cancelled) {
                            val snippet = final.ifEmpty { acc.toString() }.trim()
                                .replace(Regex("\\s+"), " ").take(160)
                            if (snippet.isNotEmpty()) {
                                if (event.interrupted) {
                                    ChatNotifications.notifyCrashed(appCtx, tabTitle(tab), snippet)
                                } else {
                                    ChatNotifications.notifyDone(appCtx, tabTitle(tab), snippet)
                                }
                            }
                        }
                        // This turn is over: release the job before flushing
                        // so the queued send's doSend never "cancels" the
                        // just-finished collection it runs inside of.
                        streamJob = null
                        stopApprovalWatch()
                        refreshApprovalCount()
                        if (event.interrupted || cancelled) {
                            // A dropped stream does NOT auto-fire the queued send.
                            //
                            // The turn is a fragment: the reply is truncated and the
                            // server-side turn may still be running with its tool calls
                            // half done. Firing the next message straight on top of that
                            // sends it against a context the agent never finished
                            // building — the same class of mistake as #214 itself, where
                            // the user resent into a turn that had not actually ended.
                            // The Error path already parks queued text for exactly this
                            // reason, and a drop is closer to a failure than to a
                            // success, so it parks too.
                            parkQueued()
                        } else {
                            flushQueued(gen)
                        }
                    }
                    is ChatEvent.Error -> {
                        // Terminal (see streamChat): nothing follows. Keep any
                        // partial reply with live-seen tools attached and
                        // marked unreported (the turn ran but carried no
                        // usable `usage`); drop the placeholder only when it
                        // is still empty.
                        //
                        // A kept partial is also flagged as a DROP (#214): an
                        // Error carrying content is a turn that ran and was
                        // cut off, not a clean failure, and without the flag
                        // it renders as a finished reply offering retry
                        // instead of Continue. A user stop never reaches this
                        // branch (stop() patches USER directly and stales the
                        // buffered events), so the flag cannot mislabel one.
                        val msgs = _state.value.messages
                        val last = msgs.lastOrNull()
                        val kept = if (last?.role == "assistant" &&
                            (last.content.isNotBlank() || last.reasoning.isNotBlank())
                        ) {
                            msgs.dropLast(1) + last.copy(
                                tools = (last.tools + liveTools).distinct().take(20),
                                unreported = true,
                            ).asInterruptedDrop()
                        } else if (last?.role == "assistant") {
                            msgs.dropLast(1)
                        } else {
                            msgs
                        }
                        _state.value = _state.value.copy(
                            messages = kept, streaming = false, error = event.message,
                            activeTools = emptyList(), activeToolLabel = "",
                            // Keep a possibly-live id for the sheet: the API
                            // forwards X-Agento-Run-Id ahead of errors, and a
                            // 500 after the run started leaves it executing.
                            // Dead ids are harmless — cancelling one 409s.
                            busyRunId = _state.value.activeRunId.ifBlank { _state.value.busyRunId },
                            activeRunId = "",
                        )
                        // A queued send never auto-fires after a failure (no failure
                        // cascades): hand it back to the composer instead.
                        parkQueued()
                        stopApprovalWatch()
                        refreshApprovalCount()
                        if (kept.size == msgs.size) upsertActive(kept)
                        persist()
                        // A kept partial is a flagged cut-off turn exactly
                        // like an interrupted Done, so it gets the same
                        // recovery poll (#214 review): for a server-sent error
                        // frame the turn is dead and the polls find nothing,
                        // for a mid-body transport failure the orphan may have
                        // finished. Unlike Done the message keeps its
                        // placeholder ts, which is what identifies it.
                        val tail = kept.lastOrNull()
                        if (tail?.role == "assistant" && tail.interrupted) {
                            recoverInterruptedTurn(path, stableSessionId, tail.ts, tail.content)
                        }
                        // Crashed while backgrounded (#287): an Error carrying
                        // a partial fragment is a cut-off turn like an
                        // interrupted Done, so it pings as a failure. A clean
                        // failure with no fragment keeps the in-app error row
                        // only — there is nothing to preview on the lock
                        // screen.
                        if (!ForegroundTracker.isForeground) {
                            val fragment = kept.lastOrNull()
                                ?.takeIf { it.role == "assistant" && it.interrupted }
                                ?.let { (it.content.ifEmpty { it.reasoning }).trim()
                                    .replace(Regex("\\s+"), " ").take(160) }
                                .orEmpty()
                            if (fragment.isNotEmpty()) {
                                ChatNotifications.notifyCrashed(appCtx, tabTitle(tab), fragment)
                            }
                        }
                        checkReachability()
                    }
                }
            }
        }
    }

    /**
     * Rebuilds the trailing assistant placeholder, replacing only the halves
     * passed non-null. Used by snapshot handling (#311), where the text and
     * the reasoning trace arrive on different frames and neither may wipe
     * the other — the same rule the Delta/Reasoning branches follow inline.
     */
    private fun patchPlaceholder(
        content: String?,
        reasoning: String?,
        model: String,
    ) {
        val msgs = _state.value.messages
        val prior = msgs.lastOrNull()
        if (prior?.role != "assistant") return
        _state.value = _state.value.copy(
            messages = msgs.dropLast(1) + prior.copy(
                content = content ?: prior.content,
                reasoning = reasoning ?: prior.reasoning,
                model = model,
            ),
        )
    }

    /** Finished assistant message with the live-seen tools and the turn's
     * server-reported token counts attached (null usage = the stream carried
     * nothing usable, so the reply is marked unreported rather than zero),
     * plus the reasoning trace and the model that served it. [final] is
     * persisted as-is (possibly empty): the UI renders its own fallback, so
     * no synthetic text ever enters history or the next turn's prompt. */
    private fun msgsDropLastPlusAssistant(
        final: String,
        finishedAt: Long,
        tools: List<String>,
        usage: TokenUsage?,
        reasoning: String,
        model: String,
        /** Stream ended without `[DONE]` — see #214. */
        interrupted: Boolean = false,
    ): List<ChatMessage> {
        val msgs = _state.value.messages
        return msgs.dropLast(1) + ChatMessage(
            "assistant",
            final,
            finishedAt,
            tools = tools.distinct().take(20),
            prompt = usage?.prompt ?: 0L,
            completion = usage?.completion ?: 0L,
            total = usage?.total ?: 0L,
            cached = usage?.cached ?: 0L,
            unreported = usage == null,
            model = model.trim(),
            reasoning = reasoning,
            interrupted = interrupted,
            // Derived rather than passed: a Done that is interrupted is interrupted
            // BY A DROP, and the only other way a message ends up flagged is the
            // user pressing stop, which sets its own copy() directly. Passing this in
            // as a second parameter would be a second place to keep in step with the
            // flag — the rule written twice, which is how the two producers of
            // `interrupted` drifted apart in the first place.
            interruptedBy = if (interrupted) InterruptedBy.DROP else InterruptedBy.USER,
        )
    }

    /** Sends a send queued mid-stream once the reply finishes cleanly.
     * [doneGen] must still be current (a stop() in between keeps the queue
     * parked instead of firing into the new turn). */
    private fun flushQueued(doneGen: Int) {
        if (doneGen != sendGen) return
        // Raw queued text, trimmed gate: the fire stores verbatim.
        val raw = _state.value.queued
        if (raw.trim().isEmpty() || _state.value.streaming) return
        // Not ready (connectivity flap): no future Done is coming to fire
        // this, so hand it back to the composer instead of stranding it.
        // Concatenated when occupied: displacing the live draft would drop text.
        if (!_state.value.ready) {
            val live = _state.value.pending
            val restore = if (raw.trim().isEmpty()) "" else raw
            _state.value = _state.value.copy(
                queued = "",
                pending = when {
                    live.isBlank() -> restore
                    restore.isEmpty() -> live
                    else -> joinPending(live, restore)
                },
            )
            return
        }
        _state.value = _state.value.copy(queued = "")
        val provider = api.providerFor(tab)
        val model = api.modelFor(tab)
        val effort = api.effortFor(tab, model)
        val path = api.pathFor(tab)
        _state.value = _state.value.copy(provider = provider, model = model, effort = effort, path = path)
        // Same whitespace normalization as send(): both fire paths must agree.
        val storedFlush = if (raw.trim().isEmpty()) "" else raw
        val history = _state.value.messages +
            ChatMessage("user", storedFlush, ChatThreads.now())
        // The queued fire is text-only by design (see send()), so staged photos
        // stay parked — said out loud. Counted wired.
        val parkedFlushCount = _state.value.pendingAttachments.wiredPhotoCount()
        if (parkedFlushCount > 0) {
            notify(
                "Sent the queued text — " +
                    attachmentMarker(parkedFlushCount) +
                    " still staged for your next message.",
                ToastKind.Info,
            )
        }
        // Composer text typed after queueing survives the flush: the fired
        // turn carries only the queued text.
        _state.value = _state.value.copy(messages = history, streaming = true, error = "")
        doSend(history, path, provider, model, effort)
    }

    /** Refreshes the server-side session totals for the active thread (#121
     * reconciliation: the multi-device truth vs device-observed sums).
     * Transport failures keep the last value; an unknown session (null)
     * clears it so the header falls back to device sums instead of showing
     * a stale total. Only one fetch runs at a time. */
    private var serverTotalsJob: Job? = null

    private fun refreshServerTotals(
        path: String = api.pathFor(tab),
        sessionId: String = threadId,
    ) {
        if (sessionId.isBlank() || !_state.value.ready) return
        serverTotalsJob?.cancel()
        serverTotalsJob = viewModelScope.launch(Dispatchers.IO) {
            val result = runCatching {
                serverApi.fetchSessionTotals(path, sessionId).getOrThrow()
            }
            // Failure = transport/parse: keep the last value. Success(null)
            // = unknown session: clear through so no stale total lingers.
            if (result.isFailure) return@launch
            val totals = result.getOrNull()
            withContext(Dispatchers.Main) {
                if (sessionId == threadId && !_state.value.streaming) {
                    _state.value = _state.value.copy(serverTokens = totals)
                }
            }
        }
    }

    /** Post-turn usage fetch: merges server-recorded tools + skills into the
     * finished message so they persist and render as chips. Silent on
     * failure — the live tools attached at Done stay. [sessionId] is the
     * stable per-thread conversation (#120); the fetch slices to the
     * last turn server-side, so earlier turns never bleed into this
     * reply's chips. The patch targets the
     * index captured at Done time (verified by timestamp, with a timestamp
     * search fallback) so a send made mid-fetch is never clobbered; a thread
     * switch simply finds no match. */
    /**
     * Hands a queued send back to the composer instead of firing it.
     *
     * Used by BOTH terminal paths that must not cascade — a failed turn and a dropped
     * stream. They were two copies of the same ten-line `pending`/`queued` dance, and two
     * copies is how this class of bug starts: the second one differs from the first by
     * accident and nobody notices until the two disagree about what happens to a queued
     * message after a half-finished turn.
     *
     * Concatenated when occupied: like stop()/New/delete/flush, the queued text
     * joins the live draft with the trimEnd/trimStart joint instead of dropping.
     */
    private fun parkQueued() {
        val s = _state.value
        if (s.queued.isBlank()) return
        _state.value = s.copy(
            pending = if (s.pending.isBlank()) {
                s.queued
            } else {
                joinPending(s.pending, s.queued)
            },
            queued = "",
        )
    }

    private fun backfillUsage(
        path: String,
        sessionId: String,
        finishedAt: Long,
        final: String,
        doneIndex: Int,
    ) {
        viewModelScope.launch(Dispatchers.IO) {
            val usage = runCatching {
                serverApi.fetchSessionUsage(path, sessionId).getOrThrow()
            }.getOrNull() ?: return@launch
            if (usage.tools.isEmpty() && usage.skills.isEmpty()) return@launch
            val expected = final
            withContext(Dispatchers.Main) {
                // A resend issued mid-fetch means a new turn is streaming;
                // skip so the in-flight placeholder is never persisted.
                if (_state.value.streaming) return@withContext
                val fresh = _state.value.messages.toMutableList()
                var idx = if (doneIndex in fresh.indices) {
                    val m = fresh[doneIndex]
                    if (m.role == "assistant" && m.ts == finishedAt) doneIndex else -1
                } else -1
                if (idx < 0) {
                    idx = fresh.indexOfLast {
                        it.role == "assistant" && it.ts == finishedAt &&
                            (expected.isEmpty() || it.content == expected)
                    }
                }
                if (idx < 0) return@withContext
                val prev = fresh[idx]
                fresh[idx] = prev.copy(
                    tools = (prev.tools + usage.tools).distinct().take(20),
                    skills = (prev.skills + usage.skills).distinct().take(20),
                )
                _state.value = _state.value.copy(messages = fresh)
                upsertActive(fresh)
                persist()
            }
        }
    }

    /**
     * Best-effort return of a dropped turn's completed text (#214).
     *
     * The orphaned turn keeps running server-side after the stream dies, so
     * a later read of the session transcript can hold MORE of the reply than
     * the stream delivered. Two bounded polls, never a loop: the first adopts
     * a longer text (flag kept — longer is not finished), the second clears
     * the flag only when the text stopped growing between polls, which is the
     * only completion signal available without a server resume primitive.
     *
     * Every patch re-checks that the flagged message is still the tail of the
     * thread (same ts, still last, still flagged, same thread): a resend, a
     * thread switch, or a user edit in between aborts the recovery rather
     * than writing into a turn the user has moved past. Deliberately no
     * auto-resend anywhere here — firing a new turn on top of a possibly
     * still-running orphan is the duplicate execution #214 exists to stop.
     */
    private fun recoverInterruptedTurn(
        path: String,
        sessionId: String,
        finishedAt: Long,
        fragment: String,
    ) {
        if (sessionId.isBlank()) return
        val threadAtKick = threadId
        cancelRecovery()
        recoveryJob = viewModelScope.launch(Dispatchers.IO) {
            delay(RECOVERY_FIRST_DELAY_MS)
            var current = fragment
            // Whether the first poll returned TEXT (even short text). Clearing
            // the flag needs two successful samples: poll 1 failed + poll 2
            // echoing the fragment is a single sample, not stability.
            var firstSeen: String? = null
            fetchRecoveryText(path, sessionId)?.let { first ->
                firstSeen = first
                if (recoveryAdoptable(current, first)) {
                    current = first
                    if (!patchRecovery(threadAtKick, finishedAt, current, clearFlag = false)) {
                        return@launch
                    }
                }
            }
            delay(RECOVERY_SECOND_DELAY_MS)
            val second = fetchRecoveryText(path, sessionId) ?: return@launch
            // Clearing needs the adopted value too, not just two agreeing
            // polls: both polls stable-but-SHORTER than the fragment
            // (transcript lagging, wrong slice) agree with each other while
            // saying nothing about the fragment — blessing it finished would
            // be the exact misreport #214 exists to stop. In the adopted
            // case current IS firstSeen, so that path is unchanged.
            if (firstSeen != null && recoveryTextSettled(firstSeen, second) && second == current) {
                patchRecovery(threadAtKick, finishedAt, current, clearFlag = true)
            } else if (recoveryAdoptable(current, second)) {
                patchRecovery(threadAtKick, finishedAt, second, clearFlag = false)
            }
        }
    }

    /** One transcript read for recovery: null on any failure (transport,
     * unknown session, unparseable body) — recovery is best-effort, and a
     * failed poll must end the attempt, never fail the turn. Cancellation is
     * rethrown, never swallowed: delay() already propagates it, and swallowing
     * it here would let a cancelled recovery keep polling and patching. */
    private suspend fun fetchRecoveryText(path: String, sessionId: String): String? {
        return try {
            serverApi.fetchLastAssistantText(path, sessionId).getOrNull()
        } catch (e: Exception) {
            if (e is CancellationException) throw e
            null
        }
    }

    /**
     * Writes recovered text into the flagged tail message. False when the
     * message is no longer the flagged tail (resent, switched, edited) —
     * the caller stops polling then, rather than writing into a turn the
     * user has moved past.
     */
    private suspend fun patchRecovery(
        threadAtKick: String,
        finishedAt: Long,
        text: String,
        clearFlag: Boolean,
    ): Boolean = withContext(Dispatchers.Main) {
        if (_state.value.streaming) return@withContext false
        if (threadId != threadAtKick) return@withContext false
        val msgs = _state.value.messages
        val idx = msgs.indexOfLast { it.role == "assistant" && it.ts == finishedAt }
        if (idx < 0 || idx != msgs.lastIndex) return@withContext false
        val prev = msgs[idx]
        if (!prev.interrupted) return@withContext false
        val fresh = msgs.toMutableList()
        fresh[idx] = prev.copy(
            content = text,
            interrupted = !clearFlag,
            // Cleared keeps USER, not DROP, on purpose: toUi derives
            // `interrupted` from `interruptedBy == DROP` on load, so
            // persisting DROP would resurrect the cleared flag on next
            // start. `interruptedBy` is only ever read under
            // `if (msg.interrupted)` (ChatScreen), so on a cleared message
            // the value is inert display detail, never a visible claim.
            interruptedBy = if (clearFlag) InterruptedBy.USER else prev.interruptedBy,
        )
        _state.value = _state.value.copy(messages = fresh)
        upsertActive(fresh)
        persist()
        true
    }
}
