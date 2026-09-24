package com.vishnu.agento

import android.app.Application
import androidx.compose.runtime.State
import androidx.compose.runtime.mutableStateOf
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.launch

data class ChatUiState(
    val provider: String = "",
    val model: String = "",
    val path: String = "",
    val messages: List<ChatMessage> = emptyList(),
    val streaming: Boolean = false,
    val pending: String = "",
    val error: String = "",
    /** True once the persisted conversation finishes loading; sends wait for it. */
    val ready: Boolean = false,
)

/**
 * One instance per chat tab (story / resumes / god). A single conversation
 * per tab persists to disk (`ChatThreads`) so history survives restarts;
 * the + button clears it and starts fresh (#51: no thread switcher).
 * The full active history is sent with each request.
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

    init {
        viewModelScope.launch(Dispatchers.IO) {
            // Legacy files may hold several threads (pre-#51 switcher);
            // keep the newest non-empty one and collapse to a single thread.
            val loaded = ChatThreads.load(appCtx, tab)
            val kept = loaded.firstOrNull { it.messages.isNotEmpty() }
                ?: loaded.firstOrNull()
                ?: ChatThread(id = ChatThreads.newId(), updatedAt = ChatThreads.now())
            threadId = kept.id.ifEmpty { ChatThreads.newId() }
            val single = listOf(kept.copy(id = threadId))
            ChatThreads.save(appCtx, tab, single)
            _state.value = _state.value.copy(
                messages = ChatThreads.toUi(kept.messages),
                ready = true,
            )
        }
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

    private fun persist() {
        val id = threadId.ifEmpty { return }
        val msgs = _state.value.messages
        val snapshot = listOf(
            ChatThread(
                id = id,
                title = ChatThreads.titleFor(msgs),
                updatedAt = ChatThreads.now(),
                messages = ChatThreads.toStored(msgs),
            )
        )
        viewModelScope.launch(Dispatchers.IO) {
            ChatThreads.save(appCtx, tab, snapshot)
        }
    }

    /** + clears the single conversation and starts fresh (#51). */
    fun newConversation() {
        streamJob?.cancel()
        streamJob = null
        threadId = ChatThreads.newId()
        _state.value = _state.value.copy(
            messages = emptyList(), error = "", streaming = false, pending = "",
        )
        persist()
    }

    fun stop() {
        streamJob?.cancel()
        streamJob = null
        _state.value = _state.value.copy(streaming = false)
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
        doSend(history, path, provider, model)
    }

    private fun doSend(
        history: List<ChatMessage>,
        path: String = api.pathFor(tab),
        provider: String = api.providerFor(tab),
        model: String = api.modelFor(tab),
    ) {
        _state.value = _state.value.copy(streaming = true, error = "")
        // Placeholder assistant message that deltas append to.
        _state.value = _state.value.copy(messages = history + ChatMessage("assistant", "", ChatThreads.now()))
        val acc = StringBuilder()
        streamJob?.cancel()
        streamJob = viewModelScope.launch {
            api.streamChat(path, provider, model, history).collect { event ->
                when (event) {
                    is ChatEvent.Delta -> {
                        acc.append(event.text)
                        val msgs = _state.value.messages
                        _state.value = _state.value.copy(
                            messages = msgs.dropLast(1) + ChatMessage("assistant", acc.toString(), ChatThreads.now()),
                        )
                    }
                    is ChatEvent.Done -> {
                        val final = event.fullText.ifEmpty { acc.toString() }
                        val msgs = _state.value.messages
                        _state.value = _state.value.copy(
                            messages = msgs.dropLast(1) + ChatMessage("assistant", final.ifEmpty { "The assistant sent an empty reply. Try asking again." }, ChatThreads.now()),
                            streaming = false,
                        )
                        persist()
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
                        _state.value = _state.value.copy(messages = msgs, streaming = false, error = event.message)
                        persist()
                    }
                }
            }
        }
    }
}
