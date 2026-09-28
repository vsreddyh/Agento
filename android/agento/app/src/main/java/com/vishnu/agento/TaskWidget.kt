package com.vishnu.agento

import android.app.PendingIntent
import android.appwidget.AppWidgetManager
import android.appwidget.AppWidgetProvider
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.util.Log
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
            for (id in ids) {
                val view = viewFor(appCtx, id)
                val tasks = views[view.state] ?: cachedViews[view.state]
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
            // adapter, which would invalidate an earlier notify.
            runCatching {
                mgr.notifyAppWidgetViewDataChanged(ids, R.id.task_widget_list_view)
            }.onFailure {
                Log.w("TaskWidget", "notifyDataChanged failed", it)
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
            return RemoteViews(context.packageName, R.layout.task_widget).apply {
                setOnClickPendingIntent(R.id.task_widget_body, openPending)
                setOnClickPendingIntent(R.id.task_widget_title, configPending)
                setOnClickPendingIntent(R.id.task_widget_view, viewPending)
                setOnClickPendingIntent(R.id.task_widget_refresh, refreshPending)
                // Error/empty state is tappable: re-pull instead of sitting
                // dead on a stale failure (#130).
                setOnClickPendingIntent(R.id.task_widget_empty, refreshPending)
                setPendingIntentTemplate(R.id.task_widget_list_view, rowPending)
                setRemoteAdapter(R.id.task_widget_list_view, svc)
                setEmptyView(
                    R.id.task_widget_list_view,
                    R.id.task_widget_empty,
                )
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
            }
        }
    }
}
