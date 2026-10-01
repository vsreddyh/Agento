@file:OptIn(ExperimentalMaterial3Api::class)

package com.vishnu.agento

/**
 * Legacy projects board (fixed statuses, local tasks.json import) — the old surface, kept whole (#163, last of four moves).
 *
 * Moved verbatim out of MainActivity.kt; the only changes are
 * visibility (`private` to `internal`, same package) and the import
 * list, which carries exactly what this file uses.
 */
import android.content.Context
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
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import androidx.core.content.edit
import java.util.Locale
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
/** Fixed task statuses (#55). Order here is the default sort order:
 * Ongoing → Paused → Todo → Done. */
internal val TASK_STATUSES = listOf("Ongoing", "Paused", "Todo", "Done")

internal fun taskRank(status: String): Int =
    TASK_STATUSES.indexOf(status).let { if (it < 0) 2 else it }

/** Maps legacy free-text statuses onto the fixed set (#55). */
internal fun normalizeStatus(raw: String): String = when (raw.trim().lowercase(Locale.ROOT)) {
    "doing", "ongoing", "in progress", "in_progress" -> "Ongoing"
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
    val snackbar = remember { SnackbarHostState() }
    // One client for the screen (its OkHttpClient is shared process-wide).
    val api = remember(context) { ProjectsApi(context) }
    var projects by remember { mutableStateOf<List<ServerProject>>(emptyList()) }
    var editing by remember { mutableStateOf<ServerProject?>(null) }
    var loading by remember { mutableStateOf(true) }
    var error by remember { mutableStateOf("") }
    // Bumped after every load/mutation so the loader below reruns.
    var refreshTick by remember { mutableIntStateOf(0) }
    var filter by remember { mutableStateOf("All") }
    var sort by remember { mutableStateOf(TaskSort.Default) }
    var sortOpen by remember { mutableStateOf(false) }
    var busy by remember { mutableStateOf(false) }

    fun fail(e: Throwable) {
        scope.launch {
            snackbar.showSnackbar(serverDetail(e.message ?: e.javaClass.simpleName))
        }
    }

    fun toast(msg: String) {
        scope.launch { snackbar.showSnackbar(msg) }
    }

    fun cycleStatus(t: ServerProject): String {
        val next = when (normalizeStatus(t.status)) {
            "Todo" -> "Ongoing"
            "Ongoing" -> "Paused"
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
                val have = api.list("all", limit = 500).getOrNull().orEmpty()
                    .map { norm(it.name) }.toMutableSet()
                for ((name, status, note) in legacy) {
                    if (norm(name) in have) continue
                    val created = api.create(name, normalizeStatus(status), note).getOrNull()
                    if (created == null) break
                    have.add(norm(created.name))
                }
                val landed = api.list("all", limit = 500).getOrNull().orEmpty()
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

    LaunchedEffect(filter, refreshTick) {
        loading = true
        error = ""
        api.list(if (filter == "All") "all" else filter).fold(
            onSuccess = { projects = it },
            onFailure = { e ->
                val msg = serverDetail(e.message ?: e.javaClass.simpleName)
                // A failed refresh over cached rows still surfaces: stale
                // data with no error affordance hides outages.
                if (projects.isEmpty()) error = msg else toast(msg)
            },
        )
        loading = false
    }

    fun doCycle(t: ServerProject) {
        busy = true
        scope.launch {
            api.update(t.id, status = cycleStatus(t)).fold(
                onSuccess = { refreshTick++; toast(Toasts.TASK_SAVED) },
                onFailure = ::fail,
            )
            busy = false
        }
    }

    fun doDelete(t: ServerProject) {
        busy = true
        scope.launch {
            api.delete(t.id).fold(
                onSuccess = { refreshTick++; toast(Toasts.TASK_DELETED) },
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

    Scaffold(
        snackbarHost = { SnackbarHost(snackbar) },
        topBar = {
            TopAppBar(
                navigationIcon = {
                    IconButton(onClick = onMenu) {
                        Icon(Icons.Filled.Menu, contentDescription = "Menu")
                    }
                },
                title = { Text("Projects") },
                actions = {
                    // #62: + starts a new project.
                    IconButton(
                        onClick = { editing = ServerProject(status = "Todo") },
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
                    EmptyState(
                        icon = Icons.Filled.List,
                        title = if (projects.isEmpty()) "No projects yet" else "Nothing matches this filter",
                        subtitle = if (projects.isEmpty()) {
                            "Capture your first project — it syncs to your server."
                        } else {
                            "Try a different status filter."
                        },
                        actionLabel = if (projects.isEmpty()) "New task" else null,
                        onAction = if (projects.isEmpty()) {
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
                            onSuccess = { editing = null; refreshTick++; toast(Toasts.TASK_SAVED) },
                            onFailure = ::fail,
                        )
                    } else {
                        api.update(saved.id, saved.name, saved.status, saved.note).fold(
                            onSuccess = { editing = null; refreshTick++; toast(Toasts.TASK_SAVED) },
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

/** One project card: title + note + colored status chip + timestamp + menu. */
@Composable
internal fun TaskCard(
    task: ServerProject,
    onCycle: () -> Unit,
    onEdit: () -> Unit,
    onDelete: () -> Unit,
) {
    var rowMenu by remember(task.id) { mutableStateOf(false) }
    val status = normalizeStatus(task.status)
    Card(modifier = Modifier.fillMaxWidth()) {
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
