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
    val interruptedBy: String = InterruptedBy.USER.storageKey,
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
     * not recognise is possible, so it resolves to [InterruptedBy.USER] rather than
     * throwing: a message with the wrong label is trivial, an unreadable thread is not.
     */
    private fun interruptedBy(raw: String): InterruptedBy =
        InterruptedBy.entries.firstOrNull { it.storageKey == raw } ?: InterruptedBy.USER

    // NAMED arguments, deliberately. These were 14 positional values into a 13-field
    // constructor, which compiles fine and then silently misassigns the day a field is
    // inserted or reordered — a swap of two same-typed fields (`it.ts` for `it.tools`)
    // is invisible to the compiler and produces wrong history rather than an error.
    // `interruptedBy` is the 14th field and the newest, so it is exactly the kind of
    // addition that triggers this.
    fun toUi(messages: List<StoredMessage>): List<ChatMessage> =
        messages.map {
            ChatMessage(
                role = it.role,
                content = it.content,
                ts = it.ts,
                tools = it.tools,
                skills = it.skills,
                prompt = it.prompt,
                completion = it.completion,
                total = it.total,
                cached = it.cached,
                unreported = it.unreported,
                model = it.model,
                reasoning = it.reasoning,
                interrupted = it.interrupted,
                interruptedBy = interruptedBy(it.interruptedBy),
            )
        }

    fun toStored(messages: List<ChatMessage>): List<StoredMessage> =
        messages.map {
            StoredMessage(
                role = it.role,
                content = it.content,
                ts = it.ts,
                tools = it.tools,
                skills = it.skills,
                prompt = it.prompt,
                completion = it.completion,
                total = it.total,
                cached = it.cached,
                unreported = it.unreported,
                model = it.model,
                reasoning = it.reasoning,
                interrupted = it.interrupted,
                interruptedBy = it.interruptedBy.storageKey,
            )
        }
}
