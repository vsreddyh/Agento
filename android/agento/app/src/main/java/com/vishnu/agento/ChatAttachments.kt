package com.vishnu.agento

import android.content.Context
import android.graphics.Bitmap
import android.graphics.BitmapFactory
import android.graphics.Matrix
import android.media.ExifInterface
import android.net.Uri
import android.util.Base64
import android.util.Log
import java.io.ByteArrayOutputStream
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ensureActive
import kotlinx.coroutines.withContext
import kotlinx.coroutines.yield
import kotlinx.serialization.Serializable
import org.json.JSONArray
import org.json.JSONObject

/**
 * One photo attached to a user turn (#318).
 *
 * Photos are downscaled and re-encoded to JPEG at pick time
 * ([prepareAttachment]), so what is stored here is already bounded:
 * [MAX_IMAGE_SIDE_PX] on the long side, base64 under
 * [MAX_IMAGE_BASE64_CHARS]. The thread file and the Settings backup hold
 * these payloads verbatim (see `docs/adr/0007-chat-attachments.md` for
 * what that costs), which is what lets a retry or an app-killed-turn
 * resume resend the exact turn the user composed.
 */
@Serializable
data class ChatAttachment(
    val mimeType: String = "image/jpeg",
    /** Raw bytes as base64, no `data:` prefix — [dataUri] adds it for the wire. */
    val base64: String = "",
    /** Stable per-pick id, minted at stage time (#339): thumbnail keys and
     * any future per-photo identity use this, never the payload — so
     * removing photo 0 doesn't re-key photo 1, and two identical picks
     * never alias. Default empty so pre-id history still decodes (those
     * fall back to a content-sample key). */
    val id: String = "",
)

/**
 * Attachments that actually wire (#339): blank payloads (hand-edited
 * threads, writer bugs) count as no attachment everywhere — the wire
 * filter, display markers, titles, and search must all agree, or a blank
 * wires as bare text while the UI claims a photo. Load-bearing assumption,
 * stated once: every persisted read flows through load(), which
 * canonicalizes (whitespace stripped, undecodable blanked) — so by the
 * time anything calls this, wrapped-or-ragged payloads are already gone.
 * Any future reader bypassing load() (backup import, debug share) must
 * canonicalize first or re-split the contract.
 */
fun List<ChatAttachment>.wiredPhotos(): List<ChatAttachment> =
    filter { it.isWired() }

/** Count-only twin of [wiredPhotos] for hot paths (#339): search runs per
 * message per keystroke and titles render per turn — a `count {}` avoids
 * the throwaway list `filter {}` allocates without changing the contract. */
fun List<ChatAttachment>.wiredPhotoCount(): Int =
    count { it.isWired() }

/** Single source of truth for the wired predicate (#339): both helpers
 * above delegate here, so the next edit that changes what counts (e.g.
 * trimming whitespace-only payloads) cannot silently re-split the
 * wire/display/cap contract. Blank-only: decode-validity (ragged alphabet,
 * bad padding) is checked at the wire filter, not here — a full or even
 * regex validation per call would run on search keystrokes, while sends
 * (the only place validity matters) are rare. */
private fun ChatAttachment.isWired(): Boolean = base64.isNotBlank()

/** Decode-validity for the wire filter (#339): blank payloads never reach
 * here (isWired), but a hand-edited ragged payload (bad length, non-base64
 * alphabet) must not emit `data:...;base64,<ragged>` into an image_url
 * part — the gateway would 400 what should be a client-side named shape.
 * Alphabet scan, no decode: runs per send, never per keystroke. Internal:
 * the persistence loader canonicalizes with this too, so memory holds no
 * wrapped-or-ragged payloads and the hot paths never need this check.
 * Padding is positional, not counted: `=` is legal only as the last ≤2
 * chars — `=AAA`, `A=A=`, `AB=C` all fail — since a bare count would pass
 * interior padding straight into the corrupt part this exists to prevent. */
internal fun isDecodableBase64(s: String): Boolean {
    if (s.isEmpty() || s.length % 4 != 0) return false
    var paddingStarted = false
    for ((i, c) in s.withIndex()) {
        if (c == '=') {
            if (i < s.length - 2) return false
            paddingStarted = true
        } else {
            if (paddingStarted) return false
            if (c != '+' && c != '/' &&
                c !in 'A'..'Z' && c !in 'a'..'z' && c !in '0'..'9'
            ) {
                return false
            }
        }
    }
    return true
}

/** Wired payloads with their staged indices (#339): the strip renders
 * wired but removes by staged slot, so it needs both — without
 * re-rolling the predicate (or the remove index) at the call site. */
fun List<ChatAttachment>.wiredPhotosWithIndex(): List<IndexedValue<ChatAttachment>> =
    mapIndexedNotNull { i, a -> if (a.isWired()) IndexedValue(i, a) else null }

/**
 * Canonical form of persisted attachments (#339): storage is
 * hand-editable, so payloads arrive stripped of framing whitespace — and
 * anything still undecodable blanks out. Applied once at the load
 * boundary (IO, never Main), so memory holds no wrapped-or-ragged
 * payloads: the hot-path isWired() can stay a blank check while the wire
 * filter asserts decode-validity, because load made them agree. Pure for
 * testability; the next persist writes back the canonical form
 * (self-healing, no notice — corrupt bytes are dropped, never sent).
 */
fun canonicalizeAttachments(attachments: List<ChatAttachment>): List<ChatAttachment> =
    attachments.map { a ->
        // Fast path first (#339): the app only ever writes clean payloads,
        // so prove-no-whitespace + validity check runs allocation-free on
        // the common path — the stripping copy below runs solely for
        // payloads that actually need it (hand-edited or legacy).
        if (a.base64.none { it.isWhitespace() } && isDecodableBase64(a.base64)) {
            a
        } else {
            val stripped = a.base64.filterNot { c -> c.isWhitespace() }
            when {
                stripped.isBlank() -> a.copy(base64 = "")
                isDecodableBase64(stripped) -> a.copy(base64 = stripped)
                else -> a.copy(base64 = "")
            }
        }
    }

/** Max photos per turn (#318). Two is the acceptance bar (two images in one
 * turn render and reach the agent); the cap keeps the wire body under the
 * gateway's 4 MiB limit (see the invariant test in [ChatAttachmentsTest]). */
const val MAX_IMAGES_PER_TURN = 2

/**
 * Single wording for the whole per-turn cap family (#339): tap-time
 * refusal, tap-time excess, mid-batch exhaustion, and the ViewModel
 * backstop all say exactly this with Info styling — nothing failed, the
 * cap held, so neither the words nor the styling may disagree by path.
 */
fun capRefused(): String =
    "At most $MAX_IMAGES_PER_TURN photos per turn — extra photos were dropped."

/** Longest side after downscale, px. A 1568px JPEG at quality 80 is
 * typically 150–600 KB — legible for the agent, small on the wire. */
const val MAX_IMAGE_SIDE_PX = 1568

/** Per-photo base64 cap, chars (~1.1 MiB raw). Two capped photos plus text
 * history stay under [MAX_CHAT_WIRE_BYTES] with headroom. */
const val MAX_IMAGE_BASE64_CHARS = 1_500_000

/** Original-file read cap, bytes. The shrink happens after the read, so a
 * mislabeled giant (e.g. a video shared as an image) must fail here with a
 * named error rather than OOMing the decode. 32 MB is an order of magnitude
 * above any real photo and far below heap danger. */
const val MAX_SOURCE_BYTES = 32_000_000

/** Client-side wire preflight, BYTES. Below the gateway's 4 MiB body cap on
 * purpose: exceeding this refuses with a named in-app error instead of an
 * opaque HTTP 413, and leaves room for JSON framing plus history text.
 * Bytes, not chars (#339): base64 and framing are ASCII so the two agree
 * for photos, but CJK/emoji history is multibyte — a char count would let
 * a long non-ASCII thread sail past into the opaque 413 this preflight
 * exists to prevent. */
const val MAX_CHAT_WIRE_BYTES = 3_500_000

/** `true` when a request body of [bytes] must be refused client-side. Long:
 * callers measure `RequestBody.contentLength()`, which is Long (-1 unknown). */
fun wireSizeExceeded(bytes: Long): Boolean = bytes < 0 || bytes > MAX_CHAT_WIRE_BYTES

/** Wire form of one attachment: the `image_url` data URI the gateway's
 * multipart parser accepts (`internal/gateway/openai.go`). Remote URLs are
 * never produced — the gateway refuses to fetch them. The type is PINNED,
 * not rendered from storage (#339): every payload is JPEG re-encoded at
 * pick time, so a hand-edited thread file cannot widen the wire type —
 * fail-closed at the client edge, like the gateway's unknown-part 400.
 * Wired-only precondition, enforced as the named family (#339): blank
 * payloads must be filtered via wiredPhotos() first — an empty data URI
 * is the corrupt part the wire filter exists to prevent, so this throws
 * NamedAttachError (pass-through bucket, not the generic remap) instead
 * of rendering one. Unreachable today (all wire sites filter first). */
fun ChatAttachment.dataUri(): String {
    if (base64.isBlank()) {
        throw NamedAttachError(
            "ChatAttachment.dataUri requires a non-blank payload (filter wiredPhotos() first)",
        )
    }
    return "data:image/jpeg;base64,$base64"
}

/** Inline marker for text-only surfaces (exports, search, titles):
 * `[1 photo attached]` / `[3 photos attached]`. */
fun attachmentMarker(n: Int): String =
    if (n == 1) "[1 photo attached]" else "[$n photos attached]"

/**
 * Text plus its photo marker for trim-tolerant surfaces (#339): search
 * matching ignores surrounding whitespace, so this trims. (Export and
 * wire history use contentWithMarkerVerbatim — content-preserving —
 * because they reproduce what the agent saw.) Empty text yields the bare
 * marker; zero photos yields the text untouched. Trim/preserve split,
 * stated once: this trims, the verbatim twin does not — pick by whether
 * the surface matches (trim) or reproduces (preserve).
 */
fun contentWithMarker(content: String, n: Int): String =
    if (n <= 0) content else (content.trim() + " " + attachmentMarker(n)).trim()

/**
 * Verbatim twin of [contentWithMarker] for the wire history path (#339):
 * same marker rule, no trim — history text rides exactly as typed
 * (pasted-code indentation included) with only the marker appended.
 * Kept as a named helper (not an inline) so the two spellings stay
 * visibly paired and the next edit can't silently re-split them.
 * Trim/preserve split, stated once: export uses this too (reproducing
 * what the agent saw); search uses the trimming twin (matching is
 * trim-tolerant).
 */
fun contentWithMarkerVerbatim(content: String, n: Int): String =
    if (n <= 0) {
        content
    } else if (content.isBlank()) {
        attachmentMarker(n)
    } else {
        content + " " + attachmentMarker(n)
    }

/** True when the message carries anything the agent should see. Text-only
 * checks (`content.isNotBlank()`) drop image-only turns from resends and
 * resume, so every history filter must use this instead. Counted wired
 * (#339): a blank payload is not a photo anywhere, including here. The
 * attachment half is user-only, matching the wire marker: a hypothetical
 * assistant photo turn filters as content-less (it would wire as bare
 * text with no marker), so resends never carry a turn the wire drops. */
fun messageHasContent(m: ChatMessage): Boolean =
    m.content.isNotBlank() || (m.role == "user" && m.attachments.wiredPhotoCount() > 0)

/**
 * Dimensions fitting `w`×`h` inside [MAX_IMAGE_SIDE_PX] on the long side,
 * preserving aspect ratio, never upscaling. Pure for testability — the
 * bitmap work in [prepareAttachment] consumes this.
 */
fun downscaledSize(w: Int, h: Int): Pair<Int, Int> {
    // (0, 0) is unreachable via prepareAttachment (bounds <= 0 throws
    // before this is called) — the zero here guards the pure unit and any
    // future caller against feeding createScaledBitmap a sizable zero.
    if (w <= 0 || h <= 0) return 0 to 0
    val longest = maxOf(w, h)
    if (longest <= MAX_IMAGE_SIDE_PX) return w to h
    // Integer math, not floating point: (4000 * 1568 / 4000) is exactly 1568
    // while (4000 * (1568.0 / 4000)) can come out 1567.9999 and truncate.
    // Long numerator: a 100 MP panorama (w * cap) still fits in a Long.
    val nw = maxOf(1L, (w.toLong() * MAX_IMAGE_SIDE_PX) / longest).toInt()
    val nh = maxOf(1L, (h.toLong() * MAX_IMAGE_SIDE_PX) / longest).toInt()
    return nw to nh
}

/**
 * Wire shape of one message's content (#339): a bare string normally, the
 * conventional multipart array when the turn carries photos. Sealed (not
 * `Any`) so callers branch exhaustively with compiler help — the next edit
 * that assumes String (a log line doing `.length`, a reader doing string
 * concat) fails to compile instead of crashing only on photo turns. The
 * two shapes mirror what the gateway parser already distinguishes.
 */
sealed interface WireContent {
    data class Text(val text: String) : WireContent
    /** Multipart photo turn. Plain class, not data (#339): JSONArray has
     * identity equals/hashCode, so data-class equality would mislead (two
     * identical multiparts never assertEquals) — and the mutable array
     * must not pretend to be a value. Callers get parts; nobody copies. */
    class Multipart(val parts: JSONArray) : WireContent
}
/**
 * Wire content for one message: [WireContent.Text] normally, the
 * conventional multipart array when this user turn carries photos.
 *
 * Only the LAST user turn in the request keeps its image parts
 * ([includeImages]): the server prompts Pi with the trailing user turn
 * only (`lastUserTurn`), and Pi holds history in its own session file —
 * so earlier turns ride as text and the body stays bounded no matter how
 * many photo turns a thread holds. A text part is included only when the
 * text is non-blank; an image-only turn is images alone, which the server
 * accepts (blank text + images is a non-blank turn there too).
 */
fun messageContentJson(
    role: String,
    content: String,
    attachments: List<ChatAttachment>,
    includeImages: Boolean,
): WireContent {
    // Fail-closed on payload, not just presence (#339): a hand-edited
    // thread file (or a future writer bug) can stage an empty base64,
    // which would otherwise wire as data:image/jpeg;base64, — a corrupt
    // part the gateway would feed Pi. Empty payloads count as no
    // attachment for wire purposes (an image-only turn of blanks falls
    // back to bare text, which the server refuses with its named
    // blank-turn error instead of a corrupt image part). Ragged payloads
    // (bad length/alphabet) filter here too — validity is send-frequency,
    // so the alphabet scan lives at the wire, never in the hot-path
    // isWired() predicate. Single pass: blank-check then validity per
    // attachment, not a wiredPhotos() list followed by a second walk.
    val images = attachments.filter { it.isWired() && isDecodableBase64(it.base64) }
    if (role != "user" || images.isEmpty() || !includeImages) {
        // Stripped earlier photo turns stay honest (#339): the server
        // prompts with the trailing turn alone, but any other reader of
        // this history should see that photos were here — not a blank.
        // Verbatim twin (NOT the trimming helper): history rides exactly
        // as typed, marker only appended.
        if (role == "user" && images.isNotEmpty() && !includeImages) {
            return WireContent.Text(contentWithMarkerVerbatim(content, images.size))
        }
        // Assistant turns never stage photos: the marker rule above is
        // user-only by construction, and a bare return here keeps a
        // hypothetical assistant-with-attachments payload from silently
        // widening the wire shape (fail-closed, like the gateway).
        return WireContent.Text(content)
    }
    val arr = JSONArray()
    if (content.isNotBlank()) {
        arr.put(JSONObject().put("type", "text").put("text", content))
    }
    for (a in images) {
        arr.put(
            JSONObject()
                .put("type", "image_url")
                .put("image_url", JSONObject().put("url", a.dataUri())),
        )
    }
    return WireContent.Multipart(arr)
}

/** Exact decoded size of a base64 payload (#339): the naive 3/4 rule
 * overstates by the padding (1–2 B), so named limit errors would name a
 * wrong cap-adjacent number ("1.1 MB" is fine; "X B" off by two is not).
 * Precondition: valid base64 (length % 4 == 0) — a hand-edited payload
 * with a ragged length floors down by up to 2 B, which is acceptable for
 * approximate error strings and never reached (wire/display paths filter
 * to decode-valid payloads via wiredPhotos()). Framing whitespace
 * (older wrapped payloads, which decode stays lenient toward) is excluded
 * inline — ALL of it, not just newlines (#339): decode skips spaces/tabs
 * too, so measuring them would overstate the named size by the space
 * count. Single counting pass, no copies: the error path already failed
 * for size, so allocating another ~1.5 M-char filter copy just to report
 * it would repeat the peak. */
fun base64Bytes(base64: String): Long {
    var content = 0
    var padding = 0
    for (c in base64) {
        if (c.isWhitespace()) continue
        content++
        padding = if (c == '=') padding + 1 else 0
    }
    return (content / 4L) * 3L - padding
}

/** `1.1 MB`-style rendering of [bytes] for limit errors. One decimal at KB
 * and above, plain count below 1000 B (never blank) (#339): the old doc
 * line ("one decimal") overpromised the sub-KB branch, which renders no
 * decimal by design.
 * Pinned to US formatting (#339): user-visible strings must not render
 * `1,1 MB` under a comma-decimal locale. Never renders `1000 <unit>`
 * (#339): the KB branch stops below 999_500 so rounding carries into MB
 * instead of printing a cross-unit "1000 KB" in a named limit error.
 * Negative input renders "unknown size" (#339): the preflight phrases
 * unknown length separately today, but the next caller passing a raw
 * contentLength() must not print "-1 B" in a user-visible string. */
fun formatBytes(bytes: Long): String {
    if (bytes < 0) return "unknown size"
    if (bytes < 1_000L) return "$bytes B"
    if (bytes < 999_500L) return "${"%.0f".format(java.util.Locale.US, bytes / 1_000.0)} KB"
    return "${"%.1f".format(java.util.Locale.US, bytes / 1_000_000.0)} MB"
}

/**
 * Our own attach failures, typed so the toast mapping never depends on
 * message wording (#339): [namedReadError] passes these (and the oversize
 * IllegalStateException) through and remaps every system throwable to the
 * generic unreadable-photo error. Rewording a message can't silently flip
 * its bucket — only changing the type can, which the compiler sees.
 * Internal (not private): a future wire site that misses the wiredPhotos()
 * filter must be able to catch this specifically instead of surfacing the
 * dev-facing precondition text through the generic toast path.
 */
internal class NamedAttachError(message: String) : IllegalArgumentException(message)

/** Size-cap failures, typed apart from every other IllegalStateException
 * (#339): providers and framework code throw ISE too ("Permission
 * Denial…"), so passing through all ISE would surface system text in the
 * toast — breaking the "never system text, logcat-only" contract. Only
 * this + NamedAttachError pass through; the rest remap to generic. */
internal class OversizeAttachError(message: String) : IllegalStateException(message)

/**
 * Maps a read failure onto the named error family (#339). Our own throws
 * pass through; system throwables (SecurityException on a revoked picker
 * grant, FileNotFoundException, provider errors) become the generic
 * unreadable-photo error. Raw publisher wording ("Permission Denial…")
 * is logcat-only — the toast contract is named errors, never system text.
 */
private fun namedReadError(e: Throwable): Throwable {
    // Cancellation is not a read failure (#339): leaving the tab mid-batch
    // cancels the decode, and remapping that into the generic error would
    // toast "couldn't read" for a user-initiated abort — while also
    // breaking structured concurrency by swallowing the cancel. Errors
    // (OOM and friends) rethrow for the same reason: masking a JVM-level
    // failure as a read stall would invite re-picks into the same crash —
    // fail loud, not toastable.
    if (e is CancellationException || e is Error) throw e
    runCatching { Log.w("ChatAttachments", "photo read failed", e) }
    return if (e is NamedAttachError || e is OversizeAttachError) e
    else NamedAttachError(
        "Couldn't read that photo — it may have been moved or deleted. Pick it again.",
    )
}

/**
 * Reads [uri] (gallery pick or camera capture) into a bounded [ChatAttachment].
 *
 * Bounds-first: the image is sampled down during decode and scaled to
 * [MAX_IMAGE_SIDE_PX], then JPEG-compressed (quality 80, retry 60), so a
 * 12 MP capture never sits in memory at full size. Anything still over
 * [MAX_IMAGE_BASE64_CHARS] fails with a NAMED error (what it is, how big
 * it came out, what the cap is, what to do) — never a silent drop, never
 * an opaque 400/413 from the server later.
 */
suspend fun prepareAttachment(context: Context, uri: Uri): Result<ChatAttachment> =
    withContext(Dispatchers.IO) {
        runCatching {
            val resolver = context.contentResolver
            // Scoped read+decode (#339): bounds, sample, pixels, and EXIF
            // all run inside this block from one bounded read, so `bytes`
            // falls out of scope before rotation/scale/compress — the
            // source buffer is GC-eligible before the real hump instead of
            // living through it. (Previously: 3× stream opens re-fetched
            // cloud URIs; then one read held through compress. This keeps
            // the single read and drops the retention.)
            val decoded = run {
                val bytes = resolver.openInputStream(uri)?.use { stream ->
                // Pre-sized 1 MB (#339): the default 32-byte buffer doubles
                // its way to 32 MB (~64 MB transient at the top copy), and
                // real photos land within an order of this anyway.
                val out = ByteArrayOutputStream(1 shl 20)
                val buf = ByteArray(8192)
                var total = 0L
                // A 0 return is legal but progress-less (#339): bail after
                // a streak instead of spinning on a stuck provider stream —
                // as a READ failure ("moved or deleted"), not a silent
                // truncation that decodes into a bogus "not a photo" error.
                var zeroStreak = 0
                while (true) {
                    // Cooperative cancel on the progressing path too (#339):
                    // yield() only ran on zero-reads, so a slow cloud-URI
                    // streaming megabytes (every read > 0) held an IO thread
                    // past tab-leave until the whole read finished.
                    ensureActive()
                    val n = stream.read(buf)
                    // < 0 only: 0 is a legal (if rare) return, especially
                    // from content-provider streams — breaking on it would
                    // truncate the file into a bogus "not readable" error.
                    if (n < 0) break
                    if (n == 0) {
                        // A stall, not a deletion (#339): 100 consecutive
                        // 0-reads is a stuck provider stream, where retrying
                        // the same Uri may succeed — so this says try again
                        // instead of sending the user off to re-pick. Yields
                        // while spinning so a stuck stream doesn't burn CPU.
                        if (++zeroStreak > 100) {
                            throw NamedAttachError(
                                "Couldn't finish reading that photo — try again.",
                            )
                        }
                        yield()
                        continue
                    }
                    zeroStreak = 0
                    total += n
                    // The shrink happens after the read, so the ORIGINAL
                    // needs its own bound — far above any real photo, far
                    // below an OOM from a mislabeled video file. Names only
                    // the cap (#339): total is bytes-read-so-far, so quoting
                    // it would present a partial count as the file's size.
                    if (total > MAX_SOURCE_BYTES) {
                        throw OversizeAttachError(
                            "That file is too large to attach " +
                                "(over about ${formatBytes(MAX_SOURCE_BYTES.toLong())}).",
                        )
                    }
                    out.write(buf, 0, n)
                }
                out.toByteArray()
            } ?: throw NamedAttachError(
                "Couldn't read that photo — it may have been moved or deleted. Pick it again.",
            )
            val bounds = BitmapFactory.Options().apply { inJustDecodeBounds = true }
            BitmapFactory.decodeByteArray(bytes, 0, bytes.size, bounds)
            if (bounds.outWidth <= 0 || bounds.outHeight <= 0) {
                throw NamedAttachError(
                    "That file isn't a readable photo — Agento attaches images only, not documents.",
                )
            }
            // Sample toward JUST ABOVE the target box (#339): advance only
            // while the next sample still covers it, so the decoded bitmap
            // stays >= the target and the exact fit below is always a
            // downscale. The old shape (first sample fitting INSIDE the
            // box) undershot ~2x on large captures (4000px → sample 4 →
            // 1000px) and then soft-UPSCALED to 1568 — worse than sampling
            // to 2000px and downscaling. Long counter like the thumbnail
            // loop: the toInt() at the inSampleSize handoff is provably
            // small (exits in ~6 iterations), same overflow discipline
            // everywhere sampling runs.
            var sample = 1L
            while (bounds.outWidth / (sample * 2) >= MAX_IMAGE_SIDE_PX ||
                bounds.outHeight / (sample * 2) >= MAX_IMAGE_SIDE_PX
            ) {
                sample *= 2
            }
            val opts = BitmapFactory.Options().apply { inSampleSize = sample.toInt() }
            val fresh = BitmapFactory.decodeByteArray(bytes, 0, bytes.size, opts)
                ?: throw NamedAttachError(
                    "Couldn't read that photo — it may have been moved or deleted. Pick it again.",
                )
                // EXIF orientation (#339 review): BitmapFactory ignores it, so a
                // portrait phone photo would stage and send sideways. The
                // framework reader needs no extra dependency (minSdk 28 > 24).
                // Flips included: front-camera selfies commonly carry
                // FLIP_HORIZONTAL/TRANSVERSE, which a rotation-only mapping
                // would leave mirrored.
                val orient = runCatching {
                    // use{} like every other stream here (#339): close is a
                    // no-op on a byte-array stream, but the hygiene is the
                    // same shape as the resolver reads above it.
                    java.io.ByteArrayInputStream(bytes).use {
                        ExifInterface(it).getAttributeInt(
                            ExifInterface.TAG_ORIENTATION,
                            ExifInterface.ORIENTATION_NORMAL,
                        )
                    }
                }.getOrDefault(ExifInterface.ORIENTATION_NORMAL)
                fresh to orient
            }
            var bitmap = decoded.first
            val orientation = decoded.second
            // One try/finally from here down (#339): rotation, scaling and
            // compression each allocate or throw, and any throw must recycle
            // the live bitmap — the recycle-before-reassign lines cover the
            // success path, the finally covers every throw between them.
            // Cooperative cancel between the native steps (#339): decode,
            // rotate, fill, and scale each run hundreds of ms with no
            // suspension point — a tab-leave must surface at each gate,
            // not only in the read/compress loops.
            // Declared outside ("": nothing measured yet): the oversize
            // check below reads them after, and a throw before the first
            // candidate must report "too large to process", never a
            // nonsense byte count. Last attempt, not min/max (#339):
            // quality descends, so the final measurement is the closest
            // to passing by construction.
            var encoded: String? = null
            var lastCandidate = ""
            try {
                ensureActive()
                // Null means no transform: transposes and flips compose a mirror
                // via negative scale, which createBitmap applies directly.
                // Mapping follows the EXIF orientation semantics (CIPA DC-008 /
                // AOSP ExifInterface: TRANSPOSE = 90° + horizontal mirror,
                // TRANSVERSE = 270° + horizontal mirror). The Bitmap path is
                // JVM-untestable by design (stubbed android.jar), so the
                // reference — not a test — is what pins this; do not "simplify"
                // the mirror cases into pure rotations. Pinned matrix, all 8
                // values (#339): 1=identity, 2=flip-H, 3=rot180, 4=flip-V,
                // 5=transpose(rot90+flip-H), 6=rot90, 7=transverse(rot270+flip-H),
                // 8=rot270 — pre vs post picks which side of the rotation the
                // mirror lands on, which is exactly the 5-vs-7 distinction.
                val matrix: Matrix? = when (orientation) {
                    ExifInterface.ORIENTATION_ROTATE_90 -> Matrix().apply { postRotate(90f) }
                    ExifInterface.ORIENTATION_ROTATE_180 -> Matrix().apply { postRotate(180f) }
                    ExifInterface.ORIENTATION_ROTATE_270 -> Matrix().apply { postRotate(270f) }
                    ExifInterface.ORIENTATION_FLIP_HORIZONTAL -> Matrix().apply { preScale(-1f, 1f) }
                    ExifInterface.ORIENTATION_FLIP_VERTICAL -> Matrix().apply { preScale(1f, -1f) }
                    ExifInterface.ORIENTATION_TRANSPOSE -> Matrix().apply {
                        postRotate(90f)
                        postScale(-1f, 1f)
                    }
                    ExifInterface.ORIENTATION_TRANSVERSE -> Matrix().apply {
                        postRotate(270f)
                        postScale(-1f, 1f)
                    }
                    else -> null
                }
                if (matrix != null) {
                    val rotated = Bitmap.createBitmap(bitmap, 0, 0, bitmap.width, bitmap.height, matrix, true)
                    // Referential on purpose: equality is referential for Bitmaps,
                    // and only a new instance may recycle the old one.
                    if (rotated !== bitmap) {
                        bitmap.recycle()
                        bitmap = rotated
                    }
                }
                ensureActive()
                val (tw, th) = downscaledSize(bitmap.width, bitmap.height)
                // Opaque white fill, only when needed (#339): JPEG has no
                // alpha channel, so a transparent screenshot/icon would
                // otherwise flatten onto black. hasAlpha()-gated — opaque
                // camera photos (the common case) skip the second full-size
                // bitmap + blit entirely.
                if (bitmap.hasAlpha()) {
                    val opaque = Bitmap.createBitmap(bitmap.width, bitmap.height, Bitmap.Config.ARGB_8888)
                    android.graphics.Canvas(opaque).apply {
                        drawColor(android.graphics.Color.WHITE)
                        drawBitmap(bitmap, 0f, 0f, null)
                    }
                    if (opaque !== bitmap) {
                        bitmap.recycle()
                        bitmap = opaque
                    }
                }
                if (bitmap.width != tw || bitmap.height != th) {
                    val scaled = Bitmap.createScaledBitmap(bitmap, tw, th, true)
                    if (scaled !== bitmap) {
                        bitmap.recycle()
                        bitmap = scaled
                    }
                }
                ensureActive()
                for (quality in listOf(80, 60)) {
                    // Cooperative cancel like the read loop (#339): two
                    // full JPEG encodes with no suspension would hold the
                    // IO thread past tab-leave until both land — the cancel
                    // surfaces only in finally/remap without this.
                    ensureActive()
                    // Pre-sized like the read buffer (#339): the default
                    // 32-byte buffer would double its way to ~1.5 MB per
                    // attempt, then toByteArray() copies again.
                    val out = ByteArrayOutputStream(1 shl 20)
                    // Peak note (#339): bitmap (~10 MB at 1568px) + out bytes
                    // + base64 string co-exist here, plus the transient
                    // opaque fill above when the source had alpha (two
                    // full-size bitmaps live until its recycle) — and the
                    // q60 retry reallocates out while bitmap stays live.
                    // The source `bytes` buffer is NOT part of this hump:
                    // it falls out of scope with the read+decode block
                    // above, so it is GC-eligible before bitmap work.
                    // Bounded and send-path-only, but the real hump, so a
                    // future "optimization" of the buffer alone would
                    // miss it.
                    // Checked, not assumed (#339): a false return leaves
                    // out empty, and an empty candidate would pass the cap
                    // as a zero-byte "photo" — silent corrupt payload wired
                    // as data:image/jpeg;base64, with no named error.
                    if (!bitmap.compress(Bitmap.CompressFormat.JPEG, quality, out)) {
                        throw NamedAttachError(
                            "Couldn't read that photo — it may have been moved or deleted. Pick it again.",
                        )
                    }
                    val candidate = Base64.encodeToString(out.toByteArray(), Base64.NO_WRAP)
                    lastCandidate = candidate
                    if (candidate.length <= MAX_IMAGE_BASE64_CHARS) {
                        encoded = candidate
                        break
                    }
                }
            } finally {
                bitmap.recycle()
            }
            val final = encoded ?: throw OversizeAttachError(
                // Empty means the loop threw before measuring (OOM in the
                // encode): report unprocessable, never "0 B" or a sentinel.
                // Sizes exact via base64Bytes (#339): padding-adjusted, so
                // the named number is the real one, not 1–2 B high.
                (if (lastCandidate.isNotEmpty()) {
                    "Photo too large even after shrinking (${formatBytes(base64Bytes(lastCandidate))}) — "
                } else {
                    "Photo too large to process — "
                }) +
                    "the per-photo limit is about " +
                    "${formatBytes(MAX_IMAGE_BASE64_CHARS * 3L / 4L)}. " +
                    "Pick a different photo.",
            )
            ChatAttachment(mimeType = "image/jpeg", base64 = final, id = java.util.UUID.randomUUID().toString())
        }.let { result ->
            // Remap system throwables onto the named family; ours pass
            // through untouched (see namedReadError).
            result.exceptionOrNull()?.let { Result.failure(namedReadError(it)) } ?: result
        }
    }

/**
 * Small bitmap for thumbnails (bubbles, composer strip). Decodes sampled to
 * ~256px so a remembered thumbnail costs kilobytes, not the full payload.
 * Null when the payload is corrupt — callers show an icon placeholder, so a
 * bad payload degrades to a missing thumbnail, never a crash.
 */
fun decodeThumbnail(base64: String): Bitmap? {
    // Explicit try/catch, not runCatching (#339): cancellation must
    // propagate (produceState restart on recompose), never collapse into
    // the null placeholder path like a corrupt payload does. Errors
    // propagate too, matching the stage path — a corrupt payload still
    // lands null via the Exception branch, never via masking the JVM.
    return try {
        // Gated on decoded size, not raw length (#339): base64Bytes
        // excludes framing whitespace the raw length counts, so a wrapped
        // hand-edited payload gates on what decode will actually read.
        if (base64Bytes(base64) > MAX_IMAGE_BASE64_CHARS * 3L / 4L) return null
        // DEFAULT on decode is deliberate asymmetry with the NO_WRAP encode
        // above: encode must not emit newlines (data URIs are single-line),
        // while decode stays lenient toward payloads written by older builds.
        val bytes = Base64.decode(base64, Base64.DEFAULT)
        val bounds = BitmapFactory.Options().apply { inJustDecodeBounds = true }
        BitmapFactory.decodeByteArray(bytes, 0, bytes.size, bounds)
        if (bounds.outWidth <= 0 || bounds.outHeight <= 0) return null
        // Long counter like the full-size loop (#339): exits in ~12
        // iterations on real headers (halving toward 256), so the toInt()
        // at the inSampleSize handoff is provably small — an adversarial
        // giant-dims header drives it furthest and still exits orders of
        // magnitude below 2^31. Same overflow discipline everywhere
        // sampling runs.
        var sample = 1L
        while (bounds.outWidth / sample > 256 ||
            bounds.outHeight / sample > 256
        ) {
            sample *= 2
        }
        BitmapFactory.decodeByteArray(
            bytes, 0, bytes.size,
            BitmapFactory.Options().apply { inSampleSize = sample.toInt() },
        )
    } catch (e: CancellationException) {
        throw e
    } catch (e: Error) {
        // Fail loud like the stage path (#339): an OOM here must crash,
        // never collapse into a placeholder that hides JVM-level failure.
        throw e
    } catch (e: Exception) {
        null
    }
}
