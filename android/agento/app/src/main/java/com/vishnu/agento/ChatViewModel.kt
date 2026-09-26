package com.vishnu.agento

import android.app.Application
import android.content.Context
import androidx.compose.runtime.State
import androidx.compose.runtime.mutableStateOf
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
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

data class ChatUiState(
    val provider: String = "",
    val model: String = "",
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
    /** Distinct tool names seen live this turn (hermes.tool.progress frames). */
    val activeTools: List<String> = emptyList(),
    /** Label of the latest live tool frame, e.g. what the tool is doing. */
    val activeToolLabel: String = "",
)

/**
 * One instance per chat tab (story / resumes / god). Conversations per tab
 * persist to disk (`ChatThreads`, up to 20 threads) so history survives
 * restarts; the switcher picks the active one and + starts a new thread
 * without deleting the rest. The full active history is sent per request.
 */
class ChatViewModel(app: Application, val tab: String) : AndroidViewModel(app) {

    private val api = ChatApi(app)
    private val appCtx: Application = app

    private val _state = mutableStateOf(
        ChatUiState(
            provider = api.providerFor(tab),
            model = api.modelFor(tab),
            path = api.pathFor(tab),
        )
    )
    val state: State<ChatUiState> = _state

    private var streamJob: Job? = null
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
        }
        checkReachability()
    }

    fun refreshConfig() {
        _state.value = _state.value.copy(
            provider = api.providerFor(tab),
            model = api.modelFor(tab),
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
        _state.value = _state.value.copy(
            messages = ChatThreads.toUi(target.messages),
            threads = summaries(),
            threadId = id,
            error = "",
            pending = "",
            activeTools = emptyList(),
            activeToolLabel = "",
        )
    }

    /** + starts a new conversation; threads with chats are kept, empty
     * drafts are dropped so they never pile up in history. */
    fun newConversation() {
        if (!_state.value.ready) return
        streamJob?.cancel()
        streamJob = null
        val fresh = ChatThread(id = ChatThreads.newId(), updatedAt = ChatThreads.now())
        threads = listOf(fresh) + threads.filter { it.messages.isNotEmpty() }
        threadId = fresh.id
        _state.value = _state.value.copy(
            messages = emptyList(), error = "", streaming = false, pending = "",
            threads = summaries(), threadId = threadId,
            activeTools = emptyList(), activeToolLabel = "",
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
            _state.value = _state.value.copy(
                messages = ChatThreads.toUi(next.messages),
                threadId = next.id, error = "", streaming = false, pending = "",
            )
        }
        _state.value = _state.value.copy(threads = summaries())
        persist()
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
                val used = listOf(
                    m.tools.takeIf { it.isNotEmpty() }?.let { "tools: ${it.joinToString(", ")}" },
                    m.skills.takeIf { it.isNotEmpty() }?.let { "skills: ${it.joinToString(", ")}" },
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

    /** Adds character counts for the Usage screen (estimates, local only). */
    private fun addUsage(sent: Int = 0, recv: Int = 0) {
        if (sent <= 0 && recv <= 0) return
        viewModelScope.launch(Dispatchers.IO) {
            runCatching {
                val prefs = appCtx.getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
                prefs.edit()
                    .putLong("usage_sent_$tab", prefs.getLong("usage_sent_$tab", 0L) + sent)
                    .putLong("usage_recv_$tab", prefs.getLong("usage_recv_$tab", 0L) + recv)
                    .apply()
            }
        }
    }

    fun stop() {
        streamJob?.cancel()
        streamJob = null
        _state.value = _state.value.copy(
            streaming = false, activeTools = emptyList(), activeToolLabel = ""
        )
    }

    /** Resends the current history (used after a failed request). */
    fun retry() {
        if (_state.value.streaming || !_state.value.ready) return
        val history = _state.value.messages.filter { it.content.isNotBlank() }
        if (history.none { it.role == "user" }) return
        doSend(history)
    }

    fun send() {
        val text = _state.value.pending.trim()
        if (text.isEmpty() || _state.value.streaming || !_state.value.ready) return
        // Re-read per-tab config at send time so Settings edits apply instantly.
        val provider = api.providerFor(tab)
        val model = api.modelFor(tab)
        val path = api.pathFor(tab)
        _state.value = _state.value.copy(provider = provider, model = model, path = path)
        val history = _state.value.messages +
            ChatMessage("user", text, ChatThreads.now())
        _state.value = _state.value.copy(messages = history, pending = "", streaming = true, error = "")
        addUsage(sent = text.length)
        doSend(history, path, provider, model)
    }

    private fun doSend(
        history: List<ChatMessage>,
        path: String = api.pathFor(tab),
        provider: String = api.providerFor(tab),
        model: String = api.modelFor(tab),
    ) {
        _state.value = _state.value.copy(
            streaming = true, error = "",
            activeTools = emptyList(), activeToolLabel = "",
        )
        // Placeholder assistant message that deltas append to.
        _state.value = _state.value.copy(messages = history + ChatMessage("assistant", "", ChatThreads.now()))
        val acc = StringBuilder()
        // Fresh id per send: pins this turn to one server session for the
        // post-turn usage fetch, without changing the stateless agent loop.
        val runSessionId = ChatThreads.newId()
        val liveTools = mutableListOf<String>()
        streamJob?.cancel()
        streamJob = viewModelScope.launch {
            api.streamChat(path, provider, model, history, runSessionId).collect { event ->
                when (event) {
                    is ChatEvent.ToolProgress -> {
                        val name = event.tool.trim()
                        if (name.isNotEmpty() && name !in liveTools) liveTools.add(name)
                        _state.value = _state.value.copy(
                            activeTools = liveTools.toList(),
                            activeToolLabel = event.label,
                        )
                    }
                    is ChatEvent.Delta -> {
                        acc.append(event.text)
                        val msgs = _state.value.messages
                        _state.value = _state.value.copy(
                            messages = msgs.dropLast(1) + ChatMessage("assistant", acc.toString(), ChatThreads.now()),
                        )
                    }
                    is ChatEvent.Done -> {
                        val final = event.fullText.ifEmpty { acc.toString() }
                        val finishedAt = ChatThreads.now()
                        val finished = msgsDropLastPlusAssistant(final, finishedAt, liveTools.toList())
                        _state.value = _state.value.copy(
                            messages = finished,
                            streaming = false,
                            online = true,
                            activeTools = emptyList(),
                            activeToolLabel = "",
                        )
                        addUsage(recv = final.length)
                        upsertActive(finished)
                        persist()
                        backfillUsage(path, runSessionId, finishedAt, final)
                        // #58: ping the user when a reply lands while the app
                        // is backgrounded (gateway has no cronjobs to report).
                        if (!ForegroundTracker.isForeground) {
                            val snippet = final.ifEmpty { acc.toString() }.trim()
                                .replace(Regex("\\s+"), " ").take(160)
                            ChatNotifications.notifyDone(appCtx, tabTitle(tab), snippet)
                        }
                    }
                    is ChatEvent.Error -> {
                        // Drop the empty placeholder on failure.
                        val msgs = _state.value.messages.dropLast(1)
                        _state.value = _state.value.copy(
                            messages = msgs, streaming = false, error = event.message,
                            activeTools = emptyList(), activeToolLabel = "",
                        )
                        persist()
                        checkReachability()
                    }
                }
            }
        }
    }

    /** Finished assistant message with the live-seen tools attached. */
    private fun msgsDropLastPlusAssistant(
        final: String,
        finishedAt: Long,
        tools: List<String>,
    ): List<ChatMessage> {
        val msgs = _state.value.messages
        return msgs.dropLast(1) + ChatMessage(
            "assistant",
            final.ifEmpty { "The assistant sent an empty reply. Try asking again." },
            finishedAt,
            tools = tools.distinct().take(20),
        )
    }

    /** Post-turn usage fetch: merges server-recorded tools + skills into the
     * finished message so they persist and render as chips. Silent on
     * failure — the live tools attached at Done stay. The patch applies to
     * the fresh state (matched by timestamp) so a send made mid-fetch is
     * never clobbered; a thread switch simply finds no match. */
    private fun backfillUsage(path: String, sessionId: String, finishedAt: Long, final: String) {
        viewModelScope.launch(Dispatchers.IO) {
            val usage = runCatching {
                ServerApi(appCtx).fetchSessionUsage(path, sessionId).getOrThrow()
            }.getOrNull() ?: return@launch
            if (usage.tools.isEmpty() && usage.skills.isEmpty()) return@launch
            val expected = final.ifEmpty { "The assistant sent an empty reply. Try asking again." }
            withContext(Dispatchers.Main) {
                val fresh = _state.value.messages.toMutableList()
                val idx = fresh.indexOfLast {
                    it.role == "assistant" && it.ts == finishedAt && it.content == expected
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
}
