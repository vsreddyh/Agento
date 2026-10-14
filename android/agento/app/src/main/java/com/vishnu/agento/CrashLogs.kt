package com.vishnu.agento

import android.content.Context
import android.os.Build
import android.os.Process
import android.util.Log
import java.io.File
import java.time.LocalDateTime
import java.time.format.DateTimeFormatter
import java.util.Locale

/**
 * Fatal-crash logs (#290): crash logs only, not every error. Client and
 * server, 5-day TTL everywhere, saved on the device; the user retrieves the
 * files by hand (adb / Device File Explorer) or through the Settings → About
 * diagnostics export, which appends them.
 *
 * One file per crash under `filesDir/crashlogs/`, written by a
 * `Thread.setDefaultUncaughtExceptionHandler` that chains to the previous
 * handler afterwards — the app still dies normally; a crash is never
 * swallowed. Catches every thread, not just main.
 *
 * Contents are the enumerated fields below: timestamp, app version, model +
 * effort, device + Android version, thread name, full stack trace. No bearer
 * token, no app password is ever written — the writer only emits these
 * fields. (An exception message can echo the server host, which carries no
 * credential; the URL itself is never stored as a field.)
 *
 * Internal storage needs no permission and nothing that can fail on a
 * locked-down device. TTL pruning runs at startup and after each write, so
 * the directory is self-limiting with no retention job.
 */
object CrashLogs {

    const val DIR_NAME = "crashlogs"

    /** 5 days: crash files older than this are deleted on prune. */
    const val TTL_MS = 5L * 24 * 60 * 60 * 1000

    /** Cap for the diagnostics-export section below. */
    const val REPORT_MAX_CHARS = 60_000

    /** Install the handler; safe to call on every cold start. */
    fun install(appCtx: Context) {
        prune(appCtx)
        val prev = Thread.getDefaultUncaughtExceptionHandler()
        Thread.setDefaultUncaughtExceptionHandler { thread, e ->
            runCatching {
                writeCrash(appCtx, thread, e)
                prune(appCtx)
            }
            if (prev != null) {
                prev.uncaughtException(thread, e)
            } else {
                // No previous handler: kill the process the way the
                // runtime would have, so the crash still reads as a crash
                // (and the system still shows its own crash dialog).
                Process.killProcess(Process.myPid())
                kotlin.system.exitProcess(10)
            }
        }
    }

    /** `crash-<yyyyMMdd-HHmmss>-<pid>.log`, per the issue's file shape. */
    internal fun fileName(now: LocalDateTime, pid: Int): String =
        "crash-" + now.format(
            DateTimeFormatter.ofPattern("yyyyMMdd-HHmmss", Locale.ROOT),
        ) + "-$pid.log"

    /** True when a file timestamp is past the TTL at `nowMillis`. */
    internal fun isExpired(lastModified: Long, nowMillis: Long): Boolean =
        nowMillis - lastModified > TTL_MS

    fun prune(appCtx: Context) {
        val now = System.currentTimeMillis()
        crashDir(appCtx).listFiles()?.forEach { f ->
            if (isExpired(f.lastModified(), now)) runCatching { f.delete() }
        }
    }

    /** Write one crash file; null when the write itself failed. Same-second
     * collisions get a numeric suffix so no crash overwrites another. */
    fun writeCrash(appCtx: Context, thread: Thread, e: Throwable): File? =
        runCatching {
            val dir = crashDir(appCtx).apply { mkdirs() }
            val stamp = LocalDateTime.now(IST)
            var file = File(dir, fileName(stamp, Process.myPid()))
            var n = 0
            while (file.exists()) {
                n++
                file = File(dir, fileName(stamp, Process.myPid()).removeSuffix(".log") + "-$n.log")
            }
            file.writeText(reportText(appCtx, thread, e))
            file
        }.getOrNull()

    internal fun reportText(appCtx: Context, thread: Thread, e: Throwable): String {
        val api = ChatApi(appCtx.applicationContext)
        val model = api.modelFor("chat")
        val effort = api.effortFor("chat", model)
        return buildList {
            add("timestamp: ${LocalDateTime.now(IST)}")
            add("app: ${BuildConfig.VERSION_NAME}")
            add("model: ${model.ifEmpty { "(not set)" }}")
            add("effort: ${effort.ifEmpty { "(default)" }}")
            add("device: ${Build.MANUFACTURER} ${Build.MODEL}")
            add("android: ${Build.VERSION.RELEASE} (SDK ${Build.VERSION.SDK_INT})")
            add("thread: ${thread.name}")
            add("stack:")
            add(Log.getStackTraceString(e))
        }.joinToString("\n")
    }

    /**
     * Diagnostics-export section: every recorded crash file, newest first,
     * capped. Empty (not blank) when there is nothing — the report says so
     * rather than silently omitting the section.
     */
    fun reportSection(appCtx: Context): String {
        val files = crashDir(appCtx).listFiles()
            ?.sortedByDescending { it.lastModified() }.orEmpty()
        if (files.isEmpty()) return "no crash logs recorded in the last 5 days"
        val perFile = REPORT_MAX_CHARS / files.size.coerceAtLeast(1)
        return files.joinToString("\n\n") { f ->
            "--- ${f.name} ---\n" +
                runCatching { f.readText() }.getOrDefault("(unreadable)").take(perFile)
        }.take(REPORT_MAX_CHARS)
    }

    private fun crashDir(appCtx: Context): File =
        File(appCtx.applicationContext.filesDir, DIR_NAME)
}
