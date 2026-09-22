package com.vishnu.agento

import android.app.DownloadManager
import android.content.Context
import android.net.Uri
import android.os.Environment
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import okhttp3.OkHttpClient
import okhttp3.Request
import org.json.JSONObject
import java.util.concurrent.TimeUnit

data class RemoteEntry(
    val name: String,
    val isDir: Boolean,
    val size: Long = 0L,
    val mtime: String = "",
)

/** VPS exports browser: list folders + download files to mobile (#26). */
class StorageApi(context: Context) {

    private val appCtx = context.applicationContext
    private val prefs = appCtx.getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
    private val http = OkHttpClient.Builder()
        .connectTimeout(15, TimeUnit.SECONDS)
        .readTimeout(60, TimeUnit.SECONDS)
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

    /** Lists one exports dir; empty path = root. */
    suspend fun list(path: String): Result<Triple<String, List<RemoteEntry>, List<RemoteEntry>>> =
        withContext(Dispatchers.IO) {
            val base = base()
            if (base.isEmpty()) {
                return@withContext Result.failure(IllegalStateException("Server URL not configured"))
            }
            val url = "$base/api/files?path=" +
                java.net.URLEncoder.encode(path, "UTF-8")
            val request = Request.Builder()
                .url(url)
                .header("Authorization", "Bearer ${password()}")
                .get()
                .build()
            runCatching {
                http.newCall(request).execute().use { response ->
                    val body = response.body?.string() ?: ""
                    if (!response.isSuccessful) {
                        throw RuntimeException("HTTP ${response.code}: ${body.take(200)}")
                    }
                    val o = JSONObject(body)
                    val dirs = mutableListOf<RemoteEntry>()
                    val files = mutableListOf<RemoteEntry>()
                    val darr = o.optJSONArray("dirs")
                    if (darr != null) {
                        for (i in 0 until darr.length()) {
                            val name = darr.optJSONObject(i)?.optString("name").orEmpty()
                            if (name.isNotEmpty()) dirs.add(RemoteEntry(name, true))
                        }
                    }
                    val farr = o.optJSONArray("files")
                    if (farr != null) {
                        for (i in 0 until farr.length()) {
                            val fo = farr.optJSONObject(i) ?: continue
                            val name = fo.optString("name").trim()
                            if (name.isEmpty()) continue
                            files.add(RemoteEntry(
                                name, false,
                                fo.optLong("size"),
                                fo.optString("mtime"),
                            ))
                        }
                    }
                    Triple(o.optString("path"), dirs, files)
                }
            }
        }

    /** Enqueues a download into public Downloads; returns the system download id. */
    fun download(path: String, name: String): Long {
        val base = base()
        val url = "$base/api/files/download?path=" +
            java.net.URLEncoder.encode(
                (if (path.isEmpty()) "" else "$path/") + name, "UTF-8")
        val req = DownloadManager.Request(Uri.parse(url))
            .addRequestHeader("Authorization", "Bearer ${password()}")
            .setTitle(name)
            .setNotificationVisibility(
                DownloadManager.Request.VISIBILITY_VISIBLE_NOTIFY_COMPLETED)
            .setDestinationInExternalPublicDir(Environment.DIRECTORY_DOWNLOADS, name)
        val dm = appCtx.getSystemService(Context.DOWNLOAD_SERVICE) as DownloadManager
        return dm.enqueue(req)
    }
}
