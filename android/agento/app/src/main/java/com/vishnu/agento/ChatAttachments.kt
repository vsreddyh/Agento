package com.vishnu.agento

import android.content.Context
import android.graphics.Bitmap
import android.graphics.BitmapFactory
import android.net.Uri
import android.util.Base64
import java.io.ByteArrayOutputStream
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
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
)

/** Max photos per turn (#318). Two is the acceptance bar (two images in one
 * turn render and reach the agent); the cap keeps the wire body under the
 * gateway's 4 MiB limit (see the invariant test in [ChatAttachmentsTest]). */
const val MAX_IMAGES_PER_TURN = 2

/** Longest side after downscale, px. A 1568px JPEG at quality 80 is
 * typically 150–600 KB — legible for the agent, small on the wire. */
const val MAX_IMAGE_SIDE_PX = 1568

/** Per-photo base64 cap, chars (~1.1 MiB raw). Two capped photos plus text
 * history stay under [MAX_CHAT_WIRE_CHARS] with headroom. */
const val MAX_IMAGE_BASE64_CHARS = 1_500_000

/** Client-side wire preflight, chars. Below the gateway's 4 MiB body cap on
 * purpose: exceeding this refuses with a named in-app error instead of an
 * opaque HTTP 413, and leaves room for JSON framing plus history text. */
const val MAX_CHAT_WIRE_CHARS = 3_500_000

/** `true` when a request body of [chars] must be refused client-side. */
fun wireSizeExceeded(chars: Int): Boolean = chars > MAX_CHAT_WIRE_CHARS

/** Wire form of one attachment: the `image_url` data URI the gateway's
 * multipart parser accepts (`internal/gateway/openai.go`). Remote URLs are
 * never produced — the gateway refuses to fetch them. */
fun ChatAttachment.dataUri(): String = "data:$mimeType;base64,$base64"

/** Inline marker for text-only surfaces (exports, search, titles):
 * `[1 photo attached]` / `[3 photos attached]`. */
fun attachmentMarker(n: Int): String =
    if (n == 1) "[1 photo attached]" else "[$n photos attached]"

/** True when the message carries anything the agent should see. Text-only
 * checks (`content.isNotBlank()`) drop image-only turns from resends and
 * resume, so every history filter must use this instead. */
fun messageHasContent(m: ChatMessage): Boolean =
    m.content.isNotBlank() || m.attachments.isNotEmpty()

/**
 * Dimensions fitting `w`×`h` inside [MAX_IMAGE_SIDE_PX] on the long side,
 * preserving aspect ratio, never upscaling. Pure for testability — the
 * bitmap work in [prepareAttachment] consumes this.
 */
fun downscaledSize(w: Int, h: Int): Pair<Int, Int> {
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
 * Wire content for one message: a bare string normally, the conventional
 * multipart array when this user turn carries photos.
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
): Any {
    if (role != "user" || attachments.isEmpty() || !includeImages) return content
    val arr = JSONArray()
    if (content.isNotBlank()) {
        arr.put(JSONObject().put("type", "text").put("text", content))
    }
    for (a in attachments) {
        arr.put(
            JSONObject()
                .put("type", "image_url")
                .put("image_url", JSONObject().put("url", a.dataUri())),
        )
    }
    return arr
}

/** Raw size of a base64 payload in bytes, for human-sized error messages. */
fun base64Bytes(base64: String): Long = (base64.length * 3L) / 4L

/** `1.1 MB`-style rendering of [bytes] for limit errors. One decimal, never blank. */
fun formatBytes(bytes: Long): String {
    if (bytes < 1_000L) return "$bytes B"
    if (bytes < 1_000_000L) return "${"%.0f".format(bytes / 1_000.0)} KB"
    return "${"%.1f".format(bytes / 1_000_000.0)} MB"
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
            val bounds = BitmapFactory.Options().apply { inJustDecodeBounds = true }
            resolver.openInputStream(uri)?.use { BitmapFactory.decodeStream(it, null, bounds) }
                ?: throw IllegalArgumentException(
                    "Couldn't read that photo — it may have been moved or deleted. Pick it again.",
                )
            if (bounds.outWidth <= 0 || bounds.outHeight <= 0) {
                throw IllegalArgumentException(
                    "That file isn't a readable photo — Agento attaches images only, not documents.",
                )
            }
            // Sample down powers of two until the decoded bitmap is near the
            // target box; the exact fit happens via scaling below.
            var sample = 1
            while (bounds.outWidth / sample > MAX_IMAGE_SIDE_PX * 2 ||
                bounds.outHeight / sample > MAX_IMAGE_SIDE_PX * 2
            ) {
                sample *= 2
            }
            val opts = BitmapFactory.Options().apply { inSampleSize = sample }
            var bitmap = resolver.openInputStream(uri)?.use {
                BitmapFactory.decodeStream(it, null, opts)
            } ?: throw IllegalArgumentException(
                "Couldn't read that photo — it may have been moved or deleted. Pick it again.",
            )
            val (tw, th) = downscaledSize(bitmap.width, bitmap.height)
            if (bitmap.width != tw || bitmap.height != th) {
                val scaled = Bitmap.createScaledBitmap(bitmap, tw, th, true)
                if (scaled != bitmap) {
                    bitmap.recycle()
                    bitmap = scaled
                }
            }
            var encoded: String? = null
            var largest = 0
            for (quality in listOf(80, 60)) {
                val out = ByteArrayOutputStream()
                bitmap.compress(Bitmap.CompressFormat.JPEG, quality, out)
                val candidate = Base64.encodeToString(out.toByteArray(), Base64.NO_WRAP)
                largest = maxOf(largest, candidate.length)
                if (candidate.length <= MAX_IMAGE_BASE64_CHARS) {
                    encoded = candidate
                    break
                }
            }
            bitmap.recycle()
            val final = encoded ?: throw IllegalStateException(
                "Photo too large even after shrinking (${formatBytes(largest * 3L / 4L)})" +
                    " — the per-photo limit is about " +
                    "${formatBytes(MAX_IMAGE_BASE64_CHARS * 3L / 4L)}. " +
                    "Try a smaller crop.",
            )
            ChatAttachment(mimeType = "image/jpeg", base64 = final)
        }
    }

/**
 * Small bitmap for thumbnails (bubbles, composer strip). Decodes sampled to
 * ~256px so a remembered thumbnail costs kilobytes, not the full payload.
 * Null when the payload is corrupt — callers show an icon placeholder, so a
 * bad payload degrades to a missing thumbnail, never a crash.
 */
fun decodeThumbnail(base64: String): Bitmap? {
    return runCatching {
        val bytes = Base64.decode(base64, Base64.DEFAULT)
        val bounds = BitmapFactory.Options().apply { inJustDecodeBounds = true }
        BitmapFactory.decodeByteArray(bytes, 0, bytes.size, bounds)
        if (bounds.outWidth <= 0 || bounds.outHeight <= 0) return null
        var sample = 1
        while (bounds.outWidth / sample > 256 || bounds.outHeight / sample > 256) {
            sample *= 2
        }
        BitmapFactory.decodeByteArray(
            bytes, 0, bytes.size,
            BitmapFactory.Options().apply { inSampleSize = sample },
        )
    }.getOrNull()
}
