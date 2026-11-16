@file:OptIn(ExperimentalMaterial3Api::class, ExperimentalFoundationApi::class)

package com.vishnu.agento

/**
 * Projects board (#519): collapsible status sections, swipe left to edit,
 * swipe right to delete with Undo, long-press drag to reorder inside a
 * section. Ordering is persisted server-side (`position`, see
 * [ProjectsApi.reorder]).
 *
 * Legacy note: this file moved verbatim out of MainActivity.kt in #163;
 * the only parts left from that move are the status helpers and the
 * one-time tasks.json import below.
 */
import androidx.compose.foundation.ExperimentalFoundationApi
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.gestures.detectDragGesturesAfterLongPress
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.grid.GridCells
import androidx.compose.foundation.lazy.grid.GridItemSpan
import androidx.compose.foundation.lazy.grid.LazyVerticalGrid
import androidx.compose.foundation.lazy.grid.items
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.Edit
import androidx.compose.material.icons.filled.KeyboardArrowDown
import androidx.compose.material.icons.filled.KeyboardArrowRight
import androidx.compose.material.icons.filled.List
import androidx.compose.material.icons.filled.Menu
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.graphicsLayer
import androidx.compose.ui.hapticfeedback.HapticFeedbackType
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalHapticFeedback
import androidx.compose.ui.unit.dp
import androidx.compose.ui.zIndex
import androidx.core.content.edit
import java.util.Locale
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
/** Fixed task statuses (#55, #527). Order here is the default sort order:
 * Ongoing → Testing → Paused → Todo → Done. */
internal val TASK_STATUSES = listOf("Ongoing", "Testing", "Paused", "Todo", "Done")

internal fun taskRank(status: String): Int =
    TASK_STATUSES.indexOf(status).let { if (it < 0) TASK_STATUSES.indexOf("Todo") else it }

/** Maps legacy free-text statuses onto the fixed set (#55, #527). */
internal fun normalizeStatus(raw: String): String = when (raw.trim().lowercase(Locale.ROOT)) {
    "doing", "ongoing", "in progress", "in_progress" -> "Ongoing"
    "testing", "test", "tests", "qa", "in testing" -> "Testing"
    "paused", "pause", "pasued", "blocked", "block", "stuck", "on hold" -> "Paused"
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

/** Manual is the persisted board order (#519); the rest are read-only
 * views that disable drag reordering. */
internal enum class TaskSort(val title: String) {
    Manual("Manual"),
    Name("Name"),
    Newest("Newest"),
    Oldest("Oldest"),
}

/** One rendered status section: its fixed status plus the rows in it. */
private data class ProjectSection(val status: String, val rows: List<ServerProject>)

/** Lazy-item keys shared by the drag math and the lists. Headers use
 * `h:` so the two key spaces can never collide. */
private const val CARD_KEY_PREFIX = "p:"

/** Server-side MaxLimit: the most projects one list call returns. A board
 * at the cap is a truncated view, so it is called out instead of passed off
 * as complete. */
private const val PROJECT_LIST_CAP = 500

private fun cardKey(id: String) = CARD_KEY_PREFIX + id

/** Project board: fixed statuses, collapsible sections, backed by the
 * shared `projects` collection over /api/projects (#104 — retired local
 * tasks.json; #519 — sections + gestures + manual order). The agent
 * manages the same rows over the project-manager MCP. First launch
 * imports any local rows missing on the server. */
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
    // Bumped after every load/mutation so the loader below reruns.
    var refreshTick by remember { mutableIntStateOf(0) }
    var sort by remember { mutableStateOf(TaskSort.Manual) }
    var sortOpen by remember { mutableStateOf(false) }
    var busy by remember { mutableStateOf(false) }
    // Persisted manual order (#519), rebuilt from the server after every
    // load and mutated locally while a drag is in flight.
    var manualOrder by remember { mutableStateOf<List<String>>(emptyList()) }
    // Collapsed status sections, comma-separated so rememberSaveable can
    // hold it (a Set is not Bundle-supported).
    var collapsedCsv by rememberSaveable { mutableStateOf("") }
    val collapsed = remember(collapsedCsv) {
        collapsedCsv.split(',').filter { it.isNotEmpty() }.toSet()
    }
    val snackbar = remember { SnackbarHostState() }
    val listState = rememberLazyListState()
    var draggingId by remember { mutableStateOf<String?>(null) }
    var dragDelta by remember { mutableFloatStateOf(0f) }
    var dragMoved by remember { mutableStateOf(false) }
    // Where the last successful swap put the dragged row: the drop replays
    // that relation onto the server's complete section order.
    var dragTargetId by remember { mutableStateOf<String?>(null) }
    var dragAfterTarget by remember { mutableStateOf(false) }
    // Set while a drag hovers a row in the same visual section but a
    // different stored section (#527 transition: Paused visual holds stored
    // Paused + unmigrated Blocked). Consumed as a toast on release when no
    // move happened, so the no-op is explained, not silent.
    var dragMigrationBlocked by remember { mutableStateOf(false) }

    fun fail(e: Throwable) {
        toasts.toast(serverDetail(e.message ?: e.javaClass.simpleName), ToastKind.Error)
    }

    fun toast(msg: String, kind: ToastKind = ToastKind.Info) {
        toasts.toast(msg, kind)
    }

    fun toggleSection(status: String) {
        val current = collapsedCsv.split(',').filter { it.isNotEmpty() }.toSet()
        collapsedCsv = (if (status in current) current - status else current + status)
            .joinToString(",")
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
                val have = api.list("all", limit = PROJECT_LIST_CAP).getOrNull().orEmpty()
                    .map { norm(it.name) }.toMutableSet()
                for ((name, status, note) in legacy) {
                    if (norm(name) in have) continue
                    val created = api.create(name, normalizeStatus(status), note).getOrNull()
                    if (created == null) break
                    have.add(norm(created.name))
                }
                val landed = api.list("all", limit = PROJECT_LIST_CAP).getOrNull().orEmpty()
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

    LaunchedEffect(refreshTick) {
        loading = true
        error = ""
        api.list("all", limit = PROJECT_LIST_CAP).fold(
            onSuccess = {
                projects = it
                // The server is the source of truth for manual order; a
                // drag's local mutation is replaced by the persisted result.
                manualOrder = manualProjectOrder(it)
                // Section counts are computed from these rows, so a board
                // at the server cap is called out as possibly truncated —
                // the list response carries no total, so an exactly-full
                // board is also covered.
                if (it.size >= PROJECT_LIST_CAP) {
                    toast(
                        "Board may be truncated at $PROJECT_LIST_CAP projects",
                        ToastKind.Info,
                    )
                }
            },
            onFailure = { e ->
                val msg = serverDetail(e.message ?: e.javaClass.simpleName)
                // A failed refresh over cached rows still surfaces: stale
                // data with no error affordance hides outages.
                if (projects.isEmpty()) error = msg else toast(msg, ToastKind.Error)
            },
        )
        loading = false
    }

    // Swipe right = delete (#519): the row settles dismissed, the delete
    // runs, and Undo recreates it from the cached fields. It is a new row,
    // so it comes back at the end of its section (id and position are
    // server-assigned). A failure bumps refreshTick so TaskCard's
    // resetToken snaps the row back.
    fun doDelete(t: ServerProject) {
        if (busy) return
        busy = true
        scope.launch {
            api.delete(t.id).fold(
                onSuccess = {
                    refreshTick++
                    busy = false
                    val result = snackbar.showSnackbar(
                        message = "Deleted \"${t.name}\"",
                        actionLabel = "Undo",
                        withDismissAction = true,
                        duration = SnackbarDuration.Short,
                    )
                    if (result == SnackbarResult.ActionPerformed) {
                        busy = true
                        api.create(t.name, normalizeStatus(t.status), t.note).fold(
                            onSuccess = {
                                refreshTick++
                                toast("Project restored", ToastKind.Success)
                            },
                            onFailure = { e ->
                                fail(e)
                                refreshTick++
                            },
                        )
                        busy = false
                    }
                },
                onFailure = { e ->
                    fail(e)
                    busy = false
                    refreshTick++
                },
            )
        }
    }

    // Long press = drag to reorder (#519). Only the Manual view can be
    // dragged: any other sort is a read-only view of the same rows.
    fun startDrag(id: String) {
        if (busy) return
        if (sort != TaskSort.Manual) {
            toast("Sort by Manual to reorder projects")
            return
        }
        draggingId = id
        dragDelta = 0f
        dragMoved = false
        dragTargetId = null
        dragAfterTarget = false
        dragMigrationBlocked = false
    }

    fun dragMove(dy: Float) {
        val id = draggingId ?: return
        dragDelta += dy
        val info = listState.layoutInfo
        val dragged = info.visibleItemsInfo.firstOrNull { it.key == cardKey(id) } ?: return
        // Reorder is per stored status (#527 transition): the Paused visual
        // section can hold stored Paused + unmigrated stored Blocked rows,
        // but the server's full-section reorder is exact-status, so a drag
        // must stay inside its own stored section, not the normalized one.
        val stored = projects.firstOrNull { it.id == id }?.status ?: return
        // The dragged card's visual centre relative to the list: its layout
        // slot is fixed, the finger offset lives in dragDelta.
        val centre = dragged.offset + dragDelta + dragged.size / 2f
        // The card under the finger, if any: a header key or empty space
        // reads as no hover, never as a cross-section attempt.
        val hovered = info.visibleItemsInfo.firstOrNull { item ->
            val key = item.key as? String ?: return@firstOrNull false
            if (!key.startsWith(CARD_KEY_PREFIX) || key == cardKey(id)) return@firstOrNull false
            centre >= item.offset && centre <= item.offset + item.size
        }?.let { (it.key as String).removePrefix(CARD_KEY_PREFIX) }
            ?.let { hid -> projects.firstOrNull { it.id == hid } }
        if (hovered == null) return
        // Crossing visual sections is an edit, not a reorder: stay silent
        // here like before (the section headers make the boundary visible).
        if (!normalizeStatus(hovered.status).equals(normalizeStatus(stored), ignoreCase = true)) return
        // Same visual section but different stored section (#527
        // transition): the move cannot persist per exact-status reorder,
        // so flag it for an explanatory toast on release instead of a
        // silent no-op.
        if (!hovered.status.equals(stored, ignoreCase = true)) {
            dragMigrationBlocked = true
            return
        }
        val target = info.visibleItemsInfo.firstOrNull { item ->
            item.key == cardKey(hovered.id)
        } ?: return
        val targetId = hovered.id
        val from = manualOrder.indexOf(id)
        val to = manualOrder.indexOf(targetId)
        if (from < 0 || to < 0 || from == to) return
        val next = manualOrder.toMutableList()
        next.removeAt(from)
        next.add(to, id)
        manualOrder = next
        dragMoved = true
        dragTargetId = targetId
        dragAfterTarget = to > from
        // Keep the card under the finger: after the move its layout slot is
        // the target's old slot (moving up) or the target's old bottom
        // minus our height (moving down).
        val newTop = if (to > from) {
            target.offset + target.size - dragged.size
        } else {
            target.offset
        }
        dragDelta += dragged.offset - newTop
    }

    fun endDrag() {
        val id = draggingId ?: return
        draggingId = null
        dragDelta = 0f
        if (!dragMoved) {
            // A hover across the invisible stored boundary inside one
            // visual section cannot persist: say so instead of dropping it
            // silently. Runs once per drag (flag resets in startDrag).
            if (dragMigrationBlocked) {
                dragMigrationBlocked = false
                toast("These Paused rows finish migrating first — reorder works again after the #527 migration", ToastKind.Info)
            }
            return
        }
        dragMoved = false
        dragMigrationBlocked = false
        // Same stored-section rule as dragMove (#527 transition): list +
        // reorder the dragged row's stored status so unmigrated Blocked rows
        // reorder their own section instead of failing against Paused.
        val status = projects.firstOrNull { it.id == id }?.status ?: return
        val targetId = dragTargetId
        val afterTarget = dragAfterTarget
        val localIds = manualOrder.filter { pid ->
            projects.firstOrNull { it.id == pid }
                ?.let { it.status.equals(status, ignoreCase = true) } == true
        }
        if (localIds.size < 2) return
        busy = true
        scope.launch {
            // Reorder needs the complete section and the loaded board is
            // capped, so fetch this section directly and replay the local
            // move on the server order. A section over the cap still fails
            // server-side (it refuses a partial list) and surfaces as a
            // toast instead of silently reordering.
            val ids = api.list(status, limit = PROJECT_LIST_CAP)
                .getOrNull()?.map { it.id }?.toMutableList()
            val movedFrom = ids?.indexOf(id) ?: -1
            if (ids == null || movedFrom < 0) {
                // Do not guess: reload and let the user retry.
                toast("Could not reorder — board reloaded", ToastKind.Error)
            } else {
                ids.removeAt(movedFrom)
                val targetNow = targetId?.let { ids.indexOf(it) } ?: -1
                if (targetNow < 0) {
                    // The drop target left the section (edited or deleted
                    // elsewhere). Appending would move the row somewhere
                    // the user never chose.
                    toast("Board changed — try the move again", ToastKind.Error)
                } else {
                    ids.add(if (afterTarget) targetNow + 1 else targetNow, id)
                    api.reorder(status, ids).fold(
                        onSuccess = { },
                        onFailure = { e -> fail(e) },
                    )
                }
            }
            refreshTick++
            busy = false
        }
    }

    val visible = when (sort) {
        TaskSort.Name -> projects.sortedBy { it.name.lowercase(Locale.ROOT) }
        TaskSort.Newest -> projects.sortedByDescending { projectTimeMs(it.updatedAt) }
        TaskSort.Oldest -> projects.sortedBy { projectTimeMs(it.updatedAt) }
        TaskSort.Manual -> sortProjectsManually(projects, manualOrder)
    }

    // Empty sections are hidden; collapsing keeps the header and hides rows.
    val sections = TASK_STATUSES.mapNotNull { status ->
        val rows = visible.filter { normalizeStatus(it.status) == status }
        if (rows.isEmpty()) null else ProjectSection(status, rows)
    }

    // The toast card floats top-end over the content (Box overlay, not
    // the bottom-anchored Scaffold slot) so it never shifts the layout.
    Box(modifier = Modifier.fillMaxSize()) {
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
                    modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp),
                    horizontalArrangement = Arrangement.spacedBy(8.dp),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
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
                    if (sort != TaskSort.Manual) {
                        Text(
                            "Reordering needs Manual sort",
                            style = MaterialTheme.typography.labelSmall,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
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
                        title = "No projects yet",
                        subtitle = "Capture your first project — it syncs to your server.",
                        actionLabel = "New task",
                        onAction = { editing = ServerProject(status = "Todo") },
                    )
                } else if (wc == WindowClass.Expanded) {
                    // Two columns have no linear order to persist: swipe
                    // still works, long-press drag is list-only.
                    LazyVerticalGrid(
                        columns = GridCells.Fixed(2),
                        modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp),
                        verticalArrangement = Arrangement.spacedBy(8.dp),
                        horizontalArrangement = Arrangement.spacedBy(8.dp),
                        contentPadding = PaddingValues(vertical = 8.dp),
                    ) {
                        sections.forEach { section ->
                            item(
                                key = "h:${section.status}",
                                span = { GridItemSpan(maxLineSpan) },
                            ) {
                                SectionHeader(
                                    status = section.status,
                                    count = section.rows.size,
                                    expanded = section.status !in collapsed,
                                    onToggle = { toggleSection(section.status) },
                                )
                            }
                            if (section.status !in collapsed) {
                                items(section.rows, key = { cardKey(it.id) }) { t ->
                                    TaskCard(
                                        task = t,
                                        onDelete = { doDelete(t) },
                                        onEdit = {
                                            if (!busy) editing =
                                                t.copy(status = normalizeStatus(t.status))
                                        },
                                        swipeEnabled = !busy,
                                        resetToken = refreshTick,
                                    )
                                }
                            }
                        }
                    }
                } else {
                    LazyColumn(
                        state = listState,
                        modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp),
                        verticalArrangement = Arrangement.spacedBy(8.dp),
                        contentPadding = PaddingValues(vertical = 8.dp),
                    ) {
                        sections.forEach { section ->
                            item(key = "h:${section.status}") {
                                SectionHeader(
                                    status = section.status,
                                    count = section.rows.size,
                                    expanded = section.status !in collapsed,
                                    onToggle = { toggleSection(section.status) },
                                )
                            }
                            if (section.status !in collapsed) {
                                items(section.rows, key = { cardKey(it.id) }) { t ->
                                    val isDragging = draggingId == t.id
                                    TaskCard(
                                        task = t,
                                        onDelete = { doDelete(t) },
                                        onEdit = {
                                            if (!busy) editing =
                                                t.copy(status = normalizeStatus(t.status))
                                        },
                                        modifier = if (isDragging) Modifier else Modifier.animateItem(),
                                        isDragging = isDragging,
                                        dragOffset = if (isDragging) dragDelta else 0f,
                                        dragEnabled = !busy,
                                        swipeEnabled = !busy,
                                        resetToken = refreshTick,
                                        onDragStart = { startDrag(t.id) },
                                        onDrag = { dy -> dragMove(dy) },
                                        onDragEnd = { endDrag() },
                                    )
                                }
                            }
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

/** Collapsible status header: chevron, status, count. */
@Composable
private fun SectionHeader(
    status: String,
    count: Int,
    expanded: Boolean,
    onToggle: () -> Unit,
) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .clip(RoundedCornerShape(12.dp))
            .clickable(onClick = onToggle)
            .padding(horizontal = 8.dp, vertical = 8.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Icon(
            imageVector = if (expanded) Icons.Filled.KeyboardArrowDown else Icons.Filled.KeyboardArrowRight,
            contentDescription = if (expanded) "Collapse $status" else "Expand $status",
        )
        Spacer(modifier = Modifier.width(6.dp))
        Text(
            status,
            style = MaterialTheme.typography.titleSmall,
            modifier = Modifier.weight(1f),
        )
        Text(
            "$count",
            style = MaterialTheme.typography.labelMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}

/** What sits behind the card while it swipes: delete on a right swipe,
 * edit on a left swipe. */
@Composable
private fun SwipeBackground(state: SwipeToDismissBoxState) {
    val direction = state.dismissDirection
    val container = when (direction) {
        SwipeToDismissBoxValue.StartToEnd -> MaterialTheme.colorScheme.errorContainer
        SwipeToDismissBoxValue.EndToStart -> MaterialTheme.colorScheme.secondaryContainer
        SwipeToDismissBoxValue.Settled -> Color.Transparent
    }
    Row(
        modifier = Modifier
            .fillMaxSize()
            .clip(RoundedCornerShape(12.dp))
            .background(container)
            .padding(horizontal = 20.dp),
        horizontalArrangement = Arrangement.SpaceBetween,
        verticalAlignment = Alignment.CenterVertically,
    ) {
        if (direction == SwipeToDismissBoxValue.StartToEnd) {
            Icon(
                Icons.Filled.Delete,
                contentDescription = "Delete",
                tint = MaterialTheme.colorScheme.onErrorContainer,
            )
        } else {
            Spacer(modifier = Modifier.width(24.dp))
        }
        if (direction == SwipeToDismissBoxValue.EndToStart) {
            Icon(
                Icons.Filled.Edit,
                contentDescription = "Edit",
                tint = MaterialTheme.colorScheme.onSecondaryContainer,
            )
        } else {
            Spacer(modifier = Modifier.width(24.dp))
        }
    }
}

/** One project card: title + note + timestamp. No menu, no edit button —
 * swipe left edits, swipe right deletes, long press drags (#519). The
 * `resetToken` recreates the swipe state after a failed delete so the row
 * snaps back instead of staying dismissed. */
@Composable
internal fun TaskCard(
    task: ServerProject,
    onDelete: () -> Unit,
    onEdit: () -> Unit,
    modifier: Modifier = Modifier,
    isDragging: Boolean = false,
    dragOffset: Float = 0f,
    dragEnabled: Boolean = false,
    swipeEnabled: Boolean = true,
    resetToken: Int = 0,
    onDragStart: () -> Unit = {},
    onDrag: (Float) -> Unit = {},
    onDragEnd: () -> Unit = {},
) {
    val haptics = LocalHapticFeedback.current
    val latestOnDragStart by rememberUpdatedState(onDragStart)
    val latestOnDrag by rememberUpdatedState(onDrag)
    val latestOnDragEnd by rememberUpdatedState(onDragEnd)
    // Only the compact list wires this up; the grid leaves it empty.
    val dragModifier = if (dragEnabled) {
        Modifier.pointerInput(Unit) {
            detectDragGesturesAfterLongPress(
                onDragStart = {
                    haptics.performHapticFeedback(HapticFeedbackType.LongPress)
                    latestOnDragStart()
                },
                onDrag = { change, amount ->
                    change.consume()
                    latestOnDrag(amount.y)
                },
                onDragEnd = { latestOnDragEnd() },
                onDragCancel = { latestOnDragEnd() },
            )
        }
    } else {
        Modifier
    }

    key(resetToken) {
        val swipeState = rememberSwipeToDismissBoxState(
            confirmValueChange = { target ->
                when (target) {
                    SwipeToDismissBoxValue.StartToEnd -> {
                        onDelete()
                        true
                    }
                    // Edit must leave the row where it was: refusing
                    // the change settles it back immediately.
                    SwipeToDismissBoxValue.EndToStart -> {
                        onEdit()
                        false
                    }
                    SwipeToDismissBoxValue.Settled -> true
                }
            },
        )
        Box(
            modifier = modifier
                .fillMaxWidth()
                .graphicsLayer {
                    translationY = if (isDragging) dragOffset else 0f
                }
                .zIndex(if (isDragging) 1f else 0f),
        ) {
            SwipeToDismissBox(
                state = swipeState,
                backgroundContent = { SwipeBackground(state = swipeState) },
                gesturesEnabled = swipeEnabled,
                modifier = Modifier.fillMaxWidth(),
            ) {
                Card(
                    modifier = Modifier
                        .fillMaxWidth()
                        .then(dragModifier),
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
                    }
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
