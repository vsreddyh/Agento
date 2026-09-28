package com.vishnu.agento

import android.appwidget.AppWidgetManager
import android.content.Context
import android.content.Intent
import android.view.View
import android.widget.RemoteViews
import android.widget.RemoteViewsService
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeoutOrNull
import java.time.LocalDate

/**
 * Collection adapter for the task widget's scrollable list. LEGACY path
 * (API <31): newer devices get rows directly via RemoteCollectionItems
 * in render(), since service-backed collections broke on Android 16
 * (issue #137). Each factory instance belongs to one placement (the
 * widget id arrives on its service intent) and shows that placement's
 * view (Open/Done/All) in its density (Comfortable/Compact). Snapshots
 * come from [TaskWidget]'s per-state cache (same process, no IPC
 * payload) so getViewAt stays cheap; row taps and the ring button fill
 * in to the [TaskCompleteActivity] template pending intent.
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
    // Read once per dataset change, not per row (#130 review).
    private var today: LocalDate = LocalDate.now(IST)

    override fun onCreate() = Unit
    override fun onDestroy() = Unit
    override fun getLoadingView(): RemoteViews? = null
    override fun getViewTypeCount(): Int = 1

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
        // Anything escaping is recorded for diagnostics (never rethrown:
        // a factory throw IS the system error view).
        runCatching {
            val state = view().state
            today = LocalDate.now(IST)
            items = TaskWidget.cachedViews[state]
                ?: runCatching {
                    runBlocking {
                        withTimeoutOrNull(10_000) {
                            TasksApi(appCtx).list(state).getOrNull()
                        }.orEmpty()
                    }
                }.getOrDefault(emptyList())
        }.onFailure {
            TaskWidget.recordFactoryError("onDataSetChanged", it)
        }
    }

    override fun getCount(): Int = items.size

    // Stable ids off: position ids would confuse recycling when the
    // dataset shifts, and id hashes can theoretically collide. Plain
    // positional binding is correct at this list size.
    override fun hasStableIds(): Boolean = false

    override fun getItemId(position: Int): Long = position.toLong()

    override fun getViewAt(position: Int): RemoteViews {
        // A throw here is the host's system error view — never let one
        // escape; record it for diagnostics and hand back a visible
        // failure row. Legacy path only (API <31); newer devices get
        // their rows directly via RemoteCollectionItems in render().
        return runCatching {
            val task = items.getOrNull(position)
                ?: return@runCatching RemoteViews(appCtx.packageName,
                    if (compact()) R.layout.task_widget_row_compact else R.layout.task_widget_row)
            TaskWidget.buildRow(appCtx.packageName, compact(), showDue(), today, task)
        }.getOrElse {
            TaskWidget.recordFactoryError("getViewAt($position)", it)
            // Visible failure, not a mysteriously empty row (the error is
            // also in diagnostics via recordFactoryError above).
            RemoteViews(appCtx.packageName, R.layout.task_widget_row).apply {
                setTextViewText(
                    R.id.task_widget_row_name,
                    appCtx.getString(R.string.task_widget_error),
                )
                setViewVisibility(R.id.task_widget_row_due, View.GONE)
            }
        }
    }
}
