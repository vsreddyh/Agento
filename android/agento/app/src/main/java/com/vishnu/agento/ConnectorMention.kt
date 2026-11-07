package com.vishnu.agento

import java.util.Locale

/**
 * Parsed `@<connector> [query]` user turn (#441).
 *
 * The app-side half of the in-chat attach pattern: the composer offers
 * `@gmail` / `@calendar` rows, tapping one inserts the mention, and the
 * turn sends wire-verbatim — the agent (which holds the gmail/calendar
 * MCP tools) resolves the reference. Stored text is never rewritten; the
 * chip below is a render-time projection, like `/skill:` in
 * [parseSkillCommand].
 */
data class ConnectorMention(
    val name: String,
    val args: String,
)

/** Connector ids the chat mention supports: the v1 pair from ADR-0009. */
internal val MENTIONABLE_CONNECTORS = listOf("gmail", "calendar")

/** `^@(gmail|calendar)(?:\s+([\s\S]*))?$`, case-insensitive: ids are
 * lowercase-canonical server-side and fold here the same way. */
private val MENTION_COMMAND =
    Regex("""^@(gmail|calendar)(?:\s+([\s\S]*))?$""", RegexOption.IGNORE_CASE)

/** Name-shaped query after `@`: the mention alphabet, so a query can never
 * promise a row the send path would refuse to chip. */
private val MENTION_QUERY_CHARS = Regex("""^[A-Za-z]*$""")

/**
 * Substring every dead-grant server message carries: the list `reconnect`
 * hint and the grant-less/unreadable errors in `serverauth.go` share the
 * "reconnect in Settings" wording, and the app's own Connect screen says
 * the same. Rendered (not matched on status codes) so paraphrases the
 * agent writes around the pasted error still surface the prompt — but the
 * raw guidance is never asked for in chat.
 */
internal const val RECONNECT_MARKER = "reconnect in Settings"

/** True when [content] carries a dead-grant reconnect hint (#441): an
 * expired/disconnected grant in this flow must surface the reconnect
 * prompt, never a password/token ask. Case-insensitive — the agent may
 * re-case quoted errors. */
fun hasReconnectHint(content: String): Boolean =
    content.contains(RECONNECT_MARKER, ignoreCase = true)

/**
 * Parses a sent user turn as a connector mention. Syntax-only, no status
 * lookup: the render path must never depend on the network, and an
 * `@gmail` turn sent while disconnected stays visible in the chip rather
 * than silently looking like plain text (the reconnect prompt beside it
 * says what to do). Unknown `@foo` is not a mention — plain text.
 */
fun parseConnectorMention(content: String): ConnectorMention? {
    val m = MENTION_COMMAND.matchEntire(content.trimEnd()) ?: return null
    return ConnectorMention(
        name = m.groupValues[1].lowercase(Locale.ROOT),
        args = m.groupValues[2],
    )
}

/**
 * `@` trigger (#441): the composer text starts with `@` and the first
 * token is a partial connector query — `@` alone (and `@ `) lists both.
 * Returns null when there is no trigger: `@` anywhere but position 0, a
 * complete `@gmail` mention (typed by hand or just inserted — the picker
 * must not reopen over its own insertion while the query is typed), or a
 * token outside the mention alphabet.
 */
fun mentionQuery(text: String): String? {
    if (!text.startsWith("@")) return null
    val token = text.substring(1).takeWhile { !it.isWhitespace() }
    if (token.isEmpty()) return ""
    val folded = token.lowercase(Locale.ROOT)
    // A complete mention closes the picker: `@gmail`, `@gmail query…`.
    // Unlike `/skill:` there is no long-form prefix to discover, so any
    // token starting with a full id plus more non-space text is either a
    // complete mention (space follows) or not mention-shaped at all.
    for (id in MENTIONABLE_CONNECTORS) {
        if (folded == id) return null
        if (folded.startsWith(id)) return null
    }
    // Not a prefix of any connector id, no rows could match: plain text,
    // not a trigger (#485 review — `@foo` must not open an empty picker).
    if (MENTIONABLE_CONNECTORS.none { it.startsWith(folded) }) return null
    if (!MENTION_QUERY_CHARS.matches(token)) return null
    return token
}

/**
 * Builds the composer text after tapping a picker row: the `@` token
 * becomes `@<name> ` (trailing space) and anything already typed after it
 * is kept as the query text. Precondition: [current] starts with `@`.
 */
fun insertConnectorMention(current: String, name: String): String {
    require(current.startsWith("@")) { "insertConnectorMention needs an @ trigger" }
    val afterToken = current.dropWhile { !it.isWhitespace() }
    return if (afterToken.isBlank()) {
        "@$name "
    } else {
        "@$name ${afterToken.trimStart()}"
    }
}
