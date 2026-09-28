package com.vishnu.agento

import android.app.PendingIntent
import android.appwidget.AppWidgetManager
import android.appwidget.AppWidgetProvider
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.os.Build
import android.util.Log
import android.view.View
import android.widget.RemoteViews
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.async
import kotlinx.coroutines.awaitAll
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.launch

/** Which server state one widget placement shows. Stored per widget id
 * so two placements can watch different slices (Open, Done, All). */
enum class TaskWidgetView(val state: String, val title: String) {
    Open("open", "Open"),
    Done("done", "Done"),
    All("all", "All"),
}

/** Row density for one widget placement. */
enum class TaskWidgetDensity(val title: String) {
    Comfortable("Comfortable"),
    Compact("Compact"),
}

/**
 * Home-screen task manager widget (#101): header (title + count + view
 * toggle + refresh) above a scrollable task list. Row taps deep-link
 * into the task's detail sheet and the ring button completes inline —
 * both via the translucent [TaskCompleteActivity] trampoline, which
 * keeps widget taps BAL-safe on all API levels. Reads the same shared
 * `tasks` collection as TaskManagerScreen via TasksApi; the app pushes
 * fresh data after every task mutation via [refresh]. No periodic
 * updates (updatePeriodMillis=0) — data pulls on add/refresh-tap/
 * view-change/mutation only, so a sleeping server costs nothing.
 *
 * Dynamic sizing: min footprint 3x2, resizable both ways. The list is a
 * RemoteViews collection ([TaskWidgetService]) with weight 1, so it
 * takes whatever height the placement offers. Display (density, due
 * line) is per-widget via [TaskWidgetConfigActivity].
 */
class TaskWidget : AppWidgetProvider() {

    override fun onUpdate(
        context: Context,
        appWidgetManager: AppWidgetManager,
        appWidgetIds: IntArray,
    ) {
        // Loading shell first so the widget never sits blank, then fill in.
        // Per-id guard: one bad placement must not abort the rest (or the
        // pull below) — an escaping throw here surfaces as a widget error.
        for (id in appWidgetIds) {
            runCatching {
                appWidgetManager.updateAppWidget(
                    id, render(context, id, viewFor(context, id), null, false))
            }.onFailure {
                Log.w("TaskWidget", "initial render failed for id=$id", it)
            }
        }
        pull(context, appWidgetIds)
    }

    override fun onReceive(context: Context, intent: Intent) {
        super.onReceive(context, intent)
        when (intent.action) {
            ACTION_REFRESH -> {
                val mgr = AppWidgetManager.getInstance(context)
                val ids = mgr.getAppWidgetIds(ComponentName(context, TaskWidget::class.java))
                if (ids.isNotEmpty()) pull(context, ids)
            }
            ACTION_VIEW -> {
                // Header view toggle: cycle this placement only, then
                // re-pull it so the list matches the new view.
                val id = intent.getIntExtra(
                    AppWidgetManager.EXTRA_APPWIDGET_ID,
                    AppWidgetManager.INVALID_APPWIDGET_ID,
                )
                if (id != AppWidgetManager.INVALID_APPWIDGET_ID) {
                    cycleView(context, id)
                    pull(context, intArrayOf(id))
                }
            }
        }
    }

    override fun onDeleted(context: Context, appWidgetIds: IntArray) {
        // Drop per-placement prefs (view/density/due) so removed widgets
        // don't leak keys forever.
        val edit = prefs(context).edit()
        for (id in appWidgetIds) {
            edit.remove("task_widget_view_$id")
                .remove("task_widget_density_$id")
                .remove("task_widget_due_$id")
                .remove("task_widget_scroll_$id")
        }
        edit.apply()
    }

    /** Fetches open tasks off-thread; goAsync keeps the broadcast alive. */
    private fun pull(context: Context, ids: IntArray) {
        val pending = goAsync()
        widgetScope.launch {
            try {
                fetchAndPush(context.applicationContext, ids)
            } finally {
                pending.finish()
            }
        }
    }

    companion object {
        /** Opens MainActivity straight on the Task Manager screen. */
        const val ACTION_TASKS = "com.vishnu.agento.action.TASKS"

        /** Widget refresh-button broadcast (handled in onReceive). */
        const val ACTION_REFRESH = "com.vishnu.agento.action.TASKS_REFRESH"

        /** Widget view-toggle broadcast (handled in onReceive). */
        const val ACTION_VIEW = "com.vishnu.agento.action.TASKS_VIEW"

        /** Deep-link: open the detail sheet for this task id. Read by
         * [TaskCompleteActivity] (widget rows, alarm taps) and
         * MainActivity (its own TASKS intents). */
        const val EXTRA_TASK_ID = "com.vishnu.agento.extra.TASK_ID"

        /** Immediate complete request, handled by [TaskCompleteActivity]. */
        const val EXTRA_COMPLETE_ID = "com.vishnu.agento.extra.COMPLETE_ID"

        // One app-scoped worker: per-call CoroutineScope(Dispatchers.IO)
        // leaks a scope per update (review #109).
        private val widgetScope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

        // Distinct PendingIntent codes (review #109): actions already keep
        // the intents apart, but codes make that explicit.
        private const val OPEN_CODE = 1001
        private const val REFRESH_CODE = 1002
        private const val VIEW_CODE = 1003
        private const val ROW_CODE = 1004
        private const val CONFIG_CODE = 1005

        /** Rows rendered by the static (non-collection) path. */
        private const val STATIC_ROW_LIMIT = 8

        // Snapshots per server state, read by TaskWidgetService's factory
        // (same process). Volatile: written on IO, read on the RemoteViews
        // service thread. Views are fetched together so mixed placements
        // (one Open, one Done) each have data.
        @Volatile var cachedViews: Map<String, List<ServerTask>> = emptyMap()
            private set

        // Last fetch failure per server state (#130). Survives across
        // pulls so the empty view can name the reason (and offer a
        // tap-to-retry) instead of a dead generic error.
        @Volatile private var lastErrors: Map<String, String> = emptyMap()

        // Last RemoteViewsFactory failure (same process — the service has
        // no android:process). Surfaced in diagnostics so an on-device
        // widget crash names itself without adb.
        @Volatile var lastFactoryError: String? = null
            private set

        fun recordFactoryError(where: String, e: Throwable) {
            lastFactoryError = "$where: ${e.javaClass.simpleName}: ${e.message}".take(300)
            Log.w("TaskWidget", "factory failure at $where", e)
        }

        /** One-screen widget health summary for Settings → About, so
         * widget failures can be diagnosed without adb. Never throws
         * (a diagnostics call must not become a second crash). */
        fun diagnostics(appCtx: Context): String = runCatching {
            val ctx = appCtx.applicationContext
            val mgr = AppWidgetManager.getInstance(ctx)
            val ids = runCatching {
                mgr.getAppWidgetIds(ComponentName(ctx, TaskWidget::class.java))
            }.getOrDefault(intArrayOf())
            buildString {
                appendLine("placements=${ids.size}")
                for (id in ids) {
                    appendLine("id=$id view=${viewFor(ctx, id)} " +
                        "density=${densityFor(ctx, id)} showDue=${showDueFor(ctx, id)} " +
                        "scrollable=${scrollableFor(ctx, id)}")
                }
                appendLine("cached=${cachedViews.mapValues { it.value.size }}")
                appendLine("errors=${lastErrors.mapValues { it.value.take(300) }}")
                appendLine("factoryError=${lastFactoryError ?: "none"}")
            }.trim()
        }.getOrDefault("(diagnostics unavailable)")

        /** Full diagnostics report for file export: health summary plus
         * a larger redacted log slice than the clipboard variant. The
         * file variant captures system-wide logs: a host-side widget
         * failure is thrown in the LAUNCHER's process, so our own log
         * never contains the reason (#137). */
        fun buildReport(appCtx: Context): String {
            val head = diagnostics(appCtx)
            val mine = dumpLog(interestingLines = 100, tailLines = 30)
            val all = dumpLog(
                allProcesses = true,
                interestingLines = 250, tailLines = 40)
            // Parenthesized: without them take() binds to the last string
            // literal only and the head is never capped.
            return ("$head\n--- log (this app) ---\n$mine\n" +
                "--- log (system, recent) ---\n$all").take(200_000)
        }

        /** Best-effort wipe of the log buffers. Android gates `logcat -c`
         * behind CLEAR_LOGS (signature|privileged), so a normal app is
         * usually denied — reported either way instead of failing
         * silently. Fresh-window capture (-T) is the workable path. */
        fun clearSystemLog(): String = runCatching {
            val proc = ProcessBuilder("logcat", "-b", "all", "-c")
                .redirectErrorStream(true).start()
            val out = proc.inputStream.bufferedReader().readText().trim()
            val finished = proc.waitFor(5, java.util.concurrent.TimeUnit.SECONDS)
            val code = runCatching { proc.exitValue() }.getOrDefault(-1)
            proc.destroy()
            val denied = out.contains("Permission denied", ignoreCase = true) ||
                out.contains("SecurityException", ignoreCase = true)
            when {
                denied -> "Clearing the system log needs a privileged " +
                    "permission the app doesn't have. Nothing lost: reports " +
                    "capture only the recent slice."
                // Silent failure is the common case, so trust exit status.
                !finished -> "logcat didn't finish — buffers probably unchanged."
                code != 0 -> "logcat exited $code (permission denied) — " +
                    "nothing cleared."
                out.isEmpty() -> "Log buffers cleared."
                else -> "logcat said: ${out.take(160)}"
            }
        }.getOrDefault("logcat unavailable")

        /** Recent log lines, self-read so no adb is needed. Self-process
         * only by default (light, privacy-safe). [allProcesses] widens to
         * the whole system buffer at Warn+, which is what a HOST-side
         * widget failure looks like: the launcher throws in its own
         * process, so our pid's log never shows it (issue #137).
         * Privacy: output is user-initiated and shared, so bearer secrets
         * are redacted before it leaves the device (see [redactSecrets]). */
        fun dumpLog(
            allProcesses: Boolean = false,
            interestingLines: Int = 40,
            tailLines: Int = 20,
        ): String = runCatching {
            val pid = android.os.Process.myPid().toString()
            // Read before wait: waiting first can deadlock on a full pipe.
            fun runLogcat(cmd: List<String>): String {
                val proc = ProcessBuilder(cmd).redirectErrorStream(true).start()
                return try {
                    val text = proc.inputStream.bufferedReader().readText()
                    proc.waitFor(10, java.util.concurrent.TimeUnit.SECONDS)
                    text
                } finally {
                    proc.destroy()
                }
            }
            val base = listOf("logcat", "-d", "-b", "all", "-v", "brief")
            val out: String
            if (allProcesses) {
                // Launcher + AppWidgetManager lines live in other pids; -b
                // all adds the crash buffer where a host failure lands.
                // -t caps the read (relative -T windows are not honored
                // everywhere and an uncapped read can be megabytes on a
                // noisy device), so a bounded recent slice it is.
                out = runLogcat(base + listOf("-t", "3000", "*:W"))
            } else {
                out = runLogcat(
                    listOf("logcat", "-d", "--pid=$pid", "-v", "brief", "-t", "400"))
            }
            run {
                val lines = out.lines()
                val interesting = lines.filter(::widgetLine).takeLast(interestingLines)
                // System-wide mode would otherwise tail other apps' lines
                // verbatim, so the tail is filtered with the same
                // predicate: no unrelated PII in an uploaded report. Self
                // mode keeps the raw tail for context (own logs only).
                val tail = if (allProcesses) {
                    lines.filter(::widgetLine).takeLast(tailLines)
                } else {
                    lines.takeLast(tailLines)
                }
                val body = (interesting + "--- tail ---" + tail).joinToString("\n")
                if (interesting.isEmpty() && tail.isEmpty()) {
                    if (allProcesses) {
                        // Android 4.1+ hides other processes' logs from apps
                        // without READ_LOGS (never grantable), so this is
                        // expected on most devices, not a capture failure.
                        "(no widget lines visible — this OS hides other " +
                            "processes' logs from apps, so the host's own " +
                            "error cannot be read here; adb logcat is needed)"
                    } else {
                        "(empty log)"
                    }
                } else {
                    redactSecrets(body)
                }
            }
        }.getOrDefault("(log unavailable)")

        /** Lines worth keeping: our own widget code, or a host-side widget
         * failure signature. Deliberately specific — a generic "widget"
         * match would drag in every other app's widget lines from the
         * system-wide buffer. */
        private fun widgetLine(l: String): Boolean =
            l.contains("TaskWidget") || l.contains("AndroidRuntime") ||
                l.contains("FATAL") || l.contains("RemoteViews") ||
                l.contains("AppWidget") || l.contains("System.err") ||
                l.contains("agento") || l.contains("com.vishnu") ||
                l.contains("RemoteCollection")

        // Bearer secrets must never ride along when the user pastes the
        // log into chat. Redacts key=value pairs for the usual secret
        // names (case-insensitive); the diagnostics summary itself never
        // carries secrets (counts and error strings only).
        private val secretRE =
            Regex("""(?i)\b(token|bearer|password|passwd|api[_-]?key|secret)\b\s*[:=]\s*\S+""")

        private fun redactSecrets(s: String): String =
            s.replace(Regex("""(?i)\bbearer\s+\S+"""), "bearer <redacted>")
                .replace(secretRE, "$1=<redacted>")

        private fun prefs(context: Context) =
            context.applicationContext.getSharedPreferences(
                AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)

        /** This placement's view, defaulting to Open. */
        fun viewFor(context: Context, appWidgetId: Int): TaskWidgetView {
            val name = prefs(context).getString("task_widget_view_$appWidgetId", null)
            return TaskWidgetView.entries.firstOrNull { it.name == name }
                ?: TaskWidgetView.Open
        }

        /** This placement's density, defaulting to Comfortable. */
        fun densityFor(context: Context, appWidgetId: Int): TaskWidgetDensity {
            val name = prefs(context).getString("task_widget_density_$appWidgetId", null)
            return TaskWidgetDensity.entries.firstOrNull { it.name == name }
                ?: TaskWidgetDensity.Comfortable
        }

        /** Whether this placement's rows show the due line. */
        fun showDueFor(context: Context, appWidgetId: Int): Boolean =
            prefs(context).getBoolean("task_widget_due_$appWidgetId", true)

        /** Whether this placement uses the scrollable collection.
         * DIAGNOSTIC (#137): defaults to FALSE — static rows — so the
         * collection path is isolated from the rest of the widget on
         * first install. Flip it in the widget's display settings to
         * compare; the collection path stays the intended end state. */
        fun scrollableFor(context: Context, appWidgetId: Int): Boolean =
            prefs(context).getBoolean("task_widget_scroll_$appWidgetId", false)

        /** One row with explicit per-row intents instead of the collection
         * template: all immutable, so hosts that balk at the mutable
         * template (or at fill-in merging) still complete/open rows
         * (issue #137). Same layout and content as [buildRow]. */
        fun buildRowWithIntents(
            context: Context,
            appWidgetId: Int,
            compact: Boolean,
            showDue: Boolean,
            today: java.time.LocalDate,
            task: ServerTask,
        ): RemoteViews {
            val complete = PendingIntent.getActivity(
                context, ("wc:$appWidgetId:${task.id}").hashCode(),
                Intent(context, TaskCompleteActivity::class.java)
                    .putExtra(EXTRA_COMPLETE_ID, task.id)
                    .setData(Uri.parse("agento://task/${task.id}/complete")),
                PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
            )
            val openTask = PendingIntent.getActivity(
                context, ("wo:$appWidgetId:${task.id}").hashCode(),
                Intent(context, MainActivity::class.java)
                    .setAction(ACTION_TASKS)
                    .putExtra(EXTRA_TASK_ID, task.id)
                    .setData(Uri.parse("agento://task/${task.id}")),
                PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
            )
            return buildRow(context.packageName, compact, showDue, today, task).apply {
                setOnClickPendingIntent(R.id.task_widget_row_check, complete)
                setOnClickPendingIntent(R.id.task_widget_row, openTask)
            }
        }

        /** Builds one collection row. Shared by the legacy factory
         * (API <31) and the direct RemoteCollectionItems path (31+). */
        fun buildRow(
            packageName: String,
            compact: Boolean,
            showDue: Boolean,
            today: java.time.LocalDate,
            task: ServerTask,
        ): RemoteViews {
            val layout = if (compact) {
                R.layout.task_widget_row_compact
            } else {
                R.layout.task_widget_row
            }
            return RemoteViews(packageName, layout).apply {
                setTextViewText(R.id.task_widget_row_name, task.name)
                // Same friendly due line as the Task Manager rows (#130),
                // IST-pinned; blank collapses to gone below.
                val due = friendlyDue(task.dueDate, task.dueTime, today)
                if (showDue && due.isNotEmpty()) {
                    setTextViewText(R.id.task_widget_row_due, due)
                    setViewVisibility(R.id.task_widget_row_due, View.VISIBLE)
                } else {
                    setViewVisibility(R.id.task_widget_row_due, View.GONE)
                }
                // Ring completes inline; anywhere else opens the detail
                // sheet. Both ride the trampoline template pending intent.
                setOnClickFillInIntent(
                    R.id.task_widget_row_check,
                    Intent().putExtra(TaskWidget.EXTRA_COMPLETE_ID, task.id),
                )
                setOnClickFillInIntent(
                    R.id.task_widget_row,
                    Intent().putExtra(TaskWidget.EXTRA_TASK_ID, task.id),
                )
            }
        }

        /** Advance Open → Done → All → Open for one placement. */
        private fun cycleView(context: Context, appWidgetId: Int) {
            val next = when (viewFor(context, appWidgetId)) {
                TaskWidgetView.Open -> TaskWidgetView.Done
                TaskWidgetView.Done -> TaskWidgetView.All
                TaskWidgetView.All -> TaskWidgetView.Open
            }
            prefs(context).edit().putString("task_widget_view_$appWidgetId", next.name).apply()
        }

        /** Re-pull server tasks and push to every installed widget. Call
         * after task mutations so the home screen never goes stale.
         * Returns the worker Job so callers that must outlive a broadcast
         * (BootReceiver) can join it. */
        fun refresh(context: Context): Job {
            val appCtx = context.applicationContext
            return widgetScope.launch {
                val mgr = AppWidgetManager.getInstance(appCtx)
                fetchAndPush(
                    appCtx,
                    mgr.getAppWidgetIds(ComponentName(appCtx, TaskWidget::class.java)),
                )
            }
        }

        /** Single fetch-and-push path shared by pull() and refresh().
         * All three states are fetched (in parallel) so every placement's
         * view has data regardless of which views are installed. */
        private suspend fun fetchAndPush(appCtx: Context, ids: IntArray) {
            if (ids.isEmpty()) return
            val api = TasksApi(appCtx)
            val views = mutableMapOf<String, List<ServerTask>>()
            val errors = mutableMapOf<String, String>()
            coroutineScope {
                TaskWidgetView.entries.map { v ->
                    async { v.state to api.list(v.state) }
                }.awaitAll().forEach { (state, res) ->
                    res.getOrNull()?.let { views[state] = it }
                        ?: run {
                            errors[state] = res.exceptionOrNull()?.message
                                ?: "unknown error"
                        }
                }
            }
            if (views.isNotEmpty()) cachedViews = cachedViews + views
            // Drop errors for states that just succeeded so a stale
            // message can never outlive its failure (#130 review).
            lastErrors = (lastErrors + errors) - views.keys
            val mgr = AppWidgetManager.getInstance(appCtx)
            // Only the service-backed collection listens for a data change;
            // direct items and static rows are re-rendered whole.
            var needNotify = false
            val notifyIds = mutableListOf<Int>()
            for (id in ids) {
                val view = viewFor(appCtx, id)
                val tasks = views[view.state] ?: cachedViews[view.state]
                if (scrollableFor(appCtx, id) && tasks != null &&
                    Build.VERSION.SDK_INT < Build.VERSION_CODES.S
                ) {
                    needNotify = true
                    notifyIds.add(id)
                }
                // Error is per-view: no data for THIS view means the fetch
                // failed (a global flag would stick others on loading when
                // only one state errored).
                val detail = if (tasks == null) {
                    // serverDetail extracts server-sent validation text;
                    // raw transport errors stay generic — lock-screen
                    // visible widget text must not echo them verbatim.
                    lastErrors[view.state]?.let { raw ->
                        serverDetail(raw).takeIf { it != raw }
                    }
                } else {
                    null
                }
                // One wedged placement must not abort the rest (#130).
                runCatching {
                    mgr.updateAppWidget(
                        id, render(appCtx, id, view, tasks, tasks == null, detail))
                }.onFailure {
                    Log.w("TaskWidget", "updateAppWidget failed for id=$id", it)
                }
            }
            // Notify after the update loop: render() re-sets the remote
            // adapter, which would invalidate an earlier notify. Only the
            // service-backed placements have a list view to notify.
            if (needNotify && notifyIds.isNotEmpty()) {
                runCatching {
                    mgr.notifyAppWidgetViewDataChanged(
                        notifyIds.toIntArray(), R.id.task_widget_list_view)
                }.onFailure {
                    Log.w("TaskWidget", "notifyDataChanged failed", it)
                }
            }
        }

        /** Full widget view: header reflects this placement's view and the
         * latest fetch (null = not loaded yet, error flag = fetch failed);
         * the list binds to [TaskWidgetService] and fills whatever height
         * the placement offers. Row taps and the ring button go through
         * the [TaskCompleteActivity] trampoline with per-row fill-ins. */
        private fun render(
            context: Context,
            appWidgetId: Int,
            view: TaskWidgetView,
            tasks: List<ServerTask>?,
            error: Boolean,
            errorDetail: String? = null,
        ): RemoteViews {
            val open = Intent(context, MainActivity::class.java).setAction(ACTION_TASKS)
            val openPending = PendingIntent.getActivity(
                context, OPEN_CODE, open,
                PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
            )
            val refresh = Intent(context, TaskWidget::class.java).setAction(ACTION_REFRESH)
            val refreshPending = PendingIntent.getBroadcast(
                context, REFRESH_CODE, refresh,
                PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
            )
            val cycle = Intent(context, TaskWidget::class.java)
                .setAction(ACTION_VIEW)
                .putExtra(AppWidgetManager.EXTRA_APPWIDGET_ID, appWidgetId)
            val viewPending = PendingIntent.getBroadcast(
                context, VIEW_CODE + appWidgetId, cycle,
                PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
            )
            val config = Intent(context, TaskWidgetConfigActivity::class.java)
                .putExtra(AppWidgetManager.EXTRA_APPWIDGET_ID, appWidgetId)
            val configPending = PendingIntent.getActivity(
                context, CONFIG_CODE + appWidgetId, config,
                PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
            )
            // Trampoline template: per-row fill-ins carry either
            // EXTRA_TASK_ID (open detail) or EXTRA_COMPLETE_ID (complete
            // inline). MUTABLE is required: fill-in extras are silently
            // dropped from an immutable template on API 31+. Per-widget
            // code, like the view/config intents, so placements never
            // share a cached PendingIntent.
            val trampoline = Intent(context, TaskCompleteActivity::class.java)
            val rowPending = PendingIntent.getActivity(
                context, ROW_CODE + appWidgetId, trampoline,
                PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_MUTABLE,
            )
            // Per-widget data URI so the launcher keeps a distinct factory
            // per id; a plain opaque URI is guaranteed unique.
            val svc = Intent(context, TaskWidgetService::class.java).apply {
                putExtra(AppWidgetManager.EXTRA_APPWIDGET_ID, appWidgetId)
                data = Uri.parse("agento://widget/$appWidgetId")
            }
            val scrollable = scrollableFor(context, appWidgetId)
            return RemoteViews(context.packageName,
                if (scrollable) R.layout.task_widget else R.layout.task_widget_static).apply {
                setOnClickPendingIntent(R.id.task_widget_body, openPending)
                setOnClickPendingIntent(R.id.task_widget_title, configPending)
                setOnClickPendingIntent(R.id.task_widget_view, viewPending)
                setOnClickPendingIntent(R.id.task_widget_refresh, refreshPending)
                // Error/empty state is tappable: re-pull instead of sitting
                // dead on a stale failure (#130).
                setOnClickPendingIntent(R.id.task_widget_empty, refreshPending)
                if (!scrollable) {
                    // Diagnostic path (#137): plain rows, no collection,
                    // no template, no service bind. Isolates a collection
                    // failure from the rest of the widget. Each row keeps
                    // its own immutable complete/open intents, so taps are
                    // identical to the collection path.
                    if (tasks != null) {
                        val today = java.time.LocalDate.now(IST)
                        val compact = densityFor(context, appWidgetId) == TaskWidgetDensity.Compact
                        val due = showDueFor(context, appWidgetId)
                        // Clear first: re-rendering into a fresh RemoteViews
                        // each time would append, leaving ghost rows when
                        // the list shrinks.
                        removeAllViews(R.id.task_widget_static_list)
                        tasks.take(STATIC_ROW_LIMIT).forEach { t ->
                            addView(R.id.task_widget_static_list,
                                buildRowWithIntents(
                                    context, appWidgetId, compact, due, today, t))
                        }
                    }
                    setViewVisibility(R.id.task_widget_static_list,
                        if (tasks.isNullOrEmpty()) View.GONE else View.VISIBLE)
                    setViewVisibility(R.id.task_widget_empty,
                        if (tasks.isNullOrEmpty()) View.VISIBLE else View.GONE)
                } else if (tasks != null && Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
                    // Service-backed collections (setRemoteAdapter) broke
                    // on Android 16 — hand the rows over directly instead
                    // of binding the service (issue #137). Rows carry
                    // explicit immutable intents (no mutable template or
                    // fill-ins: the other host-sensitive piece). Capped:
                    // the whole list rides one parcel, so a huge "All"
                    // view would TransactionTooLarge the update (the
                    // header count below still shows the true total).
                    val today = java.time.LocalDate.now(IST)
                    val compact = densityFor(context, appWidgetId) == TaskWidgetDensity.Compact
                    val due = showDueFor(context, appWidgetId)
                    val items = RemoteViews.RemoteCollectionItems.Builder().apply {
                        tasks.take(100).forEachIndexed { i, t ->
                            addItem(i.toLong(),
                                buildRowWithIntents(
                                    context, appWidgetId, compact, due, today, t))
                        }
                        setHasStableIds(false)
                        setViewTypeCount(1)
                    }.build()
                    // Overload taking the items directly (API 31+), not
                    // the service Intent below.
                    setRemoteAdapter(R.id.task_widget_list_view, items)
                    setEmptyView(
                        R.id.task_widget_list_view,
                        R.id.task_widget_empty,
                    )
                } else {
                    // API <31 has no RemoteCollectionItems: legacy service
                    // path (TaskWidgetService) with the fill-in template.
                    // tasks==null also lands here (loading/error shell,
                    // list stays empty).
                    setPendingIntentTemplate(R.id.task_widget_list_view, rowPending)
                    setRemoteAdapter(R.id.task_widget_list_view, svc)
                    setEmptyView(
                        R.id.task_widget_list_view,
                        R.id.task_widget_empty,
                    )
                }
                setTextViewText(R.id.task_widget_view, view.title)
                val plural = when (view) {
                    TaskWidgetView.Open -> R.plurals.task_widget_open
                    TaskWidgetView.Done -> R.plurals.task_widget_done
                    TaskWidgetView.All -> R.plurals.task_widget_all
                }
                val count = when {
                    tasks == null && !error -> context.getString(R.string.task_widget_loading)
                    tasks == null -> context.getString(R.string.task_widget_error)
                    tasks.isEmpty() -> context.getString(R.string.task_widget_empty)
                    // Static rows cap at 8: say so, so the count and the
                    // visible rows never silently disagree.
                    !scrollable && tasks.size > STATIC_ROW_LIMIT ->
                        context.resources.getQuantityString(plural, tasks.size, tasks.size) +
                            " · showing " + STATIC_ROW_LIMIT
                    else -> context.resources.getQuantityString(
                        plural, tasks.size, tasks.size)
                }
                setTextViewText(R.id.task_widget_count, count)
                val emptyText = when {
                    tasks == null && !error -> context.getString(R.string.task_widget_loading)
                    tasks == null -> (errorDetail?.take(140)
                        ?: context.getString(R.string.task_widget_error)) +
                        " — tap to retry"
                    tasks.isEmpty() -> context.getString(R.string.task_widget_empty)
                    else -> ""
                }
                setTextViewText(R.id.task_widget_empty, emptyText)
            } // end RemoteViews apply
        }
    }
}
