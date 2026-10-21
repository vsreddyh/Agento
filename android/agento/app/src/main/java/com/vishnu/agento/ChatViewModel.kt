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

/** One toast-host notice: text + styling + re-fire identity (#339). Data
 * (not bare strings in state) so the queue can hold two identical texts
 * as distinct entries. */
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
    /** Photos staged in the composer for the next turn (#318). Cleared on
     * send; parked untouched while streaming (a mid-turn send queues the
     * text only — the photos wait for the next explicit send). Raw
     * size/emptiness is exact here (#339): staged payloads are never blank
     * (prepareAttachment throws instead of staging one), unlike persisted
     * history, which can be hand-edited — so staged sites read this raw
     * while every history/display/wire site counts wiredPhotos(). */
    val pendingAttachments: List<ChatAttachment> = emptyList(),
    /** Queued notices for the toast host, shown head-first (#339): two
     * rapid notifies (cap refusal + decode failure in one batch) must both
     * surface — a single slot lets the second overwrite the first before
     * the host fires. Set by the ViewModel, headed by `consumeNotice()`
     * after the UI shows each — the error banner is for failed TURNS
     * (with Retry), never for this. */
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

    init {
        viewModelScope.launch(Dispatchers.IO) {
            val loaded = ChatThreads.load(appCtx, tab)
                .filter { it.id.isNotEmpty() }
                .sortedByDescending { it.updatedAt }
            // Empty threads never enter history: drop persisted drafts, keep chats.
            threads = loaded.filter { it.messages.isNotEmpty() }
            var active = threads.firstOrNull()
            if (active == null) {
                active = ChatThread(id = ChatThreads.newId(), updatedAt = ChatThreads.now())
                threads = listOf(active)
                ChatThreads.save(appCtx, tab, threads)
            }
            threadId = active.id
            _state.value = _state.value.copy(
                messages = ChatThreads.toUi(active.messages),
                threads = summaries(),
                threadId = threadId,
                ready = true,
            )
            refreshServerTotals()
            // Pending proposals badge without opening the sheet: one light
            // list at launch, then again on every retarget below.
            pollMemoryCount(api.pathFor(tab))
            // App-killed turn recovery (#311): an unanswered user tail with
            // a live server run resends into the existing run.
            maybeResume(threadId, ChatThreads.toUi(active.messages))
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
        // The path can change here (profile switch in Settings): rows and
        // the badge belong to the new profile, not the old one.
        retargetMemory()
    }

    fun onPending(v: String) {
        _state.value = _state.value.copy(pending = v)
    }

    /**
     * Stages picked photos in the composer (#318). Capped defensively at
     * [MAX_IMAGES_PER_TURN] — the picker already takes only free slots, so
     * the notice below fires only if a caller bypasses it. Excess photos
     * are dropped with a notice, never silently. One string + Info for the
     * whole cap family (#339): the UI paths say exactly this — nothing
     * failed, the cap held, so no Error styling anywhere. Blanks filtered
     * up front: only wired photos occupy cap slots, so a future caller
     * passing storage-read attachments can't spend the cap on blanks.
     * Growth accounting (`after - before`, see the strip) is exact for the
     * same reason: both sides count wired only, so a blank already in the
     * composer can never make `after <= before` while a photo staged —
     * `take.size` would overcount when the backstop drops part of the take
     * on a concurrent-batch race, while the before/after diff measures
     * what actually landed.
     */
    fun addPendingAttachments(items: List<ChatAttachment>) {
        val wired = items.wiredPhotos()
        // Silent by design, not a cap refusal (#339): blanks are not
        // photos, so nothing was presented and nothing was dropped — a
        // capRefused() toast here would misreport ("extra photos were
        // dropped" with zero photos in play). Unreachable via UI
        // (prepareAttachment throws instead of staging blanks).
        if (wired.isEmpty()) return
        val cur = _state.value.pendingAttachments
        // Counted wired (#339): cap slots are for photos that wire, so a
        // blank can never spend one even if a future caller passes
        // storage-read attachments.
        val room = MAX_IMAGES_PER_TURN - cur.wiredPhotoCount()
        if (room <= 0) {
            notify(capRefused(room), ToastKind.Info)
            return
        }
        val take = wired.take(room)
        val dropped = wired.size - take.size
        // Stored canonical-filtered, not raw (#339): the comment above
        // promises only wired photos occupy slots, and a blank already in
        // cur must not persist into the strip/title count without spending
        // room either. Same shape as send()'s stored photos.wiredPhotos().
        _state.value = _state.value.copy(pendingAttachments = cur.wiredPhotos() + take)
        if (dropped > 0) {
            notify(capRefused(room), ToastKind.Info)
        }
    }

    /** True while the composer has a free photo slot (#339: re-checked
     * mid-batch so two rapid picks don't both decode then drop). Counted
     * wired like the cap math above. */
    fun hasAttachmentRoom(): Boolean =
        _state.value.pendingAttachments.wiredPhotoCount() < MAX_IMAGES_PER_TURN

    /** Whether [id] is still the active thread (#339): batches snapshot
     * this at tap/launch time and drop (with notice) if a thread switch
     * landed mid-decode — staging thread-A photos into thread B's
     * composer would mis-send them. */
    fun isThreadCurrent(id: String): Boolean = id == threadId

    /** Live thread id for batch homing (#339, see [isThreadCurrent]). */
    fun threadIdNow(): String = threadId

    /** Live staged-photo count for tap-time cap math (#339): ChatTab reads
     * the ViewModel, not its last-recomposed snapshot, so a
     * remove-then-rapid-repick sees the freed slot. Counted wired. */
    fun pendingAttachmentCount(): Int = _state.value.pendingAttachments.wiredPhotoCount()

    /**
     * Appends heard text to the composer (#339): live results accumulate
     * sentence by sentence (like the one-shot mic path), and the read +
     * write happen atomically here so rapid results can't clobber each
     * other on a stale snapshot. Also preserves user-typed text the old
     * replace dropped. Blank input is a no-op: appending whitespace would
     * park a trailing space with no visible text. Joint symmetric like
     * every other concatenation in this file (#339): the gap normalizes,
     * both sides ride verbatim.
     */
    fun appendPending(text: String) {
        if (text.isBlank()) return
        val cur = _state.value.pending
        _state.value = _state.value.copy(
            pending = if (cur.isBlank()) text else joinPending(cur, text),
        )
    }

    /**
     * Toast-host notice (#339): appended to the queue, so a repeated
     * identical notice re-fires (distinct sequence) instead of deduping
     * silent — and two rapid notifies both surface instead of the second
     * overwriting the first before the host fires. Main-confined like all
     * `_state` writes in this PR's touchpoints (batch decode, camera
     * callbacks, and every send/queue/flush path hop to Main before
     * calling in — see the withContext(Main) sites): the remaining
     * `viewModelScope.launch(IO)` writers are pre-existing (init, totals,
     * runs, approvals, memory, recovery) and out of scope — the long-term
     * fix is `MutableStateFlow` + `update {}`, not more hopping.
     */
    fun notify(msg: String, kind: ToastKind = ToastKind.Error) {
        // Bounded at 3 (#339): ten rapid failures must not queue ten
        // toasts — the oldest drop, the newest (most relevant) survive.
        _state.value = _state.value.copy(
            notices = (_state.value.notices + ToastNotice(msg, kind, noticeSeq.incrementAndGet())).takeLast(3),
        )
    }

    /** Monotonic notice sequence for re-fire identity (see [notify]).
     * Atomic, not proof-by-convention (#339): the ++ lived safely only
     * while every caller hopped to Main first, and the pre-existing
     * launch(IO) writers prove a future notify-from-IO is one edit away —
     * a raced counter duplicates a seq and reintroduces the dedup the
     * queue exists to prevent. The state copy itself stays Main-confined;
     * only the identity counter is thread-safe. */
    private val noticeSeq = java.util.concurrent.atomic.AtomicLong(0L)

    /** Single spelling for the thread-load dead-tap answer (#339): nine
     * call sites share this so the next reword can't fix eight and miss
     * one. Callers keep their own voice gates; this only owns the words. */
    private fun notifyLoading() =
        notify("Conversations still loading — try again in a moment.", ToastKind.Info)

    /** Single spelling for the thread-change staged-discard notice (#339):
     * four thread-change sites share this for the same reason. */
    private fun stagedDiscardedNotice() =
        notify("Staged photos discarded — they belonged to the previous thread.", ToastKind.Info)

    /** Single pending-joint spelling (#339): the gap is exactly one space
     * while BOTH sides ride verbatim (pasted-code indentation included) —
     * the second side is NOT trimmed: a queued code paste handed back into
     * an occupied composer keeps its first-line indent, exactly like the
     * idle-send path stores it. Blank-first yields the second side alone
     * (no stray leading space). Every composer-merge site uses this. */
    private fun joinPending(first: String, second: String): String =
        if (first.isBlank()) second else "${first.trimEnd()} $second"

    /** Removes a staged composer photo by STAGED slot (#339): the strip
     * passes `i` from wiredPhotosWithIndex() while labeling by wired
     * position (`w+1 of N`) — passing the label number removes the wrong
     * photo once blanks exist. Pinned here so a future caller reading the
     * labels can't naturally pass `w`. */
    fun removePendingAttachment(index: Int) {
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

    /** Fresh-process detector for the camera-gate reset (#339): true once,
     * on the first sweep ever. The ViewModel dies with the process and
     * survives rotation — a saveable flag can't tell rotation from
     * kill-with-bundle (both restore), but this can. */
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
        // Drafts never hit disk: only threads with chats are worth keeping.
        val snapshot = threads.filter { it.messages.isNotEmpty() }
        viewModelScope.launch(Dispatchers.IO) {
            ChatThreads.save(appCtx, tab, snapshot)
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
        // Voiced on staged content like newConversation()/deleteThread()
        // (#339): the lumped guard below dead-taps mid-load with a photo
        // staged — split so !ready voices while same-thread/streaming stays
        // silent (a live turn owns the tap; nothing to say there).
        if (!_state.value.ready) {
            if (_state.value.pendingAttachments.wiredPhotoCount() > 0 ||
                _state.value.pending.isNotBlank()
            ) {
                notifyLoading()
            }
            return
        }
        if (id == threadId || _state.value.streaming) return
        val target = threads.firstOrNull { it.id == id } ?: return
        threadId = id
        cancelRecovery()
        runsJob?.cancel()
        stopApprovalWatch()
        retargetApprovals()
        retargetMemory()
        // Staged photos die with the text draft, said out loud (#339):
        // the composer belongs to the thread being left, and a parked
        // photo firing into the wrong thread would be a silent mis-send —
        // so the discard itself voices instead of vanishing.
        val hadStaged = _state.value.pendingAttachments.wiredPhotoCount() > 0
        _state.value = _state.value.copy(
            messages = ChatThreads.toUi(target.messages),
            threads = summaries(),
            threadId = id,
            error = "",
            pending = "",
            // The notice queue dies with them: a cap toast triggered on the
            // old thread must not render over the new composer's first
            // paint (the discard notice below is the only carry-over).
            pendingAttachments = emptyList(),
            notices = emptyList(),
            activeTools = emptyList(),
            activeToolLabel = "",
            serverTokens = null,
            queued = "",
            // Run ids belong to the old thread's turn: a switch must not
            // highlight (or stop, via a stale read) another thread's run.
            activeRunId = "", busyRunId = "",
            showRuns = false, runsLoading = false,
        )
        if (hadStaged) {
            stagedDiscardedNotice()
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
        if (!_state.value.ready) return
        // The thread was written to disk by the notification receiver after
        // this ViewModel loaded, so it is not in memory yet: merge any
        // disk-only threads in (in-memory wins on id clash — it is newer).
        viewModelScope.launch {
            if (threads.none { it.id == id }) {
                val loaded = withContext(Dispatchers.IO) {
                    ChatThreads.load(appCtx, tab).filter { it.id.isNotEmpty() }
                }
                threads = threads + loaded.filter { l -> threads.none { it.id == l.id } }
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
            // retries on the next tap (each tap bumps the gen). Consuming
            // here would lose the tap the moment the turn ends.
            if (_state.value.streaming) return false
            threadId = id
            cancelRecovery()
            runsJob?.cancel()
            stopApprovalWatch()
            retargetApprovals()
            retargetMemory()
            // Same staged-discard voice as switchThread above (#339).
            val hadStagedOpen = _state.value.pendingAttachments.wiredPhotoCount() > 0
            _state.value = _state.value.copy(
                messages = ChatThreads.toUi(target.messages),
                threads = summaries(),
                threadId = id,
                error = "",
                pending = "",
                // Staged photos die with the text draft (#339): the composer
                // belongs to the thread being left, and a parked photo firing
                // into the wrong thread is a silent mis-send. The notice dies
                // with them (same leak as switchThread above).
                pendingAttachments = emptyList(),
                notices = emptyList(),
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
            refreshServerTotals()
            maybeResume(id, ChatThreads.toUi(target.messages))
        }
        if (!autosend || _state.value.streaming) return true
        val msgs = _state.value.messages
        val last = msgs.lastOrNull() ?: return true
        // No assistant reply yet anywhere in this thread — which for the
        // single-message instruction thread is the same as a user-only
        // tail. Anything else (already answered, empty, assistant tail)
        // means leave it alone: a re-tap after a reply must not resend.
        if (last.role != "user" || msgs.any { it.role == "assistant" }) return true
        val provider = api.providerFor(tab)
        val model = api.modelFor(tab)
        val effort = api.effortFor(tab, model)
        val path = api.pathFor(tab)
        _state.value = _state.value.copy(
            provider = provider, model = model, effort = effort, path = path,
            pending = "", streaming = true, error = "", queued = "",
        )
        doSend(msgs, path, provider, model, effort)
        return true
    }

    /** + starts a new conversation; threads with chats are kept, empty
     * drafts are dropped so they never pile up in history. */
    fun newConversation() {
        // Voiced on staged content like send()/retry() (#339): an empty
        // composer tapping through load stays silent, anything staged or
        // typed gets the loading answer.
        if (!_state.value.ready) {
            if (_state.value.pendingAttachments.wiredPhotoCount() > 0 ||
                _state.value.pending.isNotBlank()
            ) {
                notifyLoading()
            }
            return
        }
        streamJob?.cancel()
        streamJob = null
        serverTotalsJob?.cancel()
        // Stop the server-side run too, not just the local socket (#339):
        // like stop(), a New that stayed local would leave the agent running
        // invisibly — and the Runs sheet is scoped to the fresh threadId, so
        // the orphan would no longer be listed anywhere. Same fire-and-forget
        // shape as stop(): captured before clearing, failures logged.
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
        threadId = fresh.id
        cancelRecovery()
        runsJob?.cancel()
        stopApprovalWatch()
        retargetApprovals()
        retargetMemory()
        // Same queued→pending restore as stop() (#339): killing the stream
        // must not eat queued text. Concatenated when the composer is
        // occupied (stop() drops there — a live-thread tradeoff); a fresh
        // thread has no turn to confuse, so keeping every word wins.
        val s = _state.value
        val parked = when {
            s.queued.isBlank() -> ""
            s.pending.isBlank() -> s.queued
            else -> joinPending(s.pending, s.queued)
        }
        // Same staged-discard voice as switchThread (#339).
        val hadStagedNew = _state.value.pendingAttachments.wiredPhotoCount() > 0
        _state.value = _state.value.copy(
            messages = emptyList(), error = "", streaming = false, pending = parked,
            // Staged photos die with the draft like every other thread
            // change (#339): a New with a parked photo must not fire it
            // into the fresh thread. The notice dies with them.
            pendingAttachments = emptyList(),
            notices = emptyList(),
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
        // Voiced on staged content like send()/retry() (#339): an empty
        // composer tapping through load stays silent, anything staged or
        // typed gets the loading answer.
        if (!_state.value.ready) {
            if (_state.value.pendingAttachments.wiredPhotoCount() > 0 ||
                _state.value.pending.isNotBlank()
            ) {
                notifyLoading()
            }
            return
        }
        streamJob?.cancel()
        streamJob = null
        // Stop the server-side run when deleting the ACTIVE thread (#339):
        // same orphan class New closes above — the run belongs to the turn
        // being abandoned, and the Runs sheet re-scopes to the fallback
        // thread. Deleting an inactive thread never touches the active run.
        // Fire-and-forget like stop(), captured before clearing.
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
        if (threadId == id) {
            // `id` is already filtered out above, so the second fallback is
            // just the newest remaining thread, whatever it is.
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
            // Same restore as newConversation above (#339).
            val d = _state.value
            val kept = when {
                d.queued.isBlank() -> ""
                d.pending.isBlank() -> d.queued
                else -> joinPending(d.pending, d.queued)
            }
            // Same staged-discard voice as switchThread (#339).
            val hadStagedDelete = d.pendingAttachments.wiredPhotoCount() > 0
            _state.value = _state.value.copy(
                messages = ChatThreads.toUi(next.messages),
                threadId = next.id, error = "", streaming = false, pending = kept,
                // Same staged-photos-die-with-the-draft rule as the other
                // thread changes above (#339). The notice dies with them.
                pendingAttachments = emptyList(),
                notices = emptyList(),
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
        // Trimmed, NOT lowercased (#339): every haystack here matches with
        // contains(ignoreCase = true), so pre-folding the query would buy
        // nothing and split the rule — one folding, region matching, on
        // all three paths (title, raw text, marker). The old lowercase-
        // everything shape paid a full-string copy per message per
        // keystroke; region matching skips it.
        val q = query.trim()
        if (q.isEmpty()) return _state.value.threads
        return threads.filter { t ->
            t.messages.isNotEmpty() && (
                // Same untitled fallback as summaries() so "new
                // conversation" finds threads without a custom title.
                t.title.ifBlank { UNTITLED }.contains(q, ignoreCase = true) ||
                    // Per-message predicate lives with the marker helpers
                    // (#339): one folding rule (region-matching) shared by
                    // title, raw text, and marker — see messageMatchesQuery.
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
        // least a "# title" header, so a blank check on the body never fires
        // and an empty thread would save a header-only draft.
        // Gone vs empty: a thread deleted while the dialog was open is
        // not an empty conversation, and must not read as one.
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
        // truncated, which the toast surfaces. A client-side cap would
        // duplicate the server constant and drift from it; the worst case
        // is one large POST the user explicitly tapped to send.
        val body = exportMarkdown(id)
        if (body.isBlank()) {
            done("Conversation is empty.", false)
            return
        }
        val path = _state.value.path
        viewModelScope.launch(Dispatchers.IO) {
            // Rechecked, not captured: a profile switch mid-save must not
            // write the old profile's draft. An error, not a silent success:
            // done(null) would toast "Draft saved" for nothing written.
            if (path != api.pathFor(tab)) {
                withContext(Dispatchers.Main) { done("Profile changed — try again.", false) }
                return@launch
            }
            // Source links the draft to its thread: the review surface
            // shows where the procedure came from.
            val result = teachApi.createDraft(path, name.trim(), description.trim(), body, source = "thread:$id")
            withContext(Dispatchers.Main) {
                // Rechecked after the write too: a switch during the network
                // call lands the draft on the old profile, and success must
                // not toast for a row the user no longer looks at.
                if (path != api.pathFor(tab)) {
                    done("Profile changed — try again.", false)
                    return@withContext
                }
                // Always answered, even across a profile switch: the dialog
                // spins on this callback, and a stuck "Saving…" is worse
                // than a toast for the old profile. Local misconfiguration
                // surfaces verbatim (it names the fix); server failures stay
                // code-only.
                val detail = result.exceptionOrNull()?.message.orEmpty()
                val truncated = result.getOrNull()?.second == true
                // Server messages pass through bounded like loadTaught does:
                // they are validation/contract strings ("unparseable
                // response body", "skill name must be…"), never memory text,
                // and the actionable one beats a bare code.
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
            // Photos are payloads, not text: the marker keeps the export
            // honest about turns whose content is an image (#318). Text
            // (photo turns included) is render-trimmed like the bubble
            // (#339): trailing newlines would export a blank line per turn
            // that the bubble doesn't show. Deliberate split from the wire
            // (#339): the wire preserves trailing newlines exactly
            // (verbatim send guarantee — a photo+code turn sends the
            // indentation the agent saw), while the export matches what the
            // bubble SHOWED. Named here so the next edit doesn't "fix" one
            // to match the other: the export is a reading record, the wire
            // is the send record, and they differ on trailing whitespace by
            // design. Counted wired and user-only like the wire rule
            // (#339): a hypothetical assistant attachment must not export
            // a marker the wire would never send.
            val wiredCount = if (m.role == "user") m.attachments.wiredPhotoCount() else 0
            val marker = if (wiredCount > 0) {
                contentWithMarkerVerbatim(m.content.trimEnd(), wiredCount)
            } else {
                m.content.trim()
            }
            sb.append("**$who:** $marker\n\n")
            if (m.role != "user" && (m.tools.isNotEmpty() || m.skills.isNotEmpty())) {
                // Backticked so a tool/skill name can never break the export's
                // Markdown; inner backticks are quoted first so the fence holds.
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
        // Bump the generation first: a Done/Error already buffered from
        // this turn goes stale and can no longer overwrite the patch below.
        sendGen++
        streamJob?.cancel()
        streamJob = null
        // A pending recovery belongs to the turn just stopped: it must not
        // resurrect text behind whatever the user does next.
        cancelRecovery()
        stopApprovalWatch()
        // The badge may still name a gate held at stop time: refresh it like
        // the Done/Error paths do, instead of freezing the pre-turn count.
        refreshApprovalCount()
        // Stop the server-side run this tab started, not just the local
        // socket (#311): closing the stream no longer cancels the work, so a
        // stop that stayed local would leave the agent running invisibly.
        // Only the active run: a 429-parked busy run belongs to another turn
        // and is cancelled from the Runs sheet, never as a side effect here.
        // Fire and forget — the cancelled stream ending (or its absence, when
        // the run already settled) is the confirmation; failures are logged,
        // never silent, so a cleared id always means "asked to stop".
        val runToCancel = _state.value.activeRunId
        val cancelPath = _state.value.path
        if (runToCancel.isNotBlank()) {
            viewModelScope.launch(Dispatchers.IO) {
                runCatching { runsApi.cancel(cancelPath, runToCancel) }
                    .onFailure { runCatching { Log.w("ChatRuns", "cancel $runToCancel failed", it) } }
            }
        }
        // Mark the in-flight reply as stopped (#122): the partial text
        // stays with an explicit interrupted flag (Continue/Regenerate
        // offered in the UI). A queued send is restored to the composer
        // rather than dropped or fired blindly.
        val msgs = _state.value.messages
        val last = msgs.lastOrNull()
        val patched = if (wasStreaming && last?.role == "assistant") {
            if (last.content.isNotBlank() || last.reasoning.isNotBlank()) {
                msgs.dropLast(1) + last.copy(
                    interrupted = true,
                    interruptedBy = InterruptedBy.USER,
                )
            } else {
                // Stopped before anything arrived: drop the empty
                // placeholder so it never enters history or counts.
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
            // A no-op stop (idle tab after a 429) keeps the parked busy run:
            // clearing it would wipe the sheet highlight for no reason.
            busyRunId = if (wasStreaming) "" else _state.value.busyRunId,
            // Queued text concatenates when the composer is occupied
            // (#339): same rule as newConversation/deleteThread — a stop
            // must not eat queued text either, whichever field it lands in.
            // Trimmed at the JOINT only: either side alone rides verbatim
            // (pasted-code indentation included), but the concatenation
            // gap itself normalizes to one space.
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

    /** Toasts that staged photos sit out this resend (#339):
     * retry/regenerate/continue resend history, never the composer's
     * staging — said out loud like every other narrowing in this file.
     * Info, not Error: nothing failed, the photos are safe in the
     * composer. Guarded wired like every marker it gates (#339): the
     * !ready voice below matches send()'s text-or-photos condition. */
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
        // Gated like send() (#339): the !ready tap voices (thread load is
        // reachable), the streaming tap stays silent (affordances hidden —
        // no dead tap reachable there). Narration fires at the resend
        // below, never before the guards, so it can't claim a resend that
        // never happens.
        if (!_state.value.ready) {
            // Voiced on text-or-wired-photos like send() (#339): a typed
            // draft with no photos staged deserves the same answer, and
            // the photo half counts wired like every marker it gates.
            if (_state.value.pendingAttachments.wiredPhotoCount() > 0 || _state.value.pending.isNotBlank()) {
                notifyLoading()
            }
            return
        }
        if (_state.value.streaming) return
        // messageHasContent, not content.isNotBlank (#318): an image-only
        // turn's text is blank and must still resend with its photos.
        val history = stripTrailingFragmentForResend(_state.value.messages)
            .filter { messageHasContent(it) }
        if (history.none { it.role == "user" }) return
        noteParkedPhotos("Retried")
        doSend(history)
    }

    fun send() {
        // Raw stored, trimmed checked (#339): the stored content rides
        // exactly as typed (pasted-code indentation included — the wire
        // verbatim path depends on it), while blankness gates on the
        // trimmed form. Queued text stays trimmed (text-only fire).
        val rawText = _state.value.pending
        val text = rawText.trim()
        // Photos send with or without text (#318): an image-only turn is a
        // real turn (the server treats blank text + images as non-blank).
        val photos = _state.value.pendingAttachments
        // Ready-gated with a voice (#339): the button disables on !ready,
        // but the IME send action bypasses it — an ungated return would
        // dead-tap during thread load, now reachable with photos staged.
        // Voiced on text-or-wired-photos like every notice it gates (#339).
        if (!_state.value.ready) {
            if (text.isNotEmpty() || photos.wiredPhotoCount() > 0) {
                notifyLoading()
            }
            return
        }
        if (text.isEmpty() && photos.wiredPhotoCount() == 0) return
        // Queue-or-send (#122): the composer stays live while streaming —
        // a send mid-stream parks the text and auto-fires on Done, instead
        // of being swallowed by the streaming guard. First queued text wins;
        // later sends wait for the composer (no silent overwrite). Photos
        // stay parked in the composer throughout: the queued fire is text
        // only, and silently attaching staged photos to a later turn would
        // send images the user never confirmed for THAT turn.
        if (_state.value.streaming) {
            if (text.isNotEmpty()) {
                if (_state.value.queued.isBlank()) {
                    // Queued verbatim like the direct send (#339): trim
                    // gates blankness, the stored turn keeps indentation —
                    // a code paste queued mid-stream must fire identical to
                    // the same paste sent idle.
                    _state.value = _state.value.copy(pending = "", queued = rawText)
                    // Same narrowing note as the branches around it (#339):
                    // the queued fire is text-only, so staged photos park —
                    // said here too, not just for the image-only tap. Info:
                    // the photos are safe, nothing failed. Counted wired
                    // like every marker in this file (#339).
                    val parkedQueuedCount = photos.wiredPhotoCount()
                    if (parkedQueuedCount > 0) {
                        notify(
                            "Queued behind this reply — text sends, " +
                                attachmentMarker(parkedQueuedCount) +
                                " stays staged for the next message.",
                            ToastKind.Info,
                        )
                    }
                } else {
                    // Second tap while a queue is parked (#339): the new
                    // text correctly stays in the composer, but with no
                    // toast the tap reads dead — the class this PR keeps
                    // eliminating. Names staged photos like the branch
                    // above so the two queued notices agree (wired count).
                    val parkedSecondCount = photos.wiredPhotoCount()
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
            } else if (photos.wiredPhotoCount() > 0) {
                // Image-only tap while streaming (#339): nothing queues
                // (the queue is text), so say the photos are waiting —
                // otherwise the enabled send button looks dead. Info, same
                // reasoning as above. Counted wired like the gate above it.
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
        _state.value = _state.value.copy(provider = provider, model = model, effort = effort, path = path, queued = "")
        // Stored canonical AND wired (#339): whitespace-only text with
        // photos normalizes to "" (the wire already sends images-only for
        // it, the bubble already renders thumbs-only) — invisible whitespace
        // must not persist through every round-trip. Stored filtered, so a
        // blank can never persist into thread JSON even if the invariant
        // ever breaks upstream.
        val stored = if (text.isEmpty()) "" else rawText
        val history = _state.value.messages +
            ChatMessage("user", stored, ChatThreads.now(), attachments = photos.wiredPhotos())
        _state.value = _state.value.copy(
            messages = history, pending = "", pendingAttachments = emptyList(),
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
        // Ready-gated with a voice like send() (#339): the Steer affordance
        // only shows with non-blank text, so the empty-text half is
        // UI-unreachable — but !ready is reachable (streaming during load),
        // and an ungated return would dead-tap there.
        if (text.isEmpty() || !_state.value.ready) {
            // Counted wired like send()'s gate (#339).
            if (!_state.value.ready &&
                (text.isNotEmpty() || _state.value.pendingAttachments.wiredPhotoCount() > 0)
            ) {
                notifyLoading()
            }
            return
        }
        if (!_state.value.streaming || _state.value.activeRunId.isBlank()) {
            send()
            return
        }
        // Steering carries text only (#339): staged photos park with no
        // part in the steered turn — said out loud like every other narrowing,
        // or the steer button silently drops the user's staging. Info:
        // parked, not failed. Counted wired. Voiced only on the Queued
        // outcome below (#339): notifying here claims success before the
        // steer resolves, and the Conflict branch calls send() which voices
        // its own "Queued behind this reply…" with the same photos — one
        // tap, two toasts, the first false on that path. Recounted live at
        // settle time (staging may change behind the IO round-trip), so
        // nothing is captured here — only the run identity and path the
        // steer needs.
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
                    // Strip only what this steer sent (#339): typing that landed
                    // mid-round-trip belongs to the user, not the turn.
                    // Prefix, not equality: composer "ABC" + steered "ABC" +
                    // typed "DEF" joins to "ABC DEF" behind the IO — the old
                    // exact-match clear would keep the whole field and
                    // re-send ABC on the next send. One joint space dropped
                    // (the joinPending separator), the remainder verbatim —
                    // a head edit (no prefix) keeps the field untouched:
                    // unsplittable, so the whole draft stays and nothing is
                    // lost (the steer already fired).
                    is SteerOutcome.Queued -> {
                        val curPending = _state.value.pending
                        _state.value = _state.value.copy(
                            pending = if (curPending.startsWith(text)) {
                                curPending.removePrefix(text).removePrefix(" ")
                            } else {
                                curPending
                            },
                        )
                        // Parked-photos voice lives here, not pre-launch
                        // (#339): the Conflict branch below calls send(),
                        // which voices its own queued notice with the same
                        // photos — notifying there too double-toasts one tap.
                        // Recounted live, not pre-launch: staging may have
                        // changed behind the IO round-trip.
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
                    // Settled race: the text becomes the next turn. When the
                    // queue was already occupied send() parks nothing and the
                    // composer keeps the text, so say the queue is full
                    // rather than claiming it queued behind.
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
        // Image-only tails count (#318): their text is blank but the photos
        // resend into the live run exactly like text (same conversation +
        // same prompt text + same image signature reattaches server-side).
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
            // Image-only tails match on blank prompt (#339): the server
            // persists Prompt as the raw text ("" for photo-only turns),
            // so a live run of "" beside an image-only tail is the same
            // turn — and resending the identical history reproduces the
            // identical reattach key (text + image signature). A false
            // positive (live TEXT run, image-only tail) only 429s at the
            // server's own text+images comparison — never misattaches.
            val imageOnlyTail = last.content.isBlank() &&
                last.attachments.wiredPhotoCount() > 0
            // Newest match wins (#339): two image-only tails in rapid
            // succession (retry killed + resent) can both be live — resume
            // into the one still producing, not the older settling one.
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
                // Prompt-identity re-check under Main (#339): entry matched
                // the live run against `last`, but the user may have sent a
                // new turn during the list() window — cur is then a newer
                // tail and resending it under the old match re-fires the NEW
                // history. Re-verify cur.last still matches the same
                // run (same promptMatches/image-signature condition) before
                // doSend.
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
        // Same gate split as retry() (#339): voice !ready, silent on
        // streaming (affordances hidden), narrate at the resend.
        if (!_state.value.ready) {
            // Voiced on text-or-wired-photos like send() (#339): a typed
            // draft with no photos staged deserves the same answer, and
            // the photo half counts wired like every marker it gates.
            if (_state.value.pendingAttachments.wiredPhotoCount() > 0 || _state.value.pending.isNotBlank()) {
                notifyLoading()
            }
            return
        }
        if (_state.value.streaming) return
        // Same image-only rule as retry() (#318).
        val msgs = _state.value.messages.filter { messageHasContent(it) }
        if (msgs.none { it.role == "user" }) return
        val trimmed = if (msgs.lastOrNull()?.role == "assistant") msgs.dropLast(1) else msgs
        if (trimmed.none { it.role == "user" }) return
        noteParkedPhotos("Regenerated")
        val provider = api.providerFor(tab)
        val model = api.modelFor(tab)
        val effort = api.effortFor(tab, model)
        val path = api.pathFor(tab)
        _state.value = _state.value.copy(
            provider = provider, model = model, effort = effort, path = path, queued = "",
        )
        doSend(trimmed, path, provider, model, effort)
    }

    /** Continues a stopped reply (#122): appends an honest "Continue" turn
     * and sends (the stateless loop resends full history, so continuation
     * is a fresh turn — never presented as server-side resumption). */
    fun continueTurn() {
        // Same gate split as retry() (#339).
        if (!_state.value.ready) {
            // Voiced on text-or-wired-photos like send() (#339): a typed
            // draft with no photos staged deserves the same answer, and
            // the photo half counts wired like every marker it gates.
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
        _state.value = _state.value.copy(
            provider = provider, model = model, effort = effort, path = path, queued = "",
        )
        val history = msgs + ChatMessage(
            "user",
            "Continue",
            ChatThreads.now(),
            // Carry the interrupted turn's photos (#339): the "Continue"
            // turn becomes the new lastUserIdx — the ONLY turn that keeps
            // image parts. Without the carry, the photo turn that produced
            // the interrupted reply drops to marker text and the continued
            // turn reasons without the pixels (unless Pi's session already
            // holds them). Same carry rule as Replace/Fork.
            attachments = msgs.lastOrNull { it.role == "user" }
                ?.attachments?.wiredPhotos().orEmpty(),
        )
        // Typed draft survives like retry()/regenerate() (#339): blanking
        // pending here would vaporize a draft typed while the interrupted
        // reply offered Continue — the same silent-loss class the parked
        // siblings (joinPending restores, replace/fork carries) closed
        // everywhere else. Continue sends history, never the composer.
        _state.value = _state.value.copy(messages = history, streaming = true, error = "")
        doSend(history, path, provider, model, effort)
    }

    /** Edits a user message and resends from there (#122 Replace): history
     * after [index] is dropped. Callers confirm first when turns are lost. */
    fun replaceAndResend(index: Int, text: String) {
        // !ready voiced like send()/retry() (#339): gated on staged
        // content like the siblings — an empty composer voices nothing.
        // Streaming stays silent (editor affordances hide while streaming —
        // no dead tap reachable).
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
        // Empty text is allowed only onto photos (#339): stripping the
        // words off a photo+text turn yields the image-only shape send()
        // supports — while an empty text-only resend stays refused.
        // Counted wired: blanks are not photos.
        if (clean.isEmpty() && msgs[index].attachments.wiredPhotoCount() == 0) return
        val provider = api.providerFor(tab)
        val model = api.modelFor(tab)
        val effort = api.effortFor(tab, model)
        val path = api.pathFor(tab)
        _state.value = _state.value.copy(
            provider = provider, model = model, effort = effort, path = path, queued = "",
        )
        // The edited turn keeps its photos (#339): resending the new text
        // without them would silently narrow the turn the user composed.
        // Carry-only by design: the editor offers no drop-photos affordance
        // (a second feature), and the HintLine + confirm clause say the
        // photos resend — so the behavior is visible, not silent.
        // Stored verbatim like send() (#339): trim gates blankness, the
        // turn keeps indentation — with the same whitespace-only
        // normalization to "". Carried wired: a blank in a hand-edited
        // source turn must not ride along to disagree with the wired
        // dialog count.
        // Staged composer photos are deliberately NOT cleared here (unlike
        // send()): send() clears because the staged photos WERE sent, while
        // this sends the edited turn's carried photos — wiping the
        // composer's unrelated staging would destroy user input. Same for
        // typed text (#339): the draft parks back in the composer instead
        // of vaporizing — it stays sendable/queueable behind the new turn.
        val draft = _state.value.pending
        val history = msgs.take(index) +
            ChatMessage(
                "user",
                if (clean.isEmpty()) "" else text,
                ChatThreads.now(),
                attachments = msgs[index].attachments.wiredPhotos(),
            )
        // Staged composer photos sit out like every other resend (#339):
        // retry/regenerate/continue all voice it via noteParkedPhotos —
        // Replace/Fork carry the EDITED turn's photos but leave the
        // composer's staging parked with no voice otherwise.
        noteParkedPhotos("Replaced")
        _state.value = _state.value.copy(messages = history, pending = draft, streaming = true, error = "")
        doSend(history, path, provider, model, effort)
    }

    /** Forks a thread at an edited user message (#122): the copy (up to and
     * including the replacement) becomes a new conversation and sends. */
    fun forkAndResend(index: Int, text: String) {
        // Same gated voice as Replace above (#339).
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
        // Same empty-text rule as Replace above (#339), counted wired.
        if (clean.isEmpty() && msgs[index].attachments.wiredPhotoCount() == 0) return
        val provider = api.providerFor(tab)
        val model = api.modelFor(tab)
        val effort = api.effortFor(tab, model)
        val path = api.pathFor(tab)
        // Same photo-carry rule as Replace above (#339): the forked turn is
        // the edited turn, photos included — stored verbatim like send(),
        // carried wired like Replace. Composer staging does NOT survive
        // here, unlike Replace (#339): forking switches to a FRESH thread,
        // and every thread change discards staging like switchThread (a
        // parked photo firing into the new thread would be a silent
        // mis-send — the round-2 leak class). The typed draft still parks
        // back (it is thread-agnostic input); the photos belong to the
        // old thread and die with the voice below.
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
        threads = listOf(fresh) + threads
        threadId = fresh.id
        cancelRecovery()
        runsJob?.cancel()
        stopApprovalWatch()
        retargetApprovals()
        retargetMemory()
        // Typed draft parks back like Replace above (#339): forking must
        // not eat composer text either — it stays sendable behind the turn
        // on the new thread. Staged photos do NOT park — they die with the
        // thread being left, said out loud like every other thread change
        // (switchThread/newConversation/deleteThread): the notice dies with
        // them too, and only this carry-over survives.
        val forkDraft = _state.value.pending
        val hadStagedFork = _state.value.pendingAttachments.wiredPhotoCount() > 0
        _state.value = _state.value.copy(
            provider = provider, model = model, effort = effort, path = path,
            messages = forked, threads = summaries(), threadId = threadId,
            error = "", pending = forkDraft, streaming = true, queued = "",
            pendingAttachments = emptyList(),
            notices = emptyList(),
            activeTools = emptyList(), activeToolLabel = "",
            serverTokens = null,
            activeRunId = "", busyRunId = "",
            showRuns = false, runsLoading = false,
        )
        if (hadStagedFork) {
            stagedDiscardedNotice()
        }
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
        // Raw queued text, trimmed gate (#339): the fire stores verbatim
        // like the direct send, so a mid-stream code paste keeps its
        // indentation — trim decides blankness only.
        val raw = _state.value.queued
        if (raw.trim().isEmpty() || _state.value.streaming) return
        // Not ready (connectivity flap): no future Done is coming to fire
        // this, so hand it back to the composer instead of stranding it.
        // Concatenated when occupied (#339): displacing the live draft
        // would drop typed text — the reverse of parkQueued's contract.
        // Verbatim both sides like the fire path: the hand-back is a
        // composer restore, and a whitespace-only queue normalizes to ""
        // exactly as a firing one would (send() gates it non-blank, so
        // this is symmetry insurance, not a live path).
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
        // Same whitespace normalization as send() (#339): blank text fires
        // as "" (unreachable via the trimmed gate above, but the two fire
        // paths must not disagree on canonical form).
        val storedFlush = if (raw.trim().isEmpty()) "" else raw
        val history = _state.value.messages +
            ChatMessage("user", storedFlush, ChatThreads.now())
        // The queued fire is text-only by design (see send()), so staged
        // photos stay parked — said out loud (#339), because a silent
        // text-only send after staging photos reads as a dropped attach.
        // Counted wired.
        val parkedFlushCount = _state.value.pendingAttachments.wiredPhotoCount()
        if (parkedFlushCount > 0) {
            notify(
                "Sent the queued text — " +
                    attachmentMarker(parkedFlushCount) +
                    " still staged for your next message.",
                ToastKind.Info,
            )
        }
        // Composer text typed after queueing survives the flush (#339):
        // clearing pending here would discard the new line with no park
        // and no notice — the same silent loss stop()/New/delete already
        // concatenate against. The fired turn carries only the queued
        // text; the draft waits for the next send.
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
     * Concatenated when occupied (#339): like stop()/New/delete/flush,
     * the queued text joins the live draft with the trimEnd/trimStart
     * joint instead of dropping — failure/drop paths are where loss
     * hurts most, so the old drop asymmetry is retired here.
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
