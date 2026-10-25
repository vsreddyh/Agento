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

/**
 * Parses a sent user turn as a skill command. Syntax-only, no inventory
 * lookup: the render path must never depend on the network, and a
 * mistyped name stays visible in the chip label rather than silently
 * looking valid. Stored text is never rewritten — this is a render-time
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
 */
fun slashQuery(text: String): String? {
    if (!text.startsWith("/")) return null
    val token = text.substring(1).takeWhile { !it.isWhitespace() }
    if (token.isEmpty()) return ""
    if (COMMAND_TOKEN.matches(token)) return null
    return token.removePrefix("skill:")
}

/**
 * Builds the composer text after tapping a picker row: the `/` token
 * becomes `/skill:<name> ` (trailing space) and anything already typed
 * after it is kept as the skill's argument text. Precondition: [current]
 * starts with `/` (callers only invoke it off an active trigger).
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
 * taught rows (`scope == "taught"`) that Pi never loaded, and sending
 * `/skill:<name>` for one burns a turn on literal text. Pure so the
 * exclusion is pinned without a Context.
 */
fun invocableSkills(skills: List<SkillInfo>): List<SkillInfo> =
    skills.filter { it.scope != "taught" }
