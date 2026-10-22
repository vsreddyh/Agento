package com.vishnu.agento

import android.app.DownloadManager
import android.content.Context
import android.os.Environment
import androidx.core.net.toUri
import java.util.concurrent.TimeUnit
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.MediaType.Companion.toMediaTypeOrNull
import okhttp3.MultipartBody
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import org.json.JSONObject

/** Upload cap, mirroring the server's maxUploadBytes (cmd/health-api/files.go).
 * Checked client-side first so an obviously oversized pick fails fast with
 * the same named error instead of uploading megabytes to be refused. */
const val FILES_UPLOAD_LIMIT_BYTES = 25L * 1024 * 1024

data class RemoteEntry(
    val name: String,
    val isDir: Boolean,
    val size: Long = 0L,
    val mtime: String = "",
)

/** VPS exports browser: list folders + download files to mobile + upload (#26, #340). */
class StorageApi(context: Context) {

    private val appCtx = context.applicationContext
    private val prefs = appCtx.getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
    private val http = OkHttpClient.Builder()
        .connectTimeout(15, TimeUnit.SECONDS)
        .readTimeout(60, TimeUnit.SECONDS)
        .build()
    // Uploads carry up to 25 MiB bodies: the list client's default 10s write
    // timeout would abort them mid-stream, so uploads get their own client.
    private val uploadHttp = OkHttpClient.Builder()
        .connectTimeout(15, TimeUnit.SECONDS)
        .writeTimeout(180, TimeUnit.SECONDS)
        .readTimeout(120, TimeUnit.SECONDS)
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

    /** Uploads `data` as `name` into the exports folder `dir` (empty = root).
     * Server policy (cmd/health-api/files.go): confined to the exports root,
     * refused — never replaced — when the name exists (409 file_exists),
     * refused past 25 MiB (413 file_too_large). Error bodies carry the
     * server's named detail so the refusal is actionable, not opaque. */
    suspend fun upload(dir: String, name: String, data: ByteArray, mime: String): Result<RemoteEntry> =
        withContext(Dispatchers.IO) {
            val base = base()
            if (base.isEmpty()) {
                return@withContext Result.failure(IllegalStateException("Server URL not configured"))
            }
            val url = "$base/api/files/upload?path=" +
                java.net.URLEncoder.encode(dir, "UTF-8") +
                "&name=" + java.net.URLEncoder.encode(name, "UTF-8")
            val body = MultipartBody.Builder()
                .setType(MultipartBody.FORM)
                .addFormDataPart(
                    "file", name,
                    data.toRequestBody(
                        mime.toMediaTypeOrNull()
                            ?: "application/octet-stream".toMediaType()),
                )
                .build()
            val request = Request.Builder()
                .url(url)
                .header("Authorization", "Bearer ${password()}")
                .post(body)
                .build()
            runCatching {
                uploadHttp.newCall(request).execute().use { response ->
                    val raw = response.body?.string() ?: ""
                    if (!response.isSuccessful) {
                        val detail = runCatching {
                            JSONObject(raw).optString("detail", raw)
                        }.getOrDefault(raw).ifEmpty { raw }
                        throw RuntimeException("HTTP ${response.code}: ${detail.take(300)}")
                    }
                    val o = JSONObject(raw)
                    RemoteEntry(
                        o.optString("name", name), false,
                        o.optLong("size"),
                        o.optString("mtime"),
                    )
                }
            }
        }

    /** Enqueues a download into public Downloads; returns the system download id. */
    fun download(path: String, name: String): Long {
        val base = base()
        val url = "$base/api/files/download?path=" +
            java.net.URLEncoder.encode(
                (if (path.isEmpty()) "" else "$path/") + name, "UTF-8")
        val req = DownloadManager.Request(url.toUri())
            .addRequestHeader("Authorization", "Bearer ${password()}")
            .setTitle(name)
            .setNotificationVisibility(
                DownloadManager.Request.VISIBILITY_VISIBLE_NOTIFY_COMPLETED)
            .setDestinationInExternalPublicDir(Environment.DIRECTORY_DOWNLOADS, name)
        val dm = appCtx.getSystemService(Context.DOWNLOAD_SERVICE) as DownloadManager
        return dm.enqueue(req)
    }
}
