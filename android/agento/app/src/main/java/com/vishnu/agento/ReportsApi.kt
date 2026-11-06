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
import java.util.Locale
import java.util.concurrent.TimeUnit

/** One message for a server that does not speak the canonical report list
 * (#433): names the missing shape and the fix, never a clean empty inbox. */
internal const val REPORTS_SHAPE_ERROR =
    "Unexpected response shape — the server sent no reports list. Update the server."

/** One message for a server that does not speak the canonical report detail
 * (#433): never a null success that reads as "deleted". */
internal const val REPORT_DETAIL_SHAPE_ERROR =
    "Unexpected response shape — the server sent no report. Update the server."

/** One ∃-Mail report summary (list rows carry no body — `get` fetches it). */
data class ReportSummary(
    val id: String,
    val title: String = "",
    val createdAt: String = "",
    val read: Boolean = false,
    val starred: Boolean = false,
    val preview: String = "",
)

/** One segment of a rendered report body: markdown text or pre-rendered math. */
data class ReportSegment(
    /** "text" (markdown, mikepenz renders) or "math" (HTML, WebView shows). */
    val kind: String,
    val text: String = "",
    val tex: String = "",
    val html: String = "",
    val display: Boolean = false,
)

/** One full ∃-Mail report with server-rendered segments. */
data class ReportDetail(
    val id: String,
    val title: String = "",
    val createdAt: String = "",
    val source: String = "",
    val read: Boolean = false,
    val starred: Boolean = false,
    val bodyMd: String = "",
    val segments: List<ReportSegment> = emptyList(),
    val mathEngine: String = "",
)

internal fun parseReportSummary(o: JSONObject): ReportSummary? {
    // Canonical `id` only (#433): health-api's docOut maps the Mongo `_id`
    // to `id` on the wire, so the old `_id` fallback is gone.
    val id = o.optString("id", "").trim()
    if (id.isEmpty()) return null
    return ReportSummary(
        id = id,
        title = o.optString("title", "").trim(),
        createdAt = o.optString("createdAt", "").trim(),
        read = o.optBoolean("read", false),
        starred = o.optBoolean("starred", false),
        preview = o.optString("preview", "").trim(),
    )
}

internal fun parseReportSegment(o: JSONObject): ReportSegment? {
    val kind = o.optString("kind", "").trim()
    if (kind != "text" && kind != "math") return null
    return ReportSegment(
        kind = kind,
        text = o.optString("text", ""),
        tex = o.optString("tex", ""),
        html = o.optString("html", ""),
        display = o.optBoolean("display", false),
    )
}

internal fun parseReportDetail(o: JSONObject): ReportDetail? {
    // Canonical `id` only (#433): health-api's docOut maps the Mongo `_id`
    // to `id` on the wire, so the old `_id` fallback is gone.
    val id = o.optString("id", "").trim()
    if (id.isEmpty()) return null
    val segments = mutableListOf<ReportSegment>()
    val arr = o.optJSONArray("segments")
    if (arr != null) {
        for (i in 0 until arr.length()) {
            val s = arr.optJSONObject(i) ?: continue
            parseReportSegment(s)?.let { segments.add(it) }
        }
    }
    return ReportDetail(
        id = id,
        title = o.optString("title", "").trim(),
        createdAt = o.optString("createdAt", "").trim(),
        source = o.optString("source", "").trim(),
        read = o.optBoolean("read", false),
        starred = o.optBoolean("starred", false),
        bodyMd = o.optString("body_md", ""),
        segments = segments,
        mathEngine = o.optString("math_engine", "").trim(),
    )
}

/** Parses one report-list body (#433): the canonical `reports` envelope only.
 * A missing envelope or an unreadable body throws [REPORTS_SHAPE_ERROR],
 * never a clean empty that reads as "no reports". A row that carries the
 * retired `_id` without a canonical `id` is an old-server shape and throws;
 * rows missing every id are corrupt, not version skew, so they are skipped
 * and counted in logcat. */
internal fun parseReportSummaries(body: String): List<ReportSummary> {
    val out = mutableListOf<ReportSummary>()
    val root = runCatching { JSONObject(body) }.getOrNull()
        ?: throw RuntimeException(REPORTS_SHAPE_ERROR)
    val arr = root.optJSONArray("reports")
        ?: throw RuntimeException(REPORTS_SHAPE_ERROR)
    var dropped = 0
    for (i in 0 until arr.length()) {
        val o = arr.optJSONObject(i)
        if (o == null) { dropped++; continue }
        // Retired `_id`-only row: the old health-api shape, not a corrupt
        // row — fail loudly so version skew never reads as an empty inbox.
        if (o.optString("id", "").trim().isEmpty() && hasValue(o, "_id")) {
            throw RuntimeException(REPORTS_SHAPE_ERROR)
        }
        val s = parseReportSummary(o)
        if (s != null) out.add(s) else dropped++
    }
    if (dropped > 0) {
        // Same diagnostics deal as the task page: background readers take
        // the list and never see the count (see TasksApi).
        runCatching {
            Log.w("ReportsApi", "dropped $dropped unparseable report row(s) in list response")
        }
    }
    return out
}

/**
 * Formats a report `createdAt` for display. The server emits BSON DateTime as
 * millis (same convention as the other stores), so millis parse first; ISO
 * instants parse as a fallback; anything else shows verbatim rather than
 * hiding the value. IST-pinned like every displayed time, month in English.
 */
internal fun formatReportTime(raw: String): String {
    val clean = raw.trim()
    if (clean.isEmpty()) return ""
    val instant = runCatching { java.time.Instant.ofEpochMilli(clean.toLong()) }.getOrNull()
        ?: runCatching { java.time.Instant.parse(clean) }.getOrNull()
        ?: return raw
    return instant.atZone(IST)
        .format(java.time.format.DateTimeFormatter.ofPattern("d MMM HH:mm", Locale.ENGLISH))
}

/**
 * ∃-Mail client over health-api (`/api/reports`, same bearer auth as the
 * other health-api clients). Unprefixed: unlike the gateway APIs, health-api
 * routes live outside `/p/profile`, so a profile prefix here would route to
 * pi-gateway and 404. Row fields are lenient (missing reads blank), but the
 * list envelope is the canonical `reports` array only and a detail body
 * without a canonical `id` is a failure, never a null success (#433).
 * No compose/reply/send paths exist — the inbox is read-only.
 */
class ReportsApi(context: Context) {

    private val appCtx = context.applicationContext
    private val prefs = appCtx.getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
    private val http = OkHttpClient.Builder()
        .connectTimeout(10, TimeUnit.SECONDS)
        .readTimeout(30, TimeUnit.SECONDS)
        .writeTimeout(30, TimeUnit.SECONDS)
        .build()

    companion object {
        private val JSON = "application/json; charset=utf-8".toMediaType()

        /** Path encoding (`+` is a space in form encoding, not in a path). */
        internal fun encode(v: String): String =
            java.net.URLEncoder.encode(v, Charsets.UTF_8.name()).replace("+", "%20")
    }

    /** The one canonical endpoint (#433): [ServerEndpoint] owns the key and
     * the shape, so this client cannot drift onto a retired one. */
    private fun base(): String = ServerEndpoint.base(prefs)

    private fun password(): String = ServerEndpoint.password(prefs)

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
                "PATCH" -> builder.patch(
                    (body?.toString() ?: "{}").toRequestBody(JSON))
                "DELETE" -> builder.delete()
                else -> builder.get()
            }
            http.newCall(builder.build()).execute().use { response ->
                val text = response.body?.string() ?: ""
                if (!response.isSuccessful) {
                    throw RuntimeException("HTTP ${response.code}: ${text.take(200)}")
                }
                text
            }
        }
    }

    /** Lists reports, newest first. Summaries carry no bodies. */
    suspend fun list(): Result<List<ReportSummary>> =
        withContext(Dispatchers.IO) {
            call("GET", "/api/reports?sort=newest").mapCatching { parseReportSummaries(it) }
        }

    /** Gets one report with server-rendered math segments. An unreadable
     * body is a failure ([REPORT_DETAIL_SHAPE_ERROR]), never a null success
     * that the screen would read as "deleted" (#433). */
    suspend fun get(id: String): Result<ReportDetail> =
        withContext(Dispatchers.IO) {
            call("GET", "/api/reports/${encode(id.trim())}").mapCatching { body ->
                val root = runCatching { JSONObject(body) }.getOrNull()
                    ?: throw RuntimeException(REPORT_DETAIL_SHAPE_ERROR)
                parseReportDetail(root)
                    ?: throw RuntimeException(REPORT_DETAIL_SHAPE_ERROR)
            }
        }

    /** Marks read and/or toggles the star. Only sent keys change. */
    suspend fun patch(id: String, read: Boolean?, starred: Boolean?): Result<Unit> =
        withContext(Dispatchers.IO) {
            val body = JSONObject()
            if (read != null) body.put("read", read)
            if (starred != null) body.put("starred", starred)
            call("PATCH", "/api/reports/${encode(id.trim())}", body).map { }
        }

    /** Hard-deletes one report. No undo, no trash — the client confirms first. */
    suspend fun delete(id: String): Result<Unit> =
        withContext(Dispatchers.IO) {
            call("DELETE", "/api/reports/${encode(id.trim())}").map { }
        }
}
