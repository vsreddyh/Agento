package com.vishnu.agento

import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.provider.Settings
import androidx.core.content.FileProvider
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import okhttp3.OkHttpClient
import okhttp3.Request
import org.json.JSONObject
import java.io.File
import java.util.concurrent.TimeUnit

/** GitHub repo whose Releases carry the signed APKs (published by android-apk.yml). */
private const val UPDATE_REPO = "vsreddyh/Discord-bots"

/** Matches "agento-v0.1.0-42", "v0.1.0" and "0.1.0" (build suffix optional). */
private val TAG_PATTERN = Regex("""^(?:agento-)?v?(\d+)\.(\d+)\.(\d+)(?:-(\d+))?$""")

/** One GitHub Release's installable APK asset. */
data class AppRelease(
    val tag: String,
    val name: String,
    val apkUrl: String,
    val apkSizeBytes: Long,
    val publishedAt: String,
    val notes: String,
)

/**
 * Issue #6: check-then-install updater. Reads `/releases/latest` from the
 * GitHub API, compares against the installed build, streams the release APK
 * to cache, and fires the platform installer. No new dependencies —
 * OkHttp (already used by ChatApi) plus org.json.
 */
object UpdateManager {

    private val http = OkHttpClient.Builder()
        .connectTimeout(15, TimeUnit.SECONDS)
        .readTimeout(60, TimeUnit.SECONDS)
        .writeTimeout(60, TimeUnit.SECONDS)
        .build()

    /** Parses a release tag into (semver parts, build number); null when unparseable. */
    fun parseTag(tag: String): Pair<List<Int>, Int>? {
        val m = TAG_PATTERN.matchEntire(tag.trim()) ?: return null
        val semver = m.groupValues.slice(1..3).map { it.toInt() }
        val build = m.groupValues[4].toIntOrNull() ?: 0
        return semver to build
    }

    /** True when [releaseTag] is newer than the installed version. */
    fun isNewer(releaseTag: String, currentVersionName: String, currentVersionCode: Long): Boolean {
        val (relVer, relBuild) = parseTag(releaseTag) ?: return false
        val (curVer, _) = parseTag(currentVersionName.trim()) ?: return true
        for (i in 0..2) {
            if (relVer[i] != curVer[i]) return relVer[i] > curVer[i]
        }
        return relBuild > currentVersionCode
    }

    /** Installed (versionName, versionCode); "?" / 0 when unreadable. */
    fun currentVersion(context: Context): Pair<String, Long> {
        return try {
            val pm = context.packageManager
            val info = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
                pm.getPackageInfo(context.packageName, PackageManager.PackageInfoFlags.of(0))
            } else {
                @Suppress("DEPRECATION")
                pm.getPackageInfo(context.packageName, 0)
            }
            val code = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) {
                info.longVersionCode
            } else {
                @Suppress("DEPRECATION")
                info.versionCode.toLong()
            }
            (info.versionName ?: "?") to code
        } catch (e: Exception) {
            "?" to 0L
        }
    }

    /** Fetches the latest published release and picks its installable APK asset. */
    suspend fun fetchLatest(): Result<AppRelease> = withContext(Dispatchers.IO) {
        try {
            // GitHub rejects API calls without a User-Agent; auth is
            // unnecessary (public repo, 60 req/hr unauthenticated).
            val request = Request.Builder()
                .url("https://api.github.com/repos/$UPDATE_REPO/releases/latest")
                .header("Accept", "application/vnd.github+json")
                .header("User-Agent", "Agento-App")
                .get()
                .build()
            http.newCall(request).execute().use { response ->
                val body = response.body?.string() ?: ""
                if (!response.isSuccessful) {
                    return@withContext Result.failure(
                        RuntimeException("GitHub API HTTP ${response.code}: ${body.take(200)}")
                    )
                }
                val json = JSONObject(body)
                val tag = json.optString("tag_name")
                if (tag.isEmpty()) {
                    return@withContext Result.failure(
                        RuntimeException("No releases published yet")
                    )
                }
                val assets = json.optJSONArray("assets")
                var apkUrl = ""
                var apkSize = 0L
                var fallbackUrl = ""
                var fallbackSize = 0L
                if (assets != null) {
                    for (i in 0 until assets.length()) {
                        val a = assets.optJSONObject(i) ?: continue
                        val name = a.optString("name")
                        if (!name.endsWith(".apk")) continue
                        val url = a.optString("browser_download_url")
                        if (url.isEmpty()) continue
                        if (fallbackUrl.isEmpty()) {
                            fallbackUrl = url
                            fallbackSize = a.optLong("size")
                        }
                        // Release flavor is the updater's target; debug is
                        // only a fallback so the button still works.
                        if ("release" in name.lowercase()) {
                            apkUrl = url
                            apkSize = a.optLong("size")
                            break
                        }
                    }
                }
                if (apkUrl.isEmpty()) apkUrl = fallbackUrl
                if (apkSize == 0L) apkSize = fallbackSize
                if (apkUrl.isEmpty()) {
                    return@withContext Result.failure(
                        RuntimeException("Release $tag has no APK asset")
                    )
                }
                Result.success(
                    AppRelease(
                        tag = tag,
                        name = json.optString("name").ifEmpty { tag },
                        apkUrl = apkUrl,
                        apkSizeBytes = apkSize,
                        publishedAt = json.optString("published_at"),
                        notes = json.optString("body"),
                    )
                )
            }
        } catch (e: Exception) {
            Result.failure(e)
        }
    }

    /** Streams [url] to [dest]; reports 0..1 progress (or -1 when size unknown). */
    suspend fun download(url: String, dest: File, onProgress: (Float) -> Unit): Result<File> =
        withContext(Dispatchers.IO) {
            try {
                dest.parentFile?.mkdirs()
                val tmp = File(dest.parentFile, dest.name + ".part")
                val request = Request.Builder()
                    .url(url)
                    .header("User-Agent", "Agento-App")
                    .get()
                    .build()
                http.newCall(request).execute().use { response ->
                    if (!response.isSuccessful) {
                        return@withContext Result.failure(
                            RuntimeException("Download HTTP ${response.code}")
                        )
                    }
                    val body = response.body
                        ?: return@withContext Result.failure(RuntimeException("Empty download body"))
                    val total = body.contentLength()
                    var done = 0L
                    tmp.outputStream().use { out ->
                        body.byteStream().use { input ->
                            val buf = ByteArray(8192)
                            while (true) {
                                val n = input.read(buf)
                                if (n < 0) break
                                out.write(buf, 0, n)
                                done += n
                                onProgress(if (total > 0) done.toFloat() / total else -1f)
                            }
                        }
                    }
                    if (tmp.exists()) tmp.renameTo(dest)
                    onProgress(1f)
                    Result.success(dest)
                }
            } catch (e: Exception) {
                Result.failure(e)
            }
        }

    /** Android 8+ requires the user to allow "install unknown apps" per app. */
    fun canInstallUnknownApps(context: Context): Boolean =
        Build.VERSION.SDK_INT < Build.VERSION_CODES.O ||
            context.packageManager.canRequestPackageInstalls()

    /** Opens this app's "install unknown apps" system toggle. */
    fun unknownSourcesIntent(context: Context): Intent =
        Intent(Settings.ACTION_MANAGE_UNKNOWN_APP_SOURCES, Uri.parse("package:${context.packageName}"))

    /** Fires the platform package installer for a downloaded APK. */
    fun installIntent(context: Context, apk: File): Intent {
        val uri = FileProvider.getUriForFile(context, "${context.packageName}.fileprovider", apk)
        return Intent(Intent.ACTION_VIEW).apply {
            setDataAndType(uri, "application/vnd.android.package-archive")
            addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_GRANT_READ_URI_PERMISSION)
        }
    }
}
