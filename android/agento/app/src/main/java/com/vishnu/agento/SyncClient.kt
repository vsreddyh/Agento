package com.vishnu.agento

import android.content.Context
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.Response
import java.util.concurrent.TimeUnit

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

    /** Single server URL (proxy: chat + sync on one port). Prefers the
     * unified `server_base_url`; falls back to the legacy `server_url` so
     * pre-unification configs and backups keep working (incl. background sync). */
    fun serverUrl(): String {
        val unified = (prefs.getString("server_base_url", "") ?: "").trim().trimEnd('/')
        if (unified.isNotEmpty()) return unified
        return (prefs.getString("server_url", "") ?: "").trim().trimEnd('/')
    }
    /** Single app password (the sync token doubles as chat credential).
     * Prefers `app_password`; falls back to the legacy `auth_token` so users
     * who configured a sync token keep working without re-entry. */
    fun password(): String {
        val v = (prefs.getString("app_password", "") ?: "").trim()
        if (v.isNotEmpty()) return v
        return (prefs.getString("auth_token", "") ?: "").trim()
    }
    /** Marks the one-time historical backfill done so later runs send today-only payloads. */
    fun markFirstSyncDone() {
        prefs.edit().putBoolean("first_sync_done", true).apply()
    }

    /** Persists the unified server URL + password; normalizes trailing slash/whitespace so post() can build the endpoint directly. */
    fun setConfig(serverUrl: String, password: String) {
        prefs.edit()
            .putString("server_base_url", serverUrl.trimEnd('/'))
            .putString("app_password", password.trim())
            .apply()
    }

    /** POSTs the payload to /api/health/sync with Bearer auth; never throws — failures become SyncResult. */
    suspend fun post(payload: HealthSyncPayload): SyncResult = withContext(Dispatchers.IO) {
        val url = serverUrl()
        if (url.isEmpty()) return@withContext SyncResult(false, null, "server URL not configured")

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
