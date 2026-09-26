package com.vishnu.agento

import android.content.Context
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import okhttp3.OkHttpClient
import okhttp3.Request
import org.json.JSONArray
import org.json.JSONException
import org.json.JSONObject
import java.util.concurrent.TimeUnit
import java.util.Locale

/** One skill from `GET {profile}/v1/skills` (read-only mirror of the dashboard). */
data class SkillInfo(
    val name: String,
    val description: String = "",
    val category: String = "",
    /** Null when the server doesn't report toggle state. */
    val enabled: Boolean? = null,
)

/** One toolset from `GET {profile}/v1/toolsets` with its concrete tools. */
data class ToolsetInfo(
    val name: String,
    val label: String = "",
    val description: String = "",
    val enabled: Boolean? = null,
    val tools: List<String> = emptyList(),
)

/** MCP server derived from `mcp__<server>__<tool>` tool names. */
data class McpServer(
    val name: String,
    val tools: List<String>,
)

/**
 * Read-only view of the gateway's skills + toolsets (dashboard Skills/MCP
 * pages, slimmed for mobile). Same bearer auth as chat; per-profile paths
 * so each assistant shows its own inventory. Parsing is lenient — shapes
 * differ across gateway versions, so unknown fields are ignored; valid
 * empty results are success and only transport/parse failures are errors.
 */
class ServerApi(context: Context) {

    private val appCtx = context.applicationContext
    private val prefs = appCtx.getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
    private val http = OkHttpClient.Builder()
        .connectTimeout(10, TimeUnit.SECONDS)
        .readTimeout(30, TimeUnit.SECONDS)
        .writeTimeout(30, TimeUnit.SECONDS)
        .build()

    private fun base(): String {
        for (k in listOf("server_base_url", "server_url", "api_base_url")) {
            val v = (prefs.getString(k, "") ?: "").trim().trimEnd('/')
            if (v.isNotEmpty()) return v
        }
        return ""
    }

    private fun password(): String {
        val v = (prefs.getString("app_password", "") ?: "").trim()
        if (v.isNotEmpty()) return v
        return (prefs.getString("auth_token", "") ?: "").trim()
    }

    private fun get(path: String, endpoint: String): Result<String> {
        val base = base()
        if (base.isEmpty()) {
            return Result.failure(IllegalStateException("Server URL not configured"))
        }
        return runCatching {
            http.newCall(Request.Builder()
                .url("$base$path/$endpoint")
                .header("Authorization", "Bearer ${password()}")
                .get()
                .build()).execute().use { response ->
                val body = response.body?.string() ?: ""
                if (!response.isSuccessful) {
                    throw RuntimeException("HTTP ${response.code}: ${body.take(200)}")
                }
                body
            }
        }
    }

    /** Lists skills for one profile path (e.g. `/p/default`). The server
     * returns a bare JSON array; object-wrapped shapes fall back gracefully.
     * A valid-but-empty response is success (the UI shows "Nothing listed");
     * only transport/parse failures are errors. */
    suspend fun listSkills(path: String): Result<List<SkillInfo>> =
        withContext(Dispatchers.IO) {
            get(path, "v1/skills").map { body ->
                val out = mutableListOf<SkillInfo>()
                val arr = rootArray(body, "skills", "data", "items")
                    ?: throw RuntimeException("Unexpected response shape")
                for (i in 0 until arr.length()) {
                    val item = arr.opt(i)
                    if (item is String) {
                        if (item.isNotBlank()) {
                            out.add(SkillInfo(name = item.trim()))
                        }
                        continue
                    }
                    val o = item as? JSONObject ?: continue
                    val name = o.optString("name", "")
                        .ifEmpty { o.optString("id", "") }
                        .ifEmpty { o.optString("slug", "") }
                        .trim()
                    if (name.isEmpty()) continue
                    out.add(SkillInfo(
                        name = name,
                        description = o.optString("description", "").trim(),
                        category = o.optString("category", "").trim(),
                        enabled = optBool(o),
                    ))
                }
                out.distinctBy { it.name.lowercase(Locale.ROOT) }.sortedBy { it.name.lowercase(Locale.ROOT) }
            }
        }

    /** Lists toolsets for one profile path; MCP servers derive from tool names.
     * The server returns a bare JSON array; object-wrapped shapes fall back. */
    suspend fun listToolsets(path: String): Result<List<ToolsetInfo>> =
        withContext(Dispatchers.IO) {
            get(path, "v1/toolsets").map { body ->
                val out = mutableListOf<ToolsetInfo>()
                val arr = rootArray(body, "toolsets", "data", "items")
                    ?: throw RuntimeException("Unexpected response shape")
                for (i in 0 until arr.length()) {
                    val item = arr.opt(i)
                    if (item is String) {
                        if (item.isNotBlank()) {
                            out.add(ToolsetInfo(name = item.trim()))
                        }
                        continue
                    }
                    val o = item as? JSONObject ?: continue
                    val name = o.optString("name", "")
                        .ifEmpty { o.optString("id", "") }
                        .trim()
                    if (name.isEmpty()) continue
                    val tools = mutableListOf<String>()
                    val tarr = o.optJSONArray("tools")
                    if (tarr != null) {
                        for (j in 0 until tarr.length()) {
                            when (val t = tarr.opt(j)) {
                                is String -> if (t.isNotBlank()) tools.add(t.trim())
                                is JSONObject -> {
                                    val tn = t.optString("name", "").trim()
                                    if (tn.isNotEmpty()) tools.add(tn)
                                }
                            }
                        }
                    }
                    out.add(ToolsetInfo(
                        name = name,
                        label = o.optString("label", "").trim(),
                        description = o.optString("description", "").trim(),
                        enabled = optBool(o),
                        tools = tools,
                    ))
                }
                out.distinctBy { it.name.lowercase(Locale.ROOT) }.sortedBy { it.name.lowercase(Locale.ROOT) }
            }
        }

    /** Tools + skills used during one server-side turn, read back from the
     * gateway session the app pinned via `X-Hermes-Session-Id`. Assistant
     * messages carry `tool_calls` (name + arguments JSON); tool-result rows
     * carry `tool_name`. Skill names come from [skillNamesFromToolCall].
     * Unknown/empty sessions are success-with-empty (the live tool frames
     * already attached at Done stay); only transport/parse failures error. */
    suspend fun fetchSessionUsage(path: String, sessionId: String): Result<SessionUsage> =
        withContext(Dispatchers.IO) {
            if (sessionId.isBlank()) {
                return@withContext Result.failure(IllegalStateException("Session id not set"))
            }
            get(path, "api/sessions/${urlEncode(sessionId)}/messages").map { body ->
                val calls = mutableListOf<SessionToolCall>()
                val arr = rootArray(body, "messages", "data", "items")
                    ?: throw RuntimeException("Unexpected response shape")
                for (i in 0 until arr.length()) {
                    val o = arr.optJSONObject(i) ?: continue
                    when (o.optString("role", "")) {
                        "assistant" -> {
                            val tarr = o.optJSONArray("tool_calls") ?: continue
                            for (j in 0 until tarr.length()) {
                                val tc = tarr.optJSONObject(j) ?: continue
                                val fn = tc.optJSONObject("function") ?: continue
                                val name = fn.optString("name", "").trim()
                                if (name.isEmpty()) continue
                                calls.add(SessionToolCall(
                                    name = name,
                                    arguments = fn.optString("arguments", ""),
                                ))
                            }
                        }
                        "tool" -> {
                            val name = o.optString("tool_name", "").trim()
                            if (name.isNotEmpty()) calls.add(SessionToolCall(name = name))
                        }
                    }
                }
                usageFromToolCalls(calls)
            }
        }

    /**
     * Strict toggle parse: only real booleans count — strings, numbers and
     * nulls read as unknown (null) instead of Off.
     */
    private fun optBool(o: JSONObject): Boolean? {
        for (k in listOf("enabled", "active")) {
            if (!o.isNull(k)) {
                val v = o.opt(k)
                if (v is Boolean) return v
            }
        }
        return null
    }

    /**
     * Response root as an array: the documented shape is a bare top-level
     * array, with object-wrapped variants (`{skills:[...]}` etc.) as fallback.
     * Null when the body is neither.
     */
    private fun rootArray(body: String, vararg keys: String): JSONArray? {
        try {
            return JSONArray(body)
        } catch (_: JSONException) {
        }
        val o = try {
            JSONObject(body)
        } catch (_: JSONException) {
            return null
        }
        for (k in keys) {
            o.optJSONArray(k)?.let { return it }
        }
        return null
    }
}

/** One tool call recorded on a gateway session message. */
data class SessionToolCall(
    val name: String,
    val arguments: String = "",
)

/** Tools + skills used during one server-side turn. */
data class SessionUsage(
    val tools: List<String> = emptyList(),
    val skills: List<String> = emptyList(),
)

/** `skills/<name>/SKILL.md` paths inside tool arguments mark skill use. */
private val SKILL_MD_PATH = Regex("""skills/([A-Za-z0-9_-]+)/SKILL\.md""", RegexOption.IGNORE_CASE)

/** Named skill reference inside `skill_*` tool arguments. Keys are
 * deliberately skill-specific: generic `name`/`id` keys (e.g.
 * `{"name": "read_file"}`) must never become "skills". */
private val SKILL_ARG_NAME = Regex(""""(?:skill|skill_name|skill_id)"\s*:\s*"([A-Za-z0-9_-]+)"""")

/**
 * Heuristic skill attribution for one tool call: the gateway has no
 * first-class "skill used" signal, so a skill counts as used when the agent
 * read its SKILL.md (via read_file/search_files/skill_view/...) or invoked
 * a `skill_*` tool naming it. Pure for testability.
 */
fun skillNamesFromToolCall(tool: String, args: String): List<String> {
    val out = mutableListOf<String>()
    SKILL_MD_PATH.findAll(args).forEach { out.add(it.groupValues[1]) }
    if (tool.equals("skill", ignoreCase = true) || tool.startsWith("skill_", ignoreCase = true)) {
        SKILL_ARG_NAME.findAll(args).forEach { out.add(it.groupValues[1]) }
    }
    return out.distinct()
}

/** Distinct tool names + attributed skills across [calls], order preserved. */
fun usageFromToolCalls(calls: List<SessionToolCall>): SessionUsage {
    val tools = calls.mapNotNull { it.name.trim().ifEmpty { null } }.distinct()
    val skills = calls.flatMap { skillNamesFromToolCall(it.name, it.arguments) }.distinct()
    return SessionUsage(tools = tools.take(20), skills = skills.take(20))
}

/** URL-encodes one path segment (session ids are UUIDs today, encoded
 * defensively so a malformed id can never break the request path; the `+`
 * fix-up is needed because form-encoding emits `+` for space, which is only
 * valid in query strings, not path segments). */
private fun urlEncode(segment: String): String =
    java.net.URLEncoder.encode(segment, Charsets.UTF_8.name()).replace("+", "%20")

/** Groups `mcp__<server>__<tool>` tools into per-server rows. */
fun mcpServersFrom(toolsets: List<ToolsetInfo>): List<McpServer> {
    val byServer = linkedMapOf<String, MutableList<String>>()
    for (ts in toolsets) {
        for (t in ts.tools) {
            val parts = t.split("__")
            if (parts.size >= 3 && parts[0] == "mcp" && parts[1].isNotBlank()) {
                byServer.getOrPut(parts[1]) { mutableListOf() }
                    .add(parts.drop(2).joinToString("__"))
            }
        }
    }
    return byServer.map { (name, tools) ->
        McpServer(name, tools.distinct().sorted())
    }.sortedBy { it.name.lowercase(Locale.ROOT) }
}

/**
 * Origin split for the Skills screen's two sections.
 *
 * Assumption: in-the-box Hermes skills always carry a `category`
 * (e.g. "creative", "web"); project skills (podman-management,
 * git-remote-preflight, …) report null/empty. So non-blank category =
 * default, blank = custom. Bare-string server shapes have no category and
 * always land in Custom; if the gateway ever omits categories entirely,
 * the screen falls back to treating every skill as default (see
 * SkillsScreen's anyCategorized guard) rather than emptying Default.
 */
fun SkillInfo.isDefault(): Boolean = category.isNotBlank()

/** Toolset names that are project MCP servers rather than built-ins. */
private val CUSTOM_MCP_TOOLSET_NAMES = setOf(
    "miser-money", "miser_money",
    "mcp-miser-money", "mcp-cookbook", "mcp-health-check",
    "cookbook", "health-check", "health_check", "money",
)

/**
 * Tools: built-in Hermes toolsets (web, browser, terminal, …) are the
 * default group; project MCP servers (money/cookbook/health-check, any
 * `mcp-*` toolset, or toolsets carrying `mcp__` tools) are custom MCP.
 * Derived [McpServer] rows are always custom.
 */
fun ToolsetInfo.isCustomMcp(): Boolean {
    val n = name.trim().lowercase(Locale.ROOT)
    if (n == "mcp" || n.startsWith("mcp-") || n.startsWith("mcp_")) return true
    if (n in CUSTOM_MCP_TOOLSET_NAMES) return true
    if (tools.any { it.lowercase(Locale.ROOT).startsWith("mcp__") }) return true
    return false
}

/** Case-insensitive search across name + description (+ category/tools). */
fun SkillInfo.matches(query: String): Boolean {
    val q = query.trim().lowercase(Locale.ROOT)
    if (q.isEmpty()) return true
    return name.lowercase(Locale.ROOT).contains(q)
        || description.lowercase(Locale.ROOT).contains(q)
        || category.lowercase(Locale.ROOT).contains(q)
}

fun ToolsetInfo.matches(query: String): Boolean {
    val q = query.trim().lowercase(Locale.ROOT)
    if (q.isEmpty()) return true
    return name.lowercase(Locale.ROOT).contains(q)
        || label.lowercase(Locale.ROOT).contains(q)
        || description.lowercase(Locale.ROOT).contains(q)
        || tools.any { it.lowercase(Locale.ROOT).contains(q) }
}

fun McpServer.matches(query: String): Boolean {
    val q = query.trim().lowercase(Locale.ROOT)
    if (q.isEmpty()) return true
    return name.lowercase(Locale.ROOT).contains(q)
        || tools.any { it.lowercase(Locale.ROOT).contains(q) }
}
