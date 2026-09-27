package com.vishnu.agento

import android.app.PendingIntent
import android.appwidget.AppWidgetManager
import android.appwidget.AppWidgetProvider
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.view.View
import android.widget.RemoteViews
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.launch

/**
 * Home-screen task manager widget (#101): open server-task count plus the
 * first open task, with tap-to-open (Task Manager screen) and a refresh
 * button. Reads the same shared `tasks` collection as TaskManagerScreen
 * via TasksApi; the app pushes fresh data after every task mutation via
 * [refresh]. No configuration, no periodic updates (updatePeriodMillis=0)
 * — data pulls on add/refresh-tap/mutation only, so a sleeping server
 * costs nothing in the background.
 */
class TaskWidget : AppWidgetProvider() {

    override fun onUpdate(
        context: Context,
        appWidgetManager: AppWidgetManager,
        appWidgetIds: IntArray,
    ) {
        // Loading shell first so the widget never sits blank, then fill in.
        for (id in appWidgetIds) {
            appWidgetManager.updateAppWidget(id, render(context, null, false))
        }
        pull(context, appWidgetIds)
    }

    override fun onReceive(context: Context, intent: Intent) {
        super.onReceive(context, intent)
        if (intent.action == ACTION_REFRESH) {
            val mgr = AppWidgetManager.getInstance(context)
            val ids = mgr.getAppWidgetIds(ComponentName(context, TaskWidget::class.java))
            if (ids.isNotEmpty()) pull(context, ids)
        }
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

        // One app-scoped worker: per-call CoroutineScope(Dispatchers.IO)
        // leaks a scope per update (review #109).
        private val widgetScope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

        // Distinct PendingIntent codes (review #109): actions already keep
        // the open/refresh intents apart, but codes make that explicit.
        private const val OPEN_CODE = 1001
        private const val REFRESH_CODE = 1002

        /** Re-pull server tasks and push to every installed widget. Call
         * after task mutations so the home screen never goes stale. */
        fun refresh(context: Context) {
            widgetScope.launch {
                val appCtx = context.applicationContext
                val mgr = AppWidgetManager.getInstance(appCtx)
                fetchAndPush(
                    appCtx,
                    mgr.getAppWidgetIds(ComponentName(appCtx, TaskWidget::class.java)),
                )
            }
        }

        /** Single fetch-and-push path shared by pull() and refresh(). */
        private suspend fun fetchAndPush(appCtx: Context, ids: IntArray) {
            if (ids.isEmpty()) return
            val tasks = TasksApi(appCtx).list("open").getOrNull()
            val mgr = AppWidgetManager.getInstance(appCtx)
            for (id in ids) {
                mgr.updateAppWidget(id, render(appCtx, tasks, tasks == null))
            }
        }

        /** Full widget view: tap targets always bound, text reflects the
         * latest fetch (null = not loaded yet, error flag = fetch failed). */
        private fun render(context: Context, tasks: List<ServerTask>?, error: Boolean): RemoteViews {
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
            return RemoteViews(context.packageName, R.layout.task_widget).apply {
                setOnClickPendingIntent(R.id.task_widget_body, openPending)
                setOnClickPendingIntent(R.id.task_widget_refresh, refreshPending)
                val count = when {
                    tasks == null && !error -> context.getString(R.string.task_widget_loading)
                    tasks == null -> context.getString(R.string.task_widget_error)
                    tasks.isEmpty() -> context.getString(R.string.task_widget_empty)
                    else -> context.resources.getQuantityString(
                        R.plurals.task_widget_open, tasks.size, tasks.size)
                }
                setTextViewText(R.id.task_widget_count, count)
                val top = tasks?.firstOrNull()?.name.orEmpty()
                setTextViewText(R.id.task_widget_top, top)
                setViewVisibility(
                    R.id.task_widget_top,
                    if (top.isEmpty()) View.GONE else View.VISIBLE,
                )
            }
        }
    }
}
