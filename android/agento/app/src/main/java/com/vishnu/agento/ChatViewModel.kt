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
    /** Threads for the thread switcher (id + title only). */
    val threads: List<Pair<String, String>> = emptyList(),
    val activeThreadId: String = "",
    /** True once persisted threads finish loading; sends wait for it. */
    val ready: Boolean = false,
)

/**
 * One instance per chat tab (story / resumes / god). Threads persist to disk
 * (`ChatThreads`) so history survives restarts and New never wipes (#17/#18);
 * the full active history is sent with each request.
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
    private var threads: List<ChatThread> = emptyList()

    init {
        viewModelScope.launch(Dispatchers.IO) {
            val loaded = ChatThreads.load(appCtx, tab)
            val list = if (loaded.isEmpty()) {
                listOf(ChatThread(id = ChatThreads.newId(), updatedAt = ChatThreads.now()))
                    .also { ChatThreads.save(appCtx, tab, it) }
            } else {
                loaded
            }
            threads = list
            val active = list.first()
            _state.value = _state.value.copy(
                messages = ChatThreads.toUi(active.messages),
                threads = list.map { it.id to it.title },
                activeThreadId = active.id,
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
        val id = _state.value.activeThreadId
        val msgs = _state.value.messages
        threads = threads.map { t ->
            if (t.id == id) {
                t.copy(
                    messages = ChatThreads.toStored(msgs),
                    updatedAt = ChatThreads.now(),
                    title = ChatThreads.titleFor(msgs),
                )
            } else {
                t
            }
        }.take(ChatThreads.MAX_THREADS)
        val snapshot = threads
        viewModelScope.launch(Dispatchers.IO) {
            ChatThreads.save(appCtx, tab, snapshot)
        }
        _state.value = _state.value.copy(threads = snapshot.map { it.id to it.title })
    }

    /** New starts a fresh thread; old threads stay in the switcher. */
    fun newConversation() {
        streamJob?.cancel()
        streamJob = null
        persist()
        val fresh = ChatThread(id = ChatThreads.newId(), updatedAt = ChatThreads.now())
        threads = listOf(fresh) + threads
        persist()
        _state.value = _state.value.copy(
            messages = emptyList(), error = "", streaming = false, pending = "",
            threads = threads.map { it.id to it.title }, activeThreadId = fresh.id,
        )
    }

    fun switchThread(id: String) {
        if (id == _state.value.activeThreadId || _state.value.streaming) return
        persist()
        val target = threads.firstOrNull { it.id == id } ?: return
        _state.value = _state.value.copy(
            messages = ChatThreads.toUi(target.messages),
            error = "", pending = "", activeThreadId = id,
        )
    }

    fun deleteThread(id: String) {
        if (_state.value.streaming) return
        threads = threads.filterNot { it.id == id }
        if (threads.isEmpty()) {
            threads = listOf(ChatThread(id = ChatThreads.newId(), updatedAt = ChatThreads.now()))
        }
        val active = threads.firstOrNull { it.id == _state.value.activeThreadId } ?: threads.first()
        _state.value = _state.value.copy(
            messages = ChatThreads.toUi(active.messages),
            error = "", activeThreadId = active.id,
            threads = threads.map { it.id to it.title },
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
                            messages = msgs.dropLast(1) + ChatMessage("assistant", final.ifEmpty { "(empty reply)" }, ChatThreads.now()),
                            streaming = false,
                        )
                        persist()
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
