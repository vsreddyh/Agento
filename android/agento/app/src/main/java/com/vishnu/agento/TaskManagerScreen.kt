@file:OptIn(ExperimentalMaterial3Api::class)

package com.vishnu.agento

/**
 * Task Manager UI: the list, rows, detail sheet, editor dialog and the
 * date/time pickers (#163, second of four moves).
 *
 * Moved verbatim out of MainActivity.kt; the only changes are visibility
 * (`private` to `internal`, same package, so every caller keeps working)
 * and the import list, which carries exactly what this file uses.
 */
import android.Manifest
import android.app.AlarmManager
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import android.provider.Settings
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.animation.AnimatedVisibility
import androidx.compose.animation.animateColorAsState
import androidx.compose.animation.core.FastOutSlowInEasing
import androidx.compose.animation.core.MutableTransitionState
import androidx.compose.animation.core.animateDpAsState
import androidx.compose.animation.core.tween
import androidx.compose.animation.expandVertically
import androidx.compose.animation.fadeIn
import androidx.compose.animation.fadeOut
import androidx.compose.animation.shrinkVertically
import androidx.compose.foundation.clickable
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.interaction.MutableInteractionSource
import androidx.compose.foundation.interaction.PressInteraction
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.toggleable
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.AccessTime
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.Assignment
import androidx.compose.material.icons.filled.Build
import androidx.compose.material.icons.filled.CheckCircle
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.ContentCopy
import androidx.compose.material.icons.filled.DateRange
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.Description
import androidx.compose.material.icons.filled.Edit
import androidx.compose.material.icons.filled.Groups
import androidx.compose.material.icons.filled.History
import androidx.compose.material.icons.filled.List
import androidx.compose.material.icons.filled.Menu
import androidx.compose.material.icons.filled.Notifications
import androidx.compose.material.icons.filled.PlayArrow
import androidx.compose.material.icons.filled.RadioButtonUnchecked
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material.icons.filled.Repeat
import androidx.compose.material.icons.filled.Schedule
import androidx.compose.material.icons.filled.Search
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material.icons.filled.Sort
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.style.TextDecoration
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.core.content.ContextCompat
import androidx.lifecycle.compose.LifecycleResumeEffect
import java.util.Locale
import kotlinx.coroutines.delay
// REQUIRED, do not remove as an unused import: `collect { }` on a Flow with a lambda is
// the kotlinx.coroutines.flow EXTENSION, not the FlowCollector member. Removing it fails to
// compile. Flagged as redundant in review twice; it is load-bearing.
import kotlinx.coroutines.flow.collect
import kotlinx.coroutines.launch
/** Task Manager: the user's own tasks from the shared `tasks` collection
 * (the same rows the assistant manages over MCP). Full CRUD here; the
 * assistant stays a second writer through chat, same as before. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
internal fun TaskManagerScreen(
    wc: WindowClass,
    onMenu: () -> Unit = {},
    deepLinkId: String? = null,
    onDeepLinkConsumed: () -> Unit = {},
) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    // Non-suspending queued toasts (#288): toast() enqueues and returns,
    // so plain callbacks call it directly with no scope.launch dance.
    val toasts = remember(scope) { ToastCenter(scope) }

    // One client for the screen (its OkHttpClient is shared process-wide).
    val api = remember(context) { TasksApi(context) }
    var tasks by remember { mutableStateOf<List<ServerTask>>(emptyList()) }
    // When the local list was last fetched from the server (epoch millis,
    // persisted — failures leave it untouched). Shown in the count line.
    var lastSyncedAt by remember { mutableLongStateOf(readLastSyncedAt(context)) }
    // Whether the server held back rows beyond its cap: the list below
    // must say so rather than silently ending mid-collection (#168).
    var listTruncated by remember { mutableStateOf(false) }
    // Persisted by enum name (enums have no default Saveable saver);
    // rotation used to reset these while the search query survived.
    var filterName by rememberSaveable { mutableStateOf(ServerTaskFilter.Open.name) }
    val filter = runCatching { ServerTaskFilter.valueOf(filterName) }
        .getOrDefault(ServerTaskFilter.Open)
    var sortName by rememberSaveable { mutableStateOf(ServerTaskSort.Start.name) }
    val sort = runCatching { ServerTaskSort.valueOf(sortName) }
        .getOrDefault(ServerTaskSort.Start)
    var query by rememberSaveable { mutableStateOf("") }
    var sortMenu by remember { mutableStateOf(false) }
    var selected by remember { mutableStateOf<ServerTask?>(null) }
    var loading by remember { mutableStateOf(true) }
    var error by remember { mutableStateOf("") }
    // Bumped after every load/mutation so the loader below reruns.
    var refreshTick by remember { mutableIntStateOf(0) }
    // Set immediately before a `refreshTick++` (#308) to make that one run a
    // BACKGROUND refresh: the fresh rows are swapped in with no full-list
    // spinner, so completing a task — or returning to a stale screen — never
    // blanks the list the user is reading. Consumed and cleared by the
    // loader, so it can only ever describe the run it was set for; the next
    // bump defaults back to a visible reload.
    var silentRefresh by remember { mutableStateOf(false) }
    // True while a background refresh is in flight; drives the thin bar that
    // stands in for the spinner on those runs.
    var refreshing by remember { mutableStateOf(false) }
    // Rows playing their leave animation (#308). A completed row stays in
    // `tasks` until its animation reports it has finished leaving, and a
    // fetch that lands meanwhile carries it over rather than yanking it out
    // from under the animation (see mergeRefresh).
    var exitingIds by remember { mutableStateOf(setOf<String>()) }
    // The task ids the list held the last time it was DRAWN (#308), or null
    // before the first draw. A row animates in only when its id was not among
    // them: that makes the first population, a filter switch, a re-sort and a
    // search all draw in place, while a row that genuinely arrives — a
    // repeat's next occurrence, a task created here or by the agent — still
    // animates in. Null (not the empty set) is what marks "never drawn", so
    // the very first task created into an empty list animates too.
    var knownTaskIds by remember { mutableStateOf<Set<String>?>(null) }
    // True once the loader below has completed at least once (success or
    // failure). The resume check below must not refetch on the very first
    // resume: the initial load is already in flight, and re-bumping the tick
    // would fetch the whole list twice for one screen opening.
    var hasLoaded by remember { mutableStateOf(false) }

    // #185: surface a task-response contract mismatch. Without this collector the
    // warning was recorded and shown to nobody, which is the bug the check exists to
    // fix — the user needs to know the app and the server disagree while the task is
    // still on screen. consume() inside the collector clears the value, so a
    // recomposition does not re-show the same warning — but only if it is still the one
    // that was shown, or a warning that arrived meanwhile would be lost.
    LaunchedEffect(Unit) {
        ContractWarnings.mismatched.collect { mismatch ->
            if (mismatch == null) return@collect
            // Refresh FIRST, then toast: the message says the list holds
            // what was actually stored, and the detail sheet reads from
            // the cached list — so the user must not keep looking at the
            // stale row. toast() does not suspend (unlike the old
            // showSnackbar, which held this collector for ~10s), so no
            // generation dance is needed around it.
            // Backgrounded (#308): the list is swapped under the toast
            // rather than blanked to a spinner.
            silentRefresh = true
            refreshTick++
            toasts.toast(
                ContractWarnings.message(mismatch.fields, mismatch.taskName),
                ToastKind.Error,
            )
            // Clear by GENERATION, not by the field list: another report
            // can land while this one is up, and matching on the list
            // would wipe that one whenever the fields happen to be the
            // same — which, against one broken server, they usually are.
            ContractWarnings.consume(mismatch.generation)
        }
    }

    // Editor draft (id empty = new task) and delete target.
    var editing by remember { mutableStateOf<ServerTaskDraft?>(null) }
    var deleting by remember { mutableStateOf<ServerTask?>(null) }
    // In-flight mutation per task id (#288): only that row's checkbox is
    // disabled while its request runs — the rest of the list, the top bar
    // and the editor stay live. Synthetic "__create__" covers the dialog
    // save path, which has no task id of its own.
    var busyIds by remember { mutableStateOf(setOf<String>()) }
    fun markBusy(id: String) { busyIds = busyIds + id }
    fun unmarkBusy(id: String) { busyIds = busyIds - id }
    // Day-grouped sections (Overdue/Today/…); flat list when off or on
    // the Done filter, where buckets carry no meaning.
    var groupByDay by rememberSaveable { mutableStateOf(true) }
    // Widget/alarm deep-link into one task's detail sheet: staged here,
    // opened once the list carries the row, then cleared upstream.
    var pendingDeepLink by rememberSaveable { mutableStateOf<String?>(null) }
    // Set when the screen itself switched filters to chase a deep-link;
    // the filter-change clearer below must not eat the id it just set.
    var deepLinkAutoSwitched by rememberSaveable { mutableStateOf(false) }
    LaunchedEffect(deepLinkId) {
        if (deepLinkId != null) {
            pendingDeepLink = deepLinkId
            onDeepLinkConsumed()
        }
    }
    // Reminder bell state + notification permission gate (Android 13+).
    var remindersOn by remember { mutableStateOf(TaskReminders.isEnabled(context)) }
    // Enable path for the reminder bell: exact-alarm grant is best
    // effort (the scheduler falls back to inexact), notifications are
    // required, so those gate through the permission request below.
    // Declared before the launcher: the callback calls it, and locals
    // are only visible after their declaration point.
    fun armRemindersAfterChecks() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            val mgr = context.getSystemService(Context.ALARM_SERVICE) as AlarmManager
            if (!mgr.canScheduleExactAlarms()) {
                runCatching {
                    context.startActivity(
                        Intent(Settings.ACTION_REQUEST_SCHEDULE_EXACT_ALARM),
                    )
                }
            }
        }
        TaskReminders.setEnabled(context, true)
        remindersOn = true
    }
    val notifPermission = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestPermission(),
    ) { granted ->
        if (granted) armRemindersAfterChecks()
        else toasts.toast("Notifications denied — reminders stay off.", ToastKind.Error)
    }

    // First successful load also (re)arms due-time alarms, covering
    // agent-side changes made while the app was closed.
    var scheduledOnce by remember { mutableStateOf(false) }

    LaunchedEffect(filter, refreshTick) {
        // Which kind of run this is (#308): a bump with [silentRefresh] set
        // swaps the fresh rows in WITHOUT the full-list spinner, so a
        // completion, a resume or a conflict reload never blanks the list the
        // user is reading. The flag is consumed here and cleared, so it can
        // only describe the run it was set for.
        //
        // Both flags are written at the top of every run rather than only at
        // the end: a run cancelled by a newer one (they share the tick as a
        // key) must not leave a spinner or a progress bar behind.
        val background = silentRefresh
        silentRefresh = false
        loading = !background
        refreshing = background
        if (!background) error = ""
        // fold's lambdas are not suspend, so the cursor read below runs after
        // the fold, not inside onSuccess.
        val loaded = api.list(filter.state).fold(
            onSuccess = { page ->
                // Rows still playing their leave animation are carried over
                // the fresh page (#308): the server stops listing a task the
                // moment it is completed, so without this the row would be
                // pulled out mid-animation and the list would blink.
                tasks = mergeRefresh(page.tasks, tasks, exitingIds)
                listTruncated = page.truncated
                error = ""
                // List-level last fetch, persisted (#288): set on success
                // only, so a failed fetch leaves the old stamp untouched.
                lastSyncedAt = System.currentTimeMillis()
                writeLastSyncedAt(context, lastSyncedAt)
                if (!scheduledOnce) {
                    scheduledOnce = true
                    TaskReminders.refresh(context)
                }
                true
            },
            onFailure = { e ->
                if (background) {
                    // A silent refresh that fails keeps the rows on screen
                    // (#308) and says so out loud: replacing a list the user
                    // is reading with an error card because a refetch missed
                    // would be the worse surprise, and the toast is the "what
                    // you are looking at may be older" signal the spinner
                    // used to give.
                    toasts.toast(
                        serverDetail(e.message ?: e.javaClass.simpleName),
                        ToastKind.Error,
                    )
                } else {
                    error = serverDetail(e.message ?: e.javaClass.simpleName)
                    // The flag describes the rows on screen, so it resets with
                    // them: a failed fetch leaves yesterday's rows up, and they
                    // must not keep advertising yesterday's truncation.
                    listTruncated = false
                }
                false
            },
        )
        // The cursor describes what is on screen (#178): recorded after the
        // rows land, never ahead of them. Best-effort — if the marker is
        // unreachable the cursor keeps its old value, so the next resume does
        // one redundant refetch and then converges (stale is the failure
        // direction that is never allowed; redundant is always safe).
        if (loaded) {
            api.changes(lastSeenChangeRevision(context)).getOrNull()?.let {
                noteChangeRevision(context, it.revision)
            }
        }
        loading = false
        refreshing = false
        hasLoaded = true
    }

    // Recorded only AFTER the rows have actually been on screen once (#308),
    // so the paint that first shows them is not treated as an arrival: an
    // effect runs after composition, which is exactly what makes this the
    // "what was on screen last time" set rather than one the first paint
    // already sees.
    LaunchedEffect(tasks, loading, error) {
        if (!loading && error.isEmpty()) knownTaskIds = tasks.mapTo(HashSet()) { it.id }
    }

    // Coming back to a screen the agent may have written to while we were away
    // (#178).
    //
    // The marker, not a timer: the tasks store advances ONE revision on every
    // task write, whichever side made it, so "is it different from what I last
    // saw" is a one-call question. Refetching unconditionally on resume would
    // cost a full list on every re-entry for something that changes a few
    // times a day — and never refetching is exactly the bug this replaces,
    // because a task the agent created over MCP while the app sat in the
    // background never reached the open list at all.
    //
    // A marker that cannot be read refreshes anyway. "Cannot answer" must
    // never degrade to "nothing changed", or the undecided case would be the
    // one that goes stale.
    //
    // The cursor is recorded by the loader, not here (#178): this effect and
    // the list fetch race, and recording a revision the in-flight rows do not
    // contain yet would leave a pre-write screen behind a post-write cursor —
    // equal on every later resume, stale until the next write. So a resume
    // that finds new data only bumps the tick; the loader records the cursor
    // once the fresh rows actually land. A resume before the first load
    // finishes does nothing at all — that load is already fetching, and it
    // owns the cursor.
    LifecycleResumeEffect(Unit) {
        // The block must end in onPauseOrDispose, so the pre-first-load
        // no-op takes the empty handler rather than an early return.
        if (hasLoaded) {
            val job = scope.launch {
                val marker = api.changes(lastSeenChangeRevision(context)).getOrNull()
                if (marker == null || marker.changed) {
                    // Backgrounded (#308): a screen the user just returned to
                    // refreshes underneath them instead of flashing a spinner
                    // over rows that are mostly the same.
                    silentRefresh = true
                    refreshTick++
                }
            }
            onPauseOrDispose { job.cancel() }
        } else {
            onPauseOrDispose { }
        }
    }

    // Open the deep-linked row as soon as it is in the list. A later
    // filter switch is an explicit context change, so a still-missing id
    // is dropped there instead of lingering (e.g. a deleted task) —
    // unless the switch was our own auto-chase below. The first run is
    // skipped so a fresh deep-link survives initial load.
    var seenFilter by remember { mutableStateOf<String?>(null) }
    LaunchedEffect(filterName) {
        if (seenFilter == null) seenFilter = filterName
        else if (seenFilter != filterName) {
            seenFilter = filterName
            if (deepLinkAutoSwitched) deepLinkAutoSwitched = false
            else pendingDeepLink = null
        }
    }
    // If a loaded, non-empty list doesn't carry the id and we haven't
    // tried All yet, switch there automatically (a Done-view row tapped
    // while the Manager sits on Open); otherwise the id is truly gone.
    LaunchedEffect(tasks, loading, pendingDeepLink) {
        val id = pendingDeepLink ?: return@LaunchedEffect
        tasks.firstOrNull { it.id == id }?.let {
            selected = it
            pendingDeepLink = null
            deepLinkAutoSwitched = false
            return@LaunchedEffect
        }
        if (loading) return@LaunchedEffect
        if (tasks.isEmpty() || filter == ServerTaskFilter.All || deepLinkAutoSwitched) {
            pendingDeepLink = null
            deepLinkAutoSwitched = false
        } else {
            deepLinkAutoSwitched = true
            filterName = ServerTaskFilter.All.name
        }
    }

    fun fail(e: Throwable) {
        toasts.toast(serverDetail(e.message ?: e.javaClass.simpleName), ToastKind.Error)
    }

    // Push fresh counts to the home-screen widget (#101) after mutations,
    // and re-arm due-time alarms (completions/deletions/edits change them).
    fun pokeWidget() {
        TaskWidget.refresh(context)
        TaskReminders.refresh(context)
    }

    /**
     * A completed row has finished leaving the list (#308): drop it for real.
     *
     * Called twice by design — once by the row's own animation, and once by
     * the timer [doComplete] arms for rows that are not on screen to report
     * it (a search hides them, so no slot exists). The guard makes the second
     * call a no-op, and stops a row the user has since got back (a completed
     * request that failed and was reverted) from being removed here.
     *
     * The list is only touched while the Open filter is still showing: that is
     * the filter a completed row has to leave. If the user switched filters
     * while it was on its way out, the other filter's own fetch decides what
     * belongs on screen (a completed row belongs on All/Done), so removing it
     * here would fight that.
     */
    fun onRowExited(id: String) {
        if (id !in exitingIds) return
        exitingIds = exitingIds - id
        if (filter == ServerTaskFilter.Open) {
            tasks = tasks.filterNot { it.id == id }
        }
    }

    fun onBell() {
        if (remindersOn) {
            TaskReminders.setEnabled(context, false)
            remindersOn = false
        } else if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU &&
            ContextCompat.checkSelfPermission(
                context, Manifest.permission.POST_NOTIFICATIONS,
            ) != PackageManager.PERMISSION_GRANTED
        ) {
            notifPermission.launch(Manifest.permission.POST_NOTIFICATIONS)
        } else {
            armRemindersAfterChecks()
        }
    }

    /**
     * Copy a task 1:1 (issue #161): every field carried over verbatim —
     * name, speak_name, description, due date/time, estimate, the whole repeat block
     * and the parallel flag — as a new **open** task. Nothing is "smarter"
     * than the original: a past due date stays a past due date, and a
     * cadence comes across as the same cadence. A copy is a starting point
     * to edit, not a rescheduled twin.
     */
    fun doDuplicate(t: ServerTask) {
        // Everything the client itself enforces is checked here, before the
        // sheet is dismissed: a refusal that closes the task leaves the user
        // with a toast and nothing to fix, because the thing they needed
        // to change is behind a tap they just made.
        val missing = listOf(
            "a name" to t.name.isBlank(),
            "some details" to t.description.isBlank(),
            "a due date" to !t.dueDate.isIsoDate(),
            "a due time" to t.dueTime.isBlank(),
        ).filter { it.second }.map { it.first }
        if (missing.isNotEmpty()) {
            toasts.toast(
                "This task has no ${missing.joinToString(" or ")} yet — add " +
                    "${if (missing.size == 1) "it" else "them"} before copying it.",
                ToastKind.Error,
            )
            return
        }
        // Closed here rather than by the caller, so a task that cannot be
        // copied stays open to be fixed.
        selected = null
        // Optimistic placeholder: the copy appears instantly with a temp id
        // and is swapped for the server answer — no full-reload flicker.
        val tempId = "__pending__${System.nanoTime()}"
        val prev = tasks
        val placeholder = t.copy(id = tempId)
        // The server filters by state: on the Done filter an open copy
        // belongs nowhere visible, so skip the placeholder there (success
        // still pokes and toasts; the row appears on the next reload only
        // if the filter would carry it).
        tasks = if (filter == ServerTaskFilter.Done) prev else listOf(placeholder) + prev
        markBusy(tempId)
        scope.launch {
            api.create(
                name = t.name,
                speakName = t.speakName,
                description = t.description,
                dueDate = t.dueDate,
                dueTime = t.dueTime,
                estimatedMinutes = t.estimatedMinutes,
                repeatEvery = t.repeatEvery,
                repeatUnit = t.repeatUnit,
                repeatDays = t.repeatDays,
                repeatCustom = t.repeatCustom,
                repeatRule = t.repeatRule,
                parallelable = t.parallelable,
            ).fold(
                onSuccess = { copy ->
                    editing = null
                    tasks = tasks.map { if (it.id == tempId) copy else it }
                    pokeWidget()
                    toasts.toast("Copied \u201c${copy.name}\u201d.", ToastKind.Success)
                },
                onFailure = { e ->
                    tasks = prev
                    fail(e)
                },
            )
            unmarkBusy(tempId)
        }
    }

    fun doComplete(t: ServerTask) {
        // Already on its way out, or still in flight (#308): the exit
        // animation owns the removal, and a second request here would finish
        // first on failure and cancel the first one's animation with
        // `tasks = prev` — leaving the completed row stuck in the Open list.
        // The row is disabled while busy/exiting, and this covers every other
        // caller (the detail sheet, a deep link) as well.
        if (t.id in exitingIds || t.id in busyIds) return
        // The sheet is already dismissed by the caller. On the Open filter the
        // row is NOT removed here (#308): it is marked as leaving and plays its
        // exit animation, then onRowExited drops it. The request runs in the
        // background meanwhile.
        selected = null
        val prev = tasks
        if (filter == ServerTaskFilter.Open) {
            exitingIds = exitingIds + t.id
            // The row is left in `tasks` exactly as it is until it has
            // finished leaving: marking it completed would move it to the
            // Completed bucket (dueBucket keys off `isOpen`) and teleport it
            // out of its section mid-animation. The row renders as done from
            // the exiting set instead.
            //
            // Safety net for a row that is not on screen at all (a search
            // hides it, so there is no slot to report the animation): drop it
            // after the animation's own length anyway. A second report is
            // harmless — onRowExited ignores an id it has already handled.
            scope.launch {
                delay(ROW_ANIM_MS + 100L)
                onRowExited(t.id)
            }
        } else {
            tasks = prev.map { if (it.id == t.id) it.copy(completedAt = "__pending__") else it }
        }
        markBusy(t.id)
        scope.launch {
            api.complete(t.id).fold(
                onSuccess = { done ->
                    // A structured cadence rolled itself over server-side:
                    // the next occurrence is a NEW row this response does
                    // not carry, so only then fall back to a refetch — a
                    // silent one, so the list under the leaving row stays put
                    // (#308). The row mid-exit is carried over that swap by
                    // the loader, so its animation finishes rather than being
                    // cut short. Every other completion patches in place.
                    val next = done?.nextDueDate.orEmpty()
                    if (next.isNotEmpty()) {
                        silentRefresh = true
                        refreshTick++
                        // The widget and the alarm set still need the push:
                        // the loader never pokes, and the new occurrence's
                        // reminders are not armed until it does.
                        pokeWidget()
                        // "Tomorrow" / "2 Oct" rather than a raw ISO date.
                        val when2 = friendlyDue(
                            next, "", java.time.LocalDate.now(IST)).ifEmpty { next }
                        toasts.toast("Task completed — next one set for $when2.")
                    } else if (filter == ServerTaskFilter.Open) {
                        pokeWidget()
                        // A custom condition is nobody's to compute but
                        // ours, so the prefilled draft asks for the date —
                        // unless an editor is already open, which must not
                        // be discarded. One-shot tasks are skipped.
                        if (editing == null && t.hasRepeat) {
                            editing = ServerTaskDraft(
                                name = t.name,
                                description = t.description,
                                // Only the date is cleared: recreating means
                                // picking a new one (save stays gated on it),
                                // but every other field is the user's own and
                                // is carried over verbatim. Clearing the time
                                // too made the user re-pick a value they had
                                // already set (#148). The time field is
                                // disabled until a date exists, so the kept
                                // time is revealed, not stranded.
                                dueDate = "",
                                dueTime = t.dueTime,
                                estimatedMinutes = t.estimatedMinutes.toString(),
                                repeatEvery = if (t.repeatEvery > 0) t.repeatEvery.toString() else "",
                                repeatUnit = t.repeatUnit.ifEmpty { "days" },
                                repeatDays = t.repeatDays,
                                repeatCustom = t.repeatCustom,
                                repeatRule = t.repeatRule,
                                parallelable = t.parallelable,
                            )
                            // Legacy tasks (created before due_time was
                            // required) have no time to keep — don't claim
                            // there is one.
                            toasts.toast(
                                if (t.dueTime.isBlank()) {
                                    "Task completed — pick a new date and time to recreate it."
                                } else {
                                    "Task completed — pick a new date to recreate it (time kept)."
                                },
                            )
                        } else {
                            toasts.toast("Task completed.", ToastKind.Success)
                        }
                    } else {
                        tasks = tasks.map { if (it.id == t.id) done else it }
                        pokeWidget()
                        toasts.toast("Task completed.", ToastKind.Success)
                    }
                },
                onFailure = { e ->
                    // Cancel the leave animation: clearing the id puts the row
                    // back on screen, where it fades in where it was (#308).
                    // Restoring `prev` also covers the case where the
                    // animation had already finished and onRowExited had
                    // dropped the row.
                    exitingIds = exitingIds - t.id
                    tasks = prev
                    fail(e)
                },
            )
            unmarkBusy(t.id)
        }
    }

    fun doReopen(t: ServerTask) {
        selected = null
        val prev = tasks
        tasks = if (filter == ServerTaskFilter.Done) {
            prev.filterNot { it.id == t.id }
        } else {
            prev.map { if (it.id == t.id) it.copy(completedAt = "") else it }
        }
        markBusy(t.id)
        scope.launch {
            api.reopen(t.id).fold(
                onSuccess = { reopened ->
                    if (filter != ServerTaskFilter.Done) {
                        tasks = tasks.map { if (it.id == t.id) reopened else it }
                    }
                    pokeWidget()
                },
                onFailure = { e ->
                    tasks = prev
                    fail(e)
                },
            )
            unmarkBusy(t.id)
        }
    }

    fun doDelete(t: ServerTask) {
        deleting = null
        editing = null
        selected = null
        val prev = tasks
        tasks = prev.filterNot { it.id == t.id }
        markBusy(t.id)
        scope.launch {
            api.delete(t.id).fold(
                onSuccess = {
                    pokeWidget()
                    toasts.toast("Task deleted.", ToastKind.Success)
                },
                onFailure = { e ->
                    tasks = prev
                    fail(e)
                },
            )
            unmarkBusy(t.id)
        }
    }

    // The toast card floats top-end over the content (Box overlay, not
    // the bottom-anchored Scaffold slot) so it never shifts the layout.
    Box(modifier = Modifier.fillMaxSize()) {
        Scaffold(
            topBar = {
                TopAppBar(
                    navigationIcon = {
                        IconButton(onClick = onMenu) {
                            Icon(Icons.Filled.Menu, contentDescription = "Menu")
                        }
                    },
                    title = { Text("Task Manager") },
                    // Never gated: mutations are per-row now, so the bell,
                    // New and Refresh stay live while a request runs.
                    actions = {
                        IconButton(onClick = ::onBell) {
                            Icon(
                                Icons.Filled.Notifications,
                                contentDescription = "Due-time reminders",
                                tint = if (remindersOn) {
                                    MaterialTheme.colorScheme.primary
                                } else {
                                    MaterialTheme.colorScheme.onSurfaceVariant
                                },
                            )
                        }
                        IconButton(onClick = { editing = ServerTaskDraft() }) {
                            Icon(Icons.Filled.Add, contentDescription = "New task")
                        }
                        IconButton(onClick = {
                            // Backgrounded (#308): an explicit refresh should
                            // not blank the list either — the thin bar above
                            // the rows reports that it is working.
                            silentRefresh = true
                            refreshTick++
                        }) {
                            Icon(Icons.Filled.Refresh, contentDescription = "Refresh")
                        }
                    },
                )
            },
        ) { padding ->
            Column(
            modifier = Modifier.fillMaxSize().padding(padding),
            horizontalAlignment = Alignment.CenterHorizontally,
        ) {
            // Background refresh (#308): a thin bar instead of the full-list
            // spinner, above the content so it never displaces the rows the
            // user is reading while the fresh list arrives.
            RefreshBar(refreshing = refreshing)
            Column(
                modifier = Modifier.contentWidth(wc)
                    .verticalScroll(rememberScrollState())
                    .padding(16.dp),
                verticalArrangement = Arrangement.spacedBy(12.dp),
            ) {
                OutlinedTextField(
                    value = query,
                    onValueChange = { query = it },
                    label = { Text("Search tasks") },
                    leadingIcon = { Icon(Icons.Filled.Search, contentDescription = null) },
                    trailingIcon = {
                        if (query.isNotEmpty()) {
                            IconButton(onClick = { query = "" }) {
                                Icon(Icons.Filled.Close, contentDescription = "Clear search")
                            }
                        }
                    },
                    singleLine = true,
                    modifier = Modifier.fillMaxWidth(),
                )
                Row(
                    modifier = Modifier.fillMaxWidth(),
                    horizontalArrangement = Arrangement.spacedBy(8.dp),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    Row(
                        modifier = Modifier.weight(1f)
                            .horizontalScroll(rememberScrollState()),
                        horizontalArrangement = Arrangement.spacedBy(8.dp),
                    ) {
                        ServerTaskFilter.entries.forEach { f ->
                            FilterChip(
                                selected = filter == f,
                                onClick = { filterName = f.name },
                                label = { Text(f.title) },
                            )
                        }
                        FilterChip(
                            selected = groupByDay,
                            onClick = { groupByDay = !groupByDay },
                            label = { Text("Group by time") },
                        )
                    }
                    Box {
                        OutlinedButton(onClick = { sortMenu = true }) {
                            Icon(Icons.Filled.Sort, contentDescription = null)
                            Spacer(modifier = Modifier.width(4.dp))
                            Text(sort.title)
                        }
                        DropdownMenu(
                            expanded = sortMenu,
                            onDismissRequest = { sortMenu = false },
                        ) {
                            ServerTaskSort.entries.forEach { s ->
                                DropdownMenuItem(
                                    text = { Text(s.title) },
                                    onClick = { sortName = s.name; sortMenu = false },
                                )
                            }
                        }
                    }
                }
                // Derived once per composition: search narrows, sort orders.
                // `today` is a remember key below so buckets recompute
                // past midnight while the app stays open.
                // IST-pinned (#124) like every other displayed date.
                // One clock per composition, reused by the overdue tests
                // and the buckets below: separate now() reads can straddle
                // midnight and disagree about what day it is.
                val now = java.time.LocalDateTime.now(IST)
                val day = now.toLocalDate()
                val today = day.toString()
                val visible = remember(tasks, query, sort, today) {
                    val q = query.trim().lowercase(Locale.ROOT)
                    tasks
                        .filter { t ->
                            q.isEmpty() ||
                                t.name.lowercase(Locale.ROOT).contains(q) ||
                                t.description.lowercase(Locale.ROOT).contains(q)
                        }
                        .sortedByMode(sort)
                }
                if (loading) {
                    Box(
                        modifier = Modifier.fillMaxWidth().padding(32.dp),
                        contentAlignment = Alignment.Center,
                    ) {
                        CircularProgressIndicator()
                    }
                } else if (error.isNotEmpty()) {
                    ErrorCard(raw = error)
                    OutlinedButton(
                        onClick = { refreshTick++ },
                        modifier = Modifier.fillMaxWidth(),
                    ) {
                        Text("Retry")
                    }
                } else if (tasks.isEmpty()) {
                    EmptyState(
                        icon = Icons.Filled.Assignment,
                        title = when (filter) {
                            ServerTaskFilter.Done -> "Nothing completed yet"
                            ServerTaskFilter.All -> "No tasks yet"
                            else -> "No open tasks"
                        },
                        subtitle = "Capture your first to-do.",
                        actionLabel = "New task",
                        onAction = { editing = ServerTaskDraft() },
                    )
                } else if (visible.isEmpty()) {
                    EmptyState(
                        icon = Icons.Filled.Search,
                        title = "No matches",
                        subtitle = "Try a different search.",
                        actionLabel = "Clear search",
                        onAction = { query = "" },
                    )
                } else {
                    // The minute ticker drives the time buckets AND the
                    // "synced Xm ago" stamp below, so it runs whenever the
                    // list is on screen — not only when buckets are shown.
                    // One recomposition a minute keeps both honest.
                    var clockTick by remember { mutableIntStateOf(0) }
                    LaunchedEffect(Unit) {
                        while (true) {
                            // Aligned to the top of the minute: a plain
                            // 60s sleep drifts by however long the
                            // composition took, so a task would sit in the
                            // wrong bucket for up to a minute past its
                            // boundary.
                            val toNextMinute = 60_000L -
                                (System.currentTimeMillis() % 60_000L)
                            delay(toNextMinute)
                            clockTick++
                        }
                    }
                    // Read through a remember key, so this frame recomposes
                    // each minute even with grouping off or on Done.
                    val syncedLabel = remember(lastSyncedAt, clockTick) {
                        formatLastSynced(lastSyncedAt, System.currentTimeMillis())
                    }
                    Text(
                        "${visible.size} of ${tasks.size}" +
                            // No narrowing promise: search filters the fetched
                            // page client-side, so past the cap it cannot
                            // find what was never fetched (#168 follow-up is
                            // wiring ?search= end to end).
                            (if (listTruncated) " — showing the first ${tasks.size}" else "") +
                            // List-level, not per-task (#288): when the local
                            // rows were last fetched. Absent before the first
                            // successful load.
                            (syncedLabel?.let { " · synced $it" } ?: ""),
                        style = MaterialTheme.typography.labelMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                    // Time sections (skipped on the Done filter, where due
                    // buckets carry no meaning); within a section the
                    // current sort still applies. IST-pinned (#124).
                    val bucketing = groupByDay && filter != ServerTaskFilter.Done
                    // Everything time-derived in this list reads clockKey,
                    // the minute-truncated instant the groups are keyed on —
                    // not the raw now(): nanos would miss the memo on every
                    // recomposition, and two different instants would let a
                    // row be coloured overdue inside a group that disagrees.
                    // The cost is a whole minute's granularity, which is the
                    // same cadence the minute ticker already refreshes at.
                    val clockKey = now.truncatedTo(java.time.temporal.ChronoUnit.MINUTES)
                    val sections = remember(visible, bucketing, filter, day, clockKey, clockTick) {
                        if (!groupByDay || filter == ServerTaskFilter.Done) {
                            listOf(null to visible)
                        } else {
                            DueBucket.entries.mapNotNull { b ->
                                val rows = visible.filter { it.dueBucket(day, clockKey) == b }
                                if (rows.isEmpty()) null else b to rows
                            }
                        }
                    }
                    sections.forEach { (bucket, rows) ->
                        Column(modifier = Modifier.fillMaxWidth()) {
                            if (bucket != null) {
                                Text(
                                    "${bucket.title} · ${rows.size}",
                                    style = MaterialTheme.typography.titleSmall,
                                    color = if (bucket == DueBucket.Overdue) {
                                        MaterialTheme.colorScheme.error
                                    } else {
                                        MaterialTheme.colorScheme.primary
                                    },
                                    modifier = Modifier.padding(top = 8.dp, bottom = 2.dp),
                                )
                            }
                            rows.forEachIndexed { i, t ->
                                // Keyed by task id (#308): these rows are
                                // rebuilt on every sort, filter and bucket
                                // change, and the animation state has to
                                // belong to the task rather than to the slot
                                // it currently sits in — otherwise a re-sort
                                // would replay an entrance for a row that
                                // never left.
                                key(t.id) {
                                    TaskRowSlot(
                                        taskId = t.id,
                                        exiting = t.id in exitingIds,
                                        // A row whose id the list was already
                                        // showing is drawn in place; only a
                                        // genuinely new one animates in.
                                        animateEnter = knownTaskIds?.contains(t.id) == false,
                                        onExited = ::onRowExited,
                                    ) {
                                        Column {
                                            ServerTaskRow(
                                                task = t,
                                                // Same instant the groups were
                                                // built from, so a row can
                                                // never be coloured overdue
                                                // inside a group that says
                                                // otherwise.
                                                overdue = t.isOverdue(today, clockKey),
                                                today = day,
                                                // Per-row (#288): only the row
                                                // with an in-flight request is
                                                // disabled — plus a row that is
                                                // leaving (#308), which must not
                                                // accept a second tap.
                                                actionsEnabled = t.id !in busyIds &&
                                                    t.id !in exitingIds,
                                                // Leaving (#308): it reads as
                                                // completed while it goes, but
                                                // only in what is drawn — the
                                                // row keeps its own data so the
                                                // bucket it was in does not
                                                // change underneath it.
                                                completed = t.id in exitingIds,
                                                // A leaving row is not a
                                                // target: opening it would
                                                // race the request that is
                                                // taking it away.
                                                onOpen = {
                                                    if (t.id !in exitingIds) selected = t
                                                },
                                                onToggle = {
                                                    if (t.isOpen()) doComplete(t) else doReopen(t)
                                                    selected = null
                                                },
                                            )
                                            if (i < rows.lastIndex) {
                                                HorizontalDivider(
                                                    modifier = Modifier.padding(start = 56.dp),
                                                )
                                            }
                                        }
                                    }
                                }
                            }
                        }
                    }
                }
                // Widget display settings moved to Settings -> Home-screen
                // widget: they configure the home screen, not this task
                // list, and they were the only controls below the rows.
                HintLine("Your tasks, shared with your assistant — changes here and in chat land in the same list.")
                }
            }
        }
        ToastHost(center = toasts, modifier = Modifier.fillMaxWidth())
    }

    val draft = editing
    if (draft != null) {
        ServerTaskDialog(
            initial = draft,
            isNew = draft.id.isEmpty(),
            onDismiss = { editing = null },
            onDelete = if (draft.id.isEmpty()) null else ({
                editing = null
                deleting = tasks.firstOrNull { it.id == draft.id }
            }),
            onSave = { next ->
                val mins = next.estimatedMinutes.trim().toIntOrNull()
                if (mins == null || mins < 0) {
                    toasts.toast(
                        "Estimated minutes is required (a number 0 or above).",
                        ToastKind.Error,
                    )
                    return@ServerTaskDialog
                }
                // Optimistic (#288): the dialog dismisses and the list
                // patches instantly; the request runs in the background.
                // Success patches the row with the server answer (revision
                // included) — no full-reload flicker. Failure reverts.
                // Offline just fails and reverts; no queue.
                if (next.id.isEmpty()) {
                    val rep = next.repeatOrNull()
                    val tempId = "__pending__${System.nanoTime()}"
                    val prev = tasks
                    tasks = if (filter == ServerTaskFilter.Done) {
                        // Open task on the Done filter: nothing visible to
                        // patch (see doDuplicate); the reload on the next
                        // resume converges.
                        prev
                    } else {
                        listOf(
                        ServerTask(
                            id = tempId,
                            name = next.name,
                            speakName = next.speakName,
                            description = next.description,
                            dueDate = next.dueDate,
                            dueTime = next.dueTime,
                            estimatedMinutes = mins,
                            repeatEvery = rep?.every ?: 0,
                            repeatUnit = rep?.unit.orEmpty(),
                            repeatDays = rep?.days.orEmpty(),
                            repeatCustom = rep?.custom == true,
                            repeatRule = rep?.text.orEmpty(),
                            parallelable = next.parallelable,
                        ),
                    ) + prev
                    }
                    editing = null
                    markBusy(tempId)
                    scope.launch {
                        api.create(
                            name = next.name,
                            speakName = next.speakName,
                            description = next.description,
                            dueDate = next.dueDate,
                            dueTime = next.dueTime,
                            estimatedMinutes = mins,
                            repeatEvery = rep?.every ?: 0,
                            repeatUnit = rep?.unit.orEmpty(),
                            repeatDays = rep?.days.orEmpty(),
                            repeatCustom = rep?.custom == true,
                            repeatRule = rep?.text.orEmpty(),
                            parallelable = next.parallelable,
                        ).fold(
                            onSuccess = { created ->
                                tasks = tasks.map { if (it.id == tempId) created else it }
                                pokeWidget()
                            },
                            onFailure = { e ->
                                tasks = prev
                                fail(e)
                            },
                        )
                        unmarkBusy(tempId)
                    }
                } else {
                    // Only resend the recurrence when it actually
                    // changed: an unrelated edit (a name tweak) must
                    // not read as a repeat rewrite, and a task edited
                    // by an older build keeps whatever cadence it has.
                    val was = editing?.repeatOrNull()
                    val nowRep = next.repeatOrNull()
                    val repChanged = was != nowRep
                    val prev = tasks
                    val prevRow = prev.firstOrNull { it.id == next.id }
                    var optimistic = (prevRow ?: ServerTask(id = next.id)).copy(
                        name = next.name,
                        speakName = next.speakName,
                        description = next.description,
                        dueDate = next.dueDate,
                        dueTime = next.dueTime,
                        estimatedMinutes = mins,
                        parallelable = next.parallelable,
                    )
                    if (repChanged && nowRep != null) {
                        optimistic = optimistic.copy(
                            repeatEvery = nowRep.every,
                            repeatUnit = nowRep.unit,
                            repeatDays = nowRep.days,
                            repeatCustom = nowRep.custom,
                            repeatRule = nowRep.text,
                        )
                    }
                    tasks = prev.map { if (it.id == next.id) optimistic else it }
                    editing = null
                    markBusy(next.id)
                    scope.launch {
                        api.update(
                            id = next.id,
                            name = next.name,
                            speakName = next.speakName,
                            description = next.description,
                            dueDate = next.dueDate,
                            dueTime = next.dueTime,
                            estimatedMinutes = mins,
                            repeatEvery = if (repChanged) nowRep?.every else null,
                            repeatUnit = if (repChanged) nowRep?.unit else null,
                            repeatDays = if (repChanged) nowRep?.days.orEmpty() else null,
                            repeatCustom = if (repChanged) nowRep?.custom else null,
                            repeatRule = if (repChanged) nowRep?.text else null,
                            parallelable = next.parallelable,
                            // The revision the dialog read, not a fresh
                            // lookup: the task may have scrolled out of the
                            // fetched page, and a null lookup would send
                            // the edit unguarded (#184).
                            expectedRevision = next.revision,
                        ).fold(
                            onSuccess = { saved ->
                                if (prevRow == null) {
                                    // The row was not in the fetched page at
                                    // all: backgrounded (#308) so pulling the
                                    // list in does not blank what is on screen.
                                    silentRefresh = true
                                    refreshTick++
                                } else {
                                    tasks = tasks.map { if (it.id == next.id) saved else it }
                                }
                                pokeWidget()
                            },
                            onFailure = { e ->
                                tasks = prev
                                if (isConflict(e)) {
                                    // Someone else wrote first: reload and
                                    // say so, rather than silently holding
                                    // the reverted row. Backgrounded (#308):
                                    // the toast is the message, and a spinner
                                    // over the list would fight it.
                                    silentRefresh = true
                                    refreshTick++
                                    toasts.toast(
                                        "That task changed elsewhere — reloaded. Reapply your edit.",
                                        ToastKind.Error,
                                    )
                                } else {
                                    fail(e)
                                }
                            },
                        )
                        unmarkBusy(next.id)
                    }
                }
            },
        )
    }

    val target = deleting
    if (target != null) {
        AlertDialog(
            onDismissRequest = { deleting = null },
            title = { Text("Delete task?") },
            text = { Text("“${target.name}” will be permanently deleted.") },
            confirmButton = {
                TextButton(onClick = { doDelete(target) }, enabled = target.id !in busyIds) { Text("Delete") }
            },
            dismissButton = {
                TextButton(onClick = { deleting = null }) { Text("Cancel") }
            },
        )
    }

    val open = selected?.let { s -> tasks.firstOrNull { it.id == s.id } ?: s }
    if (open != null) {
        // One clock for the date and the overdue test, so the sheet can
        // never straddle midnight between the two, truncated to the minute
        // for the same reason as the list — otherwise a task due at
        // 14:00:30 could be red in this sheet and not in the list it was
        // just tapped from. IST-pinned (#124).
        val sheetNow = java.time.LocalDateTime.now(IST)
            .truncatedTo(java.time.temporal.ChronoUnit.MINUTES)
        val sheetDay = sheetNow.toLocalDate()
        ServerTaskDetailSheet(
            task = open,
            overdue = open.isOverdue(sheetDay.toString(), sheetNow),
            today = sheetDay,
            actionsEnabled = open.id !in busyIds && open.id !in exitingIds,
            onDismiss = { selected = null },
            onEdit = {
                selected = null
                editing = open.toDraft()
            },
            onToggle = {
                selected = null
                if (open.isOpen()) doComplete(open) else doReopen(open)
            },
            onDuplicate = { doDuplicate(open) },
            onDelete = {
                selected = null
                deleting = open
            },
        )
    }
}

/**
 * How long a task row takes to arrive in or leave the list (#308). Short
 * enough to feel immediate, long enough that the eye can follow it — and it
 * is also the delay the not-on-screen safety net in `doComplete` waits out.
 */
private const val ROW_ANIM_MS = 220

/**
 * The slim bar that stands in for the full-list spinner while a background
 * refresh is in flight (#308).
 *
 * It is a sibling of the scrolling list rather than an item inside it, so it
 * costs no item spacing, and its height animates from nothing so an idle
 * screen is exactly as tall as it was before the bar existed.
 */
@Composable
private fun RefreshBar(refreshing: Boolean) {
    // The height animates 0 -> 4 and back rather than reserving a permanent
    // slot: an idle screen should carry no gap for a bar that is not there,
    // and starting a refresh eases the list down instead of jumping it.
    val height by animateDpAsState(
        targetValue = if (refreshing) 4.dp else 0.dp,
        animationSpec = tween(ROW_ANIM_MS),
        label = "refreshBarHeight",
    )
    Box(modifier = Modifier.fillMaxWidth().height(height)) {
        // Composed only while refreshing so the indeterminate animation is not
        // running invisibly the rest of the time.
        if (refreshing) {
            LinearProgressIndicator(modifier = Modifier.fillMaxWidth())
        }
    }
}

/**
 * One task row wrapped in its enter/leave animation (#308).
 *
 * Composed under `key(task.id)`, so the transition belongs to the task: a
 * re-sort or re-filter that merely moves a row does not replay anything, and
 * a row added by a repeat rolling over grows in where it lands. When
 * [exiting] flips true the row shrinks and fades, and only after that does
 * [onExited] run — so the caller drops the row at the moment it has actually
 * finished leaving, instead of yanking it out on the tap.
 *
 * [animateEnter] is read once, when the row first composes: the caller sets it
 * only for a row whose id the list was not already showing, so the first
 * population, a filter switch and a re-sort draw in place and only a row that
 * genuinely arrived animates in.
 */
@Composable
private fun TaskRowSlot(
    taskId: String,
    exiting: Boolean,
    animateEnter: Boolean,
    onExited: (String) -> Unit,
    content: @Composable () -> Unit,
) {
    // Hidden at first only when this row is meant to animate in; otherwise it
    // starts visible and is simply drawn. Rows already composed keep whatever
    // state they are in (that is what the key buys).
    val visibleState = remember { MutableTransitionState(!animateEnter) }
    visibleState.targetState = !exiting
    AnimatedVisibility(
        visibleState = visibleState,
        enter = fadeIn(tween(ROW_ANIM_MS)) +
            expandVertically(tween(ROW_ANIM_MS, easing = FastOutSlowInEasing)),
        exit = fadeOut(tween(ROW_ANIM_MS / 2)) +
            shrinkVertically(tween(ROW_ANIM_MS, easing = FastOutSlowInEasing)),
    ) {
        content()
    }
    // Reported from an effect rather than during composition, so the removal
    // is a state write and not a composition side effect. Keyed on the
    // transition state rather than wrapped in an `if`: an effect inside a
    // conditional branch has no stable identity (Compose lint flags it), and
    // the body's own check is just as cheap.
    LaunchedEffect(taskId, visibleState.currentState, visibleState.targetState) {
        if (!visibleState.currentState && !visibleState.targetState) onExited(taskId)
    }
}

/**
 * The row list to show after a fetch lands (#308).
 *
 * The server stops listing a task the moment it is completed, so a refresh
 * that arrives while that row is still playing its leave animation would pull
 * the row out from under the animation and the list would blink. Rows marked
 * [exiting] are therefore carried over the fresh page, and dropped for real
 * when their animation reports it has finished.
 *
 * A carried row is put back at the index it had, not appended: it is still on
 * screen mid-animation, and moving it to the end would visibly jump it — and,
 * where the sort keys tie, reorder its neighbours — for the ~220ms it has
 * left. The index is clamped, because the fresh page can be shorter than the
 * one the row came from.
 *
 * [exiting] is passed by the caller as it is AT THE MOMENT the response
 * arrives, never as it was before the request went out: a row whose animation
 * finished while the fetch was in flight must not be resurrected by it.
 */
internal fun mergeRefresh(
    fresh: List<ServerTask>,
    previous: List<ServerTask>,
    exiting: Set<String>,
): List<ServerTask> {
    if (exiting.isEmpty()) return fresh
    val freshIds = fresh.mapTo(HashSet()) { it.id }
    // One pass for the predicate, carrying each row's index with it.
    val carried = previous.withIndex()
        .filter { (_, row) -> row.id in exiting && row.id !in freshIds }
    if (carried.isEmpty()) return fresh
    // Indices ascend (we walk `previous` in order), so inserting each carried
    // row at its own clamped index keeps them in their original relative
    // order.
    val merged = fresh.toMutableList()
    carried.forEach { (index, row) -> merged.add(index.coerceAtMost(merged.size), row) }
    return merged
}

/** Flat task row: checkbox toggles complete/reopen, tap opens the
 * detail sheet. Name + one friendly due line; description, estimate,
 * repeat, and timestamps live on the detail page. Overdue open tasks
 * render the due line in error color; done rows dim with a
 * strikethrough. Deliberately cardless — the section stacks the rows
 * with inset dividers. */
@Composable
internal fun ServerTaskRow(
    task: ServerTask,
    overdue: Boolean,
    today: java.time.LocalDate,
    actionsEnabled: Boolean,
    /**
     * Draw the row as completed without the model saying so (#308): a row
     * playing its leave animation reads as done while it goes, but its data
     * is left alone so the bucket it sits in does not change under it.
     */
    completed: Boolean = false,
    onOpen: () -> Unit,
    onToggle: () -> Unit,
) {
    val done = !task.isOpen() || completed
    // On the Done/All filters a completion leaves the row where it is and
    // only flips its state, so the flip itself is animated here; the Open
    // filter animates the whole row out instead (TaskRowSlot). Colour is what
    // animates — a strikethrough has no animatable form.
    val nameColor by animateColorAsState(
        targetValue = if (done) {
            MaterialTheme.colorScheme.onSurfaceVariant
        } else {
            MaterialTheme.colorScheme.onSurface
        },
        animationSpec = tween(ROW_ANIM_MS),
        label = "taskRowName",
    )
    Row(
        modifier = Modifier.fillMaxWidth()
            .clickable(onClick = onOpen)
            .padding(horizontal = 4.dp, vertical = 8.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        IconButton(onClick = onToggle, enabled = actionsEnabled) {
            // Branches on `done`, not `task.isOpen()`: a row that is leaving
            // (#308) still carries an open model, and keying the icon off the
            // model would show it as completed in the text but with an empty
            // circle next to it.
            if (done) {
                Icon(
                    Icons.Filled.CheckCircle,
                    contentDescription = "Reopen task",
                    tint = MaterialTheme.colorScheme.primary,
                    modifier = Modifier.size(26.dp),
                )
            } else {
                Icon(
                    Icons.Filled.RadioButtonUnchecked,
                    contentDescription = "Complete task",
                    tint = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.size(26.dp),
                )
            }
        }
        Column(modifier = Modifier.weight(1f)) {
            Text(
                task.name,
                style = MaterialTheme.typography.bodyLarge,
                maxLines = 2,
                overflow = TextOverflow.Ellipsis,
                textDecoration = if (done) TextDecoration.LineThrough else null,
                color = nameColor,
            )
            val dueLine = task.startToDueLine(today)
            val dueBits = buildList {
                if (dueLine.isNotEmpty()) add(dueLine)
                val repeat = task.repeatLabel()
                if (repeat.isNotEmpty()) add(repeat)
                if (task.parallelable) add("parallel")
            }
            if (dueBits.isNotEmpty()) {
                Text(
                    dueBits.joinToString(" · "),
                    style = MaterialTheme.typography.bodySmall,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                    fontWeight = if (overdue) FontWeight.SemiBold else null,
                    color = if (overdue) {
                        MaterialTheme.colorScheme.error
                    } else {
                        MaterialTheme.colorScheme.onSurfaceVariant
                    },
                )
            } else if (task.description.isNotEmpty()) {
                Text(
                    task.description,
                    style = MaterialTheme.typography.bodySmall,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }
        if (task.description.isNotEmpty()) {
            Icon(
                Icons.Filled.Description,
                contentDescription = "Has details",
                tint = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.padding(end = 4.dp),
            )
        }
    }
}

/** Full detail sheet for one task: every field plus Complete/Reopen,
 * Edit, and Delete actions. Opened by tapping a compact row. */
@Composable
internal fun ServerTaskDetailSheet(
    task: ServerTask,
    overdue: Boolean,
    today: java.time.LocalDate,
    actionsEnabled: Boolean,
    onDismiss: () -> Unit,
    onEdit: () -> Unit,
    onToggle: () -> Unit,
    onDuplicate: () -> Unit,
    onDelete: () -> Unit,
) {
    ModalBottomSheet(onDismissRequest = onDismiss) {
        Column(
            modifier = Modifier.fillMaxWidth()
                .verticalScroll(rememberScrollState())
                .padding(horizontal = 20.dp)
                .padding(bottom = 32.dp),
            verticalArrangement = Arrangement.spacedBy(12.dp),
        ) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text(
                    task.name,
                    style = MaterialTheme.typography.headlineSmall,
                    modifier = Modifier.weight(1f),
                )
                if (task.isOpen()) {
                    // Static badge, not a chip: the status isn't actionable
                    // and a clickable chip would be a dead target.
                    Text(
                        if (overdue) "Overdue" else "Open",
                        style = MaterialTheme.typography.labelLarge,
                        color = if (overdue) {
                            MaterialTheme.colorScheme.error
                        } else {
                            MaterialTheme.colorScheme.primary
                        },
                    )
                } else {
                    Text(
                        // Skipped reads as "not done" (#287, #209): a skipped
                        // occurrence must never render as completed work.
                        if (task.skipped) "Skipped" else "Done",
                        style = MaterialTheme.typography.labelLarge,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
            if (task.description.isNotEmpty()) {
                SelectionContainer {
                    Text(
                        task.description,
                        style = MaterialTheme.typography.bodyLarge,
                    )
                }
            }
            HorizontalDivider()
            DetailLine(
                icon = Icons.Filled.Schedule,
                label = "Due",
                value = when {
                    task.dueDate.isEmpty() -> "No due date"
                    else -> friendlyDue(task.dueDate, task.dueTime, today)
                        .ifEmpty { task.dueDate }
                },
                highlight = overdue,
            )
            DetailLine(
                icon = Icons.Filled.PlayArrow,
                label = "Starts",
                value = when {
                    task.dueDate.isBlank() -> "No due time set"
                    // A date with no time has no start instant at all, so
                    // there is nothing to subtract an estimate from.
                    task.dueMillisOrNull() == null -> "Needs a due time"
                    // A zero estimate has no start of its own. Compared as
                    // instants: the two sides are rendered from different
                    // sources, so equal moments can differ as text.
                    task.startMillisOrNull() == task.dueMillisOrNull() ->
                        "Same as the due time"
                    else -> task.startParts()
                        ?.let { (d, t) -> friendlyDue(d, t, today) }
                        ?.takeIf { it.isNotEmpty() }
                        ?: "Same as the due time"
                },
            )
            DetailLine(
                icon = Icons.Filled.Repeat,
                label = "Repeats",
                value = task.repeatLabel().ifEmpty { "Does not repeat" },
            )
            DetailLine(
                icon = Icons.Filled.Groups,
                label = "Parallel",
                value = if (task.parallelable) {
                    "Yes — can run with other tasks"
                } else {
                    "No"
                },
            )
            if (task.createdAt.isNotEmpty() || task.completedAt.isNotEmpty()) {
                DetailLine(
                    icon = Icons.Filled.History,
                    label = "History",
                    value = listOf(
                        task.createdAt.take(10).takeIf { it.isNotEmpty() }
                            ?.let { "Created $it" },
                        task.completedAt.take(10).takeIf { it.isNotEmpty() }
                            ?.let { if (task.skipped) "Skipped $it" else "Done $it" },
                    ).filterNotNull().joinToString(" · ").ifEmpty { "—" },
                )
            }
            Row(
                horizontalArrangement = Arrangement.spacedBy(8.dp),
                modifier = Modifier.fillMaxWidth(),
            ) {
                Button(
                    onClick = onToggle,
                    enabled = actionsEnabled,
                    modifier = Modifier.weight(1f),
                ) {
                    Text(if (task.isOpen()) "Complete" else "Reopen")
                }
                OutlinedButton(
                    onClick = onEdit,
                    enabled = actionsEnabled,
                    modifier = Modifier.weight(1f),
                ) {
                    Icon(Icons.Filled.Edit, contentDescription = null)
                    Spacer(modifier = Modifier.width(4.dp))
                    Text("Edit")
                }
            }
            // Duplicate sits beside Delete rather than joining the primary
            // row: three buttons on one line is the cramped case, and both
            // of these are secondary.
            Row(
                horizontalArrangement = Arrangement.spacedBy(8.dp),
                modifier = Modifier.fillMaxWidth(),
            ) {
                OutlinedButton(
                    onClick = onDuplicate,
                    enabled = actionsEnabled,
                    modifier = Modifier.weight(1f),
                ) {
                    Icon(Icons.Filled.ContentCopy, contentDescription = null)
                    Spacer(modifier = Modifier.width(4.dp))
                    Text("Duplicate")
                }
                OutlinedButton(
                    onClick = onDelete,
                    enabled = actionsEnabled,
                    colors = ButtonDefaults.outlinedButtonColors(
                        contentColor = MaterialTheme.colorScheme.error,
                    ),
                    modifier = Modifier.weight(1f),
                ) {
                    Icon(Icons.Filled.Delete, contentDescription = null)
                    Spacer(modifier = Modifier.width(4.dp))
                    Text("Delete")
                }
            }
        }
    }
}

/** One icon + label + value entry in the detail sheet. Stacked
 * (label above value) so narrow screens never wrap labels mid-word. */
@Composable
internal fun DetailLine(
    icon: androidx.compose.ui.graphics.vector.ImageVector,
    label: String,
    value: String,
    highlight: Boolean = false,
) {
    Row(
        verticalAlignment = Alignment.Top,
    ) {
        Icon(
            icon,
            contentDescription = null,
            tint = if (highlight) {
                MaterialTheme.colorScheme.error
            } else {
                MaterialTheme.colorScheme.onSurfaceVariant
            },
            modifier = Modifier.padding(top = 2.dp),
        )
        Spacer(modifier = Modifier.width(12.dp))
        Column(modifier = Modifier.weight(1f)) {
            Text(
                label,
                style = MaterialTheme.typography.labelMedium,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            Text(
                value,
                style = MaterialTheme.typography.bodyLarge,
                color = if (highlight) {
                    MaterialTheme.colorScheme.error
                } else {
                    MaterialTheme.colorScheme.onSurface
                },
            )
        }
    }
}

/** New/edit dialog for a server task. Everything except Repeats is
 * required (save stays disabled until all are filled); due date/time are
 * picked, not typed. Single-column layout with scrolling chip rows for
 * tall/narrow screens. */
@Composable
internal fun ServerTaskDialog(
    initial: ServerTaskDraft,
    isNew: Boolean,
    onDismiss: () -> Unit,
    onDelete: (() -> Unit)?,
    onSave: (ServerTaskDraft) -> Unit,
) {
    var name by remember(initial) { mutableStateOf(initial.name) }
    var speakName by remember(initial) { mutableStateOf(initial.speakName) }
    var description by remember(initial) { mutableStateOf(initial.description) }
    var dueDate by remember(initial) { mutableStateOf(initial.dueDate) }
    var dueTime by remember(initial) { mutableStateOf(initial.dueTime) }
    var minutes by remember(initial) { mutableStateOf(initial.estimatedMinutes) }
    var repeatEvery by remember(initial) { mutableStateOf(initial.repeatEvery) }
    var repeatUnit by remember(initial) { mutableStateOf(initial.repeatUnit) }
    var repeatDays by remember(initial) { mutableStateOf(initial.repeatDays) }
    var repeatCustom by remember(initial) { mutableStateOf(initial.repeatCustom) }
    var repeatRule by remember(initial) { mutableStateOf(initial.repeatRule) }
    var parallelable by remember(initial) { mutableStateOf(initial.parallelable) }
    var showDatePicker by remember { mutableStateOf(false) }
    var showTimePicker by remember { mutableStateOf(false) }
    // The recurrence has to parse too: a custom condition with no words,
    // or a count outside 1-28, leaves Save disabled with the reason shown
    // under the fields.
    val repeat = ServerTaskDraft(
        repeatEvery = repeatEvery,
        repeatUnit = repeatUnit,
        repeatDays = repeatDays,
        repeatCustom = repeatCustom,
        repeatRule = repeatRule,
    ).repeatOrNull()
    // The selected-days rule (#304): the due date above is the FIRST
    // occurrence, so it has to be one of the days. Gated here rather than
    // discovered on save — the server rejects the contradiction, and a form
    // that enabled Save anyway would fail after the dialog had already
    // dismissed, leaving the user with nothing to fix.
    val daysOnDueDate = dueDateIsSelectedDay(dueDate, repeatDays)
    val formValid = name.trim().isNotEmpty() &&
        description.trim().isNotEmpty() &&
        dueDate.trim().isNotEmpty() &&
        dueTime.trim().isNotEmpty() &&
        (minutes.trim().toIntOrNull()?.let { it >= 0 } == true) &&
        repeat != null &&
        daysOnDueDate

    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(if (isNew) "New task" else "Edit task") },
        text = {
            Column(
                modifier = Modifier.verticalScroll(rememberScrollState()),
                verticalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                OutlinedTextField(
                    value = name,
                    onValueChange = { name = it },
                    label = { Text("Name *") },
                    singleLine = true,
                    modifier = Modifier.fillMaxWidth(),
                )
                OutlinedTextField(
                    value = speakName,
                    // Bytes, not chars: the builder and the server cap at
                    // SPEAK_NAME_MAX_BYTES bytes, so the field caps the same
                    // way — otherwise a multibyte phrase would shrink on save.
                    // Trim-then-truncate, matching the builder/store/fingerprint
                    // order: truncating first would waste cap on whitespace the
                    // save then trims away. Leading only — trailing space is
                    // still being typed, and the save trims it.
                    onValueChange = { speakName = truncateUtf8Bytes(it.trimStart(), SPEAK_NAME_MAX_BYTES) },
                    label = { Text("Speak as (voice)") },
                    placeholder = { Text("e.g. pay the power bill") },
                    singleLine = true,
                    supportingText = {
                        Text("${speakName.toByteArray(Charsets.UTF_8).size}/$SPEAK_NAME_MAX_BYTES")
                    },
                    modifier = Modifier.fillMaxWidth(),
                )
                OutlinedTextField(
                    value = description,
                    onValueChange = { description = it },
                    label = { Text("Details *") },
                    minLines = 2,
                    modifier = Modifier.fillMaxWidth(),
                )
                FormLabel("Due")
                val dateInteraction = remember { MutableInteractionSource() }
                val timeInteraction = remember { MutableInteractionSource() }
                OutlinedTextField(
                    value = dueDate,
                    onValueChange = {},
                    readOnly = true,
                    label = { Text("Due date *") },
                    placeholder = { Text("Tap to pick") },
                    singleLine = true,
                    interactionSource = dateInteraction,
                    trailingIcon = {
                        if (dueDate.isNotEmpty()) {
                            // An explicit "no due date" also drops the
                            // time: a time silently reappearing when the
                            // next date is picked is more surprising than
                            // losing it. The recreate flow above is the
                            // opposite case on purpose — there the date
                            // is what the user must choose.
                            IconButton(onClick = { dueDate = ""; dueTime = "" }) {
                                Icon(Icons.Filled.Close, contentDescription = "Clear due date and time")
                            }
                        } else {
                            Icon(Icons.Filled.DateRange, contentDescription = null)
                        }
                    },
                    modifier = Modifier.fillMaxWidth(),
                )
                OutlinedTextField(
                    value = dueTime,
                    onValueChange = {},
                    readOnly = true,
                    enabled = dueDate.isNotEmpty(),
                    label = { Text("Time *") },
                    placeholder = { Text("Pick a date first") },
                    singleLine = true,
                    interactionSource = timeInteraction,
                    trailingIcon = {
                        if (dueTime.isNotEmpty()) {
                            IconButton(onClick = { dueTime = "" }) {
                                Icon(Icons.Filled.Close, contentDescription = "Clear due time")
                            }
                        } else {
                            Icon(Icons.Filled.AccessTime, contentDescription = null)
                        }
                    },
                    modifier = Modifier.fillMaxWidth(),
                )
                // Recreate draft: a kept time with no date yet is the one
                // state the labels don't explain on their own, so say it
                // here rather than only in a toast that can be missed.
                if (dueDate.isEmpty() && dueTime.isNotEmpty()) {
                    HintLine("Time kept from the completed task — pick a date to keep it.")
                }
                // Tapping a read-only field opens its picker (the trailing
                // icon only clears); Release avoids firing while scrolling.
                LaunchedEffect(dateInteraction) {
                    dateInteraction.interactions.collect {
                        if (it is PressInteraction.Release) showDatePicker = true
                    }
                }
                LaunchedEffect(timeInteraction) {
                    timeInteraction.interactions.collect {
                        if (it is PressInteraction.Release && dueDate.isNotEmpty()) showTimePicker = true
                    }
                }
                FormLabel("Estimate")
                OutlinedTextField(
                    value = minutes,
                    onValueChange = { minutes = it.filter { c -> c.isDigit() } },
                    label = { Text("Minutes *") },
                    placeholder = { Text("30") },
                    singleLine = true,
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number),
                    modifier = Modifier.fillMaxWidth(),
                )
                Row(
                    modifier = Modifier.horizontalScroll(rememberScrollState()),
                    horizontalArrangement = Arrangement.spacedBy(8.dp),
                ) {
                    for (m in listOf("5", "15", "30", "60")) {
                        FilterChip(
                            selected = minutes == m,
                            onClick = { minutes = m },
                            label = { Text(m) },
                        )
                    }
                }
                // Recurrence: a real cadence (count + unit, or a set of days) or
                // the user's own words, never a mix — the server rejects the
                // combination. A blank count and no selected days is the
                // one-shot case, so the unit chips only light up once there is
                // a count to apply them to.
                FormLabel("Repeats")
                Row(
                    horizontalArrangement = Arrangement.spacedBy(8.dp),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    OutlinedTextField(
                        value = repeatEvery,
                        onValueChange = { input ->
                            repeatEvery = input.filter { c -> c.isDigit() }.take(2)
                        },
                        label = { Text("Every") },
                        placeholder = { Text("Never") },
                        singleLine = true,
                        // Disabled while the selection is the active mode, the
                        // same way it is disabled for a custom condition: two
                        // cadences cannot both be the answer, and a field that
                        // accepted input the server would ignore is worse than
                        // one that plainly does not.
                        enabled = !repeatCustom && repeatDays.isEmpty(),
                        keyboardOptions = KeyboardOptions(
                            keyboardType = KeyboardType.Number),
                        modifier = Modifier.width(112.dp),
                    )
                    Row(
                        modifier = Modifier
                            .weight(1f)
                            .horizontalScroll(rememberScrollState()),
                        horizontalArrangement = Arrangement.spacedBy(8.dp),
                    ) {
                        val on = !repeatCustom && repeatDays.isEmpty() && repeatEvery.isNotEmpty()
                        for (unit in REPEAT_UNITS) {
                            FilterChip(
                                selected = on && repeatUnit == unit,
                                enabled = !repeatCustom && repeatDays.isEmpty(),
                                onClick = {
                                    repeatUnit = unit
                                    // Picking a unit with no count means
                                    // every single one of them.
                                    if (repeatEvery.isEmpty()) repeatEvery = "1"
                                },
                                label = { Text(unit.removeSuffix("s").replaceFirstChar { it.uppercase() }) },
                            )
                        }
                    }
                }
                // "Select days" (#304): a peer of the count above, not a variant
                // of the custom condition — the server rolls this cadence over
                // itself, exactly like "every N days". Picking a day clears the
                // count, because a task carrying both is the one thing the
                // server refuses; tap it again to take it back out.
                Row(
                    modifier = Modifier
                        .fillMaxWidth()
                        .horizontalScroll(rememberScrollState()),
                    horizontalArrangement = Arrangement.spacedBy(8.dp),
                ) {
                    for (day in REPEAT_DAYS) {
                        FilterChip(
                            selected = repeatDays.contains(day),
                            enabled = !repeatCustom,
                            onClick = {
                                repeatDays = if (repeatDays.contains(day)) {
                                    repeatDays - day
                                } else {
                                    // Selecting a day also leaves the custom
                                    // mode: the chips and the custom words are
                                    // two answers to one question, and leaving
                                    // the checkbox ticked would save the words
                                    // and silently drop the days.
                                    repeatCustom = false
                                    repeatEvery = ""
                                    canonicalRepeatDays(repeatDays + day)
                                }
                            },
                            label = { Text(dayLabel(day)) },
                        )
                    }
                }
                Row(
                    modifier = Modifier
                        .fillMaxWidth()
                        .toggleable(
                            value = repeatCustom,
                            role = Role.Checkbox,
                            onValueChange = {
                                repeatCustom = it
                                // Entering the custom mode drops the selected
                                // days: the chips are disabled from here on, so
                                // leaving them lit would show a selection the
                                // saved task does not have.
                                if (it) repeatDays = emptyList()
                            },
                        ),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    Checkbox(
                        checked = repeatCustom,
                        onCheckedChange = null,
                    )
                    Text(
                        "Custom condition",
                        style = MaterialTheme.typography.bodyMedium,
                    )
                }
                OutlinedTextField(
                    value = repeatRule,
                    onValueChange = { repeatRule = it },
                    label = { Text("Custom condition *") },
                    placeholder = { Text("mon-fri only, every 3rd Friday…") },
                    singleLine = true,
                    enabled = repeatCustom,
                    modifier = Modifier.fillMaxWidth(),
                )
                if (repeatCustom && repeatRule.isBlank()) {
                    HintLine("A custom condition needs the words.")
                } else if (repeatCustom) {
                    // The date above is the first occurrence; the words only
                    // say what comes after it. Worth saying: the two sit far
                    // apart in the form and read like they compete.
                    HintLine("The due date you picked is the first one — this is the rule for the ones after it.")
                } else if (repeatDays.isNotEmpty() && !daysOnDueDate) {
                    // The one state the chips cannot explain, and the reason
                    // Save is disabled: the date is the first occurrence, so it
                    // has to be one of the days picked.
                    HintLine("The due date must be one of the selected days.")
                } else if (repeatDays.isNotEmpty()) {
                    HintLine("Repeats weekly on the selected days.")
                } else if (!repeatCustom && repeatEvery.trim().toIntOrNull()
                    ?.let { it < REPEAT_EVERY_MIN || it > REPEAT_EVERY_MAX } == true
                ) {
                    HintLine("Every $REPEAT_EVERY_MIN-$REPEAT_EVERY_MAX only.")
                }
                if (!formValid) {
                    HintLine("Fill all * fields to enable Save.")
                }
                Row(
                    modifier = Modifier.fillMaxWidth()
                        .toggleable(
                            value = parallelable,
                            role = Role.Checkbox,
                            onValueChange = { parallelable = it },
                        ),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    Checkbox(
                        checked = parallelable,
                        onCheckedChange = null,
                    )
                    Column {
                        Text(
                            "Can run in parallel",
                            style = MaterialTheme.typography.bodyMedium,
                        )
                        Text(
                            "Runs alongside other tasks",
                            style = MaterialTheme.typography.bodySmall,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                }
            }
        },
        confirmButton = {
            TextButton(
                onClick = {
                    onSave(
                        initial.copy(
                            name = name.trim(),
                            speakName = speakName.trim(),
                            description = description.trim(),
                            dueDate = dueDate.trim(),
                            dueTime = dueTime.trim(),
                            estimatedMinutes = minutes.trim(),
                            // Written from the PARSED recurrence, not the raw
                            // widgets: the update path re-parses this draft and
                            // compares it with what the dialog opened on, so a
                            // "0" left in a one-shot's count would re-parse as an
                            // out-of-range number and read as a change.
                            repeatEvery = if (repeat!!.every > 0) repeat!!.every.toString() else "",
                            repeatUnit = repeat!!.unit,
                            repeatDays = repeat!!.days,
                            repeatCustom = repeat!!.custom,
                            repeatRule = repeat!!.text,
                            parallelable = parallelable,
                        )
                    )
                },
                // Save stays gated on validity alone: the dialog
                // dismisses instantly on save and the request runs in the
                // background (#288), so there is no in-flight state to gate
                // on here.
                enabled = formValid,
            ) { Text("Save") }
        },
        dismissButton = {
            Row {
                if (onDelete != null) {
                    TextButton(onClick = onDelete) { Text("Delete") }
                }
                TextButton(onClick = onDismiss) { Text("Cancel") }
            }
        },
    )
    if (showDatePicker) {
        DueDatePickerDialog(
            initial = dueDate,
            onConfirm = { dueDate = it; showDatePicker = false },
            onDismiss = { showDatePicker = false },
        )
    }
    if (showTimePicker) {
        DueTimePickerDialog(
            initial = dueTime,
            onConfirm = { dueTime = it; showTimePicker = false },
            onDismiss = { showTimePicker = false },
        )
    }
}

/** Small section header inside the task dialog form. */
@Composable
internal fun FormLabel(text: String) {
    Text(
        text,
        style = MaterialTheme.typography.labelLarge,
        color = MaterialTheme.colorScheme.onSurfaceVariant,
        modifier = Modifier.padding(top = 4.dp),
    )
}

/** Material3 date picker returning YYYY-MM-DD. The picker speaks
 * UTC-midnight millis, so seed and read back in UTC — seeding IST
 * midnight shifts the highlight a day back on non-IST devices. */
@Composable
internal fun DueDatePickerDialog(
    initial: String,
    onConfirm: (String) -> Unit,
    onDismiss: () -> Unit,
) {
    val initialMillis = remember(initial) {
        val day = runCatching { java.time.LocalDate.parse(initial.trim()) }.getOrNull()
            ?: java.time.LocalDate.now(IST)
        day.atStartOfDay(java.time.ZoneOffset.UTC).toInstant().toEpochMilli()
    }
    val state = rememberDatePickerState(initialSelectedDateMillis = initialMillis)
    DatePickerDialog(
        onDismissRequest = onDismiss,
        confirmButton = {
            TextButton(onClick = {
                val picked = state.selectedDateMillis?.let {
                    java.time.Instant.ofEpochMilli(it).atZone(java.time.ZoneOffset.UTC).toLocalDate().toString()
                } ?: java.time.LocalDate.now(IST).toString()
                onConfirm(picked)
            }) { Text("OK") }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    ) {
        DatePicker(state = state)
    }
}

/** Material3 24-hour time picker returning HH:MM; defaults to 09:00. */
@Composable
internal fun DueTimePickerDialog(
    initial: String,
    onConfirm: (String) -> Unit,
    onDismiss: () -> Unit,
) {
    val (initHour, initMinute) = remember(initial) {
        val parts = initial.trim().split(":")
        val h = parts.getOrNull(0)?.toIntOrNull()?.takeIf { it in 0..23 } ?: 9
        val m = parts.getOrNull(1)?.toIntOrNull()?.takeIf { it in 0..59 } ?: 0
        h to m
    }
    val state = rememberTimePickerState(
        initialHour = initHour,
        initialMinute = initMinute,
        is24Hour = true,
    )
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Due time") },
        text = { TimePicker(state = state) },
        confirmButton = {
            TextButton(onClick = {
                onConfirm("%02d:%02d".format(Locale.US, state.hour, state.minute))
            }) { Text("OK") }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}

// ── The change marker this device last saw (#178) ────────────────────────────
//
// Deliberately NOT in SettingsBackup. A restored backup would claim a
// revision the new device has never observed, and the first resume would
// then compare the server against a number imported from another phone —
// concluding "changed" and refetching, at best, or "unchanged" and showing a
// list that has never been fetched here, at worst. The cursor is this
// install's own business, so it starts at 0 on every install.
private const val CHANGE_CURSOR_KEY = "task_change_revision"

private fun changePrefs(context: Context) =
    context.applicationContext.getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)

/** The revision this device last observed, or 0 if it has never looked. */
private fun lastSeenChangeRevision(context: Context): Long =
    changePrefs(context).getLong(CHANGE_CURSOR_KEY, 0L)

/** Remember the revision we have now seen. */
private fun noteChangeRevision(context: Context, revision: Long) {
    changePrefs(context).edit().putLong(CHANGE_CURSOR_KEY, revision).apply()
}

private const val LAST_SYNCED_KEY = "tasks_last_synced_at"

/** When the task list was last fetched, or 0 before the first success. */
private fun readLastSyncedAt(context: Context): Long =
    context.applicationContext
        .getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
        .getLong(LAST_SYNCED_KEY, 0L)

/** Persist the last fetch; failures never call this, so a failed load
 * leaves the old stamp untouched. */
private fun writeLastSyncedAt(context: Context, at: Long) {
    context.applicationContext
        .getSharedPreferences(AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
        .edit().putLong(LAST_SYNCED_KEY, at).apply()
}

/**
 * Relative last-synced label (#288): "just now" under a minute, "Xm ago"
 * under an hour, "Xh ago" beyond. Null before the first successful load —
 * the count line shows no stamp rather than an invented one.
 */
internal fun formatLastSynced(syncedAt: Long, nowMillis: Long): String? {
    if (syncedAt <= 0) return null
    val mins = ((nowMillis - syncedAt).coerceAtLeast(0L) / 60_000L)
    return when {
        mins < 1 -> "just now"
        mins < 60 -> "${mins}m ago"
        else -> "${mins / 60}h ago"
    }
}

/** True when the failure is a revision race: someone else wrote first. */
internal fun isConflict(e: Throwable): Boolean =
    "HTTP 409" in (e.message ?: "")
