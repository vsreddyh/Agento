package com.vishnu.agento

import android.app.Application
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
 * One instance per chat tab (story / resumes / god). Conversations per tab
 * persist to disk (`ChatThreads`, up to 20 threads) so history survives
 * restarts; the switcher picks the active one and + starts a new thread
 * without deleting the rest. The full active history is sent per request.
 */
class ChatViewModel(app: Application, val tab: String) : AndroidViewModel(app) {

    private val api = ChatApi(app)
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
        )
        refreshServerTotals()
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
        _state.value = _state.value.copy(
            messages = emptyList(), error = "", streaming = false, pending = "",
            threads = summaries(), threadId = threadId,
            activeTools = emptyList(), activeToolLabel = "",
            serverTokens = null,
            queued = "",
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
            _state.value = _state.value.copy(
                messages = ChatThreads.toUi(next.messages),
                threadId = next.id, error = "", streaming = false, pending = "",
                serverTokens = null, queued = "",
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
        _state.value = _state.value.copy(
            provider = provider, model = model, effort = effort, path = path,
            messages = forked, threads = summaries(), threadId = threadId,
            error = "", pending = "", streaming = true, queued = "",
            activeTools = emptyList(), activeToolLabel = "",
            serverTokens = null,
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
        // Fresh generation: events from any older turn still buffered go
        // stale (see stop()). Captured below; every handler checks it.
        sendGen++
        val gen = sendGen
        streamJob = viewModelScope.launch {
            api.streamChat(path, provider, model, history, stableSessionId, effort).collect { event ->
                // Stale turn (cancelled after this event buffered): never
                // let it touch state — it could resurrect a dropped
                // placeholder or clear a newer turn's interrupted flag.
                if (gen != sendGen) return@collect
                when (event) {
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
                                reasoning = kept, model = model,
                            ),
                        )
                    }
                    is ChatEvent.Reasoning -> {
                        if (racc.length < REASONING_CAP) {
                            racc.append(event.text.take(REASONING_CAP - racc.length))
                        }
                        val msgs = _state.value.messages
                        val last = msgs.lastOrNull()
                        if (last?.role == "assistant") {
                            _state.value = _state.value.copy(
                                messages = msgs.dropLast(1) + last.copy(reasoning = racc.toString()),
                            )
                        }
                    }
                    is ChatEvent.Done -> {
                        val final = event.fullText.ifEmpty { acc.toString() }
                        val finishedAt = ChatThreads.now()
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
                        val finished = if (event.interrupted && nothingArrived) {
                            _state.value.messages.dropLast(1)
                        } else msgsDropLastPlusAssistant(
                            final, finishedAt, liveTools.toList(), event.usage,
                            racc.toString(), model,
                            // #214: the stream ended without `[DONE]`, so this
                            // reply is a fragment of a turn that is still
                            // executing server-side. The text is kept — it is
                            // real output — but flagged, because reporting a
                            // dropped turn as a finished one is what made the
                            // user resend and duplicate work that had already
                            // been done by the agent's tool calls.
                            interrupted = event.interrupted,
                        )
                        _state.value = _state.value.copy(
                            messages = finished,
                            streaming = false,
                            online = true,
                            activeTools = emptyList(),
                            activeToolLabel = "",
                        )
                        addTokens(event.usage)
                        upsertActive(finished)
                        persist()
                        // Skipped when the empty bubble was dropped: there is
                        // no message at finished.size - 1 to attach tools to
                        // (it points at the previous user row), and the ts
                        // lookup would miss anyway.
                        if (!(event.interrupted && nothingArrived)) {
                            backfillUsage(path, stableSessionId, finishedAt, final, finished.size - 1)
                        }
                        refreshServerTotals(path, stableSessionId)
                        if (event.interrupted && !nothingArrived) {
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
                        if (!ForegroundTracker.isForeground) {
                            val snippet = final.ifEmpty { acc.toString() }.trim()
                                .replace(Regex("\\s+"), " ").take(160)
                            if (snippet.isNotEmpty()) {
                                ChatNotifications.notifyDone(appCtx, tabTitle(tab), snippet)
                            }
                        }
                        // This turn is over: release the job before flushing
                        // so the queued send's doSend never "cancels" the
                        // just-finished collection it runs inside of.
                        streamJob = null
                        if (event.interrupted) {
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
                        )
                        // A queued send never auto-fires after a failure (no failure
                        // cascades): hand it back to the composer instead.
                        parkQueued()
                        if (kept.size == msgs.size) upsertActive(kept)
                        persist()
                        checkReachability()
                    }
                }
            }
        }
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
