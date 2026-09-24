package com.vishnu.agento

import android.content.Context
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import okhttp3.OkHttpClient
import okhttp3.Request
import org.json.JSONArray
import org.json.JSONObject
import java.util.concurrent.TimeUnit

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
 * differ across gateway versions, so unknown fields are ignored and empty
 * results are failures the UI renders as hints, never raw dumps.
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
     * returns a bare JSON array; object-wrapped shapes fall back gracefully. */
    suspend fun listSkills(path: String): Result<List<SkillInfo>> =
        withContext(Dispatchers.IO) {
            get(path, "v1/skills").map { body ->
                val out = mutableListOf<SkillInfo>()
                val arr = rootArray(body, "skills", "data", "items")
                if (arr != null) {
                    for (i in 0 until arr.length()) {
                        val o = arr.optJSONObject(i) ?: continue
                        val name = o.optString("name", "")
                            .ifEmpty { o.optString("id", "") }
                            .ifEmpty { o.optString("slug", "") }
                            .trim()
                        if (name.isEmpty()) continue
                        val enabled = when {
                            o.has("enabled") -> o.optBoolean("enabled")
                            o.has("active") -> o.optBoolean("active")
                            else -> null
                        }
                        out.add(SkillInfo(
                            name = name,
                            description = o.optString("description", "").trim(),
                            category = o.optString("category", "").trim(),
                            enabled = enabled,
                        ))
                    }
                }
                if (out.isEmpty()) throw RuntimeException("No skills listed")
                out.sortedBy { it.name.lowercase() }
            }
        }

    /** Lists toolsets for one profile path; MCP servers derive from tool names.
     * The server returns a bare JSON array; object-wrapped shapes fall back. */
    suspend fun listToolsets(path: String): Result<List<ToolsetInfo>> =
        withContext(Dispatchers.IO) {
            get(path, "v1/toolsets").map { body ->
                val out = mutableListOf<ToolsetInfo>()
                val arr = rootArray(body, "toolsets", "data", "items")
                if (arr != null) {
                    for (i in 0 until arr.length()) {
                        val o = arr.optJSONObject(i) ?: continue
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
                        out.add(ToolsetInfo(name, tools))
                    }
                }
                if (out.isEmpty()) throw RuntimeException("No toolsets listed")
                out.sortedBy { it.name.lowercase() }
            }
        }

    /**
     * Response root as an array: the documented shape is a bare top-level
     * array, with object-wrapped variants (`{skills:[...]}` etc.) as fallback.
     * Null when the body is neither.
     */
    private fun rootArray(body: String, vararg keys: String): JSONArray? {
        try {
            return JSONArray(body)
        } catch (_: Exception) {
        }
        val o = try {
            JSONObject(body)
        } catch (_: Exception) {
            return null
        }
        for (k in keys) {
            o.optJSONArray(k)?.let { return it }
        }
        return null
    }
}

/** Groups `mcp__<server>__<tool>` tools into per-server rows. */
fun mcpServersFrom(toolsets: List<ToolsetInfo>): List<McpServer> {
    val byServer = linkedMapOf<String, MutableList<String>>()
    for (ts in toolsets) {
        for (t in ts.tools) {
            val parts = t.split("__")
            if (parts.size >= 3 && parts[0] == "mcp") {
                byServer.getOrPut(parts[1]) { mutableListOf() }
                    .add(parts.drop(2).joinToString("__"))
            }
        }
    }
    return byServer.map { (name, tools) ->
        McpServer(name, tools.distinct().sorted())
    }.sortedBy { it.name.lowercase() }
}
