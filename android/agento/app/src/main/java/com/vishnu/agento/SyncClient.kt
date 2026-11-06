package com.vishnu.agento

import android.content.Context
import androidx.core.content.edit
import java.util.concurrent.TimeUnit
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.Response

/** Thin HTTP client for the health-api sync endpoint; server URL/token come from SharedPreferences. */
class SyncClient(context: Context) {

    private val prefs = context.getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
    private val http = OkHttpClient.Builder()
        .connectTimeout(10, TimeUnit.SECONDS)
        .readTimeout(30, TimeUnit.SECONDS)
        .writeTimeout(30, TimeUnit.SECONDS)
        .build()

    companion object {
        private const val TAG = "SyncClient"
        private val JSON = "application/json; charset=utf-8".toMediaType()
    }

    /** Single server URL (proxy: chat + sync on one port). Resolved through
     * [ServerEndpoint] like every other client, pre-save included, so
     * background sync can never read a retired key chat would ignore. */
    fun serverUrl(): String = ServerEndpoint.base(prefs)

    /** Single app password (the sync token doubles as chat credential). */
    fun password(): String = ServerEndpoint.password(prefs)
    /** Marks the one-time historical backfill done so later runs send today-only payloads. */
    fun markFirstSyncDone() {
        prefs.edit {
            putBoolean("first_sync_done", true)
        }
    }

    /** Persists the canonical server URL + password; normalizes trailing
     * slash/whitespace so post() can build the endpoint directly. Retired
     * keys are deleted on write — nothing reads them any more (#433), so
     * they must not survive as a second, misleading copy. */
    fun setConfig(serverUrl: String, password: String) {
        prefs.edit {
            putString(ServerEndpoint.BASE_KEY, serverUrl.trim().trimEnd('/'))
            putString(ServerEndpoint.PASSWORD_KEY, password.trim())
            remove("server_url")
            remove("auth_token")
            remove("api_base_url")
        }
    }

    /** POSTs the payload to /api/health/sync with Bearer auth; never throws — failures become SyncResult. */
    suspend fun post(payload: HealthSyncPayload): SyncResult = withContext(Dispatchers.IO) {
        val url = serverUrl()
        if (url.isEmpty()) {
            return@withContext SyncResult(false, null, ServerEndpoint.UNCONFIGURED)
        }

        val json = Json { ignoreUnknownKeys = true }.encodeToString(payload)
        val request = Request.Builder()
            .url("$url/api/health/sync")
            .header("Authorization", "Bearer ${password()}")
            .header("Content-Type", "application/json")
            .post(json.toRequestBody(JSON))
            .build()

        try {
            http.newCall(request).execute().use { response ->
                SyncResult(
                    success = response.isSuccessful,
                    statusCode = response.code,
                    message = response.body?.string()?.take(500) ?: "",
                )
            }
        } catch (e: Exception) {
            SyncResult(false, null, e.message ?: e.javaClass.simpleName)
        }
    }
}
