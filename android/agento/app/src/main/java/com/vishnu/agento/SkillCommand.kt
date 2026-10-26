package com.vishnu.agento

/** Parsed `/skill:<name> [args]` user turn (#372). */
data class SkillInvocation(
    val name: String,
    val args: String,
)

/** `^/skill:([A-Za-z0-9_-]+)(?:\s+([\s\S]*))?$`: name required, args optional. */
private val SKILL_COMMAND = Regex("""^/skill:([A-Za-z0-9_-]+)(?:\s+([\s\S]*))?$""")

/** First-token shape of an already-complete hand-typed command. */
private val COMMAND_TOKEN = Regex("""^skill:[A-Za-z0-9_-]+$""")

/** Prefix every skill command carries, lower-case only (see [slashQuery]). */
private const val SKILL_PREFIX = "skill:"

/** Name-shaped query after the prefix: exactly the characters [SKILL_COMMAND]
 * accepts as a name, so a query can never promise a row the send path would
 * refuse to chip. */
private val QUERY_CHARS = Regex("""^[A-Za-z0-9_-]*$""")

/**
 * Parses a sent user turn as a skill command. Syntax-only, no inventory
 * lookup: the render path must never depend on the network, and a
 * mistyped name stays visible in the chip label rather than silently
 * looking valid. That cuts both ways — `/skill:typo` chips confidently
 * for a turn the server treats as literal text — and the tradeoff is
 * deliberate: the chip is a render-time projection, never a validity
 * claim. Stored text is never rewritten — this is a render-time
 * projection, like the send path is wire-verbatim.
 */
fun parseSkillCommand(content: String): SkillInvocation? {
    val m = SKILL_COMMAND.matchEntire(content) ?: return null
    return SkillInvocation(name = m.groupValues[1], args = m.groupValues[2])
}

/**
 * Slash trigger (#372): the composer text starts with `/` and the first
 * token is a partial skill query — returned for filtering, with `/`
 * alone (and `/ `) meaning the empty query that lists everything.
 * Returns null when there is no trigger: a `/` anywhere but position 0,
 * or an already-complete `/skill:<name>` command (typed by hand or just
 * inserted by the picker — the picker must not reopen over its own
 * insertion while arguments are typed).
 *
 * Deliberate: only the short form filters. A `/skill:<prefix>` token is
 * final syntax, not a query — typing the command by hand sends it as-is,
 * and discovery happens through `/` plus the Skills tab.
 *
 * Two shapes close the picker rather than filter it, because the send path
 * rejects both as plain text: an uppercase prefix (`/SKILL:notes` — the
 * prefix match is case-folded here while [SKILL_COMMAND] is lower-case
 * only, so documenting uppercase as plain text beats offering a row that
 * never chips, #395), and a token carrying characters outside
 * `[A-Za-z0-9_-]` (`/skill:na!me`, which would otherwise read as the
 * promise "no matching skills", #396).
 */
fun slashQuery(text: String): String? {
    if (!text.startsWith("/")) return null
    val token = text.substring(1).takeWhile { !it.isWhitespace() }
    if (token.isEmpty()) return ""
    val folded = token.lowercase()
    if (COMMAND_TOKEN.matches(folded)) return null
    val query = if (folded.startsWith(SKILL_PREFIX)) {
        token.substring(SKILL_PREFIX.length)
    } else {
        token
    }
    if (!QUERY_CHARS.matches(query)) return null
    return query
}

/**
 * Builds the composer text after tapping a picker row: the `/` token
 * becomes `/skill:<name> ` (trailing space) and anything already typed
 * after it is kept as the skill's argument text. Precondition: [current]
 * starts with `/` (callers only invoke it off an active trigger).
 *
 * Single-line environment by construction: the composer is one field, so a
 * newline (or any run of whitespace) between the token and the arguments
 * collapses into the one separating space — `/no\narg` becomes
 * `/skill:<name> arg`, pinned by `SkillCommandTest`. A multi-line composer
 * would have to revisit this join deliberately rather than inherit it.
 */
fun insertSkillCommand(current: String, name: String): String {
    val afterToken = current.dropWhile { !it.isWhitespace() }
    return if (afterToken.isBlank()) {
        "/skill:$name "
    } else {
        "/skill:$name ${afterToken.trimStart()}"
    }
}

/**
 * Only Pi-loaded skills may be listed (#372): the inventory also carries
 * taught rows that Pi never loaded, and sending `/skill:<name>` for one
 * burns a turn on literal text. The scope compares case-insensitively —
 * cheaper than debugging a capitalizing server. Disabled rows
 * (`enabled == false`) are excluded for the same reason; an unknown toggle
 * (null — older gateways do not report it) keeps the row. Pure so the
 * exclusion is pinned without a Context.
 */
fun invocableSkills(skills: List<SkillInfo>): List<SkillInfo> =
    skills.filter { !it.scope.equals("taught", ignoreCase = true) && it.enabled != false }
