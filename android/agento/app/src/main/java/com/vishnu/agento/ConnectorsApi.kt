package com.vishnu.agento

import android.content.Context
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import org.json.JSONObject
import java.util.concurrent.TimeUnit

/**
 * One connector row as the server lists it (#440).
 *
 * Mirrors `connectorWire` in `internal/gateway/connectors.go`: `id`,
 * `display_name`, `scopes`, `status`, optional `last_error`, and the
 * `reconnect` hint present only on expired rows. Tokens never appear here —
 * the server never selects them into API responses.
 */
data class ConnectorInfo(
    val id: String = "",
    val displayName: String = "",
    val scopes: List<String> = emptyList(),
    val status: String = "",
    val lastError: String = "",
    val reconnect: String = "",
) {
    fun isConnected(): Boolean = status == "connected"
    fun isExpired(): Boolean = status == "expired"
}

/** One connector parsed leniently: missing id means the row is dropped. */
internal fun parseConnector(o: JSONObject): ConnectorInfo? {
    val id = o.optString("id", "").trim()
    if (id.isEmpty()) return null
    val scopes = mutableListOf<String>()
    val arr = o.optJSONArray("scopes")
    if (arr != null) {
        for (i in 0 until arr.length()) {
            arr.optString(i, "").trim().takeIf { it.isNotEmpty() }?.let { scopes.add(it) }
        }
    }
    return ConnectorInfo(
        id = id,
        displayName = o.optString("display_name", "").trim().ifEmpty { id },
        scopes = scopes,
        status = o.optString("status", "").trim(),
        lastError = o.optString("last_error", "").trim(),
        reconnect = o.optString("reconnect", "").trim(),
    )
}

/** One message for a server that does not speak the canonical connector
 * list (#484 review): the server always returns its rows (disconnected
 * synthesised), so a 200 without a `connectors` array is a wrong shape —
 * wrong path/prefix, proxy HTML, health-api answer — never "zero
 * connectors exist". Loud, like TASKS_SHAPE_ERROR. */
internal const val CONNECTORS_SHAPE_ERROR =
    "Unexpected response shape — the server sent no connectors list. Update the server."

/** The consent URL is missing or the body is not the connector shape
 * (#484 review): the server fail-closes a missing grant with 502, so a
 * 200 without a non-blank `auth_url` is a wrong shape (proxy HTML,
 * non-connector JSON) — never an empty grant. Loud, like
 * [CONNECTORS_SHAPE_ERROR]. */
internal const val CONNECT_AUTH_SHAPE_ERROR =
    "Unexpected response shape — the server sent no consent URL. Update the server."

/** Reads the one-time consent URL out of a connect response (#484
 * review): anything without a non-blank `auth_url` is the wrong shape,
 * not an empty grant. Pure and JVM-pinned. */
internal fun parseAuthUrl(body: String): String {
    val url = runCatching { JSONObject(body).optString("auth_url", "") }.getOrDefault("")
    if (url.isBlank()) throw RuntimeException(CONNECT_AUTH_SHAPE_ERROR)
    return url
}

internal fun parseConnectorList(body: String): List<ConnectorInfo> {
    val root = runCatching { JSONObject(body) }.getOrNull()
        ?: throw RuntimeException(CONNECTORS_SHAPE_ERROR)
    val arr = root.optJSONArray("connectors")
        ?: throw RuntimeException(CONNECTORS_SHAPE_ERROR)
    // An empty array is the same wrong shape, not an empty account: the
    // server always returns its rows (#484 review).
    if (arr.length() == 0) throw RuntimeException(CONNECTORS_SHAPE_ERROR)
    val out = mutableListOf<ConnectorInfo>()
    for (i in 0 until arr.length()) {
        parseConnector(arr.optJSONObject(i) ?: continue)?.let { out.add(it) }
    }
    // Every row id-less is the same wrong shape one level down.
    if (out.isEmpty()) throw RuntimeException(CONNECTORS_SHAPE_ERROR)
    return out
}

/**
 * Whether a connect failure means "server has no OAuth config" (#484
 * review): matched on the status code this client built (`HTTP <code>:`),
 * never on a `"501"` substring that could sit anywhere in a truncated
 * body. Pure so the rule is pinnable.
 */
internal fun isConnectNotConfigured(message: String): Boolean =
    message.startsWith("HTTP 501:")

/**
 * Connector verbs (#440): list, connect (returns the one-time Google
 * consent URL — the app opens it in a browser and polls list, never
 * touching code/verifier/token), disconnect (clears tokens server-side).
 *
 * Same shape as [ApprovalsApi]: canonical [ServerEndpoint] resolution, no
 * legacy fallbacks (#433).
 */
class ConnectorsApi(context: Context) {

    private val appCtx = context.applicationContext
    private val prefs = appCtx.getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
    private val http = OkHttpClient.Builder()
        .connectTimeout(10, TimeUnit.SECONDS)
        .readTimeout(30, TimeUnit.SECONDS)
        .writeTimeout(30, TimeUnit.SECONDS)
        .build()

    companion object {
        private val JSON = "application/json; charset=utf-8".toMediaType()

        internal fun encode(v: String): String =
            java.net.URLEncoder.encode(v, Charsets.UTF_8.name()).replace("+", "%20")
    }

    private fun base(): String = ServerEndpoint.base(prefs)

    private fun password(): String = ServerEndpoint.password(prefs)

    private data class CallResult(val code: Int, val body: String)

    private fun call(method: String, url: String, body: JSONObject? = null): Result<CallResult> {
        val base = base()
        if (base.isEmpty()) {
            // Canonical unconfigured text, like every other client (#484
            // review): names the fix, not just the state.
            return Result.failure(IllegalStateException(ServerEndpoint.UNCONFIGURED))
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

    /** Lists connector rows (disconnected synthesised server-side). */
    suspend fun list(path: String): Result<List<ConnectorInfo>> = withContext(Dispatchers.IO) {
        call("GET", "$path/api/connectors").mapCatching {
            if (it.code != 200) throw RuntimeException("HTTP ${it.code}: ${it.body.take(200)}")
            parseConnectorList(it.body)
        }
    }

    /**
     * Starts one OAuth attempt: returns the one-time Google consent
     * `auth_url` to open in a browser (10-min TTL, single-use). The 501
     * means the server has no OAuth config — never fall back to asking
     * for a secret in chat.
     */
    suspend fun connect(path: String, id: String): Result<String> = withContext(Dispatchers.IO) {
        call("POST", "$path/api/connectors/${encode(id)}/connect").mapCatching {
            if (it.code != 200) throw RuntimeException("HTTP ${it.code}: ${it.body.take(200)}")
            parseAuthUrl(it.body)
        }
    }

    /** Stops future access (tokens cleared server-side). History stays. */
    suspend fun disconnect(path: String, id: String): Result<ConnectorInfo?> =
        withContext(Dispatchers.IO) {
            call("POST", "$path/api/connectors/${encode(id)}/disconnect").mapCatching {
                if (it.code != 200) throw RuntimeException("HTTP ${it.code}: ${it.body.take(200)}")
                val row = runCatching {
                    JSONObject(it.body).optJSONObject("connector")
                }.getOrNull()
                row?.let { parseConnector(it) }
            }
        }
}
