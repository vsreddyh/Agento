package com.vishnu.agento

import android.content.Context
import android.util.Log
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

/** One message for a server that does not speak the canonical project list
 * (#433): names the missing shape and the fix, never a clean empty list. */
internal const val PROJECTS_SHAPE_ERROR =
    "Unexpected response shape — the server sent no projects list. Update the server."

/** One project row from the shared `projects` collection, served by
 * health-api (`/api/projects`) and managed by the agent over the
 * project-manager MCP. The app renders five statuses
 * (Todo/Ongoing/Testing/Paused/Done) and maps legacy `Blocked` rows to
 * `Paused` for display; the server still accepts `Blocked` until the #527
 * migration lands. */
data class ServerProject(
    val id: String = "",
    val name: String = "",
    val status: String = "Todo",
    val note: String = "",
    val createdAt: String = "",
    val updatedAt: String = "",
    /** Manual board order (#519), ascending; null on rows written before
     * the field existed, which sort after every positioned row. */
    val position: Long? = null,
    /** RFC3339 instant the server will wake this Paused project to Ongoing
     * (one minute early, at snoozeAt-1m), or blank when it is not snoozed.
     * Only Paused rows can carry one; the server clears it on the move. */
    val snoozeAt: String = "",
    /** RFC3339 instant of the snooze a server ticker actually fired, blank on
     * any row the ticker did not wake. It names the *snooze* instant, not the
     * tick, so a client can compare it with what it cached and ignore a
     * manual Paused -> Ongoing move. */
    val snoozeFiredAt: String = "",
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
 * Client for the project board (same bearer auth as the other clients, served
 * through the proxy's /api/ route — no profile prefix; the collection is
 * global, not per-assistant). Full CRUD against the same store and
 * validation the agent uses over MCP. Row fields are lenient (missing reads
 * blank, corrupt rows without any id are skipped and counted), but the list
 * envelope is the canonical `projects` array only and a row carrying the
 * retired `_id` without a canonical `id` is an old-server shape that throws
 * [PROJECTS_SHAPE_ERROR] instead of rendering as an empty board (#433).
 */
/** One project row: null when the row carries no usable id or name.
 * Canonical `id` only (#433): the health-api maps Mongo `_id` to `id`
 * (projects.toDoc), so the old `_id` fallback only ever bridged a shape
 * this app does not speak. */
internal fun parseProject(o: JSONObject): ServerProject? {
    // Every string through optJsonStr (#130): a bare optString would coerce
    // JSON null to the literal string "null" — a phantom id that passes the
    // isEmpty gate and renders as a row.
    val id = optJsonStr(o, "id")
    if (id.isEmpty()) return null
    val name = optJsonStr(o, "name")
    if (name.isEmpty()) return null
    // Position is a JSON number on new servers; anything else (absent,
    // null, a string from a hand-edited row) reads as "no manual position"
    // so it falls back to the updatedAt ordering instead of 0, which is
    // below every assigned position and would silently pin it to the top.
    val position = (o.opt("position") as? Number)?.toLong()
    return ServerProject(
        id = id,
        name = name,
        status = optJsonStr(o, "status").ifEmpty { "Todo" },
        note = optJsonStr(o, "note"),
        createdAt = optJsonStr(o, "createdAt"),
        updatedAt = optJsonStr(o, "updatedAt"),
        position = position,
        snoozeAt = optJsonStr(o, "snoozeAt"),
        snoozeFiredAt = optJsonStr(o, "snoozeFiredAt"),
    )
}

/** Default manual board order (#519): status rank first (the board groups
 * by status), then the stored position, then newest-updated. Pure so the
 * ordering rule is unit-tested without a server. */
internal fun manualProjectOrder(list: List<ServerProject>): List<String> =
    list.sortedWith(
        compareBy<ServerProject>(
            { taskRank(normalizeStatus(it.status)) },
            { it.position ?: Long.MAX_VALUE },
            { -projectTimeMs(it.updatedAt) },
        )
    ).map { it.id }

/** Projects in the persisted manual order, rows missing from [order] (a
 * just-created project before the reload) last. */
internal fun sortProjectsManually(
    list: List<ServerProject>,
    order: List<String>,
): List<ServerProject> {
    val rank = order.withIndex().associate { (i, id) -> id to i }
    return list.sortedWith(
        compareBy<ServerProject>(
            { rank[it.id] ?: Int.MAX_VALUE },
            { taskRank(normalizeStatus(it.status)) },
            { -projectTimeMs(it.updatedAt) },
        )
    )
}

/** Parses one project-list body (#433): the canonical `projects`
 * envelope only. A missing envelope or an unreadable body throws
 * [PROJECTS_SHAPE_ERROR], never a clean empty that reads as "no
 * projects". A row that carries the retired `_id` without a canonical
 * `id` is an old-server shape and throws; rows missing every id or name
 * are corrupt, not version skew, so they are skipped and counted in
 * logcat. Pure for testability. */
internal fun parseProjectList(body: String): List<ServerProject> {
    val out = mutableListOf<ServerProject>()
    val root = runCatching { JSONObject(body) }.getOrNull()
        ?: throw RuntimeException(PROJECTS_SHAPE_ERROR)
    // Canonical `projects` envelope only (#433): the old
    // data/items fallbacks and the silent empty on an unknown
    // shape are gone, so a shape mismatch is a visible error.
    val arr = root.optJSONArray("projects")
        ?: throw RuntimeException(PROJECTS_SHAPE_ERROR)
    var dropped = 0
    for (i in 0 until arr.length()) {
        val o = arr.optJSONObject(i)
        if (o == null) { dropped++; continue }
        // Retired `_id`-only row: the old health-api shape — fail loudly
        // so version skew never reads as an empty board. optJsonStr, not
        // optString: a JSON null id must read as missing (#130), not as the
        // literal "null" that would slip past this guard.
        if (optJsonStr(o, "id").isEmpty() && hasValue(o, "_id")) {
            throw RuntimeException(PROJECTS_SHAPE_ERROR)
        }
        val p = parseProject(o)
        if (p != null) out.add(p) else dropped++
    }
    if (dropped > 0) {
        // Same diagnostics deal as the task page: background readers
        // take the list and never see the count (see TasksApi).
        runCatching {
            Log.w("ProjectsApi", "dropped $dropped unparseable project row(s) in list response")
        }
    }
    return out
}

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

    /** The one canonical endpoint (#433): [ServerEndpoint] owns the key and
     * the shape, so this client cannot drift onto a retired one. */
    private fun base(): String = ServerEndpoint.base(prefs)

    private fun password(): String = ServerEndpoint.password(prefs)

    /** Lists projects by status (`Todo|Ongoing|Testing|Paused|Done|all`, default
     * all). The picker offers these five and create/update never send
     * `Blocked` for new values, but drag-reorder addresses the dragged
     * row's stored section — so an unmigrated `Blocked` row still lists /
     * reorders `Blocked` until the #527 migration. Other unknown statuses
     * fail fast. `search` matches name/note. `limit` caps rows (server
     * default 200, max 500). */
    suspend fun list(status: String = "all", search: String = "", limit: Int = 200): Result<List<ServerProject>> =
        withContext(Dispatchers.IO) {
            val base = base()
            if (base.isEmpty()) {
                return@withContext Result.failure(IllegalStateException(ServerEndpoint.UNCONFIGURED))
            }
            val s = status.trim().ifEmpty { "all" }
            val q = "?status=" + URLEncoder.encode(s, Charsets.UTF_8.name()).replace("+", "%20") +
                (if (search.isNotBlank()) "&search=" + URLEncoder.encode(search.trim(), Charsets.UTF_8.name()).replace("+", "%20") else "") +
                "&limit=$limit"
            call("GET", "/api/projects$q").mapCatching { body -> parseProjectList(body) }
        }

    /** Creates a project; name required, blank status defaults to Todo.
     * [snoozeAt] is an RFC3339 instant and is only valid with Paused; blank
     * means no snooze. */
    suspend fun create(
        name: String,
        status: String = "Todo",
        note: String = "",
        snoozeAt: String = "",
    ): Result<ServerProject> = withContext(Dispatchers.IO) {
        if (name.trim().isEmpty()) {
            return@withContext Result.failure(IllegalArgumentException("Name is required"))
        }
        val body = JSONObject().put("name", name.trim())
        if (status.isNotBlank()) body.put("status", status.trim())
        if (note.isNotEmpty()) body.put("note", note)
        if (snoozeAt.isNotBlank()) body.put("snoozeAt", snoozeAt.trim())
        call("POST", "/api/projects", body).map { parseOne(it) }
    }

    /** Partial edit: only non-null keys are sent. A null [snoozeAt] keeps the
     * stored snooze; an empty string clears it (the server rejects a snooze on
     * a non-Paused project). */
    suspend fun update(
        id: String,
        name: String? = null,
        status: String? = null,
        note: String? = null,
        snoozeAt: String? = null,
    ): Result<ServerProject> = withContext(Dispatchers.IO) {
        val clean = encodeId(id)
        if (clean.isEmpty()) {
            return@withContext Result.failure(IllegalArgumentException("Missing project id"))
        }
        val body = JSONObject()
        if (name != null) body.put("name", name)
        if (status != null) body.put("status", status)
        if (note != null) body.put("note", note)
        if (snoozeAt != null) body.put("snoozeAt", snoozeAt.trim())
        call("PATCH", "/api/projects/$clean", body).map { parseOne(it) }
    }

    /** Rewrites the manual order of one status section (#519). The server
     * rejects ids that no longer carry [status] or that are unknown. */
    suspend fun reorder(status: String, ids: List<String>): Result<Unit> =
        withContext(Dispatchers.IO) {
            val clean = ids.map { it.trim() }.filter { it.isNotEmpty() }
            val st = status.trim()
            if (st.isEmpty()) {
                return@withContext Result.failure(IllegalArgumentException("Missing status"))
            }
            if (clean.isEmpty()) {
                return@withContext Result.failure(IllegalArgumentException("Missing project ids"))
            }
            val body = JSONObject()
                .put("status", st)
                .put("ids", org.json.JSONArray(clean))
            call("POST", "/api/projects/reorder", body).map { }
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
            ?: throw RuntimeException(PROJECTS_SHAPE_ERROR)

    /** Ids are hex today, but encoded defensively so a malformed id can
     * never break the request path (the `+` fix-up mirrors TasksApi:
     * form-encoding emits `+` for space, valid only in query strings). */
    private fun encodeId(id: String): String =
        URLEncoder.encode(id.trim(), Charsets.UTF_8.name()).replace("+", "%20")

    private fun call(method: String, url: String, body: JSONObject? = null): Result<String> {
        val base = base()
        if (base.isEmpty()) {
            return Result.failure(IllegalStateException(ServerEndpoint.UNCONFIGURED))
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
                    // Server URL points at the retired agent gateway (:8643)
                    // instead of the proxy (:8080) — :8643 served the chat-era
                    // agent this app no longer uses, while /api/* lives on
                    // health-api behind the proxy. Say so instead of
                    // surfacing a bare "Not Found".
                    if (response.code == 404 && url.startsWith("/api/")) {
                        throw RuntimeException(
                            "HTTP 404: ${text.take(200)} (is the Server URL " +
                                "the :8080 proxy? Direct :8643 is the retired agent gateway)"
                        )
                    }
                    throw RuntimeException("HTTP ${response.code}: ${text.take(200)}")
                }
                text
            }
        }
    }
}
