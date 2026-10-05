package com.vishnu.agento

import android.content.Context
import java.io.File
import kotlinx.serialization.Serializable
import kotlinx.serialization.builtins.ListSerializer
import kotlinx.serialization.json.Json

/** One persisted message (ChatMessage + timestamp; ts=0 reads as unknown).
 * tools/skills default empty so pre-visibility history reads as unknown;
 * token fields default zero so pre-usage history reads as unreported. */
@Serializable
data class StoredMessage(
    val role: String = "",
    val content: String = "",
    val ts: Long = 0L,
    val tools: List<String> = emptyList(),
    val skills: List<String> = emptyList(),
    val prompt: Long = 0L,
    val completion: Long = 0L,
    val total: Long = 0L,
    val cached: Long = 0L,
    val unreported: Boolean = false,
    val model: String = "",
    val reasoning: String = "",
    val interrupted: Boolean = false,
    // Stored as a string rather than the enum so a record written before #214 still
    // deserialises — kotlinx.serialization has no default for a missing enum field on a
    // @Serializable class, and an unreadable thread would be worse than a missing cause.
    // Unknown or absent values fall back to `user`, which is correct for every record
    // that predates a dropped stream.
    val interruptedBy: String = "user",
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

    /**
     * Stored string to enum, defensively.
     *
     * The stored value is a free string because a record written before #214 has no such
     * field and a typed enum field would fail to deserialise the whole thread — which is
     * a far worse outcome than a missing cause. The cost is that a value this build does
     * not recognise is possible, so it resolves to [InterruptedBy.user] rather than
     * throwing: a message with the wrong label is trivial, an unreadable thread is not.
     */
    private fun interruptedBy(raw: String): InterruptedBy =
        runCatching { InterruptedBy.valueOf(raw) }.getOrDefault(InterruptedBy.user)

    fun toUi(messages: List<StoredMessage>): List<ChatMessage> =
        messages.map {
            ChatMessage(
                it.role, it.content, it.ts, it.tools, it.skills,
                it.prompt, it.completion, it.total, it.cached, it.unreported,
                it.model, it.reasoning, it.interrupted, interruptedBy(it.interruptedBy),
            )
        }

    fun toStored(messages: List<ChatMessage>): List<StoredMessage> =
        messages.map {
            StoredMessage(
                it.role, it.content, it.ts, it.tools, it.skills,
                it.prompt, it.completion, it.total, it.cached, it.unreported,
                it.model, it.reasoning, it.interrupted, it.interruptedBy.name,
            )
        }
}
