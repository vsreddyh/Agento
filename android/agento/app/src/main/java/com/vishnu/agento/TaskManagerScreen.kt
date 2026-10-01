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
import java.util.Locale
import kotlinx.coroutines.delay
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
    val snackbar = remember { SnackbarHostState() }
    // One client for the screen (its OkHttpClient is shared process-wide).
    val api = remember(context) { TasksApi(context) }
    var tasks by remember { mutableStateOf<List<ServerTask>>(emptyList()) }
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
    // Editor draft (id empty = new task) and delete target.
    var editing by remember { mutableStateOf<ServerTaskDraft?>(null) }
    var deleting by remember { mutableStateOf<ServerTask?>(null) }
    var busy by remember { mutableStateOf(false) }
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
        else scope.launch { snackbar.showSnackbar("Notifications denied — reminders stay off.") }
    }

    // First successful load also (re)arms due-time alarms, covering
    // agent-side changes made while the app was closed.
    var scheduledOnce by remember { mutableStateOf(false) }

    LaunchedEffect(filter, refreshTick) {
        loading = true
        error = ""
        api.list(filter.state).fold(
            onSuccess = { page ->
                tasks = page.tasks
                listTruncated = page.truncated
                if (!scheduledOnce) {
                    scheduledOnce = true
                    TaskReminders.refresh(context)
                }
            },
            // The flag describes the rows on screen, so it resets with
            // them: a failed fetch leaves yesterday's rows up, and they
            // must not keep advertising yesterday's truncation.
            onFailure = { e ->
                error = serverDetail(e.message ?: e.javaClass.simpleName)
                listTruncated = false
            },
        )
        loading = false
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
        scope.launch {
            snackbar.showSnackbar(serverDetail(e.message ?: e.javaClass.simpleName))
        }
    }

    // Push fresh counts to the home-screen widget (#101) after mutations,
    // and re-arm due-time alarms (completions/deletions/edits change them).
    fun pokeWidget() {
        TaskWidget.refresh(context)
        TaskReminders.refresh(context)
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
     * name, description, due date/time, estimate, the whole repeat block
     * and the parallel flag — as a new **open** task. Nothing is "smarter"
     * than the original: a past due date stays a past due date, and a
     * cadence comes across as the same cadence. A copy is a starting point
     * to edit, not a rescheduled twin.
     */
    fun doDuplicate(t: ServerTask) {
        // Everything the client itself enforces is checked here, before the
        // sheet is dismissed: a refusal that closes the task leaves the user
        // with a snackbar and nothing to fix, because the thing they needed
        // to change is behind a tap they just made.
        val missing = listOf(
            "a name" to t.name.isBlank(),
            "some details" to t.description.isBlank(),
            "a due date" to !t.dueDate.isIsoDate(),
            "a due time" to t.dueTime.isBlank(),
        ).filter { it.second }.map { it.first }
        if (missing.isNotEmpty()) {
            scope.launch {
                snackbar.showSnackbar(
                    "This task has no ${missing.joinToString(" or ")} yet — add " +
                        "${if (missing.size == 1) "it" else "them"} before copying it.")
            }
            return
        }
        busy = true
        scope.launch {
            api.create(
                name = t.name,
                description = t.description,
                dueDate = t.dueDate,
                dueTime = t.dueTime,
                estimatedMinutes = t.estimatedMinutes,
                repeatEvery = t.repeatEvery,
                repeatUnit = t.repeatUnit,
                repeatCustom = t.repeatCustom,
                repeatRule = t.repeatRule,
                parallelable = t.parallelable,
            ).fold(
                onSuccess = { copy ->
                    editing = null
                    // Closed here rather than by the caller, so a task
                    // that cannot be copied stays open to be fixed.
                    selected = null
                    refreshTick++
                    pokeWidget()
                    snackbar.showSnackbar("Copied \u201c${copy.name}\u201d.")
                },
                onFailure = ::fail,
            )
            busy = false
        }
    }

    fun doComplete(t: ServerTask) {
        busy = true
        scope.launch {
            api.complete(t.id).fold(
                onSuccess = { done ->
                    refreshTick++
                    pokeWidget()
                    // A structured cadence ("every 3 days") rolled itself
                    // over server-side — the next occurrence already exists,
                    // with the date advanced and every other field carried
                    // over. Nothing to ask the user for.
                    val next = done?.nextDueDate.orEmpty()
                    if (next.isNotEmpty()) {
                        // "Tomorrow" / "2 Oct" rather than a raw ISO date.
                        val when2 = friendlyDue(
                            next, "", java.time.LocalDate.now(IST)).ifEmpty { next }
                        snackbar.showSnackbar("Task completed — next one set for $when2.")
                        return@fold
                    }
                    // A custom condition ("every 3rd Friday", "end of every
                    // month") is nobody's to compute but ours, so the
                    // prefilled draft asks for the date — unless an editor
                    // is already open, which must not be discarded. Dismiss
                    // to skip. One-shot tasks land here too and are skipped.
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
                            repeatCustom = t.repeatCustom,
                            repeatRule = t.repeatRule,
                            parallelable = t.parallelable,
                        )
                        // Legacy tasks (created before due_time was
                        // required) have no time to keep — don't claim
                        // there is one.
                        snackbar.showSnackbar(
                            if (t.dueTime.isBlank()) {
                                "Task completed — pick a new date and time to recreate it."
                            } else {
                                "Task completed — pick a new date to recreate it (time kept)."
                            },
                        )
                    } else {
                        snackbar.showSnackbar("Task completed.")
                    }
                },
                onFailure = ::fail,
            )
            busy = false
        }
    }

    fun doReopen(t: ServerTask) {
        busy = true
        scope.launch {
            api.reopen(t.id).fold(
                onSuccess = { refreshTick++; pokeWidget() },
                onFailure = ::fail,
            )
            busy = false
        }
    }

    fun doDelete(t: ServerTask) {
        busy = true
        scope.launch {
            api.delete(t.id).fold(
                onSuccess = {
                    deleting = null
                    editing = null
                    refreshTick++
                    pokeWidget()
                    snackbar.showSnackbar("Task deleted.")
                },
                onFailure = ::fail,
            )
            busy = false
        }
    }

    Scaffold(
        snackbarHost = { SnackbarHost(snackbar) },
        topBar = {
            TopAppBar(
                navigationIcon = {
                    IconButton(onClick = onMenu) {
                        Icon(Icons.Filled.Menu, contentDescription = "Menu")
                    }
                },
                title = { Text("Task Manager") },
                actions = {
                    IconButton(
                        onClick = ::onBell,
                        enabled = !busy,
                    ) {
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
                    IconButton(
                        onClick = { editing = ServerTaskDraft() },
                        enabled = !busy,
                    ) {
                        Icon(Icons.Filled.Add, contentDescription = "New task")
                    }
                    IconButton(
                        onClick = { refreshTick++ },
                        enabled = !busy,
                    ) {
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
                if (busy) {
                    LinearProgressIndicator(modifier = Modifier.fillMaxWidth())
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
                    Text(
                        "${visible.size} of ${tasks.size}" +
                            // No narrowing promise: search filters the fetched
                            // page client-side, so past the cap it cannot
                            // find what was never fetched (#168 follow-up is
                            // wiring ?search= end to end).
                            if (listTruncated) " — showing the first ${tasks.size}" else "",
                        style = MaterialTheme.typography.labelMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                    // Time sections (skipped on the Done filter, where due
                    // buckets carry no meaning); within a section the
                    // current sort still applies. IST-pinned (#124).
                    //
                    // The today's buckets are relative, so they have to move
                    // with the wall clock: a minute ticker drives the
                    // remember key, and one `now` is read per pass so every
                    // row in the same frame is decided from the same instant.
                    // The ticker only runs while the time groups are on
                    // screen; on the Done filter and with grouping off,
                    // nothing here is bucketed and a minute of recomposition
                    // would be wasted.
                    val bucketing = groupByDay && filter != ServerTaskFilter.Done
                    var clockTick by remember { mutableIntStateOf(0) }
                    LaunchedEffect(bucketing) {
                        while (bucketing) {
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
                                ServerTaskRow(
                                    task = t,
                                    // Same instant the groups were built
                                    // from, so a row can never be coloured
                                    // overdue inside a group that says
                                    // otherwise.
                                    overdue = t.isOverdue(today, clockKey),
                                    today = day,
                                    actionsEnabled = !busy,
                                    onOpen = { selected = t },
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
                // Widget display settings moved to Settings -> Home-screen
                // widget: they configure the home screen, not this task
                // list, and they were the only controls below the rows.
                HintLine("Your tasks, shared with your assistant — changes here and in chat land in the same list.")
            }
        }
    }

    val draft = editing
    if (draft != null) {
        ServerTaskDialog(
            initial = draft,
            isNew = draft.id.isEmpty(),
            busy = busy,
            onDismiss = { editing = null },
            onDelete = if (draft.id.isEmpty()) null else ({
                editing = null
                deleting = tasks.firstOrNull { it.id == draft.id }
            }),
            onSave = { next ->
                busy = true
                scope.launch {
                    val mins = next.estimatedMinutes.trim().toIntOrNull()
                    if (mins == null || mins < 0) {
                        snackbar.showSnackbar("Estimated minutes is required (a number 0 or above).")
                        busy = false
                        return@launch
                    }
                    if (next.id.isEmpty()) {
                        api.create(
                            name = next.name,
                            description = next.description,
                            dueDate = next.dueDate,
                            dueTime = next.dueTime,
                            estimatedMinutes = mins,
                            repeatEvery = next.repeatOrNull()?.first ?: 0,
                            repeatUnit = next.repeatOrNull()?.second.orEmpty(),
                            repeatCustom = next.repeatOrNull()?.third?.isNotEmpty() == true,
                            repeatRule = next.repeatOrNull()?.third.orEmpty(),
                            parallelable = next.parallelable,
                        ).fold(
                            onSuccess = { editing = null; refreshTick++; pokeWidget() },
                            onFailure = ::fail,
                        )
                    } else {
                        // Only resend the recurrence when it actually
                        // changed: an unrelated edit (a name tweak) must
                        // not read as a repeat rewrite, and a task edited
                        // by an older build keeps whatever cadence it has.
                        val was = editing?.repeatOrNull()
                        val now = next.repeatOrNull()
                        val repChanged = was != now
                        // The revision this editor read: an agent edit that
                        // landed while the dialog was open rejects here with
                        // 409 instead of being silently overwritten (#184).
                        val readRevision = tasks.firstOrNull { it.id == next.id }?.revision
                        api.update(
                            id = next.id,
                            name = next.name,
                            description = next.description,
                            dueDate = next.dueDate,
                            dueTime = next.dueTime,
                            estimatedMinutes = mins,
                            repeatEvery = if (repChanged) now?.first else null,
                            repeatUnit = if (repChanged) now?.second else null,
                            repeatCustom = if (repChanged) {
                                now?.third?.isNotEmpty() == true
                            } else {
                                null
                            },
                            repeatRule = if (repChanged) now?.third else null,
                            parallelable = next.parallelable,
                            expectedRevision = readRevision,
                        ).fold(
                            onSuccess = { editing = null; refreshTick++; pokeWidget() },
                            onFailure = ::fail,
                        )
                    }
                    busy = false
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
                TextButton(onClick = { doDelete(target) }, enabled = !busy) { Text("Delete") }
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
            actionsEnabled = !busy,
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
    onOpen: () -> Unit,
    onToggle: () -> Unit,
) {
    val done = !task.isOpen()
    Row(
        modifier = Modifier.fillMaxWidth()
            .clickable(onClick = onOpen)
            .padding(horizontal = 4.dp, vertical = 8.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        IconButton(onClick = onToggle, enabled = actionsEnabled) {
            if (task.isOpen()) {
                Icon(
                    Icons.Filled.RadioButtonUnchecked,
                    contentDescription = "Complete task",
                    tint = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.size(26.dp),
                )
            } else {
                Icon(
                    Icons.Filled.CheckCircle,
                    contentDescription = "Reopen task",
                    tint = MaterialTheme.colorScheme.primary,
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
                color = if (done) {
                    MaterialTheme.colorScheme.onSurfaceVariant
                } else {
                    MaterialTheme.colorScheme.onSurface
                },
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
                        "Done",
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
                            ?.let { "Done $it" },
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
    busy: Boolean,
    onDismiss: () -> Unit,
    onDelete: (() -> Unit)?,
    onSave: (ServerTaskDraft) -> Unit,
) {
    var name by remember(initial) { mutableStateOf(initial.name) }
    var description by remember(initial) { mutableStateOf(initial.description) }
    var dueDate by remember(initial) { mutableStateOf(initial.dueDate) }
    var dueTime by remember(initial) { mutableStateOf(initial.dueTime) }
    var minutes by remember(initial) { mutableStateOf(initial.estimatedMinutes) }
    var repeatEvery by remember(initial) { mutableStateOf(initial.repeatEvery) }
    var repeatUnit by remember(initial) { mutableStateOf(initial.repeatUnit) }
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
        repeatCustom = repeatCustom,
        repeatRule = repeatRule,
    ).repeatOrNull()
    val formValid = name.trim().isNotEmpty() &&
        description.trim().isNotEmpty() &&
        dueDate.trim().isNotEmpty() &&
        dueTime.trim().isNotEmpty() &&
        (minutes.trim().toIntOrNull()?.let { it >= 0 } == true) &&
        repeat != null

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
                // here rather than only in a snackbar that can be missed.
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
                // Recurrence: a real cadence (count + unit) or the user's
                // own words, never both — the server rejects the mix. A
                // blank count is the one-shot case, so the unit chips only
                // light up once there is a count to apply them to.
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
                        enabled = !repeatCustom,
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
                        val on = !repeatCustom && repeatEvery.isNotEmpty()
                        for (unit in REPEAT_UNITS) {
                            FilterChip(
                                selected = on && repeatUnit == unit,
                                enabled = !repeatCustom,
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
                Row(
                    modifier = Modifier
                        .fillMaxWidth()
                        .toggleable(
                            value = repeatCustom,
                            role = Role.Checkbox,
                            onValueChange = { repeatCustom = it },
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
                            description = description.trim(),
                            dueDate = dueDate.trim(),
                            dueTime = dueTime.trim(),
                            estimatedMinutes = minutes.trim(),
                            repeatEvery = repeat!!.first.toString(),
                            repeatUnit = repeat!!.second,
                            repeatCustom = repeat!!.third.isNotEmpty(),
                            repeatRule = repeat!!.third,
                            parallelable = parallelable,
                        )
                    )
                },
                enabled = !busy && formValid,
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
