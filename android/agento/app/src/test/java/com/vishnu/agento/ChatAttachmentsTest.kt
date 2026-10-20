package com.vishnu.agento

import org.json.JSONArray
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Chat photo attachments (#318).
 *
 * Covers the pure surface: sizing math, the multipart wire shape the
 * gateway parses, the text-or-photos content rule every history filter
 * must use, and the caps invariant that keeps a maxed-out turn under the
 * client preflight (and therefore under the gateway's 4 MiB body cap).
 * Bitmap/ContentResolver work in [prepareAttachment] is untestable on the
 * JVM (stubbed android.jar) and stays out of this suite by design.
 */
class ChatAttachmentsTest {

    // downscaledSize: fitting the long side, never upscaling.

    @Test
    fun smallImageKeepsDimensions() {
        assertEquals(800 to 600, downscaledSize(800, 600))
    }

    @Test
    fun atCapKeepsDimensions() {
        assertEquals(MAX_IMAGE_SIDE_PX to 100, downscaledSize(MAX_IMAGE_SIDE_PX, 100))
    }

    @Test
    fun landscapeScalesLongSideToCap() {
        val (w, h) = downscaledSize(4000, 3000)
        assertEquals(MAX_IMAGE_SIDE_PX, w)
        assertEquals(1176, h)
    }

    @Test
    fun portraitScalesLongSideToCap() {
        val (w, h) = downscaledSize(3000, 4000)
        assertEquals(1176, w)
        assertEquals(MAX_IMAGE_SIDE_PX, h)
    }

    @Test
    fun degenerateDimensionsYieldZero() {
        assertEquals(0 to 0, downscaledSize(0, 100))
        assertEquals(0 to 0, downscaledSize(-5, 100))
    }

    // Wire shape: what the gateway's multipart parser accepts.

    private fun attachment(data: String = "aGVsbG8=") =
        ChatAttachment(mimeType = "image/jpeg", base64 = data)

    @Test
    fun dataUriShape() {
        assertEquals("data:image/jpeg;base64,aGVsbG8=", attachment().dataUri())
    }

    @Test
    fun noAttachmentsIsBareString() {
        assertEquals("hi", messageContentJson("user", "hi", emptyList(), includeImages = true))
    }

    @Test
    fun nonUserNeverMultipart() {
        assertEquals(
            "hi",
            messageContentJson("assistant", "hi", listOf(attachment()), includeImages = true),
        )
    }

    @Test
    fun earlierTurnsRideAsText() {
        assertEquals(
            "earlier",
            messageContentJson("user", "earlier", listOf(attachment()), includeImages = false),
        )
    }

    @Test
    fun textPlusTwoImagesIsMultipart() {
        val out = messageContentJson(
            "user", "describe these",
            listOf(attachment("AAA="), attachment("BBB=")),
            includeImages = true,
        )
        assertTrue(out is JSONArray)
        val arr = out as JSONArray
        assertEquals(3, arr.length())
        assertEquals("text", arr.getJSONObject(0).getString("type"))
        assertEquals("describe these", arr.getJSONObject(0).getString("text"))
        for (i in 1..2) {
            val part = arr.getJSONObject(i)
            assertEquals("image_url", part.getString("type"))
            val url = part.getJSONObject("image_url").getString("url")
            assertTrue(url.startsWith("data:image/jpeg;base64,"))
        }
        assertTrue(arr.getJSONObject(1).getJSONObject("image_url").getString("url").endsWith("AAA="))
        assertTrue(arr.getJSONObject(2).getJSONObject("image_url").getString("url").endsWith("BBB="))
    }

    @Test
    fun imageOnlyTurnOmitsTextPart() {
        val out = messageContentJson("user", "  ", listOf(attachment()), includeImages = true)
        assertTrue(out is JSONArray)
        val arr = out as JSONArray
        assertEquals(1, arr.length())
        assertEquals("image_url", arr.getJSONObject(0).getString("type"))
    }

    // Content rule + markers.

    @Test
    fun contentRuleCoversImageOnlyTurns() {
        assertTrue(messageHasContent(ChatMessage("user", "hi")))
        assertTrue(
            messageHasContent(ChatMessage("user", "", 0L, attachments = listOf(attachment()))),
        )
        assertFalse(messageHasContent(ChatMessage("user", "  ")))
        assertFalse(messageHasContent(ChatMessage("assistant", "")))
    }

    @Test
    fun markerSingularAndPlural() {
        assertEquals("[1 photo attached]", attachmentMarker(1))
        assertEquals("[3 photos attached]", attachmentMarker(3))
    }

    @Test
    fun photoOnlyThreadTitlesNamePhotos() {
        assertEquals(
            "Photo",
            ChatThreads.titleFor(listOf(ChatMessage("user", "", 0L, attachments = listOf(attachment())))),
        )
        assertEquals(
            "2 photos",
            ChatThreads.titleFor(
                listOf(ChatMessage("user", "  ", 0L, attachments = listOf(attachment(), attachment()))),
            ),
        )
    }

    @Test
    fun textTitleUnchanged() {
        assertEquals("hello", ChatThreads.titleFor(listOf(ChatMessage("user", "hello"))))
    }

    @Test
    fun storedRoundTripKeepsAttachments() {
        val msgs = listOf(
            ChatMessage("user", "", 1L, attachments = listOf(attachment("AAA="), attachment("BBB="))),
            ChatMessage("assistant", "seen", 2L),
        )
        val back = ChatThreads.toUi(ChatThreads.toStored(msgs))
        assertEquals(2, back[0].attachments.size)
        assertEquals("AAA=", back[0].attachments[0].base64)
        assertEquals("image/jpeg", back[0].attachments[0].mimeType)
        assertTrue(back[1].attachments.isEmpty())
        assertEquals("seen", back[1].content)
    }

    // Caps: a maxed-out turn must fit the preflight with headroom.

    @Test
    fun maxedOutTurnFitsPreflight() {
        // Base64 is ASCII, so capped-photo chars ARE bytes; only history
        // text can be multibyte, and the headroom below absorbs it.
        val worstPhotos = MAX_IMAGES_PER_TURN.toLong() * MAX_IMAGE_BASE64_CHARS
        assertTrue(
            "2 capped photos ($worstPhotos bytes) must fit the " +
                "$MAX_CHAT_WIRE_BYTES-byte preflight with room for framing",
            worstPhotos < MAX_CHAT_WIRE_BYTES - 100_000L,
        )
    }

    @Test
    fun wirePreflightBoundary() {
        assertFalse(wireSizeExceeded(MAX_CHAT_WIRE_BYTES))
        assertTrue(wireSizeExceeded(MAX_CHAT_WIRE_BYTES + 1))
    }

    @Test
    fun byteFormatting() {
        assertEquals("512 B", formatBytes(512L))
        assertEquals("150 KB", formatBytes(150_000L))
        assertEquals("1.1 MB", formatBytes(1_100_000L))
    }
}
