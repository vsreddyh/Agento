package com.vishnu.agento

import android.content.Context
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import org.json.JSONObject
import java.net.URLEncoder
import java.util.Locale
import java.util.concurrent.TimeUnit

private val PROJECT_JSON_MEDIA = "application/json; charset=utf-8".toMediaType()

/** One project row from the shared `projects` collection, served by
 * health-api (`/api/projects`) and managed by the agent over the
 * project-manager MCP. Status is one of Todo/Ongoing/Paused/Done. */
data class ServerProject(
    val id: String = "",
    val name: String = "",
    val status: String = "Todo",
    val note: String = "",
    val createdAt: String = "",
    val updatedAt: String = "",
)

/** Parses an RFC3339 instant (server timestamps) to epoch millis for
 * shortTime(); unparseable input hides the timestamp (0). */
fun projectTimeMs(iso: String): Long {
    if (iso.isBlank()) return 0L
    return runCatching {
        java.time.Instant.parse(iso.trim()).toEpochMilli()
    }.getOrDefault(0L)
}

/**
 * Client for the project board (same bearer auth as chat/sync, served
 * through the proxy's /api/ route — no profile prefix; the collection is
 * global, not per-assistant). Full CRUD against the same store and
 * validation the agent uses over MCP. Parsing is lenient across shapes;
 * rows without an id or name are skipped, missing fields blank.
 */
class ProjectsApi(context: Context) {

    companion object {
        // One shared client: each OkHttpClient owns a connection pool and
        // dispatcher threads, so per-call instances would leak both.
        private val sharedHttp: OkHttpClient by lazy {
            OkHttpClient.Builder()
                .connectTimeout(10, TimeUnit.SECONDS)
                .readTimeout(30, TimeUnit.SECONDS)
                .writeTimeout(30, TimeUnit.SECONDS)
                .build()
        }
    }

    private val appCtx = context.applicationContext
    private val prefs = appCtx.getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
    private val http: OkHttpClient get() = sharedHttp

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

    private fun parseProject(o: JSONObject): ServerProject? {
        val id = o.optString("id", "")
            .ifEmpty { o.optString("_id", "") }.trim()
        if (id.isEmpty()) return null
        val name = o.optString("name", "").trim()
        if (name.isEmpty()) return null
        return ServerProject(
            id = id,
            name = name,
            status = o.optString("status", "Todo").trim().ifEmpty { "Todo" },
            note = o.optString("note", "").trim(),
            createdAt = o.optString("createdAt", "").trim(),
            updatedAt = o.optString("updatedAt", "").trim(),
        )
    }

    /** Lists projects by status (`Todo|Ongoing|Paused|Done|all`, default
     * all); unknown statuses fail fast. `search` matches name/note. `limit`
     * caps rows (server default 200, max 500). */
    suspend fun list(status: String = "all", search: String = "", limit: Int = 200): Result<List<ServerProject>> =
        withContext(Dispatchers.IO) {
            val base = base()
            if (base.isEmpty()) {
                return@withContext Result.failure(IllegalStateException("Server URL not configured"))
            }
            val s = status.trim().ifEmpty { "all" }
            val q = "?status=" + URLEncoder.encode(s, Charsets.UTF_8.name()).replace("+", "%20") +
                (if (search.isNotBlank()) "&search=" + URLEncoder.encode(search.trim(), Charsets.UTF_8.name()).replace("+", "%20") else "") +
                "&limit=$limit"
            call("GET", "/api/projects$q").map { body ->
                val out = mutableListOf<ServerProject>()
                val root = JSONObject(body)
                val arr = root.optJSONArray("projects")
                    ?: root.optJSONArray("data")
                    ?: root.optJSONArray("items")
                    ?: return@map out
                for (i in 0 until arr.length()) {
                    val o = arr.optJSONObject(i) ?: continue
                    parseProject(o)?.let { out.add(it) }
                }
                out
            }
        }

    /** Creates a project; name required, blank status defaults to Todo. */
    suspend fun create(
        name: String,
        status: String = "Todo",
        note: String = "",
    ): Result<ServerProject> = withContext(Dispatchers.IO) {
        if (name.trim().isEmpty()) {
            return@withContext Result.failure(IllegalArgumentException("Name is required"))
        }
        val body = JSONObject().put("name", name.trim())
        if (status.isNotBlank()) body.put("status", status.trim())
        if (note.isNotEmpty()) body.put("note", note)
        call("POST", "/api/projects", body).map { parseOne(it) }
    }

    /** Partial edit: only non-null keys are sent. */
    suspend fun update(
        id: String,
        name: String? = null,
        status: String? = null,
        note: String? = null,
    ): Result<ServerProject> = withContext(Dispatchers.IO) {
        val clean = encodeId(id)
        if (clean.isEmpty()) {
            return@withContext Result.failure(IllegalArgumentException("Missing project id"))
        }
        val body = JSONObject()
        if (name != null) body.put("name", name)
        if (status != null) body.put("status", status)
        if (note != null) body.put("note", note)
        call("PATCH", "/api/projects/$clean", body).map { parseOne(it) }
    }

    /** Permanently deletes a project. */
    suspend fun delete(id: String): Result<Unit> =
        withContext(Dispatchers.IO) {
            val clean = encodeId(id)
            if (clean.isEmpty()) {
                return@withContext Result.failure(IllegalArgumentException("Missing project id"))
            }
            call("DELETE", "/api/projects/$clean").map { }
        }

    private fun parseOne(body: String): ServerProject =
        parseProject(JSONObject(body))
            ?: throw RuntimeException("Unexpected response shape")

    /** Ids are hex today, but encoded defensively so a malformed id can
     * never break the request path (the `+` fix-up mirrors TasksApi:
     * form-encoding emits `+` for space, valid only in query strings). */
    private fun encodeId(id: String): String =
        URLEncoder.encode(id.trim(), Charsets.UTF_8.name()).replace("+", "%20")

    private fun call(method: String, url: String, body: JSONObject? = null): Result<String> {
        val base = base()
        if (base.isEmpty()) {
            return Result.failure(IllegalStateException("Server URL not configured"))
        }
        return runCatching {
            val builder = Request.Builder()
                .url(base + url)
                .header("Authorization", "Bearer ${password()}")
            when (method) {
                "GET" -> builder.get()
                "POST" -> builder.post((body?.toString() ?: "{}").toRequestBody(PROJECT_JSON_MEDIA))
                "PATCH" -> builder.patch((body?.toString() ?: "{}").toRequestBody(PROJECT_JSON_MEDIA))
                "DELETE" -> builder.delete()
                else -> throw IllegalArgumentException("Unknown method: $method")
            }
            http.newCall(builder.build()).execute().use { response ->
                val text = response.body?.string() ?: ""
                if (!response.isSuccessful) {
                    // A 404 on an /api/* route almost always means the app's
                    // Server URL points at the gateway (:8642) instead of the
                    // proxy (:8080) — the gateway serves chat only, while
                    // /api/* lives on health-api behind the proxy. Say so
                    // instead of surfacing a bare "Not Found".
                    if (response.code == 404 && url.startsWith("/api/")) {
                        throw RuntimeException(
                            "HTTP 404: ${text.take(200)} (is the Server URL " +
                                "the :8080 proxy? Direct :8642 serves chat only)"
                        )
                    }
                    throw RuntimeException("HTTP ${response.code}: ${text.take(200)}")
                }
                text
            }
        }
    }
}
