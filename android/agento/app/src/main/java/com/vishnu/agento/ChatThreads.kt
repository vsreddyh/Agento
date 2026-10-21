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
     * as often as they help. Photo signal is STAGED-honest, not
     * wire-strict, BY DECISION (#339): the title answers "does this thread
     * hold photos" (wired/blank-only count, hot-path cheap, per render),
     * while the wire answers "what did the agent receive" (wired AND
     * decodable, send-frequency). A ragged payload can title a photo the
     * wire drops — but load() blanks ragged on read, so in practice the two
     * agree; title-time decode scans would tax every render for a case the
     * boundary already heals. */
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
            // Retained input released below (#339): the ThreadLocal lives
            // for the process, so setText holds the ~250-char title until
            // the next title render — never "released with the thread".
            val breaks = titleBreaks.get()
            breaks.setText(oneLine)
            var end = 0
            var n = breaks.first()
            while (n != java.text.BreakIterator.DONE && n <= avail - 1) {
                end = n
                n = breaks.next()
            }
            // Drop the retained title string now, not on the next render.
            breaks.setText("")
            oneLine.substring(0, end) + "…"
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
        // Call-site audit for the IO-and-scan cost below (#339): the scans
        // (whitespace strip + alphabet validation over photo payloads) must
        // never run on Main. VM init + openThread merge call inside
        // Dispatchers.IO; Settings export the same; Settings backup import
        // runs inside withContext(IO) (canonicalizeFile rides along there);
        // TaskInstructReceiver calls on the broadcast thread where file IO
        // already happens (its threads carry no attachments — receiver-built
        // — so the scan is a no-op there). A future Main-thread caller must
        // hop to IO first.
        return runCatching {
            threadJson.decodeFromString(ListSerializer(ChatThread.serializer()), f.readText())
                .filter { it.id.isNotEmpty() }
                .take(MAX_THREADS)
                // Canonicalized on load via canonicalizeAttachments (#339):
                // storage is hand-editable; memory therefore holds no
                // wrapped-or-ragged payloads, which is what lets the
                // hot-path isWired() stay a blank check while the wire
                // filter asserts decode-validity. Runs on IO callers only
                // (never Main); the next persist writes back the canonical
                // form (self-healing, no notice — corrupt bytes are
                // dropped, never sent). In-memory-only by design: no
                // write-back here (load is a pure read), so a crash between
                // load and save — or any reader bypassing load() (raw file
                // share) — still sees the raw bytes. Both re-heal on the
                // next load; the window only matters to readers outside
                // every contract, who get raw bytes either way.
                .map { t ->
                    t.copy(messages = t.messages.map { m ->
                        m.copy(attachments = canonicalizeAttachments(m.attachments))
                    })
                }
        }.getOrDefault(emptyList())
    }

    /**
     * Persists threads, canonicalizing attachments at the boundary (#339):
     * load() canonicalizes, export()/import canonicalize explicitly — save()
     * was the one boundary relying on every caller holding canonical memory
     * (true today: VM memory comes from load(), staged payloads are
     * throw-validated at stage time). A future direct save() caller
     * bypassing the VM would otherwise persist ragged base64 that load
     * heals on every cold start. Re-validation is the price, stated (#339):
     * every load/save re-walks clean payloads to prove them clean — a
     * version-stamp / already-canonical flag would skip it, but that is a
     * storage-format change, and old or foreign-written files must validate
     * anyway, so the flag only pays off after a full rewrite cycle while
     * the trust-no-caller boundary goes soft in the meantime.
     * Send-frequency, IO-confined (enforced below): accepted, not
     * overlooked. Full-file every save, BY DESIGN (#339): dirty-thread-only
     * tracking would need mutable cross-call state (last-saved refs) at a
     * boundary that is deliberately stateless — and immutable copies mean
     * the active thread's ref changes on every streaming update anyway, so
     * the flag would only ever skip inactive threads while adding an
     * invalidation risk no test can see. The re-scan is the price of the
     * trust-no-caller boundary round-77 asked for.
     */
    @androidx.annotation.WorkerThread
    fun save(context: Context, tab: String, threads: List<ChatThread>) {
        requireIoThread("ChatThreads.save")
        runCatching {
            val capped = threads.take(MAX_THREADS).map { t ->
                val msgs = if (t.messages.size > MAX_MESSAGES) {
                    t.messages.takeLast(MAX_MESSAGES)
                } else {
                    t.messages
                }
                t.copy(
                    messages = msgs.map { m ->
                        m.copy(attachments = canonicalizeAttachments(m.attachments))
                    },
                )
            }
            file(context, tab).writeText(threadJson.encodeToString(ListSerializer(ChatThread.serializer()), capped))
        }
    }

    /**
     * Canonical form of a raw thread file for backup import (#339): the
     * import boundary enforces the same contract the load boundary does,
     * so a hand-edited backup can't persist wrapped-or-ragged payloads
     * that load-then-persist would otherwise self-heal a cycle later.
     * Mirrors load()'s row contract too (#339): empty-id threads drop and
     * the file caps at MAX_THREADS — otherwise an oversize/garbage file
     * persists whole until the next save trims it.
     * Returns null when the text isn't a threads list — the caller keeps
     * its skip-invalid behavior. Tasks files never reach here (shape).
     */
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
                    t.copy(messages = t.messages.map { m ->
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
                // Already canonical: load() normalized every payload
                // (whitespace stripped, undecodable blanked), so no
                // per-message work happens here on the Main thread.
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

    /**
     * UI memory → storage shape, intentionally RAW (#339): attachments ride
     * through uncanonicalized — no walks here because this runs on Main
     * (streaming upserts, fork resends, receiver builds) and alphabet scans
     * must never run there. The canonicalizing boundary is
     * save()/load()/export()/import (IO-gated via requireIoThread): a
     * ragged payload reaching disk heals on the next load, and the wire
     * filter fails closed meanwhile. Trusts the same canonical-memory
     * invariant as the field docs; save() re-validates, so the trust is
     * verified at the boundary, not assumed at every conversion.
     */
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
