package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
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
    fun dataUriPinIgnoresStoredMime() {
        // Fail-closed pin: a hand-edited thread file claiming image/png
        // must still wire as image/jpeg (every payload is pick-time JPEG).
        assertEquals(
            "data:image/jpeg;base64,AAA=",
            ChatAttachment(mimeType = "image/png", base64 = "AAA=").dataUri(),
        )
    }

    @Test
    fun dataUriRejectsBlankPayload() {
        // The wall the wire filter relies on (#339): rendering an empty
        // data URI must fail in tests, never wire it.
        try {
            ChatAttachment(base64 = "").dataUri()
            fail("blank payload must throw, not render data:image/jpeg;base64,")
        } catch (e: NamedAttachError) {
            // Expected: typed pass-through bucket, not generic remap.
        }
    }

    @Test
    fun noAttachmentsIsBareString() {
        assertEquals(WireContent.Text("hi"), messageContentJson("user", "hi", emptyList(), includeImages = true))
    }

    @Test
    fun nonUserNeverMultipart() {
        assertEquals(
            WireContent.Text("hi"),
            messageContentJson("assistant", "hi", listOf(attachment()), includeImages = true),
        )
    }

    @Test
    fun nonUserWithAttachmentsIsBareString() {
        // Fail-closed asymmetry, pinned: assistant turns never stage
        // photos, so a hypothetical attachments payload there must not
        // widen the wire shape — bare content, no marker, no parts.
        assertEquals(
            WireContent.Text("hi"),
            messageContentJson("assistant", "hi", listOf(attachment()), includeImages = false),
        )
    }

    @Test
    fun earlierTurnsRideAsTextWithMarker() {
        assertEquals(
            WireContent.Text("earlier [1 photo attached]"),
            messageContentJson("user", "earlier", listOf(attachment()), includeImages = false),
        )
    }

    @Test
    fun earlierImageOnlyTurnRidesAsMarker() {
        assertEquals(
            WireContent.Text("[2 photos attached]"),
            messageContentJson(
                "user", "  ",
                listOf(attachment(), attachment()),
                includeImages = false,
            ),
        )
    }

    @Test
    fun textPlusTwoImagesIsMultipart() {
        val out = messageContentJson(
            "user", "describe these",
            listOf(attachment("AAA="), attachment("BBB=")),
            includeImages = true,
        )
        assertTrue(out is WireContent.Multipart)
        val arr = (out as WireContent.Multipart).parts
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
        assertTrue(out is WireContent.Multipart)
        val arr = (out as WireContent.Multipart).parts
        assertEquals(1, arr.length())
        assertEquals("image_url", arr.getJSONObject(0).getString("type"))
    }

    @Test
    fun blankPayloadsAreNotWired() {
        // Fail-closed: an empty base64 (hand-edited thread, writer bug)
        // counts as no attachment — never a data:image/jpeg;base64, part.
        // Ragged payloads (bad length/alphabet) filter the same way.
        assertEquals(
            WireContent.Text("hi"),
            messageContentJson(
                "user", "hi",
                listOf(ChatAttachment(base64 = "")),
                includeImages = true,
            ),
        )
        assertEquals(
            WireContent.Text("hi"),
            messageContentJson(
                "user", "hi",
                listOf(ChatAttachment(base64 = "!!!not-base64!!!")),
                includeImages = true,
            ),
        )
        // Mixed: the blank drops, the valid image still wires (text + 1).
        val out = messageContentJson(
            "user", "hi",
            listOf(ChatAttachment(base64 = ""), attachment("AAA=")),
            includeImages = true,
        )
        assertTrue(out is WireContent.Multipart)
        assertEquals(2, (out as WireContent.Multipart).parts.length())
    }

    @Test
    fun strippedTurnMarkerCountsStagedNotDecodable() {
        // Marker branch is staged-honest (#339): stripped turns mark what
        // was staged (blank-only count, no per-send validity scan of all
        // history) — only the trailing turn proves decodability for parts.
        assertEquals(
            WireContent.Text("hi [1 photo attached]"),
            messageContentJson(
                "user", "hi",
                listOf(ChatAttachment(base64 = "!!!")),
                includeImages = false,
            ),
        )
    }

    @Test
    fun raggedPayloadPositionsRejected() {
        // Padding is positional: interior '=' never wires, even with
        // clean length and alphabet — the corrupt part stays out.
        for (bad in listOf("=AAA", "A=A=", "AB=C", "A===")) {
            assertEquals(
                WireContent.Text("hi"),
                messageContentJson("user", "hi", listOf(ChatAttachment(base64 = bad)), includeImages = true),
            )
        }
        // Sanity: real paddings still wire.
        assertTrue(
            messageContentJson("user", "hi", listOf(attachment("AAA=")), includeImages = true)
                is WireContent.Multipart,
        )
    }

    @Test
    fun decodabilityPinsInputShapes() {
        // Every shape canonicalizeAttachments() depends on (#339):
        // whitespace (wrapped legacy), URL-safe alphabet, ragged length,
        // all-padding, and giants all fail — only clean standard base64
        // passes. Same-module test, so internal is visible.
        assertTrue(isDecodableBase64("AAA="))
        assertTrue(isDecodableBase64("TWFu"))
        assertFalse(isDecodableBase64("AAA=\n"))
        assertFalse(isDecodableBase64("AA A="))
        assertFalse(isDecodableBase64("AB-_"))
        assertFalse(isDecodableBase64("AAA"))
        assertFalse(isDecodableBase64("===="))
        assertFalse(isDecodableBase64(""))
        assertFalse(isDecodableBase64("A".repeat(MAX_RAW_BASE64_CHARS + 4)))
    }

    @Test
    fun dataUriRejectsStorageInjectedOversize() {
        // The per-photo cap wall (#339): a hand-edited multi-MB valid
        // payload passes the 4M-char ceiling, so without this the send
        // would ride to the generic conversation-size preflight. Fails with
        // the same named per-photo message as the stage path.
        try {
            ChatAttachment(base64 = "A".repeat(MAX_IMAGE_BASE64_CHARS + 4)).dataUri()
            fail("over-cap payload must throw the named per-photo error, not wire")
        } catch (e: NamedAttachError) {
            assertTrue(e.message!!.contains("per-photo limit"))
        }
    }

    @Test
    fun dataUriRejectsRaggedPayload() {
        // The wall the wire filter relies on (#339): a ragged payload
        // reaching a direct caller throws the typed error, never renders.
        try {
            ChatAttachment(base64 = "!!!not-base64!!!").dataUri()
            fail("ragged payload must throw, not render a corrupt data URI")
        } catch (e: NamedAttachError) {
            // Expected: typed pass-through bucket, not generic remap.
        }
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
    fun contentRuleIgnoresAssistantPhotos() {
        // Matches the wire marker's user-only rule (#339): a hypothetical
        // assistant photo turn wires as bare text with no marker, so the
        // resend filters must not treat it as content either.
        assertFalse(
            messageHasContent(
                ChatMessage("assistant", "", 0L, attachments = listOf(attachment())),
            ),
        )
    }

    @Test
    fun blankPayloadIsNoContent() {
        // The blank-payload-is-no-content rule the wire filter relies on
        // (#339): a hand-edited {"base64":""} turn must not pass resend
        // filters as content while wiring as bare text.
        assertFalse(
            messageHasContent(
                ChatMessage("user", "", 0L, attachments = listOf(ChatAttachment(base64 = ""))),
            ),
        )
        assertTrue(
            listOf(ChatAttachment(base64 = ""), attachment()).wiredPhotos().size == 1,
        )
    }

    @Test
    fun markerSingularAndPlural() {
        assertEquals("[1 photo attached]", attachmentMarker(1))
        assertEquals("[3 photos attached]", attachmentMarker(3))
    }

    @Test
    fun contentWithMarkerSharedSpelling() {
        assertEquals("hi", contentWithMarker("hi", 0))
        assertEquals("hi [1 photo attached]", contentWithMarker("hi", 1))
        assertEquals("hi [1 photo attached]", contentWithMarker("  hi  ", 1))
        assertEquals("[2 photos attached]", contentWithMarker("", 2))
        assertEquals("[2 photos attached]", contentWithMarker("   ", 2))
    }

    @Test
    fun verbatimMarkerPreservesWhitespace() {
        // Wire twin: indentation rides verbatim, marker only appended.
        assertEquals("  code", contentWithMarkerVerbatim("  code", 0))
        assertEquals(
            "  code [1 photo attached]",
            contentWithMarkerVerbatim("  code", 1),
        )
        assertEquals("[1 photo attached]", contentWithMarkerVerbatim("   ", 1))
    }

    @Test
    fun literalMarkerLookalikeQuotes() {
        // A user-typed trailing marker must not read as a produced one:
        // the typed occurrence quotes (visible in every render — plain
        // Text and Markdown alike), the real marker appends plain.
        assertEquals(
            "\"[1 photo attached]\" [1 photo attached]",
            contentWithMarkerVerbatim("[1 photo attached]", 1),
        )
        assertEquals(
            "look \"[2 photos attached]\" [1 photo attached]",
            contentWithMarker("look [2 photos attached]", 1),
        )
    }

    @Test
    fun raggedPayloadCountsButWiresAsText() {
        // Split-by-design contract (#339): a non-load construction (ragged
        // alphabet here — load would have blanked it) counts as a photo in
        // display/titles/search (blank-only predicate, hot-path cheap) but
        // the wire filter drops it, so it rides as bare text — never a
        // corrupt image part. Fail-closed at the wire, not silent.
        val ragged = attachment(data = "!!!")
        assertEquals(1, listOf(ragged).wiredPhotoCount())
        assertEquals(
            WireContent.Text("hi"),
            messageContentJson("user", "hi", listOf(ragged), includeImages = true),
        )
    }

    @Test
    fun requireIoThreadPassesOffMain() {
        // Suite runs on a worker thread (never named "main"), so the
        // boundary gate must pass here — this is the JVM half of the
        // Looper-first/name-fallback contract.
        requireIoThread("test")
    }

    @Test
    fun requireIoThreadRejectsMainNamedThread() {
        // The throw path, pinned without Robolectric (#339): a thread
        // named "main" trips the JVM fallback exactly like the UI thread
        // trips the Looper branch on device.
        var thrown: Throwable? = null
        val t = Thread({
            thrown = runCatching { requireIoThread("test") }.exceptionOrNull()
        }, "main")
        t.start()
        t.join()
        assertTrue(thrown is IllegalStateException)
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
    fun photoOpenerWithLaterTextTitlesFromText() {
        // The opener's photos lose the title to real words, but the suffix
        // still signals the thread holds photos (#339: max-of scan).
        assertEquals(
            "hello [photo]",
            ChatThreads.titleFor(
                listOf(
                    ChatMessage("user", "", 0L, attachments = listOf(attachment())),
                    ChatMessage("assistant", "hi", 1L),
                    ChatMessage("user", "hello", 2L),
                ),
            ),
        )
    }

    @Test
    fun laterPhotoTurnStillSignals() {
        // Suffix scans any photo turn (#339): a text opener with photos
        // later still shows the signal — and names the largest turn.
        assertEquals(
            "hello [photo]",
            ChatThreads.titleFor(
                listOf(
                    ChatMessage("user", "hello", 0L),
                    ChatMessage("assistant", "hi", 1L),
                    ChatMessage("user", "more", 2L, attachments = listOf(attachment())),
                ),
            ),
        )
        assertEquals(
            "pic [2 photos]",
            ChatThreads.titleFor(
                listOf(
                    ChatMessage("user", "pic", 0L, attachments = listOf(attachment())),
                    ChatMessage("assistant", "hi", 1L),
                    ChatMessage("user", "hello", 2L, attachments = listOf(attachment(), attachment())),
                ),
            ),
        )
    }

    @Test
    fun canonicalizeAttachmentsNormalizesOnce() {
        // Load-boundary contract (#339): whitespace strips, ragged blanks,
        // valid payloads pass through untouched (same instance).
        val valid = attachment("AAA=")
        val out = canonicalizeAttachments(
            listOf(valid, ChatAttachment(base64 = "  AAA=\n"), ChatAttachment(base64 = "!!!")),
        )
        assertEquals(3, out.size)
        assertTrue(out[0] === valid)
        assertEquals("AAA=", out[1].base64)
        assertEquals("", out[2].base64)
    }

    @Test
    fun wiredPhotosWithIndexPreservesStagedSlots() {        // The strip renders wired but removes by staged slot: the mapping
        // must carry the STAGED index, or remove deletes the wrong photo.
        val mapped = listOf(
            ChatAttachment(base64 = ""),
            attachment("AAA="),
            ChatAttachment(base64 = ""),
            attachment("BBB="),
        ).wiredPhotosWithIndex()
        assertEquals(2, mapped.size)
        assertEquals(1, mapped[0].index)
        assertEquals("AAA=", mapped[0].value.base64)
        assertEquals(3, mapped[1].index)
        assertEquals("BBB=", mapped[1].value.base64)
    }

    @Test
    fun legacyThreadJsonDecodesWithoutAttachments() {        // Backward-compat pins (#339): JSON written before attachments (no
        // field at all) and attachment objects written before ids (no id
        // field) must decode to text-only with empty ids — never fail the
        // whole thread.
        val json = kotlinx.serialization.json.Json { ignoreUnknownKeys = true }
        val thread = json.decodeFromString(
            ChatThread.serializer(),
            """{"id":"t","title":"x","updatedAt":1,"messages":[{"role":"user","content":"hi","ts":2}]}""",
        )
        val ui = ChatThreads.toUi(thread.messages)
        assertEquals(1, ui.size)
        assertEquals("hi", ui.single().content)
        assertTrue(ui.single().attachments.isEmpty())
        val attachment = json.decodeFromString(
            ChatAttachment.serializer(),
            """{"mimeType":"image/jpeg","base64":"AAA="}""",
        )
        assertEquals("", attachment.id)
        assertEquals("AAA=", attachment.base64)
    }

    @Test
    fun textTitleUnchanged() {
        assertEquals("hello", ChatThreads.titleFor(listOf(ChatMessage("user", "hello"))))
    }

    @Test
    fun photoTextTitleCarriesSignal() {
        assertEquals(
            "look [photo]",
            ChatThreads.titleFor(
                listOf(ChatMessage("user", "look", 0L, attachments = listOf(attachment()))),
            ),
        )
        assertEquals(
            "look [2 photos]",
            ChatThreads.titleFor(
                listOf(ChatMessage("user", "look", 0L, attachments = listOf(attachment(), attachment()))),
            ),
        )
    }

    @Test
    fun photoTextTitleKeepsBudget() {
        val long = "word ".repeat(20).trim()
        val title = ChatThreads.titleFor(
            listOf(ChatMessage("user", long, 0L, attachments = listOf(attachment()))),
        )
        assertTrue(title.length <= 42)
        assertTrue(title.endsWith("[photo]"))
    }

    @Test
    fun photoTextTitleNeverSplitsSurrogates() {
        // Cut landing on an emoji's high surrogate drops the pair rather
        // than rendering a lone-surrogate � in the switcher row.
        assertEquals(
            "a".repeat(40) + "…",
            ChatThreads.titleFor(
                listOf(ChatMessage("user", "a".repeat(40) + "\uD83D\uDE00b", 0L)),
            ),
        )
    }

    @Test
    fun titleNeverSplitsGraphemeClusters() {
        // ZWJ family emoji (man+ZWJ+woman+ZWJ+girl) must not split
        // mid-cluster. Asserts portably (budget + no lone surrogates),
        // not exact-cluster: ICU versions may group ZWJ differently, but
        // a lone half is never acceptable output anywhere.
        val cluster = "\uD83D\uDC68\u200D\uD83D\uDC69\u200D\uD83D\uDC67"
        val title = ChatThreads.titleFor(
            listOf(ChatMessage("user", "a".repeat(39) + cluster + "b", 0L)),
        )
        assertTrue(title.length <= 42)
        val chars = title.toCharArray()
        for (i in chars.indices) {
            if (chars[i].isHighSurrogate()) {
                assertTrue(i + 1 < chars.size && chars[i + 1].isLowSurrogate())
            }
            if (chars[i].isLowSurrogate()) {
                assertTrue(i > 0 && chars[i - 1].isHighSurrogate())
            }
        }
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
        assertFalse(wireSizeExceeded(MAX_CHAT_WIRE_BYTES.toLong()))
        assertTrue(wireSizeExceeded(MAX_CHAT_WIRE_BYTES.toLong() + 1))
        assertTrue(wireSizeExceeded(-1L))
    }

    @Test
    fun byteFormatting() {
        assertEquals("512 B", formatBytes(512L))
        assertEquals("150 KB", formatBytes(150_000L))
        assertEquals("1.1 MB", formatBytes(1_100_000L))
    }

    @Test
    fun byteFormattingNeverCrossesUnits() {
        // 999_999 would round to "1000 KB" under %.0f — the MB branch
        // takes over below that so limit errors never name a wrong cap.
        assertEquals("1.0 MB", formatBytes(999_999L))
        assertEquals("999 KB", formatBytes(999_499L))
    }

    @Test
    fun byteFormattingIgnoresLocale() {
        // Named limit errors are user-visible: a comma-decimal locale must
        // not render "1,1 MB".
        val prev = java.util.Locale.getDefault()
        try {
            java.util.Locale.setDefault(java.util.Locale.GERMANY)
            assertEquals("1.1 MB", formatBytes(1_100_000L))
            assertEquals("150 KB", formatBytes(150_000L))
        } finally {
            java.util.Locale.setDefault(prev)
        }
    }

    @Test
    fun base64BytesAdjustsForPadding() {
        // "TQ==" is 1 byte, not (4*3)/4 = 3 — the named oversize error
        // must report the real size, not 1–2 B high. Framing whitespace
        // is not content either.
        assertEquals(1L, base64Bytes("TQ=="))
        assertEquals(2L, base64Bytes("TWE="))
        assertEquals(3L, base64Bytes("TWFu"))
        assertEquals(0L, base64Bytes(""))
        assertEquals(1L, base64Bytes("TQ==\n"))
        assertEquals(1L, base64Bytes("T Q= = "))
        // Interior '=' is content-positioned, never padding (#339):
        // "AB=C" measures 3 content chars → 2 B, not 3.
        assertEquals(2L, base64Bytes("AB=C"))
        assertEquals(0L, base64Bytes("=="))
    }
}
