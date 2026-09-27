package com.vishnu.agento

import android.content.Context
import android.content.Intent
import android.view.View
import android.widget.RemoteViews
import android.widget.RemoteViewsService

/**
 * Collection adapter for the task widget's scrollable list. Reads the
 * snapshot cached by [TaskWidget] (same process, no IPC payload) so
 * getViewAt stays cheap; row taps fill in to the template pending
 * intent that opens Task Manager.
 */
class TaskWidgetService : RemoteViewsService() {
    override fun onGetViewFactory(intent: Intent): RemoteViewsFactory =
        TaskFactory(applicationContext)
}

private class TaskFactory(private val appCtx: Context) : RemoteViewsService.RemoteViewsFactory {

    private var items: List<ServerTask> = emptyList()

    override fun onCreate() = Unit
    override fun onDestroy() = Unit
    override fun getLoadingView(): RemoteViews? = null
    override fun getViewTypeCount(): Int = 1
    override fun hasStableIds(): Boolean = true

    override fun onDataSetChanged() {
        items = TaskWidget.cachedTasks ?: emptyList()
    }

    override fun getCount(): Int = items.size

    override fun getItemId(position: Int): Long =
        items.getOrNull(position)?.id?.hashCode()?.toLong() ?: position.toLong()

    override fun getViewAt(position: Int): RemoteViews {
        val task = items.getOrNull(position)
            ?: return RemoteViews(appCtx.packageName, R.layout.task_widget_row)
        return RemoteViews(appCtx.packageName, R.layout.task_widget_row).apply {
            setTextViewText(R.id.task_widget_row_name, "\u2022 " + task.name)
            val due = listOf(task.dueDate.trim(), task.dueTime.trim())
                .filter { it.isNotEmpty() }.joinToString(" ")
            setTextViewText(R.id.task_widget_row_due, due)
            setViewVisibility(
                R.id.task_widget_row_due,
                if (due.isEmpty()) View.GONE else View.VISIBLE,
            )
            setOnClickFillInIntent(
                R.id.task_widget_row_name,
                Intent().setAction(TaskWidget.ACTION_TASKS),
            )
        }
    }
}
