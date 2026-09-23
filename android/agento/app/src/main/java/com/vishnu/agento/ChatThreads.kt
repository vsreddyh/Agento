package com.vishnu.agento

import android.content.Context
import java.io.File
import kotlinx.serialization.Serializable
import kotlinx.serialization.builtins.ListSerializer
import kotlinx.serialization.json.Json

/** One persisted message (ChatMessage + timestamp; ts=0 reads as unknown). */
@Serializable
data class StoredMessage(
    val role: String = "",
    val content: String = "",
    val ts: Long = 0L,
)

/** One conversation thread inside a tab. */
@Serializable
data class ChatThread(
    val id: String = "",
    val title: String = "New conversation",
    val updatedAt: Long = 0L,
    val messages: List<StoredMessage> = emptyList(),
)

private val threadJson = Json { ignoreUnknownKeys = true }

/**
 * Per-tab conversation persisted as JSON (`#17` history survival,
 * `#26` storage). One conversation per tab survives app restarts; +
 * clears it and starts fresh (#51). Caps: 20 threads/tab file rows,
 * 200 messages/thread (oldest trimmed) for legacy multi-thread files.
 */
object ChatThreads {

    const val MAX_THREADS = 20
    const val MAX_MESSAGES = 200

    private fun file(context: Context, tab: String): File =
        File(context.filesDir, "chat_threads_$tab.json")

    fun now(): Long = System.currentTimeMillis()

    fun newId(): String = java.util.UUID.randomUUID().toString()

    /** Title from the first user message, truncated; falls back to date. */
    fun titleFor(messages: List<ChatMessage>): String {
        val first = messages.firstOrNull { it.role == "user" }?.content?.trim().orEmpty()
        if (first.isEmpty()) return "New conversation"
        val oneLine = first.replace(Regex("\\s+"), " ")
        return if (oneLine.length <= 42) oneLine else oneLine.take(41) + "…"
    }

    fun load(context: Context, tab: String): List<ChatThread> {
        val f = file(context, tab)
        if (!f.exists()) return emptyList()
        return runCatching {
            threadJson.decodeFromString(ListSerializer(ChatThread.serializer()), f.readText())
                .filter { it.id.isNotEmpty() }
                .take(MAX_THREADS)
        }.getOrDefault(emptyList())
    }

    fun save(context: Context, tab: String, threads: List<ChatThread>) {
        runCatching {
            val capped = threads.take(MAX_THREADS).map { t ->
                if (t.messages.size > MAX_MESSAGES) {
                    t.copy(messages = t.messages.takeLast(MAX_MESSAGES))
                } else {
                    t
                }
            }
            file(context, tab).writeText(threadJson.encodeToString(ListSerializer(ChatThread.serializer()), capped))
        }
    }

    fun toUi(messages: List<StoredMessage>): List<ChatMessage> =
        messages.map { ChatMessage(it.role, it.content, it.ts) }

    fun toStored(messages: List<ChatMessage>): List<StoredMessage> =
        messages.map { StoredMessage(it.role, it.content, it.ts) }
}
