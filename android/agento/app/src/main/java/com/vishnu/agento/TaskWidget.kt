package com.vishnu.agento

import android.app.PendingIntent
import android.appwidget.AppWidgetManager
import android.appwidget.AppWidgetProvider
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.os.Bundle
import android.view.View
import android.widget.RemoteViews
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.launch

/**
 * Home-screen task manager widget (#101): open server-task count plus as
 * many single-line task rows as the placement fits, with tap-to-open (Task
 * Manager screen) and a refresh button. Reads the same shared `tasks`
 * collection as TaskManagerScreen via TasksApi; the app pushes fresh data
 * after every task mutation via [refresh]. No configuration, no periodic
 * updates (updatePeriodMillis=0) — data pulls on add/refresh-tap/
 * resize/mutation only, so a sleeping server costs nothing in background.
 *
 * Dynamic sizing: min footprint 3x2, resizable both ways. [render] reads
 * the per-widget size options ([AppWidgetManager.getAppWidgetOptions])
 * and shows 2 rows at 3x2, growing to [MAX_ROWS] on taller placements;
 * wide placements append each task's due label inline.
 */
class TaskWidget : AppWidgetProvider() {

    override fun onUpdate(
        context: Context,
        appWidgetManager: AppWidgetManager,
        appWidgetIds: IntArray,
    ) {
        // Loading shell first so the widget never sits blank, then fill in.
        for (id in appWidgetIds) {
            appWidgetManager.updateAppWidget(
                id, render(context, appWidgetManager, id, null, false))
        }
        pull(context, appWidgetIds)
    }

    override fun onAppWidgetOptionsChanged(
        context: Context,
        appWidgetManager: AppWidgetManager,
        appWidgetId: Int,
        newOptions: Bundle,
    ) {
        // Resize: re-render the shell at the new size immediately, then
        // re-pull so the row count reflects real data at the new height.
        appWidgetManager.updateAppWidget(
            appWidgetId, render(context, appWidgetManager, appWidgetId, lastTasks, lastError))
        pull(context, intArrayOf(appWidgetId))
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

        /** Hard cap on task rows; taller widgets just get airier text. */
        private const val MAX_ROWS = 6

        private val ITEM_IDS = intArrayOf(
            R.id.task_widget_item1,
            R.id.task_widget_item2,
            R.id.task_widget_item3,
            R.id.task_widget_item4,
            R.id.task_widget_item5,
            R.id.task_widget_item6,
        )

        // Last fetched data, reused for instant resize re-renders between
        // pulls (volatile: written on IO, read on the broadcast thread).
        @Volatile private var lastTasks: List<ServerTask>? = null
        @Volatile private var lastError: Boolean = false

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
            lastTasks = tasks
            lastError = tasks == null
            val mgr = AppWidgetManager.getInstance(appCtx)
            for (id in ids) {
                mgr.updateAppWidget(id, render(appCtx, mgr, id, tasks, tasks == null))
            }
        }

        /** Rows that fit the widget's guaranteed height: 2 at the 3x2
         * minimum (110dp), one more per ~50dp, capped at [MAX_ROWS]. */
        internal fun rowsForHeight(minHeightDp: Int): Int = when {
            minHeightDp >= 320 -> 6
            minHeightDp >= 260 -> 5
            minHeightDp >= 200 -> 4
            minHeightDp >= 150 -> 3
            minHeightDp >= 110 -> 2
            else -> 1
        }.coerceIn(1, MAX_ROWS)

        /** Full widget view: tap targets always bound, text reflects the
         * latest fetch (null = not loaded yet, error flag = fetch failed).
         * Row count and due labels adapt to the per-widget size options so
         * any placement at/above 3x2 renders without clipping. */
        private fun render(
            context: Context,
            mgr: AppWidgetManager,
            appWidgetId: Int,
            tasks: List<ServerTask>?,
            error: Boolean,
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
            val opts = runCatching { mgr.getAppWidgetOptions(appWidgetId) }.getOrNull()
            val minH = opts?.getInt(AppWidgetManager.OPTION_APPWIDGET_MIN_HEIGHT, 110) ?: 110
            val minW = opts?.getInt(AppWidgetManager.OPTION_APPWIDGET_MIN_WIDTH, 180) ?: 180
            val rows = rowsForHeight(minH)
            val wide = minW >= 250
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
                val open_tasks = if (tasks.isNullOrEmpty()) emptyList() else tasks
                val shown = open_tasks.take(rows)
                for (i in ITEM_IDS.indices) {
                    val viewId = ITEM_IDS[i]
                    if (i < shown.size) {
                        setTextViewText(viewId, "\u2022 " + label(shown[i], wide))
                        setViewVisibility(viewId, View.VISIBLE)
                    } else {
                        setViewVisibility(viewId, View.GONE)
                    }
                }
                val extra = open_tasks.size - shown.size
                if (extra > 0) {
                    setTextViewText(R.id.task_widget_overflow, "+$extra more")
                    setViewVisibility(R.id.task_widget_overflow, View.VISIBLE)
                } else {
                    setViewVisibility(R.id.task_widget_overflow, View.GONE)
                }
                setViewVisibility(
                    R.id.task_widget_list,
                    if (open_tasks.isEmpty()) View.GONE else View.VISIBLE,
                )
            }
        }

        /** Row label: name only on narrow placements; name + due label
         * when wide enough for the extra text to survive ellipsizing. */
        private fun label(task: ServerTask, wide: Boolean): String {
            val due = listOf(task.dueDate.trim(), task.dueTime.trim())
                .filter { it.isNotEmpty() }.joinToString(" ")
            return if (wide && due.isNotEmpty()) "${task.name} \u00b7 $due" else task.name
        }
    }
}
