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
    /** Raw bytes as base64, no `data:` prefix — [dataUri] adds it for the wire.
     * Counting rule, stated where the payload lives (#339): a non-blank
     * payload counts as a photo on every DISPLAY surface (titles, search,
     * export, strip, markers) — including a ragged one load() never saw.
     * The wire is stricter (wired AND decodable) and fails closed on the
     * rest. Intentional, not drift: display answers "was a photo staged",
     * the wire answers "what may send", and load() blanks ragged on read
     * so the two agree in practice. Do not "fix" display to re-validate —
     * the alphabet scan would run per keystroke and per title render for a
     * case the boundary already heals. */
    val base64: String = "",
    /** Stable per-pick id, minted at stage time (#339): thumbnail keys and
     * any future per-photo identity use this, never the payload — so
     * removing photo 0 doesn't re-key photo 1, and two identical picks
     * never alias. Default empty so pre-id history still decodes (those
     * fall back to a content-sample key). */
    val id: String = "",
)

/**
 * Attachments that count as photos: non-blank payloads (#339). The name is
 * kept short for its ~30 call sites, so read this contract, not the name:
 * NON-BLANK-guaranteed, not wire-guaranteed. Blank payloads (hand-edited
 * threads, writer bugs) count as no attachment everywhere — the wire
 * filter, display markers, titles, and search must all agree, or a blank
 * wires as bare text while the UI claims a photo. Load-bearing assumption,
 * stated once: every persisted read flows through load(), which
 * canonicalizes (whitespace stripped, undecodable blanked) — so by the
 * time anything calls this, wrapped-or-ragged payloads are already gone.
 * Any future reader bypassing load() (backup import, debug share) must
 * canonicalize first or re-split the contract. Decodability specifically
 * is LOAD-guaranteed, not predicate-guaranteed (#339): isWired() stays
 * blank-only because the alphabet scan would run per search keystroke and
 * per title render, while sends (the only place validity matters) are
 * rare — so the wire filter asserts isDecodableBase64 per send instead.
 * The two agree because load blanked the ragged, not because the predicate
 * checks it. An in-memory construction bypassing load (unit-built
 * ChatMessage, a future receiver like TaskInstructReceiver) re-splits
 * them by design: ragged counts as a photo in display but wires as bare
 * text (fail-closed at the wire, never a corrupt part). Pinned by test
 * (`raggedPayloadCountsButWiresAsText`), not just prose.
 */
fun List<ChatAttachment>.wiredPhotos(): List<ChatAttachment> =
    filter { it.isWired() }

/** Count-only twin of [wiredPhotos] for hot paths (#339): search runs per
 * message per keystroke and titles render per turn — a `count {}` avoids
 * the throwaway list `filter {}` allocates without changing the contract. */
fun List<ChatAttachment>.wiredPhotoCount(): Int =
    count { it.isWired() }

/** Any photo that would actually wire, wired AND decodable (#339): for
 * surfaces that must agree with the wire filter (the preflight's "remove
 * a photo" advice), not just the blank-only display count — a ragged-only
 * turn must not name photos that never wire. Single pass, no list, same
 * cost class as the count above. */
internal fun List<ChatAttachment>.hasDecodablePhoto(): Boolean =
    any { it.isWired() && isDecodableBase64(it.base64) }

/** Single source of truth for the wired predicate (#339): both helpers
 * above delegate here, so the next edit that changes what counts (e.g.
 * trimming whitespace-only payloads) cannot silently re-split the
 * wire/display/cap contract. Blank-only: decode-validity (ragged alphabet,
 * bad padding) is checked at the wire filter, not here — a full or even
 * regex validation per call would run on search keystrokes, while sends
 * (the only place validity matters) are rare. */
private fun ChatAttachment.isWired(): Boolean = base64.isNotBlank()

/** Absolute raw-length ceiling for untrusted payloads (#339): no scan,
// copy, or decode runs past this many chars no matter what the decoded
 * size would be — 100 MB of spaces + a tiny payload must not buy a full
 * walk plus a full copy before blanking. 4 M chars sits far above any
 * legitimate payload (~1.5 M + framing whitespace) and far below danger.
 * Checked FIRST (O(1)) in every validity/scan path below. */
const val MAX_RAW_BASE64_CHARS = 4_000_000

/** Decode-validity for the wire filter (#339): blank payloads never reach
 * here (isWired), but a hand-edited ragged payload (bad length, non-base64
 * alphabet) must not emit `data:...;base64,<ragged>` into an image_url
 * part — the gateway would 400 what should be a client-side named shape.
 * Alphabet scan, no decode: runs per send, never per keystroke. Internal:
 * the persistence loader canonicalizes with this too, so memory holds no
 * wrapped-or-ragged payloads and the hot paths never need this check.
 * STRICT on whitespace BY DESIGN (not an oversight vs the lenient pair
 * below): `base64Bytes` skips whitespace and `Base64.decode(DEFAULT)`
 * accepts it, but the wire must never emit a wrapped/multiline data: URI —
 * a wrapped-but-valid payload measures a real size and decodes fine, yet
 * FAILS this gate and wires as bare text. The three helpers deliberately
 * disagree: measure/decode tolerate (they read bytes), validate refuses
 * (it guards the wire shape). Do not "fix" one side to match the others:
 * the split is load-healed in practice (loader strips framing first), and
 * the strict gate is what keeps a hand edit from widening the wire.
 * Padding is positional, not counted: `=` is legal only as the last ≤2
 * chars — `=AAA`, `A=A=`, `AB=C` all fail — since a bare count would pass
 * interior padding straight into the corrupt part this exists to prevent.
 * Standard alphabet only (`+/`, never URL-safe `-_`): the encoder emits
 * standard base64 exclusively, so a `-_` payload is foreign input and
 * fail-closed blanks it — a future share path emitting URL-safe must
 * normalize first, or its photos will wire as text-only with no marker
 * (silent-looking data loss, by this design on purpose). */
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

/** Wired payloads with their staged indices (#339): the strip renders
 * wired but removes by staged slot, so it needs both — without
 * re-rolling the predicate (or the remove index) at the call site. */
fun List<ChatAttachment>.wiredPhotosWithIndex(): List<IndexedValue<ChatAttachment>> =
    mapIndexedNotNull { i, a -> if (a.isWired()) IndexedValue(i, a) else null }

/**
 * IO-thread gate for the persistence boundary (#339): the load/save/
 * export/import contracts all CLAIM Main-never, and a comment alone never
 * stopped a future Main-thread caller. Looper-exact on device
 * (myLooper() === mainLooper); on JVM unit tests the android.jar stubs
 * throw, which reads as "no Looper, no Main thread here" and passes —
 * the suite therefore CANNOT pin the throw path (documented, not hidden:
 * a device-only Main-thread call is caught on-device, and every caller
 * today runs inside withContext(Dispatchers.IO) anyway). No thread-name
 * heuristic: any executor may name a worker "main", which would
 * false-positive, while the Looper identity already proves Main-ness
 * where it matters. The pre-prepare window (myLooper() null
 * everywhere) needs no handling: no app code — and therefore no
 * boundary call — runs before ActivityThread prepares the main Looper.
 * Deliberately NOT on metadata-only helpers
 * (backupFilesBytes stat()s sizes without reading): the gate is for
 * callers that could block, not for every function that touches the
 * filesystem. Deliberately NOT on canonicalizeAttachments/isDecodableBase64:
 * those are pure per-payload helpers (unit-tested on JVM), and their IO
 * confinement comes from their boundary callers (load/save/export),
 * which are gated.
 */
internal fun requireIoThread(site: String) {
    // runCatching (not try/catch): the fallback value documents the JVM
    // case inline, with no unused-exception binding to trip lint.
    val onMain = runCatching {
        val mainLooper = android.os.Looper.getMainLooper()
        android.os.Looper.myLooper() !== null && android.os.Looper.myLooper() === mainLooper
    }.getOrDefault(false)
    check(!onMain) {
        "$site must run on an IO thread, never Main"
    }
}

/**
 * Canonical form of persisted attachments (#339): storage is
 * hand-editable, so payloads arrive stripped of framing whitespace — and
 * anything still undecodable blanks out. Applied once at the load
 * boundary (IO, never Main), so memory holds no wrapped-or-ragged
 * payloads: the hot-path isWired() can stay a blank check while the wire
 * filter asserts decode-validity, because load made them agree. Pure for
 * testability; the next persist writes back the canonical form
 * (self-healing, no notice — corrupt bytes are dropped, never sent).
 * Heal-silent BY DESIGN (#339): heals log nothing and voice nothing — a
 * toast per healed payload would storm on every cold start (the hand-edit
 * persists), and the photo's absence with a zero count IS the signal.
 * Fail-closed preserved: nothing corrupt ever wires; the named-error
 * contract covers user actions (pick/stage/send), not storage repair.
 */
fun canonicalizeAttachments(attachments: List<ChatAttachment>): List<ChatAttachment> {
    // Empty fast path (#339): most messages hold no photos — returning an
    // empty list skips both the per-message copy and the walk for every
    // one of them, on every save/load. A FRESH emptyList(), never the
    // input instance: the contract is enforced structurally, not by
    // comment — no caller, present or future, can alias storage through
    // a shared reference. (emptyList() is a singleton: zero alloc.)
    if (attachments.isEmpty()) return emptyList()
    return attachments.map { a ->
        // Clean-first ordering, stated (#339): the two pre-scans
        // short-circuit (first bad char / first non-space), so clean
        // payloads — everything this app writes — pay one full walk and
        // zero copies. Strip-first would tax every clean payload with a
        // ~1.5 M-char copy to spare the rare wrapped shape its short
        // pre-scans: the wrong tradeoff. No length pre-check before the
        // whitespace walks, also deliberate (#339): a length-only blank
        // would nuke a valid multi-MB payload that deserves the named
        // oversize error (dataUri/streamChat), not silent blanking — and
        // no cap below the 4M ceiling separates "big whitespace" from "big
        // photo" without walking. The 4M raw ceiling bounds the worst
        // case; per-save repetition is load-canonical memory re-proving
        // itself (see save()'s dirty-tracking note), and every walk here
        // is IO-confined, never Main, never the send path.
        // Single-walk fast path (#339): isDecodableBase64 already rejects
        // whitespace, so no separate none{isWhitespace} proof walk — clean
        // payloads (everything this app writes) validate allocation-free
        // and return untouched. Giants never reach the strip copy: the
        // raw ceiling inside isDecodableBase64 fails them O(1), and the
        // explicit length check below blanks them without building a
        // 100 MB filtered copy first. Worst case below the ceiling is two
        // walks + one bounded copy, all IO-confined (#339) — and both
        // walks short-circuit: validity fails at the first bad char,
        // all{} fails at the first non-whitespace, so only a fully-valid
        // (then stripped + revalidated) or fully-homogeneous payload pays
        // both. Adversarial ceiling for the whole branch is the 4M-char
        // raw cap: bounded CPU on IO, bounded copies, never the send path.
        if (isDecodableBase64(a.base64)) {
            a
        } else if (a.base64.length > MAX_RAW_BASE64_CHARS) {
            a.copy(base64 = "")
        } else if (a.base64.all { it.isWhitespace() }) {
            // All-whitespace just under the ceiling (#339): blank without
            // building the ~N-char filtered copy to discover it — the exact
            // adversarial shape the ceiling exists for, allocation-free.
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

/** Max photos per turn (#318). Two is the acceptance bar (two images in one
 * turn render and reach the agent); the cap keeps the wire body under the
 * gateway's 4 MiB limit (see the invariant test in [ChatAttachmentsTest]). */
const val MAX_IMAGES_PER_TURN = 2

/** Consecutive unreadable picks before a batch aborts (#339): failures
 * yield to the next Uri by design, but each burns a full prepareAttachment
 * (up to 32 MB read + decodes + EXIF) — without a bound, one bad batch
 * spins through the whole picker result. Mixed batches are safe: only
 * CONSECUTIVE failures count, so bad-good-bad still stages the good. */
const val MAX_CONSECUTIVE_ATTACH_FAILURES = 3

/**
 * Single wording family for the whole per-turn cap (#339): tap-time
 * refusal, tap-time excess, mid-batch exhaustion, and the ViewModel
 * backstop all share the head with Info styling — nothing failed, the
 * cap held, so neither the head nor the styling may disagree by path.
 * The tail is conditional on free slots: a full composer ("remove one
 * first") is a different action from a partial stage ("extras dropped"),
 * and the unified string used to misname the no-op full-refusal path.
 */
fun capRefused(freeSlots: Int): String =
    if (freeSlots <= 0) {
        "At most $MAX_IMAGES_PER_TURN photos per turn — the composer is full, remove one first."
    } else {
        "At most $MAX_IMAGES_PER_TURN photos per turn — extra photos were dropped."
    }

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

/**
 * Per-photo oversize message for one attachment, or null when within the
 * cap (#339): the wording [dataUri] throws and the stage path reports.
 * One spelling, one cap — callers never reword it. Two-tier like the
 * thumbnail gate: at/below-cap raws skip the precise measure; only
 * over-length raws pay the full walk.
 */
internal fun ChatAttachment.oversizeMessage(): String? {
    if (base64.length <= MAX_IMAGE_BASE64_CHARS) return null
    val bytes = base64Bytes(base64)
    if (bytes <= MAX_IMAGE_BASE64_CHARS * 3L / 4L) return null
    return oversizeMessageFor(base64)
}

/**
 * Shared per-photo oversize wording (#339): the stage failure and the
 * dataUri wall say exactly this — the next reword fixes both paths instead
 * of one. Takes the raw
 * payload, not the attachment: the stage path measures candidates that
 * never become attachments.
 */
internal fun oversizeMessageFor(base64: String): String {
    val bytes = base64Bytes(base64)
    return "Photo too large even after shrinking (${formatBytes(bytes)}) — " +
        "the per-photo limit is about " +
        "${formatBytes(MAX_IMAGE_BASE64_CHARS * 3L / 4L)}. " +
        "Pick a different photo."
}

/**
 * Per-message search predicate, shared by thread search (#339): raw-text
 * region match first (no marker alloc on hits), marker only on miss.
 * Case-folding is Unicode region-matching (ignoreCase), NOT
 * ROOT-lowercasing, BY DECISION — pinned by the Turkish-folding tests
 * below (`I` matches dotless `ı` under region matching, misses under
 * ROOT-lowercasing). A future "restore lowercase for locale safety" edit
 * must keep those tests green, which it cannot do without keeping this
 * predicate. Same rule on both branches (raw and marker): one folding,
 * no split-brain locale behavior.
 */
internal fun messageMatchesQuery(
    role: String,
    content: String,
    attachments: List<ChatAttachment>,
    query: String,
): Boolean {
    if (content.contains(query, ignoreCase = true)) return true
    val photoCount = if (role == "user") attachments.wiredPhotoCount() else 0
    return photoCount > 0 &&
        contentWithMarker(content, photoCount).contains(query, ignoreCase = true)
}

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
    // Decodability asserted too, not just non-blankness (#339): the wire
    // filter requires wired + decodable, so a wrapped ("AAA=\n") or ragged
    // ("!!!") payload reaching a direct caller must fail here — never emit
    // a corrupt multiline/invalid data: URI while isWired() counts it as a
    // photo and the wire drops it (UI claiming a photo that wires as bare
    // text). Unreachable via canonical memory (load blanks these); this is
    // the wall for future direct callers.
    if (!isDecodableBase64(base64)) {
        throw NamedAttachError(
            "ChatAttachment.dataUri requires a decodable payload (ragged base64 never wires)",
        )
    }
    // Per-photo cap, enforced — not just staged (#339): the 4M-char
    // ceiling admits a hand-edited multi-MB valid payload that stage-time
    // caps never saw. Without this wall it wires until the generic ~3.5 MB
    // conversation preflight fails it; with it the send fails with the SAME
    // named per-photo message as the stage path. Unreachable via the app
    // (staged payloads are capped at pick time) — storage-injected
    // oversize only. Thrown (not returned) by design: streamChat maps it
    // to a ChatEvent.Error at the build site, so the throw always has a
    // delivery path and can never wedge a turn again.
    oversizeMessage()?.let { throw NamedAttachError(it) }
    return "data:image/jpeg;base64,$base64"
}

/** Inline marker for text-only surfaces (exports, search, titles):
 * `[1 photo attached]` / `[3 photos attached]`. */
fun attachmentMarker(n: Int): String =
    if (n == 1) "[1 photo attached]" else "[$n photos attached]"

/** Trailing user-typed marker lookalike (#339): `[N photo(s) attached]`
 * ANYWHERE in the text (not just trailing) — a mid-string lookalike
 * ("[1 photo attached] hello" + a real photo) would otherwise ride with
 * two identical markers for one photo. No `$` anchor by design: the real
 * marker is appended AFTER escaping, so it can never self-quote. */
private val literalMarker = Regex("""\[(\d+ photos?) attached\]""")

/**
 * Quotes a user-typed marker lookalike ANYWHERE in the text (#339): a user who
 * literally types `[1 photo attached]` would otherwise read
 * `"<typed> [1 photo attached]"`, indistinguishable from a real photo
 * turn on text surfaces. Quoted, not backslash-escaped: the escape
 * (`\[`) renders as `[` in Markdown (an invisible distinction) and leaks
 * as a stray backslash in plain-Text surfaces — user bubbles are Text(),
 * and the sharesheet shares raw text — while quotes disambiguate in every
 * render. The wire parts stay authoritative — this only disambiguates the
 * hint, never the send.
 */
private fun quoteLiteralMarker(content: String): String =
    literalMarker.replace(content) { m -> "\"" + m.value + "\"" }

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
    if (n <= 0) content
    else (quoteLiteralMarker(content.trim()) + " " + attachmentMarker(n)).trim()

/**
 * Verbatim twin of [contentWithMarker] for the wire history path (#339):
 * same marker rule, no trim — history text rides exactly as typed
 * (pasted-code indentation included) with only the marker appended.
 * Kept as a named helper (not an inline) so the two spellings stay
 * visibly paired and the next edit can't silently re-split them.
 * Trim/preserve split, stated once: export uses this too (reproducing
 * what the agent saw); search uses the trimming twin (matching is
 * trim-tolerant). A literal lookalike quotes like the twin.
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
    // blank-turn error instead of a corrupt image part).
    // Marker count is blank-only cheap (#339): stripped (non-trailing)
    // turns ride as text with a marker, and the marker answers "were
    // photos staged" — staged-honest like titles/search, NOT wire-strict.
    // Scanning every history payload per send just to mark stripped turns
    // would tax O(history) per tap for no wire effect; validity is proven
    // only where parts are built, on the trailing turn below.
    // STAGED-HONEST vs WIRE-STRICT split, named here (#339): the over-limit
    // preflight counts decodable (it advises the SEND — "remove a photo"
    // must name photos that would actually wire), while this marker counts
    // wired (it describes the THREAD). A hand-edited ragged payload marks
    // but never wires — both positions reasoned, disagreeing only on that
    // hand-edited shape, which load() heals on read anyway.
    val markerCount = attachments.wiredPhotoCount()
    if (role != "user" || !includeImages) {
        // Stripped earlier photo turns stay honest (#339): the server
        // prompts with the trailing turn alone, but any other reader of
        // this history should see that photos were here — not a blank.
        // Verbatim twin (NOT the trimming helper): history rides exactly
        // as typed, marker only appended.
        if (role == "user" && markerCount > 0 && !includeImages) {
            return WireContent.Text(contentWithMarkerVerbatim(content, markerCount))
        }
        // Assistant turns never stage photos: the marker rule above is
        // user-only by construction, and a bare return here keeps a
        // hypothetical assistant-with-attachments payload from silently
        // widening the wire shape (fail-closed, like the gateway).
        return WireContent.Text(content)
    }
    // Trailing turn: parts need PROVEN payloads (#339). Ragged payloads
    // (bad length/alphabet) filter here — validity is send-frequency, so
    // the alphabet scan lives at the wire, never in the hot-path
    // isWired() predicate. Single pass: blank-check then validity per
    // attachment, not a wiredPhotos() list followed by a second walk.
    // dataUri() re-validates per part (#339): the filter walk plus the
    // dataUri decodability + cap walls is 2–3 full walks per photo per
    // send — accepted, not deduplicated. The alternative (an unchecked
    // twin, or passing the validated size through) would save a walk but
    // move enforcement out of the builder: any direct messageContentJson
    // caller (or a future one that skips streamChat's pre-check) would
    // wire oversize with no wall. Enforcement lives in the lowest common
    // function every path funnels through — never in the caller, no
    // matter how hot the path feels. Send-frequency on the send path
    // (never scroll/search), so the walks are a send tax, not a scroll
    // tax.
    val images = attachments.filter { it.isWired() && isDecodableBase64(it.base64) }
    if (images.isEmpty()) {
        // Marker when text survives, bare when nothing does (#339): a
        // text+ragged turn marks what was staged (staged-honest, like
        // history turns), but an image-only ragged turn must NOT gain a
        // marker-only text — that would sail past the server's named
        // blank-turn refusal and hand the agent a photo claim with no
        // image. Bare "" keeps the loud fail-closed for the imageless
        // case; the marker keeps the honest claim for the text case.
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
        // Validated by construction above (#339): the filter proved wired +
        // decodable, and the per-photo cap wall lives inside dataUri() —
        // the single choke point every wire path funnels through.
        arr.put(
            JSONObject()
                .put("type", "image_url")
                .put("image_url", JSONObject().put("url", a.dataUri()))
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
 * it would repeat the peak. Padding counts trailing-only (#339): interior
 * '=' resets the run, so "AB=C" measures 3 content chars → 2 B instead of
 * overstating — only the trailing run pads, capped at 2, floored at 0. */
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
    // Multiply first (#339): (3/4)*3 truncates to 0 while (3*3)/4 = 2 —
    // division order is load-bearing for ragged inputs like "AB=C".
    return maxOf(0L, ((content + tailPad) * 3L) / 4L - minOf(tailPad, 2))
}

/** `1.1 MB`-style rendering of [bytes] for limit errors. One decimal at MB
 * and above, integer KB below that, plain count below 1000 B (never blank)
 * (#339): the old doc line ("one decimal") overpromised the sub-KB branch,
 * which renders no decimal by design.
 * Pinned to US formatting (#339): user-visible strings must not render
 * `1,1 MB` under a comma-decimal locale. Never renders a cross-unit value
 * (#339): the KB branch floors (no "1000 KB" from rounding up) up to
 * 999_950, where 1-decimal MB honestly reads "1.0" — "999 KB" below,
 * "1.0 MB" at and above, no value names the wrong unit.
 * Negative input renders "unknown size" (#339): the preflight phrases
 * unknown length separately today, but the next caller passing a raw
 * contentLength() must not print "-1 B" in a user-visible string.
 * Decimal SI throughout (1000-based KB/MB), while the backstop it sits
 * under is the gateway's 4 MiB (1024-based) body cap (#339): the units
 * differ by ~5%, deliberately conservative — 3.5 M bytes refuses well
 * below 4 MiB either way, so the sentence pairs a smaller SI number
 * against a larger binary cap and can never under-refuse. */
fun formatBytes(bytes: Long): String {
    if (bytes < 0) return "unknown size"
    if (bytes < 1_000L) return "$bytes B"
    // Floor + 999_950 threshold per the KDoc above: no cross-unit values.
    if (bytes < 999_950L) return "${bytes / 1_000} KB"
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
    // Expired picker grant, diagnosed honestly (#339): GetMultipleContents
    // grants are consumed seconds later on IO, so a SecurityException here
    // is revocation, not a moved file — and re-picking (fresh grant) is
    // still the fix, so the advice holds while the diagnosis stops lying.
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
                // Gate the copy, not just the read (#339): the loop bounds
                // the buffer at MAX + one chunk, but toByteArray() copies it
                // — fail on out.size() first so an over-cap buffer never
                // doubles before throwing. total tracks the same bytes, but
                // the gate reads the buffer so it survives a future counter
                // drift; unreachable today (the loop throws first), kept as
                // the last unpinned copy's pin.
                if (out.size() > MAX_SOURCE_BYTES) {
                    throw OversizeAttachError(
                        "That file is too large to attach " +
                            "(over about ${formatBytes(MAX_SOURCE_BYTES.toLong())}).",
                    )
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
                // TRANSVERSE = 270° + horizontal mirror). The mirror composes
                // AFTER the rotation, so the rotated result is what gets
                // mirrored — mirroring first would swap the two cases
                // (verified by the 2x1 corner probe in ExifMatrixTest, not
                // just by reading the reference). Every case maps to
                // POSITIVE coordinates explicitly (#339): the pure
                // negative-scale flip recipe renders into negative space and
                // relies on createBitmap to re-normalize — which the rotation
                // cases visibly get (they need it just as much) but must
                // never be load-bearing. The postTranslate companions below
                // make each matrix positive by construction, correct with or
                // without normalization. POST reads in POINT order (#339):
                // per the platform contract postFoo(op) is M' = op * M, so
                // each call applies AFTER everything above it — the block
                // below reads top-down as the pixel flows (rotate, shift
                // positive, mirror, shift positive). preFoo would read
                // bottom-up (M' = M * op, op applied FIRST) and invite
                // exactly the translate-backwards bug — verified against
                // developer.android.com/reference/android/graphics/Matrix,
                // not from memory. The Bitmap path is JVM-untestable by
                // design (stubbed android.jar), so the affine math — not
                // just the prose — is pinned by ExifMatrixTest's 2x1 corner
                // probe, which replicates these documented post identities
                // (not the code's call order) and asserts the CIPA geometric
                // outcomes; do not "simplify" the mirror cases into pure
                // rotations, and keep the explicit translates. Pinned
                // matrix, all 8 values (#339): 1=identity, 2=flip-H, 3=rot180,
                // 4=flip-V, 5=transpose(rot90 then flip-H), 6=rot90,
                // 7=transverse(rot270 then flip-H), 8=rot270.
                val w = bitmap.width.toFloat()
                val h = bitmap.height.toFloat()
                val matrix: Matrix? = when (orientation) {
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
                // the named number is the real one, not 1–2 B high. Shared
                // spelling via oversizeMessageFor: the dataUri wall and the
                // streamChat pre-check say exactly this too.
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
        // Absolute raw ceiling first (#339): no scan or decode runs on
        // giants regardless of decoded size (see MAX_RAW_BASE64_CHARS).
        // Then the decoded-size gate with its O(1) raw-length fast path
        // (decoded bytes never exceed raw chars, so at/below-cap raws skip
        // the full walk; only over-length raws pay the precise measure for
        // the whitespace-wrapped edge).
        if (base64.length > MAX_RAW_BASE64_CHARS) return null
        if (base64.length > MAX_IMAGE_BASE64_CHARS &&
            base64Bytes(base64) > MAX_IMAGE_BASE64_CHARS * 3L / 4L
        ) {
            return null
        }
        // Decodability gate, same as dataUri's wall (#339): the decoder
        // below (Base64.DEFAULT) tolerates framing whitespace that the wire
        // refuses — without this, a wrapped-but-valid payload renders a
        // thumbnail while the send path drops it to bare text, re-splitting
        // the display/wire agreement the predicate was created to close.
        // Unreachable via canonical memory (load() strips framing), IO
        // thread already, so the full walk costs nothing new.
        if (!isDecodableBase64(base64)) return null
        // DEFAULT on decode is historical leniency, now moot (#339): the
        // gate above already rejected anything DEFAULT would forgive, so
        // the flag changes nothing — kept (not switched to NO_WRAP) to
        // avoid churning a path whose contract lives in the gate.
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
