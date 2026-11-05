package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * `@` connector mentions (#441): the trigger predicate, the mention
 * parse, the insert rewrite, and the reconnect-hint detector. All pure —
 * the picker, the chip renderer and the reconnect card share these, so a
 * drift in any one breaks loudly here.
 */
class ConnectorMentionTest {

    // ── Trigger ──────────────────────────────────────────────────────────

    @Test
    fun `bare at and partial tokens trigger`() {
        assertEquals("", mentionQuery("@"))
        assertEquals("", mentionQuery("@ "))
        assertEquals("g", mentionQuery("@g"))
        assertEquals("gm", mentionQuery("@gm"))
        assertEquals("cal", mentionQuery("@cal"))
    }

    @Test
    fun `mid-sentence at never triggers`() {
        assertNull(mentionQuery("mail @gmail today"))
        assertNull(mentionQuery(" @gmail"))
        assertNull(mentionQuery(""))
        assertNull(mentionQuery("gmail"))
    }

    @Test
    fun `complete mentions do not trigger`() {
        assertNull(mentionQuery("@gmail"))
        assertNull(mentionQuery("@gmail "))
        assertNull(mentionQuery("@gmail latest invoice"))
        assertNull(mentionQuery("@GMAIL"))
        assertNull(mentionQuery("@calendar next week"))
    }

    @Test
    fun `non-connector tokens do not trigger`() {
        assertNull(mentionQuery("@foo"))
        assertNull(mentionQuery("@gmailx"))
        assertNull(mentionQuery("@gm!"))
        assertNull(mentionQuery("@123"))
    }

    // ── Parse ────────────────────────────────────────────────────────────

    @Test
    fun `mention parses with and without args`() {
        assertEquals(ConnectorMention("gmail", ""), parseConnectorMention("@gmail"))
        assertEquals(
            ConnectorMention("gmail", "latest invoice"),
            parseConnectorMention("@gmail latest invoice"),
        )
        assertEquals(
            ConnectorMention("calendar", "next week"),
            parseConnectorMention("@calendar next week"),
        )
    }

    @Test
    fun `mention parse folds case and trims end`() {
        assertEquals(ConnectorMention("gmail", ""), parseConnectorMention("@GMail"))
        assertEquals(
            ConnectorMention("gmail", "x"),
            parseConnectorMention("@gmail x   "),
        )
    }

    @Test
    fun `unknown mentions stay plain text`() {
        assertNull(parseConnectorMention("@foo bar"))
        assertNull(parseConnectorMention("hello @gmail"))
        assertNull(parseConnectorMention(""))
    }

    // ── Insert ───────────────────────────────────────────────────────────

    @Test
    fun `insert builds mention plus kept remainder`() {
        assertEquals("@gmail ", insertConnectorMention("@", "gmail"))
        assertEquals("@gmail ", insertConnectorMention("@gm", "gmail"))
        assertEquals("@calendar mon 9am", insertConnectorMention("@ mon 9am", "calendar"))
    }

    // ── Reconnect hint ───────────────────────────────────────────────────

    @Test
    fun `reconnect marker detected case-insensitively`() {
        assertTrue(hasReconnectHint("this session expired; reconnect in Settings — never paste a token"))
        assertTrue(hasReconnectHint("Gmail is not connected; Reconnect in settings and retry"))
        assertFalse(hasReconnectHint("here is your inbox"))
        assertFalse(hasReconnectHint(""))
    }
}
