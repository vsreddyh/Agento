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
import java.util.concurrent.TimeUnit

/** One held tool call awaiting a human (#313). */
data class GateApproval(
    val id: String,
    val server: String = "",
    val tool: String = "",
    /** Redacted arguments, flattened for display. */
    val args: Map<String, String> = emptyMap(),
    val status: String = "",
    val decidedBy: String = "",
    val createdAt: Long = 0L,
    val updatedAt: Long = 0L,
    val decidedAt: Long = 0L,
) {
    companion object {
        /** Whitespace collapse for the argument preview: hoisted so the
         * regex is compiled once, not per card per recomposition. */
        private val WS = Regex("\\s+")
    }

    fun isPending(): Boolean = status == "pending"

    /** One-line argument preview for the approval card. Keys sorted so the
     * card reads the same run to run (`JSONObject.keys()` order is not
     * deterministic). */
    fun argsSummary(): String = args.toSortedMap().entries
        .joinToString(", ") { (k, v) -> "$k=$v" }
        .replace(WS, " ")
        .take(200)
}

/** One audit row: a proxied call and what happened to it (#313). */
data class AuditRow(
    val id: String,
    val server: String = "",
    val tool: String = "",
    val disposition: String = "",
    val error: String = "",
    val approvalId: String = "",
    val at: Long = 0L,
)

/** One approval parsed leniently: missing id means the row is dropped. */
internal fun parseApproval(o: JSONObject): GateApproval? {
    val id = o.optString("id", "").trim()
    if (id.isEmpty()) return null
    val args = mutableMapOf<String, String>()
    val arr = o.optJSONObject("args")
    if (arr != null) {
        val keys = arr.keys()
        while (keys.hasNext()) {
            val k = keys.next()
            args[k] = arr.opt(k)?.toString().orEmpty()
        }
    }
    return GateApproval(
        id = id,
        server = o.optString("server", "").trim(),
        tool = o.optString("tool", "").trim(),
        args = args,
        status = o.optString("status", "").trim(),
        decidedBy = o.optString("decided_by", "").trim(),
        createdAt = o.optLong("created_at", 0L),
        updatedAt = o.optLong("updated_at", 0L),
        decidedAt = o.optLong("decided_at", 0L),
    )
}

internal fun parseAuditRow(o: JSONObject): AuditRow? {
    val id = o.optString("id", "").trim()
    if (id.isEmpty()) return null
    return AuditRow(
        id = id,
        server = o.optString("server", "").trim(),
        tool = o.optString("tool", "").trim(),
        disposition = o.optString("disposition", "").trim(),
        error = o.optString("error", "").trim(),
        approvalId = o.optString("approval_id", "").trim(),
        at = o.optLong("at", 0L),
    )
}

/**
 * Client over the gateway's approvals + audit API (same bearer auth as chat,
 * served through the proxy's /p/ route — always call with the profile path,
 * e.g. `/p/chat`, since an unprefixed `/api/` path belongs to health-api).
 * Parsing is lenient; missing fields show as blank, undecodable rows are
 * logged and skipped rather than failing the list.
 */
/** One approvals page: rows plus whether the store can decide them.
 * Degraded means the gateway and relay hold different in-process stores:
 * decisions made here can never reach a held call. */
data class ApprovalList(
    val approvals: List<GateApproval> = emptyList(),
    val degraded: Boolean = false,
)

internal fun parseApprovalList(body: String): ApprovalList {
    val out = mutableListOf<GateApproval>()
    val root = runCatching { JSONObject(body) }.getOrNull()
    if (root == null) {
        runCatching { Log.w("ApprovalsApi", "unparseable approvals body: ${body.take(120)}") }
        return ApprovalList()
    }
    val arr = root.optJSONArray("approvals")
    if (arr == null) {
        runCatching { Log.w("ApprovalsApi", "approvals body has no array: ${body.take(120)}") }
        return ApprovalList()
    }
    var dropped = 0
    for (i in 0 until arr.length()) {
        val obj = arr.optJSONObject(i) ?: continue
        val row = parseApproval(obj)
        if (row == null) {
            dropped++
            continue
        }
        out.add(row)
    }
    if (dropped > 0) {
        runCatching { Log.w("ApprovalsApi", "dropped $dropped id-less approval row(s)") }
    }
    return ApprovalList(out, root.optString("degraded", "").isNotBlank())
}

class ApprovalsApi(context: Context) {

    private val appCtx = context.applicationContext
    private val prefs = appCtx.getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
    private val http = OkHttpClient.Builder()
        .connectTimeout(10, TimeUnit.SECONDS)
        .readTimeout(30, TimeUnit.SECONDS)
        .writeTimeout(30, TimeUnit.SECONDS)
        .build()

    companion object {
        private val JSON = "application/json; charset=utf-8".toMediaType()

        /** Query/path encoding (`+` is a space in form encoding, not in a
         * path — `%20` keeps ids exact). */
        internal fun encode(v: String): String =
            URLEncoder.encode(v, Charsets.UTF_8.name()).replace("+", "%20")

        /** `"HTTP 409: …"` → `"409"`, else null. Sheet errors name the code,
         * never the body (verbs on ids, not pasted server text). */
        internal fun httpCode(message: String?): String? =
            Regex("^HTTP (\\d+)").find(message.orEmpty())?.groupValues?.getOrNull(1)
    }

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

    private data class CallResult(val code: Int, val body: String)

    private fun call(method: String, url: String, body: JSONObject? = null): Result<CallResult> {
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
                "POST" -> builder.post(
                    (body?.toString() ?: "{}").toRequestBody(JSON))
                else -> throw IllegalArgumentException("unknown method $method")
            }
            http.newCall(builder.build()).execute().use { response ->
                CallResult(response.code, response.body?.string() ?: "")
            }
        }
    }

    /** Lists approvals, newest first. Null status means all. */
    suspend fun listApprovals(
        path: String,
        status: String? = null,
        limit: Int = 50,
    ): Result<ApprovalList> = withContext(Dispatchers.IO) {
        var url = "$path/api/approvals?limit=$limit"
        if (!status.isNullOrBlank()) url += "&status=${encode(status.trim())}"
        call("GET", url).mapCatching {
            if (it.code != 200) throw RuntimeException("HTTP ${it.code}: ${it.body.take(200)}")
            parseApprovalList(it.body)
        }
    }

    /**
     * Decides a held call. True = approved (the relay forwards it),
     * false = denied (Pi gets a tool error and the turn continues).
     * A 409 means someone else decided first — refresh, don't retry blind.
     */
    suspend fun decide(path: String, id: String, approve: Boolean): Result<Unit> =
        withContext(Dispatchers.IO) {
            val verb = if (approve) "approve" else "deny"
            call("POST", "$path/api/approvals/${encode(id)}/$verb").mapCatching {
                if (it.code != 200) throw RuntimeException("HTTP ${it.code}: ${it.body.take(200)}")
            }
        }

    /** Lists audit rows, newest first. Null filters mean no constraint. */
    suspend fun listAudit(
        path: String,
        server: String? = null,
        tool: String? = null,
        limit: Int = 50,
    ): Result<List<AuditRow>> = withContext(Dispatchers.IO) {
        var url = "$path/api/audit?limit=$limit"
        if (!server.isNullOrBlank()) url += "&server=${encode(server.trim())}"
        if (!tool.isNullOrBlank()) url += "&tool=${encode(tool.trim())}"
        call("GET", url).mapCatching {
            if (it.code != 200) throw RuntimeException("HTTP ${it.code}: ${it.body.take(200)}")
            parseAuditList(it.body)
        }
    }

    private fun parseAuditList(body: String): List<AuditRow> {
        val out = mutableListOf<AuditRow>()
        val root = runCatching { JSONObject(body) }.getOrNull()
        if (root == null) {
            runCatching { Log.w("ApprovalsApi", "unparseable audit body: ${body.take(120)}") }
            return out
        }
        val arr = root.optJSONArray("entries") ?: return out
        for (i in 0 until arr.length()) {
            parseAuditRow(arr.optJSONObject(i) ?: continue)?.let { out.add(it) }
        }
        return out
    }
}
