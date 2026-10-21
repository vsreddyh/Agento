package com.vishnu.agento

import android.content.Context
import java.io.File
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.serialization.Serializable
import kotlinx.serialization.builtins.ListSerializer
import kotlinx.serialization.json.Json

/** One persisted message (ChatMessage + timestamp; ts=0 reads as unknown).
 * Optional fields default empty/zero so older history reads as unknown
 * rather than as recorded data. */
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
    // deserialises — an unreadable thread would be worse than a missing cause.
    // Unknown or absent values fall back to `user`, correct for every pre-#214 record.
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

/** Whitespace splitter for titles, hoisted: compiled once, not per title
 * render and switcher-search filter pass. */
private val titleWhitespace = Regex("\\s+")

/** Grapheme walker for title cuts, thread-local: BreakIterator is stateful and
 * not thread-safe. ROOT locale pins deterministic cluster rules across devices;
 * each use releases its input via setText(""). */
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

    /** Title from the first user message carrying text, truncated; falls back to
     * the photo name when no user turn has words, to the default when the thread
     * has no user turns at all.
     * Opener-by-design: the first text turn titles the thread even when a later
     * turn is more substantive ("ok" then an essay still titles "ok") — search and
     * export carry markers, so the thread stays findable.
     * The photo signal is STAGED-honest, not wire-strict: the title counts wired
     * photos (cheap, per render), the wire counts wired AND decodable ones;
     * load() blanks ragged payloads on read, so the two agree in practice.
     * Cluster rules are ROOT-pinned, not locale-following: deterministic across
     * devices and tests, at the cost of Thai/Lao cluster subtleties. */
    fun titleFor(messages: List<ChatMessage>): String {
        // Single pass: first text turn + largest photo count in one walk (it
        // runs per title render and per switcher-search filter pass). Strict
        // title, lenient search: a hand-edited "User" turn contributes no
        // title words and no photo count, while search still finds it.
        var textMsg: ChatMessage? = null
        var maxPhotos = 0
        for (m in messages) {
            if (m.role != "user") continue
            maxPhotos = maxOf(maxPhotos, m.attachments.wiredPhotoCount())
            if (textMsg == null && m.content.isNotBlank()) textMsg = m
        }
        if (textMsg == null) {
            // An image-only thread has no words: name the LARGEST photo turn,
            // like the suffix below. Counted wired (blank-only, not decodable)
            // — staged-honest, as the KDoc explains.
            if (maxPhotos > 0) return if (maxPhotos == 1) "Photo" else "$maxPhotos photos"
            return "New conversation"
        }
        val first = textMsg.content.trim()
        // Suffix from the LARGEST photo turn, not the first — the signal
        // should say so. Counted wired so blanks never title a thread.
        val photos = maxPhotos
        val oneLine = first.replace(titleWhitespace, " ")
        // The marker reserves its width first, so the row never exceeds 42
        // chars; avail < 2 yields the bare suffix (one grapheme plus "…"
        // needs two cells) — the photo signal is the useful half there.
        // The fit check counts UTF-16 CODE UNITS while the cut walks
        // graphemes: a 30-emoji title (60 units) reads as over-budget and
        // truncates, though it fits in 30 graphemes. Deliberately
        // conservative — the budget still holds, and counting graphemes per
        // title render is not worth the cost.
        val suffix = if (photos == 0) "" else if (photos == 1) " [photo]" else " [$photos photos]"
        val avail = maxOf(0, 42 - suffix.length)
        val base = if (oneLine.length <= avail) {
            oneLine
        } else if (avail >= 2) {
            // Grapheme-safe cut: the last boundary within budget, so ZWJ
            // sequences, flag pairs and surrogate pairs never split
            // mid-cluster. The process-sticky walker releases input below.
            val breaks = titleBreaks.get()
            var end = 0
            try {
                breaks.setText(oneLine)
                var n = breaks.first()
                while (n != java.text.BreakIterator.DONE && n <= avail - 1) {
                    end = n
                    n = breaks.next()
                }
            } finally {
                // Drop the retained title string now, not on the next render
                // — and on a throw too, or the title stays pinned in the
                // process-sticky ThreadLocal.
                breaks.setText("")
            }
            // A cluster wider than the budget leaves end == 0: fall back
            // like avail < 2 — only a no-suffix title keeps the ellipsis.
            if (end > 0) {
                oneLine.substring(0, end) + "…"
            } else if (suffix.isEmpty()) {
                "…"
            } else {
                ""
            }
        } else {
            ""
        }
        return base + suffix
    }

    @androidx.annotation.WorkerThread
    fun load(context: Context, tab: String): List<ChatThread> {
        requireIoThread("ChatThreads.load")
        val f = file(context, tab)
        if (!f.exists()) return emptyList()
        // The scans (whitespace strip + alphabet validation over photo
        // payloads) must never run on Main: every caller — VM, Settings
        // export/import — is inside Dispatchers.IO.
        return runCatching {
            threadJson.decodeFromString(ListSerializer(ChatThread.serializer()), f.readText(Charsets.UTF_8))
                .filter { it.id.isNotEmpty() }
                .take(MAX_THREADS)
                // Same message cap as save() (#339): a file with 1000
                // messages would load whole and then be silently trimmed to
                // 200 by the next save — the trim must happen at the
                // boundary that reads, or the loss is invisible.
                // Canonicalized on load: storage is hand-editable, so memory
                // holds no wrapped-or-ragged payloads — that is what lets the
                // hot-path isWired() stay a blank check while the wire filter
                // asserts decode-validity. The next persist writes it back.
                .map { t ->
                    val capped = if (t.messages.size > MAX_MESSAGES) {
                        t.copy(messages = t.messages.takeLast(MAX_MESSAGES))
                    } else {
                        t
                    }
                    capped.copy(messages = capped.messages.map { m ->
                        m.copy(attachments = canonicalizeAttachments(m.attachments))
                    })
                }
        }.getOrDefault(emptyList())
    }

    /**
     * Persists threads, canonicalizing attachments at the boundary: load()
     * canonicalizes and export()/import canonicalize explicitly, so save()
     * cannot trust every caller to hold canonical memory. The re-walk is the
     * price; a version-stamp flag would skip it, but that is a storage-format
     * change and foreign-written files must validate anyway.
     * Dirty tracking is by reference (see canonicalizeUnchanged); persists
     * happen at turn boundaries, never the per-token streaming loop.
     */
    @androidx.annotation.WorkerThread
    fun save(context: Context, tab: String, threads: List<ChatThread>) {
        requireIoThread("ChatThreads.save")
        runCatching {
            val capped = threads.take(MAX_THREADS).map { t ->
                if (t.messages.size > MAX_MESSAGES) {
                    t.copy(messages = t.messages.takeLast(MAX_MESSAGES))
                } else {
                    t
                }
            }
            // Canonicalize only what changed since the last save: same
            // instance means same immutable content, already proven canonical
            // by the save that cached it. No refuse/trim gate here, by design
            // (unlike export): refusing would persist load()'s emptyList()
            // over the good file, and trimming deletes user photos.
            val cached = lastCanonical[tab]
            val out = canonicalizeUnchanged(cached?.first, cached?.second, capped)
            file(context, tab).writeText(threadJson.encodeToString(ListSerializer(ChatThread.serializer()), out), Charsets.UTF_8)
            lastCanonical[tab] = capped to out
        }
    }

    /** Drops the dirty-tracking snapshot for [tab]: an out-of-band writer (the
     * backup import writes the thread file directly, bypassing [save]) must
     * invalidate it, or the next save restores the pre-import bytes. */
    internal fun invalidate(tab: String) {
        lastCanonical.remove(tab)
    }

    /** Tabs whose thread file was rewritten out-of-band, with a per-tab
     * counter (#339): the backup import writes the file directly, so a live
     * ViewModel still holds the pre-import threads and its next persist
     * would write them back over the restore. A counter per tab (not a
     * global flag) so a rewrite of one tab never disturbs another, and
     * replayed through a StateFlow so a ViewModel that starts after the
     * rewrite still sees the newest count. */
    private val rewriteCounters = kotlinx.coroutines.flow.MutableStateFlow<Map<String, Long>>(emptyMap())

    /** The rewrite counters, for ViewModels to watch. */
    val rewrites: kotlinx.coroutines.flow.StateFlow<Map<String, Long>> = rewriteCounters.asStateFlow()

    /** Announces that [tab]'s thread file was rewritten out-of-band. */
    internal fun notifyRewritten(tab: String) {
        rewriteCounters.update { current ->
            current + (tab to ((current[tab] ?: 0L) + 1L))
        }
    }

    /** Last save's (input, canonicalized-output) snapshot per tab: the
     * dirty-tracking cache behind [save]. The CURRENT list is compared
     * element-wise against the last INPUT, never the last output (the VM keeps
     * its original instances, so canonicalized copies would never match). It
     * pins only SHELLS (#339): the payload strings are the caller's own
     * immutable instances, shared by reference — the cache never duplicates
     * photo bytes — and the entry is replaced wholesale by the next save of
     * that tab, so an evicted thread's payloads are released then. */
    private val lastCanonical: MutableMap<String, Pair<List<ChatThread>, List<ChatThread>>> =
        java.util.concurrent.ConcurrentHashMap()

    /** Canonicalizes only threads that changed since the last save: unchanged
     * threads reuse the cached output by reference; new or copied threads
     * canonicalize fully. */
    internal fun canonicalizeUnchanged(
        prevIn: List<ChatThread>?,
        prevOut: List<ChatThread>?,
        current: List<ChatThread>,
    ): List<ChatThread> {
        if (prevIn == null || prevOut == null) return current.map(::canonicalizeThread)
        // By ID, not by position (#339): a new thread is PREPENDED, so a
        // positional compare shifts every index and the next save
        // re-canonicalizes the whole list (a full payload walk per thread).
        // The id map keeps the hits; the identity check (same instance)
        // still decides whether the content changed.
        val prevById = HashMap<String, Pair<ChatThread, ChatThread>>(prevIn.size)
        prevIn.forEachIndexed { i, t -> prevById[t.id] = t to prevOut[i] }
        return current.map { t ->
            val entry = prevById[t.id]
            if (entry != null && entry.first === t) entry.second else canonicalizeThread(t)
        }
    }

    /** Canonicalizes one thread's attachments, skipping text-only messages
     * without copying them: empty attachments have nothing to prove. */
    private fun canonicalizeThread(t: ChatThread): ChatThread =
        t.copy(
            messages = t.messages.map { m ->
                if (m.attachments.isEmpty()) m
                else m.copy(attachments = canonicalizeAttachments(m.attachments))
            },
        )

    /** Canonical form of a raw thread file for backup import: the import
     * boundary enforces the same contract as load(), so a hand-edited backup
     * can't persist ragged payloads. Mirrors load()'s row contract (empty-id
     * threads drop, the file caps at MAX_THREADS). Null when the text isn't a
     * threads list — the caller keeps its skip-invalid behavior. */
    @androidx.annotation.WorkerThread
    fun canonicalizeFile(text: String): String? {
        requireIoThread("ChatThreads.canonicalizeFile")
        return runCatching {
            val threads = threadJson.decodeFromString(ListSerializer(ChatThread.serializer()), text)
                .filter { it.id.isNotEmpty() }
                .take(MAX_THREADS)
            threadJson.encodeToString(
                ListSerializer(ChatThread.serializer()),
                threads.map { t ->
                    // Same message cap as load()/save() (#339): an imported
                    // file must not carry messages the next save would
                    // silently drop.
                    val capped = if (t.messages.size > MAX_MESSAGES) {
                        t.copy(messages = t.messages.takeLast(MAX_MESSAGES))
                    } else {
                        t
                    }
                    capped.copy(messages = capped.messages.map { m ->
                        m.copy(attachments = canonicalizeAttachments(m.attachments))
                    })
                },
            )
        }.getOrNull()
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
                // Already canonical: load() normalized every payload, so
                // no per-message work happens here on the Main thread.
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

    /** UI memory → storage shape, intentionally RAW: no canonicalization walk
     * here (this runs on Main). The canonicalizing boundary is
     * save()/load()/export()/import (IO-gated): a ragged payload heals on the
     * next load, and the wire filter fails closed meanwhile. */
    fun toStored(messages: List<ChatMessage>): List<StoredMessage> =        messages.map {
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
