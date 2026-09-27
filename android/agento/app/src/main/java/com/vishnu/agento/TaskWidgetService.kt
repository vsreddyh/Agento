package com.vishnu.agento

import android.appwidget.AppWidgetManager
import android.content.Context
import android.content.Intent
import android.view.View
import android.widget.RemoteViews
import android.widget.RemoteViewsService
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeoutOrNull

/**
 * Collection adapter for the task widget's scrollable list. Each factory
 * instance belongs to one placement (the widget id arrives on its
 * service intent) and shows that placement's view (Open/Done/All) in its
 * density (Comfortable/Compact). Snapshots come from [TaskWidget]'s
 * per-state cache (same process, no IPC payload) so getViewAt stays
 * cheap; row taps and the ring button fill in to the
 * [TaskCompleteActivity] template pending intent.
 */
class TaskWidgetService : RemoteViewsService() {
    override fun onGetViewFactory(intent: Intent): RemoteViewsFactory {
        val id = intent.getIntExtra(
            AppWidgetManager.EXTRA_APPWIDGET_ID,
            AppWidgetManager.INVALID_APPWIDGET_ID,
        )
        return TaskFactory(applicationContext, id)
    }
}

private class TaskFactory(
    private val appCtx: Context,
    private val appWidgetId: Int,
) : RemoteViewsService.RemoteViewsFactory {

    private var items: List<ServerTask> = emptyList()

    override fun onCreate() = Unit
    override fun onDestroy() = Unit
    override fun getLoadingView(): RemoteViews? = null
    override fun getViewTypeCount(): Int = 1
    override fun hasStableIds(): Boolean = true

    private fun view(): TaskWidgetView =
        if (appWidgetId == AppWidgetManager.INVALID_APPWIDGET_ID) {
            TaskWidgetView.Open
        } else {
            TaskWidget.viewFor(appCtx, appWidgetId)
        }

    private fun compact(): Boolean =
        appWidgetId != AppWidgetManager.INVALID_APPWIDGET_ID &&
            TaskWidget.densityFor(appCtx, appWidgetId) == TaskWidgetDensity.Compact

    private fun showDue(): Boolean =
        appWidgetId == AppWidgetManager.INVALID_APPWIDGET_ID ||
            TaskWidget.showDueFor(appCtx, appWidgetId)

    override fun onDataSetChanged() {
        // In-memory snapshot first; after process death/reboot the cache
        // is empty, so fall back to a synchronous fetch (blocking is
        // explicitly allowed here) instead of showing Loading… forever.
        // Bounded and throw-proof: this runs on the AppWidget binder
        // thread, and the shared client's 30s read timeout (or any
        // unexpected throw) would stall/kill the host bind and surface
        // as a widget load error. Slow path just shows empty/stale.
        val state = view().state
        items = TaskWidget.cachedViews[state]
            ?: runCatching {
                runBlocking {
                    withTimeoutOrNull(10_000) {
                        TasksApi(appCtx).list(state).getOrNull()
                    }.orEmpty()
                }
            }.getOrDefault(emptyList())
    }

    override fun getCount(): Int = items.size

    override fun getItemId(position: Int): Long =
        items.getOrNull(position)?.id?.hashCode()?.toLong() ?: position.toLong()

    override fun getViewAt(position: Int): RemoteViews {
        val layout = if (compact()) {
            R.layout.task_widget_row_compact
        } else {
            R.layout.task_widget_row
        }
        val task = items.getOrNull(position)
            ?: return RemoteViews(appCtx.packageName, layout)
        return RemoteViews(appCtx.packageName, layout).apply {
            setTextViewText(R.id.task_widget_row_name, task.name)
            val due = listOf(task.dueDate.trim(), task.dueTime.trim())
                .filter { it.isNotEmpty() }.joinToString(" ")
            if (showDue() && due.isNotEmpty()) {
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
}
