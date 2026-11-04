@file:OptIn(ExperimentalMaterial3Api::class, ExperimentalFoundationApi::class)

package com.vishnu.agento

/**
 * Legacy projects board (fixed statuses, local tasks.json import) — the old surface, kept whole (#163, last of four moves).
 *
 * Moved verbatim out of MainActivity.kt; the only changes are
 * visibility (`private` to `internal`, same package) and the import
 * list, which carries exactly what this file uses.
 */
import android.content.Context
import androidx.compose.foundation.ExperimentalFoundationApi
import androidx.compose.foundation.combinedClickable
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.grid.GridCells
import androidx.compose.foundation.lazy.grid.LazyVerticalGrid
import androidx.compose.foundation.lazy.grid.items
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.Edit
import androidx.compose.material.icons.filled.List
import androidx.compose.material.icons.filled.Menu
import androidx.compose.material.icons.filled.MoreVert
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material.icons.filled.Sort
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.hapticfeedback.HapticFeedbackType
import androidx.lifecycle.compose.LifecycleResumeEffect
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalHapticFeedback
import androidx.compose.ui.unit.dp
import androidx.core.content.edit
import java.util.Locale
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
/** Fixed task statuses (#55). Order here is the default sort order:
 * Ongoing → Testing → Blocked → Paused → Todo → Done. */
internal val TASK_STATUSES = listOf("Ongoing", "Testing", "Blocked", "Paused", "Todo", "Done")

internal fun taskRank(status: String): Int =
    TASK_STATUSES.indexOf(status).let { if (it < 0) TASK_STATUSES.indexOf("Todo") else it }

/** Maps legacy free-text statuses onto the fixed set (#55). */
internal fun normalizeStatus(raw: String): String = when (raw.trim().lowercase(Locale.ROOT)) {
    "doing", "ongoing", "in progress", "in_progress" -> "Ongoing"
    "testing", "test", "tests", "qa", "in testing" -> "Testing"
    "blocked", "block", "stuck", "on hold" -> "Blocked"
    "paused", "pause", "pasued" -> "Paused"
    "done", "complete", "completed" -> "Done"
    else -> "Todo"
}

/** One-time read of the retired local tasks.json (#104): name/status/note
 * triples for server import. Lenient — a corrupt file imports as empty.
 * Capped at 500 to match the server MaxLimit used by the migration calls. */
internal fun loadLegacyTasks(context: android.content.Context): List<Triple<String, String, String>> {
    val f = java.io.File(context.filesDir, "tasks.json")
    if (!f.exists()) return emptyList()
    return runCatching {
        val arr = org.json.JSONArray(f.readText())
        List(arr.length()) { i ->
            val o = arr.optJSONObject(i) ?: return@List null
            Triple(o.optString("name"), o.optString("status"), o.optString("note"))
        }.mapNotNull { it }
            .filter { it.first.isNotBlank() }
            .take(500)
    }.getOrDefault(emptyList())
}

internal enum class TaskSort(val title: String) {
    Default("Status"),
    Name("Name"),
    Newest("Newest"),
    Oldest("Oldest"),
}

/** Project board: fixed statuses, filters + sorts, backed by the shared
 * `projects` collection over /api/projects (#104 — retired local
 * tasks.json). The agent manages the same rows over the project-manager
 * MCP. First launch imports any local rows missing on the server. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
internal fun TasksScreen(wc: WindowClass, onMenu: () -> Unit = {}) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    val toasts = remember(scope) { ToastCenter(scope) }
    // One client for the screen (its OkHttpClient is shared process-wide).
    val api = remember(context) { ProjectsApi(context) }
    var projects by remember { mutableStateOf<List<ServerProject>>(emptyList()) }
    var editing by remember { mutableStateOf<ServerProject?>(null) }
    var loading by remember { mutableStateOf(true) }
    var error by remember { mutableStateOf("") }
    /** Rows the last successful parse skipped (#430, #447 review): a partial
     * list must say so instead of ending silently mid-collection. Reset on
     * every load so a stale count can't outlive its fetch. */
    var dropped by remember { mutableIntStateOf(0) }
    /** Whether the last successful fetch hit the server cap (#447 review):
     * without this the board silently ends mid-collection past the cap. */
    var truncated by remember { mutableStateOf(false) }
    // Bumped after every load/mutation so the loader below reruns.
    var refreshTick by remember { mutableIntStateOf(0) }
    var filter by remember { mutableStateOf("All") }
    var sort by remember { mutableStateOf(TaskSort.Default) }
    var sortOpen by remember { mutableStateOf(false) }
    var busy by remember { mutableStateOf(false) }

    fun fail(e: Throwable) {
        toasts.toast(serverDetail(e.message ?: e.javaClass.simpleName), ToastKind.Error)
    }

    fun toast(msg: String, kind: ToastKind = ToastKind.Info) {
        toasts.toast(msg, kind)
    }

    fun cycleStatus(t: ServerProject): String {
        val next = when (normalizeStatus(t.status)) {
            "Todo" -> "Ongoing"
            "Ongoing" -> "Testing"
            "Testing" -> "Blocked"
            "Blocked" -> "Paused"
            "Paused" -> "Done"
            else -> "Todo"
        }
        return next
    }

    // One-time import of the retired local tasks.json (#104): uploads rows
    // missing on the server (name match is trimmed + case-insensitive, and
    // successes join the set as they land so a retry never duplicates).
    // The file is deleted only after every legacy row is confirmed on the
    // server — a failed run leaves everything in place and retries next
    // launch.
    LaunchedEffect(Unit) {
        val prefs = context.getSharedPreferences(
            AgentoApp.PREFS_NAME, android.content.Context.MODE_PRIVATE)
        if (!prefs.getBoolean("projects_migrated", false)) {
            val legacy = withContext(Dispatchers.IO) { loadLegacyTasks(context) }
            if (legacy.isEmpty()) {
                withContext(Dispatchers.IO) {
                    java.io.File(context.filesDir, "tasks.json").delete()
                }
                prefs.edit {
                    putBoolean("projects_migrated", true)
                }
            } else {
                val norm = { s: String -> s.trim().lowercase(Locale.ROOT) }
                // Limit 500 (server max): the default 200 would see an
                // incomplete board past 200 rows and duplicate/stall.
                // A failed list aborts the run (#447 review): the strict
                // envelopes fail loudly, and reading that failure as an
                // empty board would re-create every legacy row as a
                // duplicate — then fail the landed check and do it again on
                // every launch. The file stays, the flag stays unset, the
                // next launch retries.
                val haveList = api.list("all", limit = 500).getOrNull()
                    ?: return@LaunchedEffect
                // A capped page is not the whole board (#447 review): names
                // past the cutoff would read as missing and get duplicated,
                // then fail the landed check below forever. Abort like a
                // failed fetch — the file stays, the flag stays unset.
                if (haveList.truncated) return@LaunchedEffect
                val have = haveList.projects
                    .map { norm(it.name) }.toMutableSet()
                for ((name, status, note) in legacy) {
                    if (norm(name) in have) continue
                    val created = api.create(name, normalizeStatus(status), note).getOrNull()
                    if (created == null) break
                    have.add(norm(created.name))
                }
                val landedList = api.list("all", limit = 500).getOrNull()
                    ?: return@LaunchedEffect
                // Same capped-page guard as the have-check above: verifying
                // against a truncated list could delete the file while
                // legacy names sit past the cutoff, unmigrated forever.
                if (landedList.truncated) return@LaunchedEffect
                val landed = landedList.projects
                    .map { norm(it.name) }.toSet()
                if (legacy.all { norm(it.first) in landed }) {
                    withContext(Dispatchers.IO) {
                        java.io.File(context.filesDir, "tasks.json").delete()
                    }
                    prefs.edit {
                        putBoolean("projects_migrated", true)
                    }
                    refreshTick++
                }
            }
        }
    }

    fun load(silent: Boolean = false) {
        if (!silent) {
            loading = true
            error = ""
        }
        scope.launch {
            api.list(if (filter == "All") "all" else filter).fold(
                onSuccess = { projects = it.projects; dropped = it.dropped; truncated = it.truncated },
                onFailure = { e ->
                    val msg = serverDetail(e.message ?: e.javaClass.simpleName)
                    // A failed refresh over cached rows still surfaces: stale
                    // data with no error affordance hides outages. The dropped
                    // count stays: it describes the rows on screen (last
                    // success), not the failed fetch — same rule as the Task
                    // Manager list.
                    if (projects.isEmpty()) error = msg else toast(msg, ToastKind.Error)
                },
            )
            if (!silent) loading = false
        }
    }

    LaunchedEffect(filter, refreshTick) { load() }

    // Silent refresh on return (#447 review): same rule as the Scheduler —
    // a gateway outage between backgrounding and foregrounding must not
    // leave pre-outage rows on screen with no signal.
    LifecycleResumeEffect(Unit) {
        if (!loading) {
            val job = scope.launch { load(silent = true) }
            onPauseOrDispose { job.cancel() }
        } else {
            onPauseOrDispose { }
        }
    }

    fun doCycle(t: ServerProject) {
        busy = true
        scope.launch {
            api.update(t.id, status = cycleStatus(t)).fold(
                onSuccess = { refreshTick++; toast(Toasts.TASK_SAVED, ToastKind.Success) },
                onFailure = ::fail,
            )
            busy = false
        }
    }

    fun doDelete(t: ServerProject) {
        busy = true
        scope.launch {
            api.delete(t.id).fold(
                onSuccess = { refreshTick++; toast(Toasts.TASK_DELETED, ToastKind.Success) },
                onFailure = ::fail,
            )
            busy = false
        }
    }

    val visible = projects
        .let { list ->
            when (sort) {
                TaskSort.Name -> list.sortedBy { it.name.lowercase(Locale.ROOT) }
                TaskSort.Newest -> list.sortedByDescending { projectTimeMs(it.updatedAt) }
                TaskSort.Oldest -> list.sortedBy { projectTimeMs(it.updatedAt) }
                TaskSort.Default -> list.sortedWith(
                    compareBy({ taskRank(normalizeStatus(it.status)) }, { -projectTimeMs(it.updatedAt) })
                )
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
                title = { Text("Projects") },
                actions = {
                    // #62: + starts a new project. Disabled while rows are
                    // unreadable (#447 review): the EmptyState guard below
                    // must not be bypassable from the toolbar.
                    IconButton(
                        onClick = { editing = ServerProject(status = "Todo") },
                        enabled = !busy && dropped == 0,
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
            Column(modifier = Modifier.contentWidth(wc)) {
                Row(
                    modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp)
                        .horizontalScroll(rememberScrollState()),
                    horizontalArrangement = Arrangement.spacedBy(8.dp),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    listOf("All").plus(TASK_STATUSES).forEach { f ->
                        FilterChip(
                            selected = filter == f,
                            onClick = { filter = f },
                            label = { Text(f) },
                        )
                    }
                    Box {
                        TextButton(onClick = { sortOpen = true }) { Text("Sort: ${sort.title}") }
                        DropdownMenu(expanded = sortOpen, onDismissRequest = { sortOpen = false }) {
                            TaskSort.entries.forEach { s ->
                                DropdownMenuItem(
                                    text = { Text(s.title) },
                                    onClick = { sort = s; sortOpen = false },
                                )
                            }
                        }
                    }
                }
                if (busy) {
                    LinearProgressIndicator(modifier = Modifier.fillMaxWidth())
                }
                if (dropped > 0 && visible.isNotEmpty()) {
                    HintLine(
                        "$dropped project(s) could not be parsed — the list below is partial.",
                        modifier = Modifier.padding(horizontal = 12.dp),
                    )
                }
                if (truncated && visible.isNotEmpty()) {
                    HintLine(
                        "Showing the first ${visible.size} — the board continues past the fetched page.",
                        modifier = Modifier.padding(horizontal = 12.dp),
                    )
                }
                if (loading && projects.isEmpty()) {
                    Box(modifier = Modifier.fillMaxWidth().padding(32.dp), contentAlignment = Alignment.Center) {
                        CircularProgressIndicator()
                    }
                } else if (error.isNotEmpty() && projects.isEmpty()) {
                    ErrorCard(
                        raw = error,
                        onRetry = { refreshTick++ },
                        modifier = Modifier.padding(horizontal = 12.dp, vertical = 4.dp),
                    )
                } else if (!loading && error.isEmpty() && visible.isEmpty()) {
                    // All-dropped reads as empty without this (#447 review):
                    // same guard as the Task Manager and Scheduler lists.
                    val allDropped = projects.isEmpty() && dropped > 0
                    EmptyState(
                        icon = Icons.Filled.List,
                        title = if (allDropped) {
                            "Couldn't read projects"
                        } else if (projects.isEmpty()) {
                            "No projects yet"
                        } else {
                            "Nothing matches this filter"
                        },
                        subtitle = if (allDropped) {
                            "The server sent rows this app can't read — check the gateway version."
                        } else if (projects.isEmpty()) {
                            "Capture your first project — it syncs to your server."
                        } else if (dropped > 0) {
                            // A filter miss while rows are unparsed is not a
                            // clean negative (#447 review): the dropped rows
                            // were never searched and could contain matches.
                            "Try a different status filter — note $dropped unparseable row(s) were not searched."
                        } else {
                            "Try a different status filter."
                        },
                        // No New action while rows are unreadable (#447
                        // review): same duplicate risk as the Task Manager.
                        actionLabel = if (projects.isEmpty() && !allDropped) "New task" else null,
                        onAction = if (projects.isEmpty() && !allDropped) {
                            { editing = ServerProject(status = "Todo") }
                        } else null,
                    )
                } else if (wc == WindowClass.Expanded) {
                    LazyVerticalGrid(
                        columns = GridCells.Fixed(2),
                        modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp),
                        verticalArrangement = Arrangement.spacedBy(8.dp),
                        horizontalArrangement = Arrangement.spacedBy(8.dp),
                        contentPadding = PaddingValues(vertical = 8.dp),
                    ) {
                        items(visible, key = { it.id }) { t ->
                            TaskCard(
                                task = t,
                                // Cards stay tappable but ignore taps mid-mutation:
                                // concurrent edits race the refreshTick reload.
                                onCycle = { if (!busy) doCycle(t) },
                                onEdit = { if (!busy) editing = t.copy(status = normalizeStatus(t.status)) },
                                onDelete = { if (!busy) doDelete(t) },
                            )
                        }
                    }
                } else {
                    LazyColumn(
                        modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp),
                        verticalArrangement = Arrangement.spacedBy(8.dp),
                        contentPadding = PaddingValues(vertical = 8.dp),
                    ) {
                        items(visible, key = { it.id }) { t ->
                            TaskCard(
                                task = t,
                                onCycle = { if (!busy) doCycle(t) },
                                onEdit = { if (!busy) editing = t.copy(status = normalizeStatus(t.status)) },
                                onDelete = { if (!busy) doDelete(t) },
                            )
                        }
                    }
                }
            }
        }
        ToastHost(center = toasts, modifier = Modifier.fillMaxWidth())
    }
    }

    val draft = editing
    if (draft != null) {
        TaskDialog(
            initial = draft,
            isNew = draft.id.isEmpty(),
            onDismiss = { editing = null },
            onSave = { saved ->
                busy = true
                scope.launch {
                    if (saved.id.isEmpty()) {
                        api.create(saved.name, saved.status, saved.note).fold(
                            onSuccess = { editing = null; refreshTick++; toast(Toasts.TASK_SAVED, ToastKind.Success) },
                            onFailure = ::fail,
                        )
                    } else {
                        api.update(saved.id, saved.name, saved.status, saved.note).fold(
                            onSuccess = { editing = null; refreshTick++; toast(Toasts.TASK_SAVED, ToastKind.Success) },
                            onFailure = ::fail,
                        )
                    }
                    busy = false
                }
            },
            saving = busy,
        )
    }
}

/** One project card: title + note + colored status chip + timestamp + menu.
 * Long-press opens the edit dialog. */
@Composable
internal fun TaskCard(
    task: ServerProject,
    onCycle: () -> Unit,
    onEdit: () -> Unit,
    onDelete: () -> Unit,
) {
    var rowMenu by remember(task.id) { mutableStateOf(false) }
    val status = normalizeStatus(task.status)
    val haptics = LocalHapticFeedback.current
    Card(
        modifier = Modifier.fillMaxWidth().combinedClickable(
            onClick = {},
            onLongClick = {
                haptics.performHapticFeedback(HapticFeedbackType.LongPress)
                onEdit()
            },
        ),
    ) {
        Row(
            modifier = Modifier.fillMaxWidth().padding(12.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Column(modifier = Modifier.weight(1f)) {
                Text(
                    task.name.ifEmpty { "(untitled)" },
                    style = MaterialTheme.typography.bodyLarge,
                    maxLines = 2,
                )
                if (task.note.isNotEmpty()) {
                    Text(
                        task.note,
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        maxLines = 3,
                    )
                }
                val ts = shortTime(projectTimeMs(task.updatedAt))
                if (ts.isNotEmpty()) {
                    Text(
                        "Updated $ts",
                        style = MaterialTheme.typography.labelSmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
            Spacer(modifier = Modifier.width(8.dp))
            StatusChip(status = status, onClick = onCycle)
            Box {
                IconButton(onClick = { rowMenu = true }) {
                    Icon(Icons.Filled.MoreVert, contentDescription = "Task menu")
                }
                DropdownMenu(expanded = rowMenu, onDismissRequest = { rowMenu = false }) {
                    DropdownMenuItem(
                        text = { Text("Edit") },
                        onClick = { rowMenu = false; onEdit() },
                    )
                    DropdownMenuItem(
                        text = { Text("Delete") },
                        onClick = { rowMenu = false; onDelete() },
                    )
                }
            }
        }
    }
}

/** Add/edit dialog for one project row (status is a fixed picker, #55). */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
internal fun TaskDialog(
    initial: ServerProject,
    isNew: Boolean,
    onDismiss: () -> Unit,
    onSave: (ServerProject) -> Unit,
    saving: Boolean = false,
) {
    var name by remember(initial.id) { mutableStateOf(initial.name) }
    var status by remember(initial.id) { mutableStateOf(normalizeStatus(initial.status)) }
    var note by remember(initial.id) { mutableStateOf(initial.note) }
    var statusOpen by remember { mutableStateOf(false) }
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(if (isNew) "New task" else "Edit task") },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                OutlinedTextField(
                    value = name,
                    onValueChange = { name = it },
                    label = { Text("Task name") },
                    singleLine = true,
                    modifier = Modifier.fillMaxWidth(),
                )
                ExposedDropdownMenuBox(
                    expanded = statusOpen,
                    onExpandedChange = { statusOpen = it },
                ) {
                    OutlinedTextField(
                        value = status,
                        onValueChange = {},
                        readOnly = true,
                        label = { Text("Status") },
                        trailingIcon = { ExposedDropdownMenuDefaults.TrailingIcon(expanded = statusOpen) },
                        modifier = Modifier.fillMaxWidth().menuAnchor(),
                    )
                    ExposedDropdownMenu(
                        expanded = statusOpen,
                        onDismissRequest = { statusOpen = false },
                    ) {
                        TASK_STATUSES.forEach { s ->
                            DropdownMenuItem(
                                text = { Text(s) },
                                onClick = { status = s; statusOpen = false },
                            )
                        }
                    }
                }
                OutlinedTextField(
                    value = note,
                    onValueChange = { note = it },
                    label = { Text("Note") },
                    modifier = Modifier.fillMaxWidth(),
                    maxLines = 4,
                )
            }
        },
        confirmButton = {
            TextButton(
                onClick = {
                    onSave(initial.copy(
                        name = name.trim(),
                        status = status,
                        note = note.trim(),
                    ))
                },
                // Gated on !saving: double-tap would create/update twice.
                enabled = name.isNotBlank() && !saving,
            ) { Text("Save") }
        },
        dismissButton = {
            TextButton(onClick = onDismiss) { Text("Cancel") }
        },
    )
}
