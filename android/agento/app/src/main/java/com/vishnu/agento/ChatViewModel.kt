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

data class ChatUiState(
    val provider: String = "",
    val model: String = "",
    val effort: String = "",
    val path: String = "",
    val messages: List<ChatMessage> = emptyList(),
    val streaming: Boolean = false,
    val pending: String = "",
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
    /** Approval ids with a decision in flight: cards disable their buttons
     * so a double tap cannot fire two decisions. */
    val decidingApprovals: Set<String> = emptySet(),
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
    }

    fun onPending(v: String) {
        _state.value = _state.value.copy(pending = v)
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
        if (id == threadId || _state.value.streaming || !_state.value.ready) return
        val target = threads.firstOrNull { it.id == id } ?: return
        threadId = id
        cancelRecovery()
        runsJob?.cancel()
        stopApprovalWatch()
        retargetApprovals()
        _state.value = _state.value.copy(
            messages = ChatThreads.toUi(target.messages),
            threads = summaries(),
            threadId = id,
            error = "",
            pending = "",
            activeTools = emptyList(),
            activeToolLabel = "",
            serverTokens = null,
            queued = "",
            // Run ids belong to the old thread's turn: a switch must not
            // highlight (or stop, via a stale read) another thread's run.
            activeRunId = "", busyRunId = "",
            showRuns = false, runsLoading = false,
        )
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
            _state.value = _state.value.copy(
                messages = ChatThreads.toUi(target.messages),
                threads = summaries(),
                threadId = id,
                error = "",
                pending = "",
                activeTools = emptyList(),
                activeToolLabel = "",
                serverTokens = null,
                queued = "",
                activeRunId = "", busyRunId = "",
                showRuns = false, runsLoading = false,
            )
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
        if (!_state.value.ready) return
        streamJob?.cancel()
        streamJob = null
        serverTotalsJob?.cancel()
        val fresh = ChatThread(id = ChatThreads.newId(), updatedAt = ChatThreads.now())
        threads = listOf(fresh) + threads.filter { it.messages.isNotEmpty() }
        threadId = fresh.id
        cancelRecovery()
        runsJob?.cancel()
        stopApprovalWatch()
        retargetApprovals()
        _state.value = _state.value.copy(
            messages = emptyList(), error = "", streaming = false, pending = "",
            threads = summaries(), threadId = threadId,
            activeTools = emptyList(), activeToolLabel = "",
            serverTokens = null,
            queued = "",
            activeRunId = "", busyRunId = "",
            showRuns = false, runsLoading = false,
        )
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
        if (!_state.value.ready) return
        streamJob?.cancel()
        streamJob = null
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
            _state.value = _state.value.copy(
                messages = ChatThreads.toUi(next.messages),
                threadId = next.id, error = "", streaming = false, pending = "",
                serverTokens = null, queued = "",
                activeRunId = "", busyRunId = "",
                showRuns = false, runsLoading = false,
            )
        }
        _state.value = _state.value.copy(threads = summaries())
        persist()
        refreshServerTotals()
    }

    /** Threads matching [query] in title or message text (switcher search). */
    fun searchThreads(query: String): List<ThreadSummary> {
        val q = query.trim().lowercase()
        if (q.isEmpty()) return _state.value.threads
        return threads.filter { t ->
            t.messages.isNotEmpty() && (
                // Same untitled fallback as summaries() so "new
                // conversation" finds threads without a custom title.
                t.title.ifBlank { UNTITLED }.lowercase().contains(q) ||
                    t.messages.any { it.content.lowercase().contains(q) }
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

    /** Renders a conversation as Markdown for the sharesheet export. */
    fun exportMarkdown(id: String): String {
        val t = threads.firstOrNull { it.id == id } ?: return ""
        val title = t.title.ifBlank { UNTITLED }
        val sb = StringBuilder("# $title\n\n")
        for (m in t.messages) {
            val who = if (m.role == "user") "You" else "Assistant"
            sb.append("**$who:** ${m.content.trim()}\n\n")
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
        _state.value = _state.value.copy(
            messages = patched, streaming = false,
            activeTools = emptyList(), activeToolLabel = "",
            queued = "",
            activeRunId = "",
            // A no-op stop (idle tab after a 429) keeps the parked busy run:
            // clearing it would wipe the sheet highlight for no reason.
            busyRunId = if (wasStreaming) "" else _state.value.busyRunId,
            pending = if (queued.isNotBlank() && _state.value.pending.isBlank()) queued
                else _state.value.pending,
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

    /** Resends the current history (used after a failed request).
     *
     * A trailing fragment never goes back out (#214): retry used to resend
     * the interrupted reply as part of history, priming a new turn with its
     * own truncated output while the orphaned turn might still be running.
     * Like regenerate, the resend drops it; the visible thread keeps it
     * until the new turn's Done replaces it (doSend rebuilds from history).
     */
    fun retry() {
        if (_state.value.streaming || !_state.value.ready) return
        val history = stripTrailingFragmentForResend(_state.value.messages)
            .filter { it.content.isNotBlank() }
        if (history.none { it.role == "user" }) return
        doSend(history)
    }

    fun send() {
        val text = _state.value.pending.trim()
        if (text.isEmpty() || !_state.value.ready) return
        // Queue-or-send (#122): the composer stays live while streaming —
        // a send mid-stream parks the text and auto-fires on Done, instead
        // of being swallowed by the streaming guard. First queued text wins;
        // later sends wait for the composer (no silent overwrite).
        if (_state.value.streaming) {
            if (_state.value.queued.isBlank()) {
                _state.value = _state.value.copy(pending = "", queued = text)
            }
            return
        }
        // Re-read per-tab config at send time so Settings edits apply instantly.
        val provider = api.providerFor(tab)
        val model = api.modelFor(tab)
        val effort = api.effortFor(tab, model)
        val path = api.pathFor(tab)
        _state.value = _state.value.copy(provider = provider, model = model, effort = effort, path = path, queued = "")
        val history = _state.value.messages +
            ChatMessage("user", text, ChatThreads.now())
        _state.value = _state.value.copy(messages = history, pending = "", streaming = true, error = "")
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
        if (text.isEmpty() || !_state.value.ready) return
        if (!_state.value.streaming || _state.value.activeRunId.isBlank()) {
            send()
            return
        }
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
                    // Clear only what this steer sent: typing that landed
                    // mid-round-trip belongs to the user, not the turn.
                    is SteerOutcome.Queued ->
                        if (_state.value.pending.trim() == text) {
                            _state.value = _state.value.copy(pending = "")
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
                    approvals = pending.getOrNull() ?: _state.value.approvals,
                    auditRows = audit.getOrNull() ?: _state.value.auditRows,
                    approvalsLoading = false,
                    approvalsError = listOf(notice.orEmpty(), loadErr)
                        .filter { it.isNotBlank() }
                        .joinToString(" "),
                    pendingApprovalCount = pending.getOrNull()?.size
                        ?: _state.value.pendingApprovalCount,
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
        val n = approvalsApi.listApprovals(path, status = "pending", limit = 100)
            .getOrNull()?.size ?: return
        withContext(Dispatchers.Main) {
            // Stale poll (profile switched mid-flight): the old profile's
            // count must not land on the new one.
            if (_state.value.path != path) return@withContext
            _state.value = _state.value.copy(pendingApprovalCount = n)
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
        if (last.role != "user" || last.content.isBlank()) return
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
            val run = runs.firstOrNull {
                !it.isTerminal() && it.conversationId == threadId && promptMatches(last.content, it.prompt)
            } ?: return@launch
            // Bounded: re-clearing only loses the once-guard for threads long
            // dead, whose resend finds no live run and no-ops.
            withContext(Dispatchers.Main) {
                if (threadId != this@ChatViewModel.threadId || _state.value.streaming || !_state.value.ready) {
                    return@withContext
                }
                val cur = _state.value.messages
                if (cur.lastOrNull()?.role != "user") return@withContext
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
        if (_state.value.streaming || !_state.value.ready) return
        val msgs = _state.value.messages.filter { it.content.isNotBlank() }
        if (msgs.none { it.role == "user" }) return
        val trimmed = if (msgs.lastOrNull()?.role == "assistant") msgs.dropLast(1) else msgs
        if (trimmed.none { it.role == "user" }) return
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
        if (_state.value.streaming || !_state.value.ready) return
        val msgs = _state.value.messages
        val last = msgs.lastOrNull()
        if (last?.role != "assistant" || !last.interrupted) return
        val provider = api.providerFor(tab)
        val model = api.modelFor(tab)
        val effort = api.effortFor(tab, model)
        val path = api.pathFor(tab)
        _state.value = _state.value.copy(
            provider = provider, model = model, effort = effort, path = path, queued = "",
        )
        val history = msgs + ChatMessage("user", "Continue", ChatThreads.now())
        _state.value = _state.value.copy(messages = history, pending = "", streaming = true, error = "")
        doSend(history, path, provider, model, effort)
    }

    /** Edits a user message and resends from there (#122 Replace): history
     * after [index] is dropped. Callers confirm first when turns are lost. */
    fun replaceAndResend(index: Int, text: String) {
        if (_state.value.streaming || !_state.value.ready) return
        val clean = text.trim()
        if (clean.isEmpty()) return
        val msgs = _state.value.messages
        if (index !in msgs.indices || msgs[index].role != "user") return
        val provider = api.providerFor(tab)
        val model = api.modelFor(tab)
        val effort = api.effortFor(tab, model)
        val path = api.pathFor(tab)
        _state.value = _state.value.copy(
            provider = provider, model = model, effort = effort, path = path, queued = "",
        )
        val history = msgs.take(index) + ChatMessage("user", clean, ChatThreads.now())
        _state.value = _state.value.copy(messages = history, pending = "", streaming = true, error = "")
        doSend(history, path, provider, model, effort)
    }

    /** Forks a thread at an edited user message (#122): the copy (up to and
     * including the replacement) becomes a new conversation and sends. */
    fun forkAndResend(index: Int, text: String) {
        if (_state.value.streaming || !_state.value.ready) return
        val clean = text.trim()
        if (clean.isEmpty()) return
        val msgs = _state.value.messages
        if (index !in msgs.indices || msgs[index].role != "user") return
        val provider = api.providerFor(tab)
        val model = api.modelFor(tab)
        val effort = api.effortFor(tab, model)
        val path = api.pathFor(tab)
        val forked = msgs.take(index) + ChatMessage("user", clean, ChatThreads.now())
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
        _state.value = _state.value.copy(
            provider = provider, model = model, effort = effort, path = path,
            messages = forked, threads = summaries(), threadId = threadId,
            error = "", pending = "", streaming = true, queued = "",
            activeTools = emptyList(), activeToolLabel = "",
            serverTokens = null,
            activeRunId = "", busyRunId = "",
            showRuns = false, runsLoading = false,
        )
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
        val q = _state.value.queued.trim()
        // A newer turn started mid-flush: keep the queue for its Done
        // rather than firing into the live request.
        if (q.isEmpty() || _state.value.streaming) return
        // Not ready (connectivity flap): no future Done is coming to fire
        // this, so hand it back to the composer instead of stranding it.
        if (!_state.value.ready) {
            _state.value = _state.value.copy(queued = "", pending = q)
            return
        }
        _state.value = _state.value.copy(queued = "")
        val provider = api.providerFor(tab)
        val model = api.modelFor(tab)
        val effort = api.effortFor(tab, model)
        val path = api.pathFor(tab)
        _state.value = _state.value.copy(provider = provider, model = model, effort = effort, path = path)
        val history = _state.value.messages +
            ChatMessage("user", q, ChatThreads.now())
        _state.value = _state.value.copy(messages = history, pending = "", streaming = true, error = "")
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
     * Queued text never overwrites what the user is already typing — if `pending` is
     * occupied, the queued text is dropped rather than displacing live input. That
     * asymmetry is deliberate and is the one thing worth remembering about this function.
     */
    private fun parkQueued() {
        val s = _state.value
        if (s.queued.isBlank()) return
        _state.value = s.copy(
            pending = if (s.pending.isBlank()) s.queued else s.pending,
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
