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
import kotlinx.coroutines.currentCoroutineContext
import kotlinx.coroutines.ensureActive
import kotlinx.coroutines.withContext
import kotlinx.coroutines.yield
import kotlinx.serialization.Serializable
import org.json.JSONArray
import org.json.JSONObject

/** One photo attached to a user turn. Stored payloads are already bounded:
 * downscaled and re-encoded to JPEG at pick time ([prepareAttachment]) —
 * [MAX_IMAGE_SIDE_PX] long side, base64 under [MAX_IMAGE_BASE64_CHARS]. The
 * thread file and backup hold them verbatim, so a retry resends the turn. */
@Serializable
data class ChatAttachment(
    val mimeType: String = "image/jpeg",
    /** Raw bytes as base64, no `data:` prefix — [dataUri] adds it for the wire.
     * Display counts any non-blank payload ("was a photo staged"); the wire is
     * stricter — wired AND decodable — and fails closed ("what may send"). */
    val base64: String = "",
    /** Stable per-pick id, minted at stage time: thumbnail keys and per-photo
     * identity use this, never the payload, so removing photo 0 doesn't re-key
     * photo 1. Default empty so pre-id history still decodes. */
    val id: String = "",
)

/**
 * Attachments that count as photos: non-blank payloads. Display and the wire
 * filter must agree, or a blank wires as bare text while the UI claims a
 * photo — so every persisted read flows through load(), which strips
 * whitespace and blanks undecodable payloads. Ragged payloads count in
 * display but wire as bare text. */
fun List<ChatAttachment>.wiredPhotos(): List<ChatAttachment> =
    filter { it.isWired() }

/** Count-only twin of [wiredPhotos] for hot paths (search runs per keystroke):
 * avoids the throwaway list without changing the contract. */
fun List<ChatAttachment>.wiredPhotoCount(): Int =
    count { it.isWired() }

/** Any photo that would actually wire — wired AND decodable: for surfaces that
 * must agree with the wire filter (the preflight's "remove a photo" advice),
 * so a ragged-only turn is never told to remove a photo that never wires. */
internal fun List<ChatAttachment>.hasDecodablePhoto(): Boolean =
    any { it.isWired() && isDecodableBase64(it.base64) }

/** Single source of truth for the display predicate: the helpers above
 * delegate here. Blank-only — decode validity is checked at the wire filter,
 * not here; surfaces that must match the wire use [hasDecodablePhoto].
 *
 * RAGGED COUNTS AS A PHOTO HERE, NEVER ON THE WIRE (#339): a hand-edited
 * payload with a bad alphabet or odd length shows up in the strip, the
 * bubble, the title, the search and the export, but the wire filter drops
 * it (it would be a corrupt part). Deliberate and fail-closed — the app
 * never writes a ragged payload, and load() heals or blanks it on the way
 * in, so the two agree for everything the app stores. */
private fun ChatAttachment.isWired(): Boolean = base64.isNotBlank()

/** Absolute raw-length ceiling for untrusted payloads: no scan, copy, or decode
 * runs past this many chars, however small the decoded size would be. Checked
 * FIRST (O(1)) in every validity/scan path below. */
const val MAX_RAW_BASE64_CHARS = 4_000_000

/** Decode-validity for the wire filter: a hand-edited ragged payload (bad
 * length, non-base64 alphabet) must not emit a corrupt `data:` part. Alphabet
 * scan, no decode: runs per send, never per keystroke. STRICT on whitespace
 * BY DESIGN: [base64Bytes] and `Base64.decode(DEFAULT)` tolerate it, but the
 * wire must never emit a wrapped data: URI — the helpers deliberately disagree
 * (measure/decode read bytes, validate guards the wire shape); do not "fix"
 * one to match. Padding is positional, not counted (`=AAA`, `A=A=` fail);
 * standard alphabet only (`+/`, never URL-safe), else fail closed. Alphabet,
 * NOT image magic, accepted risk: valid non-JPEG base64 wires and gets a named
 * server refusal. */
internal fun isDecodableBase64(s: String): Boolean {
    if (s.isEmpty() || s.length % 4 != 0 || s.length > MAX_RAW_BASE64_CHARS) return false
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

/** Wired payloads with their staged indices: the strip renders wired but
 * removes by staged slot, so it needs both. */
fun List<ChatAttachment>.wiredPhotosWithIndex(): List<IndexedValue<ChatAttachment>> =
    mapIndexedNotNull { i, a -> if (a.isWired()) IndexedValue(i, a) else null }

/**
 * IO-thread gate for the persistence boundary: load/save/export/import all
 * claim Main-never. Looper-exact on device; on JVM unit tests the android.jar
 * stubs throw, which reads as "no Main thread here" and passes. NEVER FATAL on
 * purpose: Main-ness is a jank bug, not a correctness bug, so it logs a
 * stacked error instead of taking the chat tab down.
 *
 * ADVISORY, not enforcement — and that matters for the callers' peak
 * budgets (#339): the backup export holds ~25 MB of heap, which is only
 * safe OFF Main. Enforcement is the caller's job — every UI caller hops
 * with withContext(Dispatchers.IO) and every boundary is annotated
 * @WorkerThread — so this gate is the tripwire for a future caller that
 * forgets, not the thing that makes the cap safe. A Main caller still
 * gets the right bytes, just on the wrong thread. */
internal fun requireIoThread(site: String, context: Context? = null) {
    // runCatching (not try/catch): the fallback documents the JVM case inline.
    val onMain = runCatching {
        val mainLooper = android.os.Looper.getMainLooper()
        android.os.Looper.myLooper() !== null && android.os.Looper.myLooper() === mainLooper
    }.getOrDefault(false)
    if (onMain) {
        // Debug builds FAIL FAST (#339): a debuggable app throws, so a dev
        // device surfaces the slip immediately instead of janking silently.
        // Production only logs — a crash there would take the chat tab
        // down over a performance mistake.
        if (context != null &&
            (context.applicationInfo.flags and android.content.pm.ApplicationInfo.FLAG_DEBUGGABLE) != 0
        ) {
            throw IllegalStateException("$site must run on an IO thread, never Main")
        }
        // Stacked, not thrown: a Main-thread caller still does the right work.
        Log.e("ChatAttachments", "$site ran on Main (should be IO)", IllegalStateException(site))
    }
}

/**
 * Canonical form of persisted attachments: storage is hand-editable, so framing
 * whitespace is stripped and anything still undecodable blanks out. Applied
 * once at the load boundary, so the hot-path [isWired] can
 * stay a blank check. Heal-silent BY DESIGN: a toast per healed payload would
 * storm on every cold start.
 *
 * IO-CONFINED BY CALLER, not by a guard here (#339): this is a pure
 * per-payload helper (unit-tested on the JVM), and its callers —
 * load/save/export/import — are the IO-gated boundary. Same shape as
 * [isDecodableBase64].
 *
 * Orientation is NOT normalized here: staged payloads are already EXIF-rotated
 * at capture time; an imported EXIF-6 payload keeps its metadata. */
fun canonicalizeAttachments(attachments: List<ChatAttachment>): List<ChatAttachment> {
    // Empty fast path: emptyList() is a process-wide singleton, safe here.
    if (attachments.isEmpty()) return emptyList()
    return attachments.map { a ->
        // Clean-first ordering: clean payloads pay one full walk and zero
        // copies; strip-first would tax every clean payload with a ~1.5 M-char
        // copy. Ceiling-BEFORE-strip is deliberate: healing a giant would
        // build the filtered copy the ceiling exists to prevent.
        if (isDecodableBase64(a.base64)) {
            a
        } else if (a.base64.length > MAX_RAW_BASE64_CHARS) {
            // Over the raw ceiling: blanked, NOT healed (#339) — healing
            // would first build a filtered copy of that same giant, the
            // exact allocation the ceiling exists to prevent. A valid
            // payload wrapped past the ceiling is therefore blanked on
            // purpose: it wires as no-photo instead of costing a 4 M-char
            // copy, and the app never writes wrapped payloads at all.
            a.copy(base64 = "")
        } else if (a.base64.all { it.isWhitespace() }) {
            // All-whitespace just under the ceiling: blank without building the copy.
            a.copy(base64 = "")
        } else {
            val stripped = a.base64.filterNot { c -> c.isWhitespace() }
            when {
                stripped.isBlank() -> a.copy(base64 = "")
                isDecodableBase64(stripped) -> a.copy(base64 = stripped)
                else -> a.copy(base64 = "")
            }
        }
    }
}

/** Max photos per turn. Two is the acceptance bar (two images in one turn
 * render and reach the agent); the cap keeps the wire body under the
 * gateway's 4 MiB limit. */
const val MAX_IMAGES_PER_TURN = 2

/** Consecutive unreadable picks before a batch aborts: failures yield to the
 * next Uri by design, but each burns a full prepareAttachment (up to 32 MB
 * read + decodes + EXIF) — without a bound, one bad batch spins through the
 * whole picker result. Only CONSECUTIVE failures count, so bad-good-bad
 * still stages the good. */
const val MAX_CONSECUTIVE_ATTACH_FAILURES = 3

/**
 * Single wording family for the whole per-turn cap: tap-time refusal,
 * tap-time excess, mid-batch exhaustion, and the ViewModel backstop all share
 * the head — nothing failed, the cap held. The tail is conditional on free
 * slots: a full composer ("remove one first") differs from a partial stage. */
fun capRefused(freeSlots: Int): String =
    if (freeSlots <= 0) {
        "At most $MAX_IMAGES_PER_TURN photos per turn — the composer is full, remove one first."
    } else {
        "At most $MAX_IMAGES_PER_TURN photos per turn — extra photos were dropped."
    }

/** Longest side after downscale, px. A 1568px JPEG at quality 80 is
 * typically 150–600 KB — legible for the agent, small on the wire. */
const val MAX_IMAGE_SIDE_PX = 1568

/** Decoded-pixel budget for one staged image (#339): the side rule alone
 * lets a source just under 2x the target decode at FULL size (3135x3135 ≈
 * 39 MB), and that bitmap lives beside the source bytes. 4 M pixels ≈ 16 MB
 * per decode. */
const val MAX_DECODE_PIXELS = 4_000_000L

/** Per-photo base64 cap, chars (~1.1 MiB raw). Two capped photos plus text
 * history stay under [MAX_CHAT_WIRE_BYTES] with headroom. */
const val MAX_IMAGE_BASE64_CHARS = 1_500_000

/** Original-file read cap, bytes. The shrink happens after the read, so a
 * mislabeled giant (e.g. a video shared as an image) must fail here with a
 * named error rather than OOMing the decode. */
const val MAX_SOURCE_BYTES = 32_000_000

/** Client-side wire preflight, BYTES. Below the gateway's 4 MiB body cap on
 * purpose: exceeding this refuses with a named in-app error instead of an
 * opaque HTTP 413, and leaves room for JSON framing plus history text.
 * Bytes, not chars: CJK/emoji history is multibyte, so a char count would
 * sail past into the opaque 413 this preflight prevents. */
const val MAX_CHAT_WIRE_BYTES = 3_500_000

/** `true` when a request body of [bytes] must be refused client-side. Long:
 * callers measure `RequestBody.contentLength()`, which is Long (-1 unknown). */
fun wireSizeExceeded(bytes: Long): Boolean = bytes < 0 || bytes > MAX_CHAT_WIRE_BYTES

/**
 * Per-photo oversize message for one attachment, or null when within the
 * cap: the wording [dataUri] throws and the stage path reports. One spelling,
 * one cap — callers never reword it. At/below-cap raws skip the precise
 * measure. */
internal fun ChatAttachment.oversizeMessage(): String? {
    if (base64.length <= MAX_IMAGE_BASE64_CHARS) return null
    val bytes = base64Bytes(base64)
    if (bytes <= MAX_IMAGE_BASE64_CHARS * 3L / 4L) return null
    return oversizeMessageFor(base64)
}

/**
 * Shared per-photo oversize wording: the stage failure and the dataUri wall
 * say exactly this. Takes the raw payload, not the attachment: the stage path
 * measures candidates that never become attachments. */
internal fun oversizeMessageFor(base64: String): String {
    val bytes = base64Bytes(base64)
    return "Photo too large even after shrinking (${formatBytes(bytes)}) — " +
        "the per-photo limit is about " +
        "${formatBytes(MAX_IMAGE_BASE64_CHARS * 3L / 4L)}. " +
        "Pick a different photo."
}

/**
 * Per-message search predicate, shared by thread search: raw-text region match
 * first (no marker alloc on hits), marker only on miss. Case-folding is
 * Unicode region-matching (ignoreCase), NOT ROOT-lowercasing, BY DECISION —
 * pinned by the Turkish-folding tests. Same folding on both branches. */
internal fun messageMatchesQuery(
    role: String,
    content: String,
    attachments: List<ChatAttachment>,
    query: String,
): Boolean {
    if (content.contains(query, ignoreCase = true)) return true
    // Search stays lenient on role casing: this only decides findability,
    // never what wires. The wire gates stay strict (== "user"), so an
    // unknown-cased role fails closed to bare text.
    val photoCount = if (role.equals("user", ignoreCase = true)) attachments.wiredPhotoCount() else 0
    return photoCount > 0 &&
        contentWithMarker(content, photoCount).contains(query, ignoreCase = true)
}

/** Wire form of one attachment: the `image_url` data URI the gateway's
 * multipart parser accepts. Remote URLs are never produced. The type is
 * PINNED, not rendered from storage: every payload is JPEG re-encoded at pick
 * time, so a hand-edited thread file cannot widen the wire type. Precondition:
 * blank payloads must be filtered via wiredPhotos() first — an empty data URI
 * is the corrupt part the wire filter exists to prevent, so this throws
 * NamedAttachError. */
fun ChatAttachment.dataUri(): String {
    if (base64.isBlank()) {
        throw NamedAttachError(
            "ChatAttachment.dataUri requires a non-blank payload (filter wiredPhotos() first)",
        )
    }
    // Decodability asserted too, not just non-blankness: a wrapped ("AAA=\n")
    // or ragged ("!!!") payload must fail here — never emit a corrupt data:
    // URI while isWired() counts it as a photo.
    if (!isDecodableBase64(base64)) {
        throw NamedAttachError(
            "ChatAttachment.dataUri requires a decodable payload (ragged base64 never wires)",
        )
    }
    // Per-photo cap, enforced — not just staged: the 4M-char ceiling admits a
    // hand-edited multi-MB valid payload that stage-time caps never saw.
    // Without this wall it wires until the generic ~3.5 MB conversation
    // preflight fails it; with it the send fails with the SAME named per-photo
    // message as the stage path. Thrown (not returned) by design: streamChat
    // maps it to a ChatEvent.Error, so the throw has a delivery path.
    oversizeMessage()?.let { throw NamedAttachError(it) }
    return "data:image/jpeg;base64,$base64"
}

/** Inline marker for text-only surfaces (exports, search, titles):
 * `[1 photo attached]` / `[3 photos attached]`.
 *
 * The marker counts WIRE (blank-only) photos, while the image parts need
 * decodable ones (#339): a hand-edited ragged tail therefore titles,
 * exports and searches as a photo while it wires as bare text. Deliberate
 * and documented at both ends — the marker describes the THREAD, the parts
 * must be provable. */
fun attachmentMarker(n: Int): String =
    if (n == 1) "[1 photo attached]" else "[$n photos attached]"

/** User-typed marker lookalike: `[N photo(s) attached]` ANYWHERE in the text
 * (not just trailing) — a mid-string lookalike would otherwise ride with two
 * identical markers for one photo. No `$` anchor by design: the real marker
 * is appended AFTER escaping, so it can never self-quote. Case-insensitive by
 * design, so a mixed-case lookalike quotes like the canonical one. */
private val literalMarker = Regex("""\[(\d+ photos?) attached\]""", RegexOption.IGNORE_CASE)

/**
 * Quotes a user-typed marker lookalike ANYWHERE in the text: a user who types
 * `[1 photo attached]` would otherwise read `"<typed> [1 photo attached]"`,
 * indistinguishable from a real photo turn. Quoted, not backslash-escaped: the
 * escape (`\[`) renders as `[` in Markdown and leaks a stray backslash in
 * plain-Text surfaces, while quotes disambiguate in every render. */
private fun quoteLiteralMarker(content: String): String =
    literalMarker.replace(content) { m -> "\"" + m.value + "\"" }

/**
 * Text plus its photo marker for trim-tolerant surfaces: search matching
 * ignores surrounding whitespace, so this trims. (Export and wire history use
 * [contentWithMarkerVerbatim] — content-preserving.) Empty text yields the
 * bare marker; zero photos yields the text untouched. Trim/preserve split:
 * this trims, the twin does not. */
fun contentWithMarker(content: String, n: Int): String =
    if (n <= 0) content
    else (quoteLiteralMarker(content.trim()) + " " + attachmentMarker(n)).trim()

/**
 * Verbatim twin of [contentWithMarker] for the wire history path: same marker
 * rule, no trim — history text rides exactly as typed with only the marker
 * appended. Kept as a named helper so the two spellings stay visibly paired.
 * Trim/preserve split: export uses this too; search uses the trimming twin.
 */
fun contentWithMarkerVerbatim(content: String, n: Int): String =
    if (n <= 0) {
        content
    } else if (content.isBlank()) {
        attachmentMarker(n)
    } else {
        quoteLiteralMarker(content) + " " + attachmentMarker(n)
    }

/** True when the message carries anything the agent should see. Text-only
 * checks (`content.isNotBlank()`) drop image-only turns from resends and
 * resume, so every history filter must use this instead. Counted wired: a
 * blank payload is not a photo anywhere; the attachment half is user-only,
 * matching the wire marker.
 *
 * Role check is strict == "user" like the wire gate: lenient-search vs
 * strict-wire is the deliberate split — a "User" turn stays findable but
 * never counts or wires. */
fun messageHasContent(m: ChatMessage): Boolean =
    m.content.isNotBlank() || (m.role == "user" && m.attachments.wiredPhotoCount() > 0)

/** Dimensions fitting `w`×`h` inside [MAX_IMAGE_SIDE_PX] on the long side,
 * preserving aspect ratio, never upscaling. */
fun downscaledSize(w: Int, h: Int): Pair<Int, Int> {
    // (0, 0) is unreachable via prepareAttachment (bounds <= 0 throws
    // first) — the guard protects any future caller from a sizable zero.
    if (w <= 0 || h <= 0) return 0 to 0
    val longest = maxOf(w, h)
    if (longest <= MAX_IMAGE_SIDE_PX) return w to h
    // Integer math, not floating point: (4000 * 1568 / 4000) is exactly 1568
    // while (4000 * (1568.0 / 4000)) can truncate. Long numerator: a 100 MP
    // panorama (w * cap) still fits in a Long.
    val nw = maxOf(1L, (w.toLong() * MAX_IMAGE_SIDE_PX) / longest).toInt()
    val nh = maxOf(1L, (h.toLong() * MAX_IMAGE_SIDE_PX) / longest).toInt()
    return nw to nh
}

/**
 * EXIF orientation → affine matrix, or null for NORMAL/unknown — the
 * transform BitmapFactory ignores, so a portrait capture would stage, send,
 * and render sideways. Shared by the stage path and the thumbnail decode.
 * POST reads in POINT order: postFoo(op) is M' = op * M, so each call applies
 * AFTER everything above it; preFoo would invite a translate-backwards bug.
 * Every case maps to POSITIVE coordinates via postTranslate (the negative-
 * scale flip recipe must never be load-bearing). The mirror composes AFTER
 * the rotation (TRANSPOSE = 90° + flip-H, TRANSVERSE = 270° + flip-H).
 * Pinned matrix: 1=identity, 2=flip-H, 3=rot180, 4=flip-V, 5=transpose,
 * 6=rot90, 7=transverse, 8=rot270 — pinned by ExifMatrixTest's 2x1 corner
 * probe; do not simplify the mirror cases or drop the explicit translates. */
internal fun orientationMatrix(orientation: Int, w: Float, h: Float): Matrix? =
    when (orientation) {
        ExifInterface.ORIENTATION_ROTATE_90 -> Matrix().apply {
            postRotate(90f)
            postTranslate(h, 0f)
        }
        ExifInterface.ORIENTATION_ROTATE_180 -> Matrix().apply {
            postRotate(180f)
            postTranslate(w, h)
        }
        ExifInterface.ORIENTATION_ROTATE_270 -> Matrix().apply {
            postRotate(270f)
            postTranslate(0f, w)
        }
        ExifInterface.ORIENTATION_FLIP_HORIZONTAL -> Matrix().apply {
            postScale(-1f, 1f)
            postTranslate(w, 0f)
        }
        ExifInterface.ORIENTATION_FLIP_VERTICAL -> Matrix().apply {
            postScale(1f, -1f)
            postTranslate(0f, h)
        }
        ExifInterface.ORIENTATION_TRANSPOSE -> Matrix().apply {
            postRotate(90f)
            postTranslate(h, 0f)
            postScale(-1f, 1f)
            postTranslate(h, 0f)
        }
        ExifInterface.ORIENTATION_TRANSVERSE -> Matrix().apply {
            postRotate(270f)
            postTranslate(0f, w)
            postScale(-1f, 1f)
            postTranslate(h, 0f)
        }
        else -> null
    }

/**
 * Wire shape of one message's content: a bare string normally, the multipart
 * array when the turn carries photos. Sealed (not `Any`) so callers branch
 * exhaustively — a reader that assumes String fails to compile instead of
 * crashing only on photo turns. */
sealed interface WireContent {
    data class Text(val text: String) : WireContent
    /** Multipart photo turn. Plain class, not data: JSONArray has identity
     * equals/hashCode, so data-class equality would mislead — and the mutable
     * array must not pretend to be a value. */
    class Multipart(val parts: JSONArray) : WireContent
}
/**
 * Wire content for one message: [WireContent.Text] normally, the multipart
 * array when this user turn carries photos.
 *
 * Only the LAST user turn keeps its image parts ([includeImages]): the server
 * prompts Pi with the trailing user turn only, and Pi holds history in its
 * own session file — so earlier turns ride as text and the body stays
 * bounded. An image-only turn is images alone. */
fun messageContentJson(
    role: String,
    content: String,
    attachments: List<ChatAttachment>,
    includeImages: Boolean,
): WireContent {
    // Fail-closed on payload, not just presence: an empty base64 would wire as
    // data:image/jpeg;base64, — a corrupt part the gateway would feed Pi. Empty
    // payloads count as no attachment; the marker count is blank-only.
    // STAGED-HONEST vs WIRE-STRICT split: the over-limit preflight counts
    // decodable (it advises the SEND — "remove a photo" must name photos that
    // would actually wire), while this marker counts wired (it describes the
    // THREAD).
    val markerCount = attachments.wiredPhotoCount()
    // Role check is strict == "user" here, the wire half of the deliberate
    // split documented on messageMatchesQuery (lenient search) and
    // messageHasContent (strict content).
    if (role != "user" || !includeImages) {
        // Stripped earlier photo turns stay honest: any other reader of this
        // history should see that photos were here — not a blank. Verbatim twin
        // (NOT the trimming helper): history rides exactly as typed.
        // STRICT ROLE, on purpose (#339): a hand-edited "User"/"System" tail
        // with photos drops them entirely (no image part AND no marker) —
        // fail-closed, and the same strict rule the image index uses, so
        // index, marker and wire agree on what rode. Only canonical "user"
        // turns keep photos, and the app only ever writes those.
        if (role == "user" && markerCount > 0 && !includeImages) {
            return WireContent.Text(contentWithMarkerVerbatim(content, markerCount))
        }
        // Assistant turns never stage photos: a bare return keeps a
        // hypothetical assistant-with-attachments payload from widening the
        // wire shape.
        return WireContent.Text(content)
    }
    // Trailing turn: parts need PROVEN payloads. Ragged payloads filter here —
    // validity is send-frequency, so the alphabet scan lives at the wire,
    // never in the hot-path isWired() predicate. dataUri() re-validates per
    // part (2–3 walks per photo per send — accepted): enforcement lives in
    // the lowest common function every path funnels through.
    val images = attachments.filter { it.isWired() && isDecodableBase64(it.base64) }
    if (images.isEmpty()) {
        // Marker when text survives, bare when nothing does: an image-only
        // ragged turn must NOT gain a marker-only text — that would sail past
        // the server's named blank-turn refusal and hand the agent a photo
        // claim with no image.
        return if (markerCount > 0 && content.isNotBlank()) {
            WireContent.Text(contentWithMarkerVerbatim(content, markerCount))
        } else {
            WireContent.Text(content)
        }
    }
    val arr = JSONArray()
    if (content.isNotBlank()) {
        arr.put(JSONObject().put("type", "text").put("text", content))
    }
    for (a in images) {
        // Validated by construction: the filter proved wired + decodable, and
        // the per-photo cap wall lives inside dataUri().
        arr.put(
            JSONObject()
                .put("type", "image_url")
                .put("image_url", JSONObject().put("url", a.dataUri()))
        )
    }
    return WireContent.Multipart(arr)
}

/** Exact decoded size of a base64 payload: the naive 3/4 rule overstates by
 * the padding (1–2 B), so named limit errors would name a wrong cap-adjacent
 * number. Precondition: valid base64 (length % 4 == 0) — a ragged length
 * floors down by up to 2 B, acceptable for approximate error strings.
 * Framing whitespace is excluded inline — ALL of it: decode skips spaces/tabs
 * too. Single counting pass, no copies. Padding counts trailing-only:
 * interior '=' resets the run, so "AB=C" measures 3 content chars → 2 B. */
fun base64Bytes(base64: String): Long {
    var content = 0
    var tailPad = 0
    for (c in base64) {
        if (c.isWhitespace()) continue
        if (c == '=') {
            tailPad++
        } else {
            content++
            tailPad = 0
        }
    }
    // Multiply first: (3/4)*3 truncates to 0 while (3*3)/4 = 2 — division
    // order is load-bearing for ragged inputs like "AB=C".
    return maxOf(0L, ((content + tailPad) * 3L) / 4L - minOf(tailPad, 2))
}

/** `1.1 MB`-style rendering of [bytes] for limit errors. One decimal at MB
 * and above, integer KB below that, plain count below 1000 B (never blank).
 * Pinned to US formatting: user-visible strings must not render `1,1 MB`.
 * Never renders a cross-unit value: the KB branch floors up to 999_950, where
 * 1-decimal MB reads "1.0". Negative input renders "unknown size".
 * Decimal SI throughout (1000-based KB/MB), while the backstop is the
 * gateway's 4 MiB (1024-based) body cap: the units differ by ~5%, but
 * 3.5 M bytes refuses well below 4 MiB either way. */
fun formatBytes(bytes: Long): String {
    if (bytes < 0) return "unknown size"
    if (bytes < 1_000L) return "$bytes B"
    // Floor + 999_950 threshold per the KDoc above: no cross-unit values.
    if (bytes < 999_950L) return "${bytes / 1_000} KB"
    return "${"%.1f".format(java.util.Locale.US, bytes / 1_000_000.0)} MB"
}

/**
 * Our own attach failures, typed so the toast mapping never depends on
 * message wording: [namedReadError] passes these (and the oversize
 * IllegalStateException) through and remaps every system throwable to the
 * generic unreadable-photo error — rewording a message can't flip its
 * bucket, only changing the type can. */
internal class NamedAttachError(message: String) : IllegalArgumentException(message)

/** Size-cap failures, typed apart from every other IllegalStateException:
 * providers and framework code throw ISE too, so passing through all ISE
 * would surface system text in the toast — breaking the "never system text,
 * logcat-only" contract. */
internal class OversizeAttachError(message: String) : IllegalStateException(message)

/**
 * Maps a read failure onto the named error family. Our own throws pass
 * through; system throwables (SecurityException on a revoked picker grant,
 * FileNotFoundException, provider errors) become the generic unreadable-photo
 * error. Raw publisher wording is logcat-only — never system text in a toast. */
private fun namedReadError(e: Throwable): Throwable {
    // Cancellation is not a read failure: remapping it would toast "couldn't
    // read" for a user-initiated abort and break structured concurrency.
    // Errors (OOM and friends) rethrow for the same reason: fail loud.
    if (e is CancellationException || e is Error) throw e
    runCatching { Log.w("ChatAttachments", "photo read failed", e) }
    // Expired picker grant, diagnosed honestly: a SecurityException here is
    // revocation, not a moved file — re-picking (fresh grant) is the fix.
    if (e is SecurityException) {
        return NamedAttachError(
            "Photo permission expired — pick it again.",
        )
    }
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
 * [MAX_IMAGE_BASE64_CHARS] fails with a NAMED error — never a silent drop. */
suspend fun prepareAttachment(context: Context, uri: Uri): Result<ChatAttachment> =
    withContext(Dispatchers.IO) {
        runCatching {
            val resolver = context.contentResolver
            // Known-size fast fail: a mislabeled video reports its size
            // through OpenableColumns, so refuse it BEFORE the read allocates
            // anything (which would peak ~64 MB on the shape the cap exists
            // for). Unknown size (-1) falls through to the bounded read.
            val knownBytes = runCatching {
                resolver.query(uri, arrayOf(android.provider.OpenableColumns.SIZE), null, null, null)
                    ?.use { c ->
                        val i = c.getColumnIndex(android.provider.OpenableColumns.SIZE)
                        if (i >= 0 && c.moveToFirst()) c.getLong(i) else -1L
                    } ?: -1L
            }.getOrDefault(-1L)
            if (knownBytes > MAX_SOURCE_BYTES) {
                throw OversizeAttachError(
                    "That file is too large to attach " +
                        "(over about ${formatBytes(MAX_SOURCE_BYTES.toLong())}).",
                )
            }
            // Scoped read+decode: bounds, sample, pixels, and EXIF all run
            // inside this block from one bounded read, so `bytes` falls out of
            // scope before rotation/scale/compress — the source buffer is
            // GC-eligible before the real hump.
            val decoded = run {
                val bytes = resolver.openInputStream(uri)?.use { stream ->
                // Pre-sized 1 MB, or the provider's known size when in range.
                val out = ByteArrayOutputStream(
                    if (knownBytes in 1L..MAX_SOURCE_BYTES.toLong()) knownBytes.toInt() else 1 shl 20,
                )
                val buf = ByteArray(8192)
                var total = 0L
                // A 0 return is legal but progress-less: bail after a streak
                // instead of spinning on a stuck provider stream.
                var zeroStreak = 0
                while (true) {
                    // Cooperative cancel on the progressing path too: yield()
                    // only ran on zero-reads, so a slow cloud-URI stream held
                    // an IO thread past tab-leave.
                    ensureActive()
                    val n = stream.read(buf)
                    // < 0 only: 0 is a legal (if rare) provider-stream return.
                    if (n < 0) break
                    if (n == 0) {
                        // A stall, not a deletion: 100 consecutive 0-reads is a
                        // stuck provider stream, where retrying the same Uri
                        // may succeed — so this says try again.
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
                    // The shrink happens after the read, so the ORIGINAL needs
                    // its own bound. Names only the cap: total is bytes-read-
                    // so-far, so quoting it would present a partial count.
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
                // A null stream is not proof of deletion: some providers return
                // null for a still-valid pick, so the wording names what
                // happened — "open" — instead of asserting the file moved.
                "Couldn't open that photo — pick it again.",
            )
            val bounds = BitmapFactory.Options().apply { inJustDecodeBounds = true }
            BitmapFactory.decodeByteArray(bytes, 0, bytes.size, bounds)
            if (bounds.outWidth <= 0 || bounds.outHeight <= 0) {
                throw NamedAttachError(
                    "That file isn't a readable photo — Agento attaches images only, not documents.",
                )
            }
            // Sample toward JUST ABOVE the target box: advance only while the
            // next sample still covers it, so the decoded bitmap stays >= the
            // target and the exact fit below is always a downscale. Long
            // counter: the toInt() handoff is small.
            // Pixel budget too, not just sides (#339): the side rule alone
            // lets a source just under 2x the target decode at FULL size
            // (3135x3135 ≈ 39 MB), and that bitmap lives beside the source
            // bytes. 4 M pixels ≈ 16 MB per decode.
            var sample = 1L
            while (bounds.outWidth / (sample * 2) >= MAX_IMAGE_SIDE_PX ||
                bounds.outHeight / (sample * 2) >= MAX_IMAGE_SIDE_PX ||
                (bounds.outWidth.toLong() / sample) * (bounds.outHeight.toLong() / sample) > MAX_DECODE_PIXELS
            ) {
                sample *= 2
            }
            val opts = BitmapFactory.Options().apply { inSampleSize = sample.toInt() }
            val fresh = BitmapFactory.decodeByteArray(bytes, 0, bytes.size, opts)
                ?: throw NamedAttachError(
                    "Couldn't read that photo — it may have been moved or deleted. Pick it again.",
                )
                // EXIF orientation: BitmapFactory ignores it, so a portrait phone
                // photo would stage and send sideways. Flips included: selfies
                // carry FLIP_HORIZONTAL/TRANSVERSE.
                val orient = try {
                    java.io.ByteArrayInputStream(bytes).use {
                        ExifInterface(it).getAttributeInt(
                            ExifInterface.TAG_ORIENTATION,
                            ExifInterface.ORIENTATION_NORMAL,
                        )
                    }
                } catch (e: CancellationException) {
                    // Never masked into ORIENTATION_NORMAL: a tab-leave cancel
                    // landing mid-EXIF-read must surface, not continue into
                    // rotate/scale/compress with a defaulted orient. Same
                    // rethrow-both rule as namedReadError/decodeThumbnail. The
                    // decoded bitmap is recycled first: this throw happens
                    // before the try/finally below owns it, so without this the
                    // 1568px bitmap leaks until GC.
                    fresh.recycle()
                    throw e
                } catch (e: Error) {
                    fresh.recycle()
                    throw e
                } catch (_: Exception) {
                    ExifInterface.ORIENTATION_NORMAL
                }
                fresh to orient
            }
            var bitmap = decoded.first
            val orientation = decoded.second
            // One try/finally from here down: rotation, scaling and compression
            // each allocate or throw, and any throw must recycle the live
            // bitmap — the finally covers every throw between them.
            // Cooperative cancel between the native steps: decode, rotate,
            // fill, and scale each run hundreds of ms with no suspension point
            // — a tab-leave must surface at each gate. Declared outside
            // (""): nothing measured yet, so a throw before the first
            // candidate must report "too large to process". Last attempt, not
            // min/max: quality descends, so the last measurement is closest.
            var encoded: String? = null
            var lastCandidate = ""
            try {
                ensureActive()
                // One table for the stage path and the thumbnail decode (see
                // orientationMatrix), so a portrait photo is upright everywhere.
                val matrix: Matrix? = orientationMatrix(
                    orientation,
                    bitmap.width.toFloat(),
                    bitmap.height.toFloat(),
                )
                if (matrix != null) {
                    val rotated = Bitmap.createBitmap(bitmap, 0, 0, bitmap.width, bitmap.height, matrix, true)
                    // Referential on purpose: only a new instance may recycle
                    // the old one.
                    if (rotated !== bitmap) {
                        bitmap.recycle()
                        bitmap = rotated
                    }
                }
                ensureActive()
                val (tw, th) = downscaledSize(bitmap.width, bitmap.height)
                // Downscale FIRST, fill second: the old order filled at full
                // size then scaled, holding sampled + opaque-sampled + scaled
                // live for alpha screenshots. Scaling first allocates the fill
                // at target size — same pixels out, ~half the transient.
                if (bitmap.width != tw || bitmap.height != th) {
                    val scaled = Bitmap.createScaledBitmap(bitmap, tw, th, true)
                    if (scaled !== bitmap) {
                        bitmap.recycle()
                        bitmap = scaled
                    }
                }
                // Opaque white fill, only when needed: JPEG has no alpha
                // channel, so a transparent screenshot/icon would otherwise
                // flatten onto black. hasAlpha()-gated.
                if (bitmap.hasAlpha()) {
                    val opaque = Bitmap.createBitmap(tw, th, Bitmap.Config.ARGB_8888)
                    android.graphics.Canvas(opaque).apply {
                        drawColor(android.graphics.Color.WHITE)
                        drawBitmap(bitmap, 0f, 0f, null)
                    }
                    if (opaque !== bitmap) {
                        bitmap.recycle()
                        bitmap = opaque
                    }
                }
                ensureActive()
                for (quality in listOf(80, 60)) {
                    // Cooperative cancel like the read loop: two full JPEG
                    // encodes with no suspension would hold the IO thread past
                    // tab-leave until both land.
                    ensureActive()
                    // Pre-sized like the read buffer: the default 32-byte
                    // buffer would double its way to ~1.5 MB per attempt.
                    val out = ByteArrayOutputStream(1 shl 20)
                    // Peak note: bitmap (~10 MB at 1568px) + out bytes +
                    // base64 string co-exist here, plus the transient opaque
                    // fill above when the source had alpha. Checked, not
                    // assumed: a false return leaves out empty, and an empty
                    // candidate would pass the cap as a zero-byte "photo" —
                    // silent corrupt payload with no named error.
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
                // Sizes exact via base64Bytes and shared spelling via
                // oversizeMessageFor: the dataUri wall says exactly this.
                if (lastCandidate.isNotEmpty()) {
                    oversizeMessageFor(lastCandidate)
                } else {
                    "Photo too large to process — " +
                        "the per-photo limit is about " +
                        "${formatBytes(MAX_IMAGE_BASE64_CHARS * 3L / 4L)}. " +
                        "Pick a different photo."
                },
            )
            ChatAttachment(mimeType = "image/jpeg", base64 = final, id = java.util.UUID.randomUUID().toString())
        }.let { result ->
            // Remap system throwables onto the named family (see namedReadError).
            result.exceptionOrNull()?.let { Result.failure(namedReadError(it)) } ?: result
        }
    }

/**
 * Small bitmap for thumbnails (bubbles, composer strip). Decodes sampled to
 * ~256px so a remembered thumbnail costs kilobytes, not the full payload.
 * Null when the payload is corrupt — it degrades to a missing thumbnail.
 * Ownership: the returned bitmap is LIVE and belongs to the caller (the
 * thumbnail cache keeps it; do not recycle it behind the cache's back). */
suspend fun decodeThumbnail(base64: String): Bitmap? {
    // Explicit try/catch, not runCatching: cancellation must propagate
    // (produceState restart on recompose), never collapse into the null
    // placeholder path like a corrupt payload does. IO-ONLY caller contract:
    // Base64.decode + two full decodeByteArrays run synchronously with no
    // suspension point — a Main-thread caller would jank the whole decode.
    requireIoThread("decodeThumbnail")
    return try {
        // Absolute raw ceiling first: no scan or decode runs on giants
        // regardless of decoded size. Decodability BEFORE the precise measure:
        // an over-length ragged payload would otherwise pay the base64Bytes
        // walk and then the isDecodableBase64 walk before failing anyway. The
        // remaining double-walk (measure + validate on an over-length VALID
        // payload) is structural: base64Bytes tolerates whitespace while
        // isDecodableBase64 refuses it, so no single walk proves both.
        if (base64.length > MAX_RAW_BASE64_CHARS) return null
        if (!isDecodableBase64(base64)) return null
        if (base64.length > MAX_IMAGE_BASE64_CHARS &&
            base64Bytes(base64) > MAX_IMAGE_BASE64_CHARS * 3L / 4L
        ) {
            return null
        }
        // DEFAULT on decode is historical leniency, now moot: the gate above
        // already rejected anything DEFAULT would forgive.
        val bytes = Base64.decode(base64, Base64.DEFAULT)
        val bounds = BitmapFactory.Options().apply { inJustDecodeBounds = true }
        BitmapFactory.decodeByteArray(bytes, 0, bytes.size, bounds)
        if (bounds.outWidth <= 0 || bounds.outHeight <= 0) return null
        // Cancel point before the pixel decode: the bounds probe above
        // already spent a full decodeByteArray; the sampled decode below
        // spends another with no suspension inside the native call. Explicit
        // context receiver: this is a bare suspend fun with no CoroutineScope
        // receiver, so the plain ensureActive() used elsewhere would not
        // resolve here.
        currentCoroutineContext().ensureActive()
        // Long counter like the full-size loop: exits in ~12 iterations on
        // real headers (halving toward 256), so the toInt() handoff is small.
        var sample = 1L
        while (bounds.outWidth / sample > 256 ||
            bounds.outHeight / sample > 256
        ) {
            sample *= 2
        }
        var bmp = BitmapFactory.decodeByteArray(
            bytes, 0, bytes.size,
            BitmapFactory.Options().apply { inSampleSize = sample.toInt() },
        ) ?: return null
        // EXIF rotate: BitmapFactory ignores orientation, so a portrait capture
        // rendered sideways here while the staged bubble showed upright — same
        // orientationMatrix table as the stage path. Display-only: the wire
        // always sends the staged bytes, so a mis-read is a tilted thumbnail.
        // DEFENSE IN DEPTH (#339): staged payloads are EXIF-rotated at capture
        // and re-encoded (compress strips metadata), so this read returns
        // NORMAL for everything the app writes — it only matters for an
        // imported/hand-edited payload that kept its EXIF block. One parse
        // per visible row, paid for the imported case.
        val orientation = try {
            java.io.ByteArrayInputStream(bytes).use {
                ExifInterface(it).getAttributeInt(
                    ExifInterface.TAG_ORIENTATION,
                    ExifInterface.ORIENTATION_NORMAL,
                )
            }
        } catch (e: CancellationException) {
            // Same rethrow-both rule as the stage path.
            bmp.recycle()
            throw e
        } catch (e: Error) {
            bmp.recycle()
            throw e
        } catch (_: Exception) {
            ExifInterface.ORIENTATION_NORMAL
        }
        val matrix = orientationMatrix(orientation, bmp.width.toFloat(), bmp.height.toFloat())
        if (matrix != null) {
            val rotated = try {
                Bitmap.createBitmap(bmp, 0, 0, bmp.width, bmp.height, matrix, true)
            } catch (e: Throwable) {
                // The source bitmap is still live here: the createBitmap
                // call owns no reference of its own, so it must be
                // recycled before the throw leaves the function.
                bmp.recycle()
                throw e
            }
            // Referential on purpose: only a new instance may recycle the old.
            if (rotated !== bmp) {
                bmp.recycle()
                bmp = rotated
            }
        }
        bmp
    } catch (e: CancellationException) {
        throw e
    } catch (e: Error) {
        // Fail loud like the stage path: an OOM here must crash, never
        // collapse into a placeholder that hides a JVM-level failure.
        throw e
    } catch (e: Exception) {
        null
    }
}
