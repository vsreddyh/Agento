package com.vishnu.agento

import android.content.Context
import java.io.File
import kotlinx.serialization.Serializable
import kotlinx.serialization.builtins.ListSerializer
import kotlinx.serialization.json.Json

/** One persisted message (ChatMessage + timestamp; ts=0 reads as unknown).
 * tools/skills default empty so pre-visibility history reads as unknown;
 * token fields default zero so pre-usage history reads as unreported;
 * attachments default empty so pre-#318 history reads as text-only. */
@Serializable
data class StoredMessage(
    val role: String = "",
    val content: String = "",
    val ts: Long = 0L,
    val attachments: List<ChatAttachment> = emptyList(),
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

/** Whitespace splitter for titles, hoisted (#339): compiled once, not on
 * every title render and switcher-search filter pass. */
private val titleWhitespace = Regex("\\s+")

/** Grapheme walker for title cuts, thread-local (#339): BreakIterator is
 * stateful and not thread-safe, and constructing one per title render
 * (plus per switcher-search filter pass over 200-msg threads) is churn.
 * ROOT locale pins deterministic cluster rules across locales (Thai/Lao
 * boundaries are locale-sensitive). */
private val titleBreaks: ThreadLocal<java.text.BreakIterator> =
    ThreadLocal.withInitial {
        java.text.BreakIterator.getCharacterInstance(java.util.Locale.ROOT)
    }

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

    /** Title from the first user message carrying text, truncated; falls
     * back to the photo name when no user turn has words, to date when the
     * thread has no user turns at all. Opener-by-design (#339): the first
     * text turn titles even when a later turn is more substantive ("ok"
     * then an essay still titles "ok") — search/export carry markers so
     * the thread stays findable, and longest-text heuristics guess wrong
     * as often as they help. */
    fun titleFor(messages: List<ChatMessage>): String {
        // Single pass (#339): first text turn + largest photo count in one
        // walk — this runs per title render and per switcher-search filter
        // pass over up to 200-message threads, so two full walks per call
        // is churn worth avoiding.
        var textMsg: ChatMessage? = null
        var maxPhotos = 0
        for (m in messages) {
            if (m.role != "user") continue
            maxPhotos = maxOf(maxPhotos, m.attachments.wiredPhotoCount())
            if (textMsg == null && m.content.isNotBlank()) textMsg = m
        }
        if (textMsg == null) {
            // An image-only thread has no words to title from: name the
            // LARGEST photo turn (#339), like the suffix below — a 1-photo
            // opener followed by a 2-photo turn holds a 2-photo turn.
            // Counted wired like every other signal.
            if (maxPhotos > 0) return if (maxPhotos == 1) "Photo" else "$maxPhotos photos"
            return "New conversation"
        }
        val first = textMsg.content.trim()
        // Suffix from the LARGEST photo turn (#339), not the first: a
        // 1-photo turn followed by a 2-photo turn holds a 2-photo turn,
        // and the signal should say so. Counted wired so blanks never
        // title a thread.
        val photos = maxPhotos
        val oneLine = first.replace(titleWhitespace, " ")
        // Photo signal with the budget kept (#339): the marker reserves
        // its width first, so the row never exceeds 42 chars. avail == 0
        // yields the bare suffix (unreachable today — suffixes are short —
        // but the budget math must hold for all inputs, not just short ones).
        val suffix = if (photos == 0) "" else if (photos == 1) " [photo]" else " [$photos photos]"
        val avail = maxOf(0, 42 - suffix.length)
        val base = if (oneLine.length <= avail) {
            oneLine
        } else if (avail >= 1) {
            // Grapheme-safe cut via the shared walker (#339): the last
            // boundary within budget, so ZWJ sequences, flag pairs, and
            // surrogate pairs never split mid-cluster in the switcher row.
            val breaks = titleBreaks.get() ?: java.text.BreakIterator.getCharacterInstance()
            breaks.setText(oneLine)
            var end = 0
            var n = breaks.first()
            while (n != java.text.BreakIterator.DONE && n <= avail - 1) {
                end = n
                n = breaks.next()
            }
            oneLine.substring(0, end) + "…"
        } else {
            ""
        }
        return base + suffix
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
            val by = interruptedBy(it.interruptedBy)
            ChatMessage(
                role = it.role,
                content = it.content,
                ts = it.ts,
                attachments = it.attachments,
                tools = it.tools,
                skills = it.skills,
                prompt = it.prompt,
                completion = it.completion,
                total = it.total,
                cached = it.cached,
                unreported = it.unreported,
                model = it.model,
                reasoning = it.reasoning,
                // Normalised on load, not trusted as stored: the two fields can disagree
                // (a hand-edited backup, or a bug in an older writer) and `interrupted`
                // alone decides whether the UI offers "Continue". A `DROP` record stored
                // with `interrupted=false` would render as a finished reply — the exact
                // thing #214 exists to prevent — so the marker that says *why* wins.
                // `interruptedBy` stays exactly as stored; it is only ever display detail.
                interrupted = it.interrupted || by == InterruptedBy.DROP,
                interruptedBy = by,
            )
        }

    fun toStored(messages: List<ChatMessage>): List<StoredMessage> =
        messages.map {
            StoredMessage(
                role = it.role,
                content = it.content,
                ts = it.ts,
                attachments = it.attachments,
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

/**
 * Marks a message as cut off by a dead connection (#214).
 *
 * An `Error` carrying partial text/reasoning is a turn that ran and was cut
 * off, not a clean failure — without the flag it renders as a finished reply
 * and offers retry instead of Continue, which is the same misreport as the
 * dropped-stream case one layer down. Everything else is preserved: the
 * fragment, its tools, and `unreported` all still describe what happened.
 */
internal fun ChatMessage.asInterruptedDrop(): ChatMessage =
    copy(interrupted = true, interruptedBy = InterruptedBy.DROP)

/**
 * Strips a trailing fragment before a resend (#214).
 *
 * `retry()` used to resend the interrupted fragment as part of history, so a
 * resend replayed its own truncated reply as context — against a server-side
 * turn that may still be running, that is a second execution primed with its
 * own partial output. Like `regenerate`, a resend goes out without the
 * trailing assistant message.
 *
 * Stripped on `interrupted` ONLY, never on `unreported`: a clean reply whose
 * stream carried no `usage` is still a real answer, and dropping it would
 * delete work the user already has. Cut-off error fragments are covered
 * because the Error path flags them via [asInterruptedDrop].
 */
internal fun stripTrailingFragmentForResend(messages: List<ChatMessage>): List<ChatMessage> {
    val last = messages.lastOrNull()
    if (last?.role == "assistant" && last.interrupted) {
        return messages.dropLast(1)
    }
    return messages
}

/**
 * Whether recovered server-side text should replace the local fragment
 * (#214). The orphaned turn keeps running after a drop, so a later poll of
 * the session transcript can hold MORE of the reply than the stream
 * delivered. Adopt only a strictly longer, non-blank text: equal-or-shorter
 * means the server has nothing the client does not, and adopting it would
 * throw away the stream's fragment for no gain.
 */
internal fun recoveryAdoptable(fragment: String, recovered: String?): Boolean =
    !recovered.isNullOrBlank() && recovered.length > fragment.length

/**
 * Whether the recovered text has settled (#214). Two polls that return the
 * same text mean the orphaned turn stopped producing — the only completion
 * signal available without a server resume primitive — so the flag can be
 * cleared and the turn reported as finished. Still-growing text stays
 * flagged: it is a longer fragment, not a completed reply.
 */
internal fun recoveryTextSettled(first: String?, second: String?): Boolean =
    first != null && first == second
