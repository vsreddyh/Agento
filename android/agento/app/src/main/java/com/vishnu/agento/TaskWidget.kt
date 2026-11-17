package com.vishnu.agento

import android.app.PendingIntent
import android.appwidget.AppWidgetManager
import android.appwidget.AppWidgetProvider
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.os.Build
import android.os.Bundle
import android.util.Log
import android.view.View
import android.widget.RemoteViews
import androidx.core.content.ContextCompat
import androidx.core.content.edit
import androidx.core.net.toUri
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Deferred
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.async
import kotlinx.coroutines.awaitAll
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.launch

/**
 * Header/background tap-contract ids per layout (#370): the chrome ids that
 * handle a tap themselves, named once so render() and the test read the
 * same lists — one per layout ([PLAIN_TAP_IDS], [LIST_TAP_IDS],
 * [SMALL_TAP_IDS]). [R.id.task_widget_refresh] is in all three because it
 * is one of the two targets ([WidgetTapTarget.REFRESH]); every other id
 * here opens the Task Manager. Membership differs by layout: the view
 * label exists only in the plain layout (the scrollable header names the
 * view in its title instead), `empty` only in the two full layouts, and
 * `next`/`next_due` only in the small one.
 *
 * The two collection containers — `task_widget_static_list` and
 * `task_widget_list_view` — are deliberately absent: their taps ride the
 * pending-intent template (or land on the body behind them), so listing
 * them here would steal a row tap. Leaving them out is the contract, not an
 * oversight; touching the template needs its own change.
 */
internal val PLAIN_TAP_IDS = listOf(
    R.id.task_widget_body,
    R.id.task_widget_title,
    R.id.task_widget_count,
    R.id.task_widget_view,
    R.id.task_widget_empty,
    R.id.task_widget_refresh,
)

internal val LIST_TAP_IDS = listOf(
    R.id.task_widget_body,
    R.id.task_widget_title,
    R.id.task_widget_count,
    R.id.task_widget_empty,
    R.id.task_widget_refresh,
)

/**
 * Compact-layout chrome (#532). The compact placement shows the next task
 * as two plain text lines rather than collection rows, so those lines join
 * the background in opening the Task Manager — a dead label reads as a
 * broken widget.
 */
internal val SMALL_TAP_IDS = listOf(
    R.id.task_widget_body,
    R.id.task_widget_title,
    R.id.task_widget_count,
    R.id.task_widget_next,
    R.id.task_widget_next_due,
    R.id.task_widget_refresh,
)

/** Where one widget header/background tap lands (#370). Rows are not in
 * this table: the ring and the row body ride the collection template (or
 * per-row intents), which own their view ids. */
internal enum class WidgetTapTarget {
    OPEN_TASKS,
    REFRESH,
}

/**
 * The tap contract, keyed by view id: background, title, count, view
 * label and empty line open the Task Manager; the refresh button
 * refreshes. Anything else (rows, the ListView itself) returns null —
 * render() leaves those to the template/fill-ins or the body fallback.
 * Pure and JVM-pinned, so the contract survives without an on-device
 * tap-through.
 */
internal fun widgetTapTarget(viewId: Int): WidgetTapTarget? = when (viewId) {
    R.id.task_widget_body,
    R.id.task_widget_title,
    R.id.task_widget_count,
    R.id.task_widget_view,
    R.id.task_widget_empty,
    R.id.task_widget_next,
    R.id.task_widget_next_due -> WidgetTapTarget.OPEN_TASKS
    R.id.task_widget_refresh -> WidgetTapTarget.REFRESH
    else -> null
}

/**
 * Installs the contract's handlers on one layout. Ids returning null
 * from [widgetTapTarget] are skipped — rows own theirs — so callers pass
 * only header/background ids.
 */
internal fun applyTapTargets(
    views: RemoteViews,
    openTasks: PendingIntent,
    refresh: PendingIntent,
    ids: List<Int>,
) {
    for (id in ids) {
        when (widgetTapTarget(id)) {
            WidgetTapTarget.OPEN_TASKS -> views.setOnClickPendingIntent(id, openTasks)
            WidgetTapTarget.REFRESH -> views.setOnClickPendingIntent(id, refresh)
            null -> Unit
        }
    }
}

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
 * Row order for one widget placement (#369). `Server` keeps the fetch
 * order (today's behaviour); the rest mirror the Task Manager's
 * [ServerTaskSort] titles one-for-one, so a placement can match the
 * in-app list ordering. Stored per widget id like the other display
 * settings, with Server as the default.
 */
enum class TaskWidgetSort(val title: String) {
    Server("Server order"),
    Start("Start time"),
    Name("Name"),
    Created("Newest"),
    Estimate("Estimate"),
}

/**
 * Orders one placement's rows. Server keeps the fetch order untouched;
 * every other mode reuses the Task Manager's own [sortedByMode], so the
 * widget and the in-app list can never disagree about what "Name" means.
 * Pure and JVM-pinned.
 */
internal fun List<ServerTask>.sortedForWidget(sort: TaskWidgetSort): List<ServerTask> =
    when (sort) {
        TaskWidgetSort.Server -> this
        TaskWidgetSort.Start -> sortedByMode(ServerTaskSort.Start)
        TaskWidgetSort.Name -> sortedByMode(ServerTaskSort.Name)
        TaskWidgetSort.Created -> sortedByMode(ServerTaskSort.Created)
        TaskWidgetSort.Estimate -> sortedByMode(ServerTaskSort.Estimate)
    }

/**
 * One item in the widget's collection (#532): an agenda header or a task
 * row. Headers are collection items rather than static views because the
 * list scrolls as one unit, and they get their own layout: the platform
 * adapter derives each item's view type from its RemoteViews layout id, so
 * two distinct layouts plus a view-type count of 2 is what makes mixed
 * items legal on both the legacy factory and the direct-items path.
 */
internal sealed interface WidgetItem {
    data class Header(val bucket: DueBucket, val count: Int) : WidgetItem
    data class Row(val task: ServerTask) : WidgetItem
}

/**
 * Builds one placement's agenda in the Task Manager's own bucket order
 * (#532). Pure and JVM-pinned: the widget must never invent an order the
 * app does not show. [grouped] is false for the Done view, where buckets
 * carry no meaning.
 */
internal fun widgetAgenda(
    tasks: List<ServerTask>,
    grouped: Boolean,
    today: java.time.LocalDate,
    now: java.time.LocalDateTime,
): List<WidgetItem> {
    if (!grouped) return tasks.map { WidgetItem.Row(it) }
    val byBucket = tasks.groupBy { it.dueBucket(today, now) }
    return DueBucket.entries.flatMap { bucket ->
        val rows = byBucket[bucket].orEmpty()
        if (rows.isEmpty()) {
            emptyList()
        } else {
            listOf(WidgetItem.Header(bucket, rows.size)) + rows.map { WidgetItem.Row(it) }
        }
    }
}

/**
 * The "showing N" qualifier for a capped widget count line (#532): present
 * exactly when the surface shows fewer rows than exist. Pure and
 * JVM-pinned, so the static path (8) and the compact placement (1) cannot
 * drift into different honesty rules.
 */
internal fun shownSuffix(total: Int, shownRows: Int?): String =
    if (shownRows != null && total > shownRows) " · showing $shownRows" else ""

/**
 * The task a compact placement names as "next" (#532): the first row of the
 * same agenda the full layout builds, so a placement sorted by Name or
 * Estimate cannot name a different task than its full-size twin shows first.
 * Pure and JVM-pinned.
 */
internal fun widgetNextTask(
    tasks: List<ServerTask>,
    grouped: Boolean,
    today: java.time.LocalDate,
    now: java.time.LocalDateTime,
): ServerTask? = widgetAgenda(tasks, grouped, today, now)
    .filterIsInstance<WidgetItem.Row>()
    .firstOrNull()
    ?.task

/**
 * Trims an agenda to [maxRows] task rows for the non-scrolling layout
 * (#532): the cap counts rows, and a section header only survives when at
 * least one of its rows does, so the header count and "showing N" can never
 * disagree. Pure and JVM-pinned.
 */
internal fun List<WidgetItem>.takeAgendaRows(maxRows: Int): List<WidgetItem> {
    if (maxRows <= 0) return emptyList()
    val out = ArrayList<WidgetItem>(maxRows + 4)
    var rows = 0
    for (item in this) {
        if (item is WidgetItem.Row) {
            if (rows == maxRows) break
            rows++
        }
        out.add(item)
    }
    while (out.isNotEmpty() && out.last() is WidgetItem.Header) out.removeAt(out.size - 1)
    return out
}

/** Longest custom title a placement keeps, in characters (#532). */
internal const val WIDGET_TITLE_MAX = 30

/** A placement's header title: its custom title, trimmed and capped, or
 * [fallback] when blank. Pure and JVM-pinned, so the blank-means-default
 * rule cannot drift between the widget and its config screen. */
internal fun widgetTitle(custom: String?, fallback: String): String {
    val trimmed = custom?.trim().orEmpty()
    return if (trimmed.isEmpty()) fallback else trimmed.take(WIDGET_TITLE_MAX)
}

/** The broadcast action a widget row posts for [target] (#532): the
 * notification actions themselves, so the receiver needs no second route
 * and a widget tap shares the notification's failure handling. Pure and
 * JVM-pinned by WidgetAgendaTest. */
internal fun widgetMutationAction(target: NotifAction): String = when (target) {
    NotifAction.COMPLETE -> TaskNotifActionReceiver.ACTION_COMPLETE
    NotifAction.SKIP -> TaskNotifActionReceiver.ACTION_SKIP
}

/**
 * Size at or above which a placement gets the full list (#532): the
 * provider's default 3x2 footprint in task_widget_info.xml, i.e. three
 * cells wide (180dp) and two rows tall (110dp). A placement smaller than
 * EITHER bound — narrower than three cells or shorter than two rows, e.g.
 * a 4x1 — shrinks to the compact layout instead of squeezing the header +
 * list into the gap. Pure so the thresholds are testable without a
 * launcher.
 */
internal const val COMPACT_MAX_WIDTH_DP = 180
internal const val COMPACT_MIN_HEIGHT_DP = 110

/** Whether a placement reporting these options is compact (#532). Only
 * BOTH options missing/0 keeps the full list (the historical behaviour on
 * a launcher that will not say how big it is) — a known-small dimension
 * compacts even when the other is unknown, so (110, 0) and (0, 50) are
 * compact and only (0, 0) is not. */
internal fun widgetIsCompact(minWidthDp: Int, minHeightDp: Int): Boolean =
    (minWidthDp in 1 until COMPACT_MAX_WIDTH_DP) ||
        (minHeightDp in 1 until COMPACT_MIN_HEIGHT_DP)

/**
 * The secondary line a widget row shows (#532): the shared due · repeat ·
 * parallel line, or — when the placement turns due dates off — only the
 * repeat/parallel marks, so the repeat condition never disappears with a
 * display toggle that was never about it.
 */
internal fun widgetSecondaryLine(
    task: ServerTask,
    today: java.time.LocalDate,
    showDue: Boolean,
): String {
    if (showDue) return task.dueBits(today)
    val bits = ArrayList<String>(2)
    val repeat = task.repeatLabel()
    if (repeat.isNotEmpty()) bits.add(repeat)
    if (task.parallelable) bits.add("parallel")
    return bits.joinToString(" · ")
}

/** A stored widget view name, defaulting to Open (#437). Unknown names
 * reset — and that reset reports through [widgetViewCoercion], so a
 * corrupt or version-skewed pref is visible instead of a placement
 * changing for no apparent reason. */
internal fun widgetViewForName(raw: String?): TaskWidgetView =
    raw?.let { n -> TaskWidgetView.entries.firstOrNull { it.name == n } }
        ?: TaskWidgetView.Open

/** Names a widget view reset (#437): null when [raw] is absent (the
 * default itself is fine) or known, otherwise `"raw"->became`. */
internal fun widgetViewCoercion(raw: String?): String? =
    if (raw == null || TaskWidgetView.entries.any { it.name == raw }) null
    else "${visibleTaskRaw(raw)}->${TaskWidgetView.Open.name}"

/** A stored widget density name, defaulting to Comfortable (#437). */
internal fun widgetDensityForName(raw: String?): TaskWidgetDensity =
    raw?.let { n -> TaskWidgetDensity.entries.firstOrNull { it.name == n } }
        ?: TaskWidgetDensity.Comfortable

/** Names a widget density reset (#437): null when absent or known. */
internal fun widgetDensityCoercion(raw: String?): String? =
    if (raw == null || TaskWidgetDensity.entries.any { it.name == raw }) null
    else "${visibleTaskRaw(raw)}->${TaskWidgetDensity.Comfortable.name}"

/** A stored widget sort name, defaulting to Server (#437). */
internal fun widgetSortForName(raw: String?): TaskWidgetSort =
    raw?.let { n -> TaskWidgetSort.entries.firstOrNull { it.name == n } }
        ?: TaskWidgetSort.Server

/** Names a widget sort reset (#437): null when absent or known. */
internal fun widgetSortCoercion(raw: String?): String? =
    if (raw == null || TaskWidgetSort.entries.any { it.name == raw }) null
    else "${visibleTaskRaw(raw)}->${TaskWidgetSort.Server.name}"

/** Whether two millis fall on the same device-local calendar day (#435
 * review): a yesterday 09:14 stamp badged as "updated 09:14" reads as
 * today, understating the cache age by a day. Pure java.util for JVM
 * pinning; the widget renders device-local, so the comparison is too. */
internal fun isSameDay(aMillis: Long, bMillis: Long): Boolean {
    val a = java.util.Calendar.getInstance().apply { timeInMillis = aMillis }
    val b = java.util.Calendar.getInstance().apply { timeInMillis = bMillis }
    return a.get(java.util.Calendar.YEAR) == b.get(java.util.Calendar.YEAR) &&
        a.get(java.util.Calendar.DAY_OF_YEAR) == b.get(java.util.Calendar.DAY_OF_YEAR)
}

/** Stale-widget badge (#435): rows rendered from cache after a failed
 * fetch. [fetchedAt] is null when the fetch time is unknown — the badge
 * then reads "refresh failed" with no time, never a clean count. */
internal data class StaleBadge(val fetchedAt: Long?)

/** Badge behind a widget render: non-null when the live fetch failed and
 * cached rows stand in, null when the rows on screen are live (#435).
 * Top-level and pure so a JVM test can pin the rule without Android —
 * the "updated …" suffix itself needs a Context and stays in
 * [TaskWidget]'s count line. A null stamp still badges (#435 review):
 * [cachedViews] and [cachedAt] are separate Volatile writes, so a reader
 * can observe a cached entry with a missing stamp, and that must never
 * render as live data. */
internal fun staleBadgeFor(
    fresh: List<ServerTask>?,
    cached: List<ServerTask>?,
    fetchedAt: Long?,
): StaleBadge? = if (fresh == null && cached != null) StaleBadge(fetchedAt) else null

/**
 * Ids of *this* provider's placements.
 *
 * [AppWidgetManager.getAppWidgetIds] is the documented way to ask, but on
 * the affected launcher it handed back another provider's id too — which is
 * why the voice widget showed up in the task widget's settings as a second
 * "task" placement. Every id is therefore cross-checked against its own
 * [android.appwidget.AppWidgetProviderInfo], the authority on who owns it;
 * anything else is dropped rather than shown and configured as a task
 * widget.
 */
fun taskWidgetIds(context: Context): IntArray {
    val mgr = AppWidgetManager.getInstance(context)
    val me = ComponentName(context, TaskWidget::class.java)
    return mgr.getAppWidgetIds(me).filter { id ->
        mgr.getAppWidgetInfo(id)?.provider == me
    }.toIntArray()
}


/**
 * Home-screen task manager widget (#101): header (title + count +
 * refresh) above a scrollable or plain-rows task list. View switching and
 * display settings live in [TaskWidgetConfigActivity], reachable from
 * Settings → Home-screen widget — the widget's own controls never rendered
 * on some launchers (#137). Tap contract (#370): the ring completes inline,
 * a row opens its detail sheet, and everything else (background, header,
 * empty line) opens the Task Manager. Row taps deep-link into the task's
 * detail sheet; the ring completes and the skip control skips inline. On
 * the legacy service-backed collection (API <31) both ride the translucent
 * [TaskCompleteActivity] trampoline, because that list has one
 * PendingIntent template; placements built from explicit intents (direct
 * collection items on API 31+, and the plain-rows layout at any API) send
 * them as broadcasts to [TaskNotifActionReceiver] and never open an
 * activity (#532). Reads the same shared
 * `tasks` collection as TaskManagerScreen via TasksApi; the app pushes
 * fresh data after every task mutation via [refresh]. No periodic
 * updates (updatePeriodMillis=0) — data pulls on add/refresh-tap/
 * view-change/mutation only, so a sleeping server costs nothing.
 *
 * Dynamic sizing: default 3x2 footprint, resizable down to two cells
 * wide (110dp) and one row tall (40dp) — below 180dp x 110dp on either
 * axis the small layout takes over. The list is a
 * RemoteViews collection ([TaskWidgetService]) with weight 1, so it
 * takes whatever height the placement offers. Every display setting is
 * per-widget via [TaskWidgetConfigActivity] — read `render()`'s per-key
 * prefs lookups for the current set rather than trusting a list here,
 * which is exactly how this KDoc drifted before.
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

    /**
     * Resize re-render (#532): the layout depends on the placement's size,
     * so a resize must swap layouts immediately rather than waiting for the
     * next data change. Draws from the in-memory snapshot, and only fetches
     * when there is no snapshot to draw — a resize drag fires this many
     * times per second, and a resize never changes which tasks exist.
     */
    override fun onAppWidgetOptionsChanged(
        context: Context,
        appWidgetManager: AppWidgetManager,
        appWidgetId: Int,
        newOptions: Bundle,
    ) {
        if (!renderCached(context, appWidgetManager, appWidgetId)) {
            pull(context, intArrayOf(appWidgetId))
        }
    }

    /** Draws one placement from the snapshot, without a network fetch.
     * Returns whether a snapshot existed — false means the caller should
     * fetch, so a resize after process death is not stuck on "Loading…". */
    private fun renderCached(
        context: Context,
        mgr: AppWidgetManager,
        appWidgetId: Int,
    ): Boolean {
        var hasSnapshot = false
        runCatching {
            val view = viewFor(context, appWidgetId)
            val state = view.state
            val cached = cachedViews[state]?.sortedForWidget(sortFor(context, appWidgetId))
            hasSnapshot = cached != null
            // Same honesty rule as the pushed path (#435): a snapshot drawn
            // after a failed fetch is stale and must say so. A successful
            // last fetch means these rows are live, so no badge.
            val failed = lastErrors.containsKey(state)
            val stale = if (failed) staleBadgeFor(null, cached, cachedAt[state]) else null
            mgr.updateAppWidget(
                appWidgetId,
                render(
                    context, appWidgetId, view, cached,
                    error = cached == null && failed, staleBadge = stale,
                ),
            )
        }.onFailure {
            Log.w("TaskWidget", "resize render failed for id=$appWidgetId", it)
        }
        return hasSnapshot
    }

    override fun onReceive(context: Context, intent: Intent) {
        super.onReceive(context, intent)
        // The header shortcuts are gone from fresh renders (#370): the
        // title opened the config screen and the view label cycled the
        // view, but the tap contract says header opens the Task Manager,
        // and config lives in Settings → Home-screen widget. ACTION_VIEW
        // is still handled so a PendingIntent cached by an older build
        // still cycles instead of erroring.
        if (intent.action == ACTION_REFRESH) {
            val ids = taskWidgetIds(context)
            if (ids.isNotEmpty()) pull(context, ids)
        } else if (intent.action == ACTION_VIEW) {
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

    override fun onDeleted(context: Context, appWidgetIds: IntArray) {
        // Drop this placement's per-widget prefs so removed widgets don't
        // leak keys forever. Every key the widget writes is removed below —
        // a new display key needs FOUR matching edits, not a list trusted
        // to stay current (#388): the write in TaskWidgetConfigActivity,
        // the read in render(), this removal loop, and the diagnostics
        // prune below it.
        prefs(context).edit {
            for (id in appWidgetIds) {
                remove("task_widget_view_$id")
                remove("task_widget_density_$id")
                remove("task_widget_sort_$id")
                remove("task_widget_due_$id")
                remove("task_widget_scroll_$id")
                remove("task_widget_title_$id")
                // Retired diagnostic ladder: drop its key on delete too.
                remove("task_widget_style_$id")
            }
        }
        // Diagnostics must not outlive the placement (#480 review): the
        // clean-read clear never fires for a deleted id, so prune its
        // entries (and its log-dedupe entries, so a reused id re-logs a
        // real corruption) here.
        for (id in appWidgetIds) {
            pruneCoercionsFor(id)
        }
        // Task-notes lifecycle mirrors the pref prune above (#480 review).
        // Factory entries are keyed per deleted placement
        // (`widget-factory-<id>`), so they die with it unconditionally —
        // the namespace names the deleted id and cannot touch a live
        // placement's detail, even when the lookup below fails. The push
        // path refetches all three states every cycle while any placement
        // lives, so `widget:*` stays fresh on its own and is dropped only
        // with the last placement — counted through the provider-filtered
        // [taskWidgetIds], since the raw lookup can hand back another
        // provider's id on the affected launcher and a foreign id must not
        // pin this source's entries; a failed lookup keeps it, so a
        // transient failure cannot delete live detail.
        for (id in appWidgetIds) {
            TasksApi.clearCoercedNotesSource("widget-factory-$id")
        }
        val remaining = runCatching { taskWidgetIds(context) }.getOrNull()
        if (remaining != null && remaining.isEmpty()) {
            TasksApi.clearCoercedNotesSource("widget")
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

        /** Widget view-toggle broadcast (handled in onReceive). */
        const val ACTION_VIEW = "com.vishnu.agento.action.TASKS_VIEW"

        /** Deep-link: open the detail sheet for this task id. Read by
         * [TaskCompleteActivity] (widget rows) and MainActivity (its own
         * TASKS intents). Reminder notifications complete/skip through
         * [TaskNotifActionReceiver] instead (#368) — no activity at all. */
        const val EXTRA_TASK_ID = "com.vishnu.agento.extra.TASK_ID"

        /** Immediate complete request: the widget ring's fill-in on the
         * legacy API <31 scrollable collection (handled by
         * [TaskCompleteActivity]) and its broadcast everywhere a row owns
         * its intents — direct items on 31+ and the plain-rows layout at
         * any API (handled by [TaskNotifActionReceiver]) — the same extra
         * key the reminder notifications use (#368, #532). */
        const val EXTRA_COMPLETE_ID = "com.vishnu.agento.extra.COMPLETE_ID"

        // One app-scoped worker: per-call CoroutineScope(Dispatchers.IO)
        // leaks a scope per update (review #109).
        private val widgetScope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

        // Distinct PendingIntent codes (review #109): actions already keep
        // the intents apart, but codes make that explicit. VIEW_CODE and
        // CONFIG_CODE are reserved for the removed header toggle/settings
        // taps: gaps in the numbering are deliberate, so a PendingIntent
        // already cached by an older build still matches its own code.
        // Unused by construction — the suppression says why, so a lint
        // cleanup never "reclaims" them and collides with a cached PI.
        private const val OPEN_CODE = 1001
        private const val REFRESH_CODE = 1002
        @Suppress("unused")
        private const val VIEW_CODE = 1003
        private const val ROW_CODE = 1004
        @Suppress("unused")
        private const val CONFIG_CODE = 1005

        /** Rows rendered by the non-scrolling (plain-rows) path. Shown in
         * the widget settings preview so the cap isn't a surprise. */
        internal const val STATIC_ROW_LIMIT = 8

        /** Rows handed to the direct-items collection (API 31+): the whole
         * list rides one parcel, so an unbounded "All" view would
         * TransactionTooLarge the update. Counts ROWS and drops a trailing
         * header, the same rule as the plain-rows path ([takeAgendaRows]).
         * Headroom, not a live limit: the widget fetches 50 rows per state
         * (at most ~62 items with headers), so this only fires if that
         * fetch limit ever grows. The header counts still show the true
         * per-bucket totals. */
        private const val MAX_COLLECTION_ROWS = 100

        // Row tap codes; per-task uniqueness comes from the data URI.
        private const val ROW_COMPLETE_CODE = 1006
        private const val ROW_OPEN_CODE = 1007
        private const val ROW_SKIP_CODE = 1008

        // Snapshots per server state, read by TaskWidgetService's factory
        // (same process). Volatile: written on IO, read on the RemoteViews
        // service thread. Views are fetched together so mixed placements
        // (one Open, one Done) each have data.
        @Volatile var cachedViews: Map<String, List<ServerTask>> = emptyMap()
            private set

        // When each cached state was last fetched successfully (#435):
        // in-memory like the cache it describes. A stale render (fetch
        // failed, cache used) names this time so yesterday's rows can never
        // pass for live data.
        @Volatile private var cachedAt: Map<String, Long> = emptyMap()
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

        /** "seen v3, app speaks v1" for diagnostics: which task contracts
         * this install has actually spoken (#185). 0 seen = no task response
         * parsed yet (or only pre-version servers), not a version. */
        private fun taskContractLine(appCtx: Context): String {
            val seen = appCtx.applicationContext
                .getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
                .getInt(CONTRACT_VERSION_SEEN_KEY, 0)
            return "seen v$seen, app speaks v$TASK_CONTRACT_VERSION"
        }

        /** One-screen widget health summary for Settings → About, so
         * widget failures can be diagnosed without adb. Never throws
         * (a diagnostics call must not become a second crash). */
        fun diagnostics(appCtx: Context): String = runCatching {
            val ctx = appCtx.applicationContext
            val ids = runCatching {
                taskWidgetIds(ctx)
            }.getOrDefault(intArrayOf())
            buildString {
                appendLine("placements=${ids.size}")
                for (id in ids) {
                    appendLine("id=$id view=${viewFor(ctx, id)} " +
                        "title=${titleFor(ctx, id, "(default)")} " +
                        "density=${densityFor(ctx, id)} sort=${sortFor(ctx, id)} " +
                        "showDue=${showDueFor(ctx, id)} " +
                        "scrollable=${scrollableFor(ctx, id)}")
                }
                // Resets that notified (#437): the latest raw->became per
                // pref key, so a placement that changed for no apparent
                // reason traces to its cause here even after log rotation.
                val coerced = lastCoercions
                if (coerced.isNotEmpty()) {
                    appendLine("coerced=" + coerced.toSortedMap()
                        .map { (k, v) -> "$k $v" }.joinToString("; ").take(500))
                }
                // Task rows that parsed only by resetting a mistyped number
                // (#437): the latest notes per fetched state, so each
                // screen's hint points at its own rows' detail (#480).
                val taskCoerced = TasksApi.lastCoercedNotes
                if (taskCoerced.isNotEmpty()) {
                    appendLine("taskCoerced=" + taskCoerced.toSortedMap()
                        .map { (s, ns) -> "$s: " + ns.joinToString("; ") }
                        .joinToString(" | ").take(500))
                }
                appendLine("cached=${cachedViews.mapValues { it.value.size }}")
                appendLine("errors=${lastErrors.mapValues { it.value.take(300) }}")
                appendLine("factoryError=${lastFactoryError ?: "none"}")
                // What the reminder budget had to drop, if anything (#165).
                // Without this a task whose alarm was never armed looks
                // exactly like a task whose alarm never worked.
                appendLine(
                    "reminders=" + (
                        TaskReminders.budget(ctx)?.line() ?: "no refresh yet"))
                // Highest task contract this install has spoken, and the one
                // it speaks (#185). A value nothing reads is not
                // diagnosability, it is a write — so it lives here, in the
                // report a user can copy out of Settings → About.
                appendLine("taskContract=" + taskContractLine(ctx))
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
                "--- log (system, recent) ---\n$all\n" +
                "--- crash logs (5-day TTL) ---\n${CrashLogs.reportSection(appCtx)}"
                ).take(200_000)
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

        /** Lines worth keeping: our own widget code, our own API parse
         * diagnostics (drop/shape warnings from task/sync log under
         * their own tags, #430), or a host-side widget failure
         * signature. Deliberately specific — a generic "widget" match
         * would drag in every other app's widget lines from the
         * system-wide buffer. */
        internal fun widgetLine(l: String): Boolean =
            l.contains("TaskWidget") || l.contains("AndroidRuntime") ||
                l.contains("FATAL") || l.contains("RemoteViews") ||
                l.contains("AppWidget") || l.contains("System.err") ||
                l.contains("agento") || l.contains("com.vishnu") ||
                l.contains("RemoteCollection") ||
                l.contains("TasksApi") || l.contains("TaskSyncWorker") ||
                l.contains("SyncRunner") ||
                // Inline complete/skip failures (#532) log under TaskNotif;
                // the claim "logged to diagnostics" is otherwise false.
                l.contains("TaskNotif")

        // Bearer secrets must never ride along when the user pastes the
        // log elsewhere. Redacts key=value pairs for the usual secret
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

        /** This placement's view, defaulting to Open. A stored name
         * nothing knows resets AND logs once (#437): what key held what
         * raw value and what it became. The line surfaces in Settings →
         * About diagnostics, so corruption reads as corruption instead of
         * a placement changing for no apparent reason. */
        fun viewFor(context: Context, appWidgetId: Int): TaskWidgetView {
            val key = "task_widget_view_$appWidgetId"
            val name = prefs(context).getString(key, null)
            noteCoercion(key, widgetViewCoercion(name), name)
            return widgetViewForName(name)
        }

        /** This placement's density, defaulting to Comfortable. Resets
         * report like [viewFor] (#437). */
        fun densityFor(context: Context, appWidgetId: Int): TaskWidgetDensity {
            val key = "task_widget_density_$appWidgetId"
            val name = prefs(context).getString(key, null)
            noteCoercion(key, widgetDensityCoercion(name), name)
            return widgetDensityForName(name)
        }

        /** This placement's row order, defaulting to Server (fetch order).
         * Resets report like [viewFor] (#437). */
        fun sortFor(context: Context, appWidgetId: Int): TaskWidgetSort {
            val key = "task_widget_sort_$appWidgetId"
            val name = prefs(context).getString(key, null)
            noteCoercion(key, widgetSortCoercion(name), name)
            return widgetSortForName(name)
        }

        /** This placement's custom header title (#532), or [fallback] when
         * it has none. No coercion path: a title is free text, so it is
         * trimmed and capped on read instead of resetting. */
        fun titleFor(context: Context, appWidgetId: Int, fallback: String): String =
            widgetTitle(
                prefs(context).getString("task_widget_title_$appWidgetId", null),
                fallback,
            )

        // Resets that already notified (#437): one log line per distinct
        // key+raw per process, so every render does not re-log the same
        // corruption. Reads and writes under [coercionLock]; the latest
        // reset per key also lands in [lastCoercions] for diagnostics().
        // Internal for the JVM pin below, like the pure coercion helpers.
        internal val coercionSeen = mutableSetOf<String>()
        internal val coercionLock = Any()
        @Volatile internal var lastCoercions: Map<String, String> = emptyMap()

        internal fun noteCoercion(key: String, short: String?, full: String? = short) {
            // A clean read clears the key AND re-arms the log dedupe: a
            // fixed pref stops advertising its old reset, and a later
            // re-corruption notifies again — a fix is a vintage boundary
            // like a delete, not log spam protection (#480 review).
            // Dedupe runs on the FULL raw, display on the capped short:
            // two distinct stored values sharing a 40-char prefix are two
            // vintages, not one (#480 review).
            if (short == null) {
                synchronized(coercionLock) {
                    lastCoercions = lastCoercions - key
                    coercionSeen.removeIf { seen -> seen.startsWith("$key|") }
                }
                return
            }
            val token = "$key|${full ?: short}"
            val fresh = synchronized(coercionLock) {
                lastCoercions = lastCoercions + (key to short)
                coercionSeen.add(token)
            }
            if (fresh) runCatching { Log.w("TaskWidget", "coerced $key $short") }
        }

        /**
         * Drops every diagnostics + log-dedupe entry for one placement's
         * display keys (#480 review): deletion is the ultimate fix, and
         * the clean-read clear never fires for a gone id — without this
         * the About screen would name resets for widgets that no longer
         * exist. Clearing the seen-set too, so a later placement reusing
         * the id re-logs a real corruption.
         */
        internal fun pruneCoercionsFor(appWidgetId: Int) {
            synchronized(coercionLock) {
                val keys = setOf(
                    "task_widget_view_$appWidgetId",
                    "task_widget_density_$appWidgetId",
                    "task_widget_sort_$appWidgetId",
                )
                lastCoercions = lastCoercions.filterKeys { it !in keys }
                coercionSeen.removeIf { seen -> keys.any { seen.startsWith("$it|") } }
            }
        }

        /** Whether this placement's rows show the due line. */
        fun showDueFor(context: Context, appWidgetId: Int): Boolean =
            prefs(context).getBoolean("task_widget_due_$appWidgetId", true)

        /** Whether this placement uses the scrollable collection. Plain
         * rows are the fallback for hosts that reject the list; a fresh
         * placement gets the scrollable list, which is the intended UX. */
        fun scrollableFor(context: Context, appWidgetId: Int): Boolean =
            prefs(context).getBoolean("task_widget_scroll_$appWidgetId", true)

        /** One row with explicit per-row intents instead of the collection
         * template: all immutable, so hosts that balk at the mutable
         * template (or at fill-in merging) still complete/open rows
         * (issue #137). Same layout and content as [buildRow]; complete and
         * skip are broadcasts so the tap never foregrounds the app (#532). */
        fun buildRowWithIntents(
            context: Context,
            compact: Boolean,
            showDue: Boolean,
            today: java.time.LocalDate,
            task: ServerTask,
        ): RemoteViews {
            // PendingIntent identity is (requestCode + data URI), and the
            // data URIs below are unique per task AND per action, so rows
            // can never share a PendingIntent — the codes only have to be
            // distinct constants, not a per-row hash (which can collide).
            val complete = PendingIntent.getBroadcast(
                context, ROW_COMPLETE_CODE,
                completeIntent(context, task.id),
                PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
            )
            val skip = PendingIntent.getBroadcast(
                context, ROW_SKIP_CODE,
                skipIntent(context, task.id),
                PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
            )
            val openTask = PendingIntent.getActivity(
                context, ROW_OPEN_CODE,
                Intent(context, MainActivity::class.java)
                    .setAction(ACTION_TASKS)
                    .putExtra(EXTRA_TASK_ID, task.id)
                    .setData("agento://task/${task.id}".toUri()),
                PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
            )
            return buildRow(context.packageName, compact, showDue, today, task).apply {
                setOnClickPendingIntent(R.id.task_widget_row_check, complete)
                setOnClickPendingIntent(R.id.task_widget_row_skip, skip)
                setOnClickPendingIntent(R.id.task_widget_row, openTask)
            }
        }

        /**
         * The broadcast behind a widget row's ring (#532): the reminder
         * notification's own complete action, so a widget tap and a
         * notification tap are one code path (and one failure path).
         * Explicit component (the receiver is non-exported — an implicit
         * broadcast would never reach it) and a per-task data URI, so two
         * rows never share a PendingIntent. IMMUTABLE at the call site:
         * nothing fills this in.
         */
        internal fun completeIntent(context: Context, taskId: String): Intent =
            mutationIntent(context, taskId, NotifAction.COMPLETE)

        /** The skip broadcast (#532); same shape, the skip action. */
        internal fun skipIntent(context: Context, taskId: String): Intent =
            mutationIntent(context, taskId, NotifAction.SKIP)

        private fun mutationIntent(
            context: Context,
            taskId: String,
            target: NotifAction,
        ): Intent = Intent(context, TaskNotifActionReceiver::class.java)
            .setAction(widgetMutationAction(target))
            .putExtra(target.extraKey, taskId)
            .setData("agento://task/$taskId/${target.name.lowercase()}".toUri())

        /** One agenda section header (#532), a collection item of its own.
         * [open] is the Task Manager intent the direct/plain paths pin
         * explicitly; the legacy factory passes null and lets its fill-in
         * template handle the tap (see TaskWidgetService). Internal because
         * [DueBucket] is, and a public signature cannot expose it. */
        internal fun buildSection(
            context: Context,
            bucket: DueBucket,
            count: Int,
            open: PendingIntent? = null,
        ): RemoteViews =
            RemoteViews(context.packageName, R.layout.task_widget_section).apply {
                setTextViewText(R.id.task_widget_section, "${bucket.title} · $count")
                if (open != null) {
                    setOnClickPendingIntent(R.id.task_widget_section, open)
                }
                setTextColor(
                    R.id.task_widget_section,
                    ContextCompat.getColor(
                        context,
                        if (bucket == DueBucket.Overdue) {
                            R.color.task_widget_overdue
                        } else {
                            R.color.task_widget_text_primary
                        },
                    ),
                )
            }

        /** Builds one collection row: text only, no taps. Callers attach
         * them — [buildRowWithIntents] wherever a row owns its intents
         * (the direct-items path on 31+ AND the plain-rows layout at any
         * API: explicit broadcasts) and [buildLegacyRow] on the API <31
         * scrollable template (fill-ins). Keeping the two apart means a row
         * can never carry both an explicit intent and a fill-in for the
         * same view, where which one wins is an implementation detail of
         * the host. */
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
                // Same line as the Task Manager rows (#130, #532): due →
                // repeat → parallel, IST-pinned, shared helper.
                val due = widgetSecondaryLine(task, today, showDue)
                if (due.isNotEmpty()) {
                    setTextViewText(R.id.task_widget_row_due, due)
                    setViewVisibility(R.id.task_widget_row_due, View.VISIBLE)
                } else {
                    setViewVisibility(R.id.task_widget_row_due, View.GONE)
                }
            }
        }

        /**
         * One row for the legacy collection (API <31), whose ListView has a
         * single PendingIntent template — the [TaskCompleteActivity]
         * trampoline, because the row body must open an activity and a
         * template cannot be both. The fill-ins therefore only carry which
         * id and which verb; the trampoline decides (#532).
         */
        fun buildLegacyRow(
            packageName: String,
            compact: Boolean,
            showDue: Boolean,
            today: java.time.LocalDate,
            task: ServerTask,
        ): RemoteViews = buildRow(packageName, compact, showDue, today, task).apply {
            setOnClickFillInIntent(
                R.id.task_widget_row_check,
                Intent().putExtra(TaskWidget.EXTRA_COMPLETE_ID, task.id),
            )
            setOnClickFillInIntent(
                R.id.task_widget_row_skip,
                Intent().putExtra(TaskNotifActionReceiver.EXTRA_SKIP_ID, task.id),
            )
            setOnClickFillInIntent(
                R.id.task_widget_row,
                Intent().putExtra(TaskWidget.EXTRA_TASK_ID, task.id),
            )
        }

        /** Advance Open → Done → All → Open for one placement. */
        private fun cycleView(context: Context, appWidgetId: Int) {
            val next = when (viewFor(context, appWidgetId)) {
                TaskWidgetView.Open -> TaskWidgetView.Done
                TaskWidgetView.Done -> TaskWidgetView.All
                TaskWidgetView.All -> TaskWidgetView.Open
            }
            prefs(context).edit {
                putString("task_widget_view_$appWidgetId", next.name)
            }
        }

        /** Re-pull server tasks and push to every installed widget. Call
         * after task mutations so the home screen never goes stale.
         * Returns a Deferred so callers that must outlive a broadcast
         * (BootReceiver) can wait on it — and so [TaskSyncWorker] can read
         * how the run ended, one run at a time rather than out of a shared
         * field. */
        fun refresh(context: Context): Deferred<RefreshOutcome> {
            val appCtx = context.applicationContext
            // async rather than launch: the outcome travels back as a value
            // the caller can await, which a Job has nowhere to put.
            // The Deferred itself never fails: an unexpected throw becomes
            // Failed, so the fire-and-forget callers (pokeWidget, the config
            // activity, the timezone path) can keep dropping the result
            // without leaving an unobserved exception behind. Cancellation
            // still propagates — a cancelled run is not a failed fetch.
            return widgetScope.async {
                try {
                    fetchAndPush(
                        appCtx,
                        taskWidgetIds(appCtx),
                    )
                } catch (e: CancellationException) {
                    throw e
                } catch (e: Exception) {
                    Log.w("TaskWidget", "refresh failed", e)
                    RefreshOutcome.Failed
                }
            }
        }

        /**
         * Single fetch-and-push path shared by pull() and refresh().
         *
         * Returns how it ended rather than leaving that in a field: the
         * scope is shared, so a refresh started by an in-app mutation and
         * one started by [TaskSyncWorker] can be in flight together, and a
         * shared value hands each caller the other one's result.
         *
         * All three states are fetched (in parallel) so every placement's
         * view has data regardless of which views are installed.
         */
        private suspend fun fetchAndPush(
            appCtx: Context,
            ids: IntArray,
        ): RefreshOutcome {
            if (ids.isEmpty()) return RefreshOutcome.NoPlacements
            val api = TasksApi(appCtx)
            val views = mutableMapOf<String, List<ServerTask>>()
            val errors = mutableMapOf<String, String>()
            coroutineScope {
                TaskWidgetView.entries.map { v ->
                    // Limit 50 per state, and the truncation flag is
                    // deliberately unread: a placement shows a screenful, so
                    // 50 rows is the whole visible universe. Do not "fix"
                    // this into an unbounded fetch — the collection behind
                    // it is (#168).
                    async { v.state to api.list(v.state, limit = 50, source = "widget") }
                }.awaitAll().forEach { (state, res) ->
                    res.getOrNull()?.tasks?.let { views[state] = it }
                        ?: run {
                            errors[state] = res.exceptionOrNull()?.message
                                ?: "unknown error"
                        }
                }
            }
            if (views.isNotEmpty()) {
                val now = System.currentTimeMillis()
                cachedViews = cachedViews + views
                cachedAt = cachedAt + views.keys.associateWith { now }
            }
            // Drop errors for states that just succeeded so a stale
            // message can never outlive its failure (#130 review).
            lastErrors = (lastErrors + errors) - views.keys
            // Every state failed means nothing was delivered and the widget
            // is now showing whatever it had cached. A partial failure is
            // Ok: the states that worked are pushed, and the ones that did
            // not render their own error, which is the existing behaviour.
            val mgr = AppWidgetManager.getInstance(appCtx)
            // Only the service-backed collection listens for a data change;
            // direct items and static rows are re-rendered whole.
            var needNotify = false
            val notifyIds = mutableListOf<Int>()
            for (id in ids) {
                val view = viewFor(appCtx, id)
                // Read once: the push below and the error branch both need
                // the same order, and a second prefs read could straddle a
                // settings write.
                val sort = sortFor(appCtx, id)
                val fresh = views[view.state]
                // Cache used because THIS state's fetch failed (#435): a
                // failed fetch must never render as live data. Render the
                // cached rows but badge them with when they were fetched.
                val staleBadge = staleBadgeFor(fresh, cachedViews[view.state], cachedAt[view.state])
                val tasks = fresh?.sortedForWidget(sort)
                    ?: cachedViews[view.state]?.sortedForWidget(sort)
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
                        id, render(appCtx, id, view, tasks, tasks == null, detail, staleBadge))
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
            return if (views.isEmpty()) RefreshOutcome.Failed else RefreshOutcome.Ok
        }

        /** Full widget view: header reflects this placement's view and the
         * latest fetch (null = not loaded yet, error flag = fetch failed);
         * the list binds to [TaskWidgetService] and fills whatever height
         * the placement offers. Row taps and the ring button go through
         * the [TaskCompleteActivity] trampoline with per-row fill-ins on
         * API <31; the direct-items path (31+) and the plain-rows path (any
         * API) use explicit broadcast complete/skip (#532). A placement
         * below either bound — narrower than [COMPACT_MAX_WIDTH_DP] or
         * shorter than [COMPACT_MIN_HEIGHT_DP] — gets the compact layout
         * instead. */
        private fun render(
            context: Context,
            appWidgetId: Int,
            view: TaskWidgetView,
            tasks: List<ServerTask>?,
            error: Boolean,
            errorDetail: String? = null,
            /** Stale-cache badge, null when the rows are live (#435). */
            staleBadge: StaleBadge? = null,
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
            // Compact placements (#532): title + count + the next task. No
            // collection: below either full-size bound there is no room
            // for a scrolling list with a header over it, and an empty
            // list would just be a header over nothing.
            val options = AppWidgetManager.getInstance(context)
                .getAppWidgetOptions(appWidgetId)
            val compactPlacement = widgetIsCompact(
                options.getInt(AppWidgetManager.OPTION_APPWIDGET_MIN_WIDTH, 0),
                options.getInt(AppWidgetManager.OPTION_APPWIDGET_MIN_HEIGHT, 0),
            )
            if (compactPlacement) {
                // "Next" is the first row of the same agenda the full layout
                // builds (#532), not the first task in the placement's own
                // sort: a Name- or Estimate-sorted placement must not name a
                // different task from its full-size twin. Byte-identical
                // grouping, so the same clock and buckets.
                val today = java.time.LocalDate.now(IST)
                val next = widgetNextTask(
                    tasks.orEmpty(),
                    grouped = view != TaskWidgetView.Done,
                    today = today,
                    now = java.time.LocalDateTime.now(IST)
                        .truncatedTo(java.time.temporal.ChronoUnit.MINUTES),
                )
                return RemoteViews(context.packageName, R.layout.task_widget_small).apply {
                    applyTapTargets(this, openPending, refreshPending, SMALL_TAP_IDS)
                    setTextViewText(
                        R.id.task_widget_title,
                        // Custom title wins; the view name is the default,
                        // same rule as the full layout.
                        titleFor(context, appWidgetId, view.title),
                    )
                    setTextViewText(
                        R.id.task_widget_count,
                        // One named task, however many exist — the same
                        // "showing N" honesty as the static path (#532).
                        countText(context, view, tasks, error,
                            shownRows = 1, staleBadge = staleBadge),
                    )
                    if (next != null) {
                        setTextViewText(R.id.task_widget_next, next.name)
                        val line = widgetSecondaryLine(
                            next, today, showDueFor(context, appWidgetId))
                        setTextViewText(R.id.task_widget_next_due, line)
                        setViewVisibility(
                            R.id.task_widget_next_due,
                            if (line.isEmpty()) View.GONE else View.VISIBLE,
                        )
                    } else {
                        setViewVisibility(R.id.task_widget_next, View.GONE)
                        setViewVisibility(R.id.task_widget_next_due, View.GONE)
                    }
                }
            }
            // Header tap contract (#370): the ring completes inline, a row
            // opens its detail sheet, and EVERYTHING else — background,
            // title, count, view label, empty line — opens the Task Manager.
            // Handlers are explicit on every id, not inherited from the
            // body: some hosts don't propagate a parent tap to children
            // without their own handler, which left the scrollable title
            // dead. The refresh button keeps its own job (it is a control,
            // not background). The view-cycle and config shortcuts are
            // gone from fresh renders — ACTION_VIEW is still handled so a
            // PendingIntent cached by an older build keeps working — and
            // their codes stay reserved, never reused. The table itself
            // lives in [widgetTapTarget], which render() consults through
            // [applyTapTargets] — one source, pinned by WidgetTapTest.
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
                data = "agento://widget/$appWidgetId".toUri()
            }
            // Two shapes, both proven on the affected launcher (#137):
            // the scrollable list (default) and the plain-row layout.
            val scrollable = scrollableFor(context, appWidgetId)
            if (!scrollable) {
                return RemoteViews(context.packageName, R.layout.task_widget_full).apply {
                    // The view label exists only here (the scrollable
                    // layout names the view in its title instead).
                    applyTapTargets(this, openPending, refreshPending, PLAIN_TAP_IDS)
                    if (tasks != null) {
                        val today = java.time.LocalDate.now(IST)
                        val compact = densityFor(context, appWidgetId) == TaskWidgetDensity.Compact
                        val due = showDueFor(context, appWidgetId)
                        // Same agenda order as the collection paths (#532).
                        val agenda = widgetAgenda(
                            tasks,
                            grouped = view != TaskWidgetView.Done,
                            today = today,
                            now = java.time.LocalDateTime.now(IST)
                                .truncatedTo(java.time.temporal.ChronoUnit.MINUTES),
                        )
                        // Clear first: re-rendering into a fresh RemoteViews
                        // each time would append, leaving ghost rows when
                        // the list shrinks.
                        removeAllViews(R.id.task_widget_static_list)
                        agenda.takeAgendaRows(STATIC_ROW_LIMIT).forEach { item ->
                            addView(
                                R.id.task_widget_static_list,
                                when (item) {
                                    is WidgetItem.Header ->
                                        buildSection(
                                            context, item.bucket, item.count, openPending)
                                    is WidgetItem.Row ->
                                        buildRowWithIntents(
                                            context, compact, due, today, item.task)
                                },
                            )
                        }
                    }
                    val showList = !tasks.isNullOrEmpty()
                    setViewVisibility(R.id.task_widget_static_list,
                        if (showList) View.VISIBLE else View.GONE)
                    val empty = emptyText(context, tasks, error, errorDetail)
                    setTextViewText(R.id.task_widget_empty, empty)
                    setViewVisibility(R.id.task_widget_empty,
                        if (empty.isEmpty()) View.GONE else View.VISIBLE)
                    setTextViewText(R.id.task_widget_view, view.title)
                    setTextViewText(R.id.task_widget_title,
                        titleFor(context, appWidgetId, view.title))
                    setTextViewText(R.id.task_widget_count,
                        countText(
                            context, view, tasks, error,
                            shownRows = STATIC_ROW_LIMIT, staleBadge = staleBadge))
                }
            }
            // E: the collection widget, on the same proven chrome.
            return RemoteViews(context.packageName, R.layout.task_widget_list).apply {
                applyTapTargets(this, openPending, refreshPending, LIST_TAP_IDS)
                if (tasks != null && Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
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
                    // Agenda order (#532): overdue → today → upcoming →
                    // someday, with a header item per non-empty bucket. The
                    // Done view stays flat — a bucket for finished work
                    // says nothing. Two layouts means a view-type count of
                    // 2; the platform derives an item's type from its
                    // RemoteViews layout id.
                    val agenda = widgetAgenda(
                        tasks,
                        grouped = view != TaskWidgetView.Done,
                        today = today,
                        now = java.time.LocalDateTime.now(IST)
                            .truncatedTo(java.time.temporal.ChronoUnit.MINUTES),
                    )
                    val items = RemoteViews.RemoteCollectionItems.Builder().apply {
                        agenda.takeAgendaRows(MAX_COLLECTION_ROWS).forEachIndexed { i, item ->
                            addItem(
                                i.toLong(),
                                when (item) {
                                    is WidgetItem.Header ->
                                        buildSection(
                                            context, item.bucket, item.count, openPending)
                                    is WidgetItem.Row ->
                                        buildRowWithIntents(
                                            context, compact, due, today, item.task)
                                },
                            )
                        }
                        setHasStableIds(false)
                        setViewTypeCount(2)
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
                // The header title doubles as the view name (the toggle
                // button is gone — it never rendered on this host), and a
                // placement's custom title (#532) wins over it.
                setTextViewText(R.id.task_widget_title,
                    titleFor(context, appWidgetId, view.title))
                setTextViewText(R.id.task_widget_count,
                    // Same honesty as the other capped surfaces (#532):
                    // dead at today's fetch limit of 50, live the day it
                    // grows past MAX_COLLECTION_ROWS.
                    countText(context, view, tasks, error,
                        shownRows = MAX_COLLECTION_ROWS, staleBadge = staleBadge))
                val empty = emptyText(context, tasks, error, errorDetail)
                setTextViewText(R.id.task_widget_empty, empty)
                // Hide the empty line when rows are showing: an empty
                // string would still contribute its 8dp top padding.
                setViewVisibility(R.id.task_widget_empty,
                    if (empty.isEmpty()) View.GONE else View.VISIBLE)
            } // end RemoteViews apply
        }

        /** Empty/loading/error line under the list. No "tap to retry"
         * suffix: the empty view opens the Task Manager like the rest of
         * the background (#370), so promising a retry tap would be worse
         * than none. */
        private fun emptyText(
            context: Context,
            tasks: List<ServerTask>?,
            error: Boolean,
            errorDetail: String?,
        ): String = when {
            tasks == null && !error -> context.getString(R.string.task_widget_loading)
            tasks == null -> errorDetail?.take(140)
                ?: context.getString(R.string.task_widget_error)
            tasks.isEmpty() -> context.getString(R.string.task_widget_empty)
            else -> ""
        }

        /** Header count line, shared by the full widget and the diagnostic
         * styles. [shownRows] is the surface's row cap when it has one, so
         * the count and the visible rows never silently disagree: the static
         * path shows up to [STATIC_ROW_LIMIT], the compact placement exactly
         * one task, the scrollable collection up to [MAX_COLLECTION_ROWS].
         * Null means uncapped, so no qualifier is possible. */
        private fun countText(
            context: Context,
            view: TaskWidgetView,
            tasks: List<ServerTask>?,
            error: Boolean,
            shownRows: Int? = null,
            /** Stale-cache badge, null when the rows are live (#435). */
            staleBadge: StaleBadge? = null,
        ): String {
            val plural = when (view) {
                TaskWidgetView.Open -> R.plurals.task_widget_open
                TaskWidgetView.Done -> R.plurals.task_widget_done
                TaskWidgetView.All -> R.plurals.task_widget_all
            }
            val base = when {
                tasks == null && !error -> context.getString(R.string.task_widget_loading)
                tasks == null -> context.getString(R.string.task_widget_error)
                tasks.isEmpty() -> context.getString(R.string.task_widget_empty)
                shownRows != null && tasks.size > shownRows ->
                    context.resources.getQuantityString(plural, tasks.size, tasks.size) +
                        shownSuffix(tasks.size, shownRows)
                else -> context.resources.getQuantityString(plural, tasks.size, tasks.size)
            }
            // Cache used after a failed fetch (#435): say so. Rows rendered
            // without this read as live data, which is fabricated freshness.
            // A missing stamp still badges, timelessly — the two cache maps
            // are separate Volatile writes, so a torn read must never render
            // a clean count. A non-today stamp names the date too (#435
            // review): "updated 09:14" for yesterday's fetch reads as today.
            if (staleBadge == null) return base
            val staleAt = staleBadge.fetchedAt
            if (staleAt == null) {
                return base + " · " + context.getString(R.string.task_widget_stale_unknown)
            }
            val timePart = android.text.format.DateFormat.getTimeFormat(context)
                .format(java.util.Date(staleAt))
            val when0 = if (isSameDay(staleAt, System.currentTimeMillis())) {
                context.getString(R.string.task_widget_stale, timePart)
            } else {
                val datePart = android.text.format.DateFormat.getDateFormat(context)
                    .format(java.util.Date(staleAt))
                context.getString(R.string.task_widget_stale, "$datePart $timePart")
            }
            return "$base · $when0"
        }
    }
}
