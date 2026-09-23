@file:OptIn(ExperimentalMaterial3Api::class)

package com.vishnu.agento

import android.Manifest
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.compose.setContent
import androidx.activity.result.contract.ActivityResultContracts
import androidx.activity.viewModels
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.ContentCopy
import androidx.compose.material.icons.filled.Description
import androidx.compose.material.icons.filled.ExpandLess
import androidx.compose.material.icons.filled.ExpandMore
import androidx.compose.material.icons.filled.List
import androidx.compose.material.icons.filled.Menu
import androidx.compose.material.icons.filled.Folder
import androidx.compose.material.icons.filled.MenuBook
import androidx.compose.material.icons.filled.MoreVert
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material.icons.filled.Star
import androidx.compose.material.icons.filled.Tune
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.unit.dp
import androidx.core.content.ContextCompat
import androidx.health.connect.client.PermissionController
import androidx.lifecycle.ViewModel
import androidx.lifecycle.ViewModelProvider
import androidx.lifecycle.viewmodel.compose.viewModel
import kotlinx.coroutines.launch
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext

/** Builds per-tab chat ViewModels so story/resumes/god keep isolated history. */
class ChatViewModelFactory(
    private val app: android.app.Application,
    private val tab: String,
) : ViewModelProvider.Factory {
    @Suppress("UNCHECKED_CAST")
    override fun <T : ViewModel> create(modelClass: Class<T>): T {
        return ChatViewModel(app, tab) as T
    }
}

/** Sidebar destinations; first three map 1:1 to gateway profiles.
 * Order is God, Story, Portfolio (#53); the resumes profile shows as
 * "Portfolio" (#54) but the tab key (prefs, files, profile path) is
 * unchanged so backend mapping never breaks. */
private enum class Destination(val title: String) {
    God("God"),
    Story("Story"),
    Portfolio("Portfolio"),
    Tasks("Tasks"),
    Storage("Storage"),
    Settings("Settings"),
}

private fun Destination.icon() = when (this) {
    Destination.Story -> Icons.Filled.MenuBook
    Destination.Portfolio -> Icons.Filled.Description
    Destination.God -> Icons.Filled.Star
    Destination.Tasks -> Icons.Filled.List
    Destination.Storage -> Icons.Filled.Folder
    Destination.Settings -> Icons.Filled.Settings
}

/** Settings subsections; each gets its own drawer entry + screen (#60). */
private enum class SettingSection(val title: String) {
    Server("Server"),
    Appearance("Appearance"),
    Updates("App updates"),
    Health("Health sync"),
    Notifications("Hermes notifications"),
    Backup("Settings backup"),
}


/** App theme mode keys (prefs `theme_mode`; #28). */
object ThemeStore {
    const val KEY = "theme_mode"
    const val SYSTEM = "system"
    const val LIGHT = "light"
    const val DARK = "dark"

    fun load(context: android.content.Context): String =
        context.getSharedPreferences(AgentoApp.PREFS_NAME, android.content.Context.MODE_PRIVATE)
            .getString(KEY, SYSTEM) ?: SYSTEM

    fun save(context: android.content.Context, mode: String) {
        context.getSharedPreferences(AgentoApp.PREFS_NAME, android.content.Context.MODE_PRIVATE)
            .edit().putString(KEY, mode).apply()
    }
}

/** Single-activity host; health state lives here, chat state per tab. */
class MainActivity : ComponentActivity() {

    private val healthModel: MainViewModel by viewModels()

    /** Sidebar drawer navigation (#18); theme from prefs (#28). */
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContent {
            val context = LocalContext.current
            var themeMode by remember { mutableStateOf(ThemeStore.load(context)) }
            val dark = when (themeMode) {
                ThemeStore.LIGHT -> false
                ThemeStore.DARK -> true
                else -> isSystemInDarkTheme()
            }
            MaterialTheme(
                colorScheme = if (dark) darkColorScheme() else lightColorScheme(),
            ) {
                // #59: paint the Material background over the full window so
                // dark mode never shows the light window background through.
                Surface(
                    modifier = Modifier.fillMaxSize(),
                    color = MaterialTheme.colorScheme.background,
                ) {
                var dest by remember { mutableStateOf(Destination.God) }
                var section by remember { mutableStateOf(SettingSection.Server) }
                var settingsOpen by remember { mutableStateOf(false) }
                val drawerState = rememberDrawerState(DrawerValue.Closed)
                val scope = rememberCoroutineScope()
                ModalNavigationDrawer(
                    drawerState = drawerState,
                    drawerContent = {
                        ModalDrawerSheet {
                            Text(
                                "Agento",
                                style = MaterialTheme.typography.titleLarge,
                                modifier = Modifier.padding(16.dp),
                            )
                            Destination.entries.forEach { d ->
                                if (d == Destination.Settings) {
                                    NavigationDrawerItem(
                                        label = { Text(d.title) },
                                        icon = { Icon(d.icon(), contentDescription = null) },
                                        badge = {
                                            Icon(
                                                if (settingsOpen) Icons.Filled.ExpandLess
                                                else Icons.Filled.ExpandMore,
                                                contentDescription = null,
                                            )
                                        },
                                        selected = dest == d,
                                        onClick = {
                                            dest = d
                                            settingsOpen = !settingsOpen
                                        },
                                        modifier = Modifier.padding(horizontal = 8.dp),
                                    )
                                    // #60: settings subsections live in the
                                    // sidebar; each opens its own screen.
                                    if (settingsOpen) {
                                        SettingSection.entries.forEach { s ->
                                            NavigationDrawerItem(
                                                label = { Text(s.title) },
                                                selected = dest == Destination.Settings && section == s,
                                                onClick = {
                                                    dest = Destination.Settings
                                                    section = s
                                                    scope.launch { drawerState.close() }
                                                },
                                                modifier = Modifier.padding(horizontal = 8.dp)
                                                    .padding(start = 24.dp),
                                            )
                                        }
                                    }
                                } else {
                                    NavigationDrawerItem(
                                        label = { Text(d.title) },
                                        icon = { Icon(d.icon(), contentDescription = null) },
                                        selected = dest == d,
                                        onClick = {
                                            dest = d
                                            scope.launch { drawerState.close() }
                                        },
                                        modifier = Modifier.padding(horizontal = 8.dp),
                                    )
                                }
                            }
                        }
                    },
                ) {
                    Box {
                        when (dest) {
                            Destination.God -> ChatTab(
                                app = application, tab = "god", title = "God",
                                onMenu = { scope.launch { drawerState.open() } },
                            )
                            Destination.Story -> ChatTab(
                                app = application, tab = "story", title = "Story",
                                onMenu = { scope.launch { drawerState.open() } },
                            )
                            Destination.Portfolio -> ChatTab(
                                app = application, tab = "resumes", title = "Portfolio",
                                onMenu = { scope.launch { drawerState.open() } },
                            )
                            Destination.Tasks -> TasksScreen(
                                onMenu = { scope.launch { drawerState.open() } },
                            )
                            Destination.Storage -> StorageScreen(
                                onMenu = { scope.launch { drawerState.open() } },
                            )
                            Destination.Settings -> SettingsScreen(
                                healthModel,
                                section = section,
                                onMenu = { scope.launch { drawerState.open() } },
                                themeMode = themeMode,
                                onTheme = {
                                    themeMode = it
                                    ThemeStore.save(context, it)
                                },
                            )
                        }
                    }
                }
                }
            }
        }
    }
}

/** Scopes one chat ViewModel per tab key so drafts/history survive tab switches. */
@Composable
private fun ChatTab(
    app: android.app.Application,
    tab: String,
    title: String,
    onMenu: () -> Unit,
) {
    val factory = remember(tab) { ChatViewModelFactory(app, tab) }
    // Keyed per tab — otherwise all three tabs would share one ViewModel.
    val vm: ChatViewModel = viewModel(key = "chat_$tab", factory = factory)
    val state by vm.state
    LaunchedEffect(Unit) { vm.refreshConfig() }
    var showModel by remember { mutableStateOf(false) }
    val subtitle = (if (state.model.isEmpty()) "not set" else state.model) +
        " · " + state.provider.ifEmpty { "not set" }
    ChatScreen(
        title = title, model = subtitle, state = state,
        onMenu = onMenu,
        onModel = { showModel = true },
        onPending = vm::onPending, onSend = vm::send, onStop = vm::stop,
        onNew = vm::newConversation, onRetry = vm::retry,
    )
    if (showModel) {
        TabModelSheet(app = app, tab = tab, title = title,
            onChanged = vm::refreshConfig, onClose = { showModel = false })
    }
}

/** Fixed task statuses (#55). Order here is the default sort order:
 * Ongoing → Paused → Todo → Done. */
private val TASK_STATUSES = listOf("Ongoing", "Paused", "Todo", "Done")

private fun taskRank(status: String): Int =
    TASK_STATUSES.indexOf(status).let { if (it < 0) 2 else it }

/** Maps legacy free-text statuses onto the fixed set (#55). */
private fun normalizeStatus(raw: String): String = when (raw.trim().lowercase()) {
    "doing", "ongoing", "in progress", "in_progress" -> "Ongoing"
    "paused", "pause", "pasued" -> "Paused"
    "done", "complete", "completed" -> "Done"
    else -> "Todo"
}

private enum class TaskSort(val title: String) {
    Default("Status"),
    Name("Name"),
    Newest("Newest"),
    Oldest("Oldest"),
}

/** Task table: fixed statuses, filters + sorts, persisted locally (#34, #55). */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun TasksScreen(onMenu: () -> Unit = {}) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    var tasks by remember { mutableStateOf<List<TaskItem>>(emptyList()) }
    var editing by remember { mutableStateOf<TaskItem?>(null) }
    var loaded by remember { mutableStateOf(false) }
    var filter by remember { mutableStateOf("All") }
    var sort by remember { mutableStateOf(TaskSort.Default) }
    var sortOpen by remember { mutableStateOf(false) }

    LaunchedEffect(Unit) {
        val stored = withContext(Dispatchers.IO) { TaskStore.load(context) }
        // One-time migration of legacy free-text statuses.
        val migrated = stored.map {
            val fixed = normalizeStatus(it.status)
            if (fixed != it.status) it.copy(status = fixed) else it
        }
        if (migrated != stored) {
            withContext(Dispatchers.IO) { TaskStore.save(context, migrated) }
        }
        tasks = migrated
        loaded = true
    }

    fun persist(next: List<TaskItem>) {
        tasks = next
        scope.launch(Dispatchers.IO) { TaskStore.save(context, next) }
    }

    fun cycleStatus(t: TaskItem): String {
        val next = when (normalizeStatus(t.status)) {
            "Todo" -> "Ongoing"
            "Ongoing" -> "Paused"
            "Paused" -> "Done"
            else -> "Todo"
        }
        return next
    }

    val visible = tasks
        .filter { filter == "All" || normalizeStatus(it.status) == filter }
        .let { list ->
            when (sort) {
                TaskSort.Name -> list.sortedBy { it.name.lowercase() }
                TaskSort.Newest -> list.sortedByDescending { it.updatedAt }
                TaskSort.Oldest -> list.sortedBy { it.updatedAt }
                TaskSort.Default -> list.sortedWith(
                    compareBy({ taskRank(normalizeStatus(it.status)) }, { -it.updatedAt })
                )
            }
        }

    Column(modifier = Modifier.fillMaxSize()) {
        TopAppBar(
            navigationIcon = {
                IconButton(onClick = onMenu) {
                    Icon(Icons.Filled.Menu, contentDescription = "Menu")
                }
            },
            title = { Text("Tasks") },
            actions = {
                // #62: + starts a new task.
                IconButton(onClick = {
                    editing = TaskItem(id = TaskStore.newId(), status = "Todo")
                }) {
                    Icon(Icons.Filled.Add, contentDescription = "New task")
                }
            },
        )
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
        if (!loaded) {
            Text(
                "Loading…",
                style = MaterialTheme.typography.bodyMedium,
                modifier = Modifier.padding(16.dp),
            )
        } else if (visible.isEmpty()) {
            Text(
                if (tasks.isEmpty()) "No tasks yet. Tap + to create the first row."
                else "No tasks match this filter.",
                style = MaterialTheme.typography.bodyMedium,
                modifier = Modifier.padding(16.dp),
            )
        }
        // Table header.
        if (visible.isNotEmpty()) {
            Row(
                modifier = Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 4.dp),
            ) {
                Text(
                    "Task",
                    style = MaterialTheme.typography.labelMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.weight(1f),
                )
                Text(
                    "Status",
                    style = MaterialTheme.typography.labelMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.width(96.dp),
                )
                Text(
                    "Updated",
                    style = MaterialTheme.typography.labelMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.width(88.dp),
                )
                Spacer(modifier = Modifier.width(48.dp))
            }
            HorizontalDivider()
        }
        LazyColumn(
            modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp),
            verticalArrangement = Arrangement.spacedBy(4.dp),
            contentPadding = PaddingValues(vertical = 4.dp),
        ) {
            items(visible, key = { it.id }) { t ->
                var rowMenu by remember(t.id) { mutableStateOf(false) }
                val status = normalizeStatus(t.status)
                Row(
                    modifier = Modifier.fillMaxWidth().padding(vertical = 6.dp),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    Column(modifier = Modifier.weight(1f)) {
                        Text(
                            t.name.ifEmpty { "(untitled)" },
                            style = MaterialTheme.typography.bodyLarge,
                            maxLines = 2,
                        )
                        if (t.note.isNotEmpty()) {
                            Text(
                                t.note,
                                style = MaterialTheme.typography.bodySmall,
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                                maxLines = 2,
                            )
                        }
                    }
                    AssistChip(
                        onClick = { persist(tasks.map {
                            if (it.id == t.id) it.copy(
                                status = cycleStatus(it),
                                updatedAt = ChatThreads.now(),
                            ) else it
                        }) },
                        label = { Text(status) },
                        modifier = Modifier.width(96.dp),
                    )
                    Text(
                        shortTime(t.updatedAt),
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        modifier = Modifier.width(88.dp).padding(start = 8.dp),
                    )
                    Box {
                        IconButton(onClick = { rowMenu = true }) {
                            Icon(Icons.Filled.MoreVert, contentDescription = "Task menu")
                        }
                        DropdownMenu(expanded = rowMenu, onDismissRequest = { rowMenu = false }) {
                            DropdownMenuItem(
                                text = { Text("Edit") },
                                onClick = { rowMenu = false; editing = t.copy(status = status) },
                            )
                            DropdownMenuItem(
                                text = { Text("Delete") },
                                onClick = {
                                    rowMenu = false
                                    persist(tasks.filterNot { it.id == t.id })
                                },
                            )
                        }
                    }
                }
                HorizontalDivider()
            }
        }
    }

    val draft = editing
    if (draft != null) {
        TaskDialog(
            initial = draft,
            isNew = tasks.none { it.id == draft.id },
            onDismiss = { editing = null },
            onSave = { saved ->
                val next = if (tasks.any { it.id == saved.id }) {
                    tasks.map { if (it.id == saved.id) saved else it }
                } else {
                    listOf(saved) + tasks
                }
                persist(next)
                editing = null
            },
        )
    }
}

/** Add/edit dialog for one task row (status is a fixed picker, #55). */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun TaskDialog(
    initial: TaskItem,
    isNew: Boolean,
    onDismiss: () -> Unit,
    onSave: (TaskItem) -> Unit,
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
            TextButton(onClick = {
                onSave(initial.copy(
                    name = name.trim(),
                    status = status,
                    note = note.trim(),
                    updatedAt = ChatThreads.now(),
                ))
            }) { Text("Save") }
        },
        dismissButton = {
            TextButton(onClick = onDismiss) { Text("Cancel") }
        },
    )
}

/** VPS exports browser with subfolders + mobile downloads (#26). */
@Composable
private fun StorageScreen(onMenu: () -> Unit = {}) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    var path by remember { mutableStateOf("") }
    var dirs by remember { mutableStateOf<List<RemoteEntry>>(emptyList()) }
    var files by remember { mutableStateOf<List<RemoteEntry>>(emptyList()) }
    var status by remember { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }

    fun load(p: String) {
        busy = true
        status = ""
        scope.launch {
            StorageApi(context).list(p).fold(
                onSuccess = { (cur, d, f) ->
                    path = cur.ifEmpty { "" }
                    dirs = d
                    files = f
                },
                onFailure = { e -> status = "FAILED — ${e.message}" },
            )
            busy = false
        }
    }

    LaunchedEffect(Unit) { load("") }

    Column(modifier = Modifier.fillMaxSize()) {
        TopAppBar(
            title = { Text(if (path.isEmpty()) "Storage" else "Storage / $path", maxLines = 1) },
            navigationIcon = {
                IconButton(onClick = onMenu) {
                    Icon(Icons.Filled.Menu, contentDescription = "Menu")
                }
            },
            actions = {
                if (path.isNotEmpty()) {
                    TextButton(onClick = {
                        val parent = if ("/" in path) path.substringBeforeLast("/") else ""
                        load(parent)
                    }) { Text("Up") }
                }
                TextButton(onClick = { load(path) }, enabled = !busy) { Text("Refresh") }
            },
        )
        if (status.isNotEmpty()) {
            Text(
                status,
                color = MaterialTheme.colorScheme.error,
                style = MaterialTheme.typography.bodySmall,
                modifier = Modifier.padding(horizontal = 16.dp, vertical = 4.dp),
            )
        }
        if (busy && dirs.isEmpty() && files.isEmpty()) {
            Text(
                "Loading…",
                style = MaterialTheme.typography.bodyMedium,
                modifier = Modifier.padding(16.dp),
            )
        }
        LazyColumn(
            modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp),
            verticalArrangement = Arrangement.spacedBy(4.dp),
            contentPadding = PaddingValues(vertical = 8.dp),
        ) {
            items(dirs, key = { "d:" + it.name }) { d ->
                Card(
                    onClick = { load(if (path.isEmpty()) d.name else "$path/${d.name}") },
                    modifier = Modifier.fillMaxWidth(),
                ) {
                    Row(
                        modifier = Modifier.padding(12.dp),
                        verticalAlignment = Alignment.CenterVertically,
                    ) {
                        Icon(Icons.Filled.Folder, contentDescription = null)
                        Spacer(modifier = Modifier.width(8.dp))
                        Text(d.name, style = MaterialTheme.typography.bodyLarge)
                    }
                }
            }
            items(files, key = { "f:" + it.name }) { f ->
                Card(modifier = Modifier.fillMaxWidth()) {
                    Row(
                        modifier = Modifier.padding(12.dp),
                        verticalAlignment = Alignment.CenterVertically,
                    ) {
                        Column(modifier = Modifier.weight(1f)) {
                            Text(f.name, style = MaterialTheme.typography.bodyLarge)
                            Text(
                                humanSize(f.size),
                                style = MaterialTheme.typography.bodySmall,
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                            )
                        }
                        TextButton(onClick = {
                            runCatching {
                                StorageApi(context).download(path, f.name)
                                status = "Downloading ${f.name}…"
                            }.onFailure { e ->
                                status = "FAILED — ${e.message}"
                            }
                        }) { Text("Save") }
                    }
                }
            }
            if (!busy && dirs.isEmpty() && files.isEmpty() && status.isEmpty()) {
                item {
                    Text(
                        "Empty — drop files into the VPS exports/ folder.",
                        style = MaterialTheme.typography.bodyMedium,
                        modifier = Modifier.padding(16.dp),
                    )
                }
            }
        }
    }
}

private fun humanSize(bytes: Long): String {
    if (bytes <= 0) return "0 B"
    val units = listOf("B", "KB", "MB", "GB")
    var v = bytes.toDouble()
    var u = 0
    while (v >= 1024 && u < units.size - 1) {
        v /= 1024
        u++
    }
    return if (u == 0) "$bytes B" else "%.1f %s".format(v, units[u])
}

/** Short HH:mm (plus date when not today); empty for unknown timestamps. */
private fun shortTime(ts: Long): String {
    if (ts <= 0) return ""
    return try {
        val zdt = java.time.Instant.ofEpochMilli(ts)
            .atZone(java.time.ZoneId.systemDefault())
        val today = java.time.LocalDate.now()
        if (zdt.toLocalDate() == today) {
            zdt.format(java.time.format.DateTimeFormatter.ofPattern("HH:mm"))
        } else {
            zdt.format(java.time.format.DateTimeFormatter.ofPattern("d MMM HH:mm"))
        }
    } catch (e: Exception) {
        ""
    }
}

/**
 * Per-tab provider/model picker (#18: model selection lives on each
 * profile's own page). Saves immediately on pick; blanks mean not set.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun TabModelSheet(
    app: android.app.Application,
    tab: String,
    title: String,
    onChanged: () -> Unit,
    onClose: () -> Unit,
) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    var provider by remember { mutableStateOf("") }
    var model by remember { mutableStateOf("") }
    var catalog by remember { mutableStateOf<List<ProviderOption>>(emptyList()) }
    var status by remember { mutableStateOf("") }
    LaunchedEffect(tab) {
        val prefs = context.getSharedPreferences(AgentoApp.PREFS_NAME, android.content.Context.MODE_PRIVATE)
        provider = (prefs.getString("provider_$tab", "") ?: "").trim()
        model = (prefs.getString("model_$tab", "") ?: "").trim()
        val api = ChatApi(context)
        catalog = runCatching {
            api.fetchCatalog(catalogPath(api)).getOrThrow()
        }.getOrDefault(emptyList())
    }
    fun save(p: String, m: String) {
        val api = ChatApi(context)
        api.setChatConfig(api.baseUrl(), api.password(), tab, p, m)
        onChanged()
    }
    ModalBottomSheet(onDismissRequest = onClose) {
        Column(modifier = Modifier.fillMaxWidth().padding(16.dp)) {
            Text("$title model", style = MaterialTheme.typography.titleMedium)
            Spacer(modifier = Modifier.height(8.dp))
            val options = providerOptionsFor(catalog, provider)
            val shownProvider = options.firstOrNull { it.slug == provider }
                ?.let { providerDisplay(it.slug, it.label) }
                ?: provider.ifEmpty { "(select provider)" }
            OptionMenu(
                label = "Provider",
                shown = shownProvider,
                options = options.map { o -> o.slug to providerDisplay(o.slug, o.label) },
                onPick = {
                    if (it != provider) model = ""
                    provider = it
                    save(provider, model)
                },
            )
            Spacer(modifier = Modifier.height(8.dp))
            OptionMenu(
                label = "Model",
                shown = model.ifEmpty { "(select model)" },
                options = modelOptionsFor(options, provider).map { m -> m to m },
                onPick = {
                    model = it
                    save(provider, model)
                },
            )
            if (status.isNotEmpty()) {
                Text(status, style = MaterialTheme.typography.bodySmall)
            }
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                OutlinedButton(
                    onClick = {
                        status = "Loading providers…"
                        scope.launch {
                            val api = ChatApi(context)
                            api.fetchCatalog(api.pathFor(tab), refresh = true).fold(
                                onSuccess = { list ->
                                    catalog = list
                                    status = list.joinToString("\n") { o ->
                                        "${o.label}: ${o.models.size} model(s)"
                                    }
                                },
                                onFailure = { e -> status = "FAILED — ${e.message}" },
                            )
                        }
                    },
                    modifier = Modifier.weight(1f),
                ) {
                    Text("Reload catalog")
                }
            }
            if (catalog.isEmpty()) {
                Text(
                    "Providers not loaded — check Server URL + Password in Settings, then Reload.",
                    style = MaterialTheme.typography.bodySmall,
                )
            }
            Spacer(modifier = Modifier.height(16.dp))
        }
    }
}

/** Streaming chat surface (#52: conversational bubbles); auto-scrolls on
 * new tokens, delegates I/O to callbacks. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun ChatScreen(
    title: String,
    model: String,
    state: ChatUiState,
    onMenu: () -> Unit,
    onModel: () -> Unit,
    onPending: (String) -> Unit,
    onSend: () -> Unit,
    onStop: () -> Unit,
    onNew: () -> Unit,
    onRetry: () -> Unit,
) {
    val listState = rememberLazyListState()
    LaunchedEffect(state.messages.size, state.messages.lastOrNull()?.content?.length) {
        if (state.messages.isNotEmpty()) listState.animateScrollToItem(state.messages.size - 1)
    }
    val clipboard = LocalClipboardManager.current
    Column(modifier = Modifier.fillMaxSize()) {
        TopAppBar(
            navigationIcon = {
                IconButton(onClick = onMenu) {
                    Icon(Icons.Filled.Menu, contentDescription = "Menu")
                }
            },
            title = { Text("$title · $model", maxLines = 1) },
            actions = {
                IconButton(onClick = onModel, enabled = !state.streaming) {
                    Icon(Icons.Filled.Tune, contentDescription = "Model")
                }
                // #62: + starts a new conversation.
                IconButton(onClick = onNew, enabled = !state.streaming) {
                    Icon(Icons.Filled.Add, contentDescription = "New conversation")
                }
            },
        )
        if (state.error.isNotEmpty()) {
            Row(
                modifier = Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 4.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Text(
                    state.error,
                    color = MaterialTheme.colorScheme.error,
                    style = MaterialTheme.typography.bodySmall,
                    modifier = Modifier.weight(1f),
                )
                if (!state.streaming) {
                    TextButton(onClick = onRetry) {
                        Icon(Icons.Filled.Refresh, contentDescription = null)
                        Spacer(modifier = Modifier.width(4.dp))
                        Text("Retry")
                    }
                }
            }
        }
        LazyColumn(
            state = listState,
            modifier = Modifier.weight(1f).fillMaxWidth().padding(horizontal = 12.dp),
            verticalArrangement = Arrangement.spacedBy(8.dp),
            contentPadding = PaddingValues(vertical = 8.dp),
        ) {
            if (state.messages.isEmpty()) {
                item {
                    Text(
                        "No messages yet. Ask anything — the conversation is kept until you tap +.",
                        style = MaterialTheme.typography.bodyMedium,
                    )
                }
            }
            items(state.messages) { msg ->
                val isUser = msg.role == "user"
                // #52: proper conversational bubbles — user right, assistant left.
                Box(
                    modifier = Modifier.fillMaxWidth(),
                    contentAlignment = if (isUser) Alignment.CenterEnd else Alignment.CenterStart,
                ) {
                    Surface(
                        modifier = Modifier.fillMaxWidth(0.85f),
                        shape = RoundedCornerShape(16.dp),
                        color = if (isUser) {
                            MaterialTheme.colorScheme.primaryContainer
                        } else {
                            MaterialTheme.colorScheme.surfaceVariant
                        },
                    ) {
                        Column(modifier = Modifier.padding(12.dp)) {
                            Row(
                                modifier = Modifier.fillMaxWidth(),
                                verticalAlignment = Alignment.CenterVertically,
                            ) {
                                Text(
                                    if (isUser) "You" else "Assistant",
                                    style = MaterialTheme.typography.labelSmall,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                    modifier = Modifier.weight(1f),
                                )
                                val ts = shortTime(msg.ts)
                                if (ts.isNotEmpty()) {
                                    Text(
                                        ts,
                                        style = MaterialTheme.typography.labelSmall,
                                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                                    )
                                }
                                if (!isUser && msg.content.isNotEmpty()) {
                                    IconButton(
                                        onClick = { clipboard.setText(AnnotatedString(msg.content)) },
                                        modifier = Modifier.size(28.dp),
                                    ) {
                                        Icon(Icons.Filled.ContentCopy, contentDescription = "Copy")
                                    }
                                }
                            }
                            Spacer(modifier = Modifier.height(2.dp))
                            SelectionContainer {
                                Text(
                                    msg.content.ifEmpty { "…" },
                                    style = MaterialTheme.typography.bodyMedium,
                                )
                            }
                        }
                    }
                }
            }
        }
        Row(
            modifier = Modifier.fillMaxWidth().padding(12.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            OutlinedTextField(
                value = state.pending,
                onValueChange = onPending,
                label = { Text("Message") },
                modifier = Modifier.weight(1f),
                maxLines = 4,
            )
            Spacer(modifier = Modifier.width(8.dp))
            if (state.streaming) {
                Button(onClick = onStop) { Text("Stop") }
            } else {
                Button(onClick = onSend, enabled = state.pending.isNotBlank()) { Text("Send") }
            }
        }
    }
}

/** Read-only option menu (saved values stay intact; picks write the slug). */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun OptionMenu(
    label: String,
    shown: String,
    options: List<Pair<String, String>>,
    onPick: (String) -> Unit,
) {
    var expanded by remember { mutableStateOf(false) }
    ExposedDropdownMenuBox(
        expanded = expanded,
        onExpandedChange = { expanded = it },
    ) {
        OutlinedTextField(
            value = shown,
            onValueChange = {},
            readOnly = true,
            label = { Text(label) },
            trailingIcon = { ExposedDropdownMenuDefaults.TrailingIcon(expanded = expanded) },
            modifier = Modifier.fillMaxWidth().menuAnchor(),
        )
        ExposedDropdownMenu(
            expanded = expanded,
            onDismissRequest = { expanded = false },
        ) {
            options.forEach { (value, text) ->
                DropdownMenuItem(
                    text = { Text(text) },
                    onClick = { onPick(value); expanded = false },
                )
            }
        }
    }
}

/** Providers are gateway-global; one catalog fetch covers all tabs. */
private fun catalogPath(api: ChatApi): String =
    listOf(api.pathFor("story"), api.pathFor("resumes"), api.pathFor("god")).distinct().first()

/** Display name for a provider slug. The wire value stays the gateway slug
 * (`opencode-go` per the gateway config); the catalog label is preferred. */
private fun providerDisplay(slug: String, label: String): String =
    if (label == slug) slug else "$label ($slug)"

/** Dropdown options for a tab: live catalog, else known slugs; the saved
 * value is always kept so legacy/unknown slugs are never lost. */
private fun providerOptionsFor(catalog: List<ProviderOption>, saved: String): List<ProviderOption> {
    val base = catalog.ifEmpty {
        LlmProvider.entries.map { ProviderOption(it.id, it.id, emptyList()) }
    }
    return if (saved.isNotBlank() && base.none { it.slug == saved }) {
        listOf(ProviderOption(saved, "$saved (saved)", emptyList())) + base
    } else {
        base
    }
}

/** Model options for a tab: ONLY the selected provider's catalog models.
 * No provider selected → no options; nothing is ever borrowed from other
 * providers or stale saves. */
private fun modelOptionsFor(
    options: List<ProviderOption>,
    provider: String,
): List<String> =
    if (provider.isBlank()) emptyList()
    else options.firstOrNull { it.slug == provider }?.models.orEmpty()

/** Settings hub (#60): subsections live in the sidebar and each gets its
 * own screen; model pickers live on each tab's own page (#18). Theme toggle
 * lives under Appearance (#28). */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun SettingsScreen(
    viewModel: MainViewModel,
    section: SettingSection = SettingSection.Server,
    onMenu: () -> Unit = {},
    themeMode: String = ThemeStore.SYSTEM,
    onTheme: (String) -> Unit = {},
) {
    val state by viewModel.state
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    var modelsResult by remember { mutableStateOf("") }
    // Live picker inventory (providers + their models); empty until loaded.
    var catalog by remember { mutableStateOf<List<ProviderOption>>(emptyList()) }
    // Bumped after a settings import so the fields below reload from prefs.
    var settingsRefresh by remember { mutableStateOf(0) }

    /** Preloads the live provider/model catalog (pickers live per tab now). */
    LaunchedEffect(settingsRefresh) {
        val api = ChatApi(context)
        catalog = runCatching {
            api.fetchCatalog(catalogPath(api)).getOrDefault(emptyList())
        }.getOrDefault(emptyList())
    }

    /** Refreshes health state after any permission flow returns. */
    val hcPermissionLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestPermission()
    ) {
        viewModel.refresh()
    }

    val hcRequest = rememberLauncherForActivityResult(
        PermissionController.createRequestPermissionResultContract()
    ) {
        viewModel.refresh()
        if (!viewModel.state.value.permissionsGranted) {
            viewModel.onSyncError("Still not granted — open the Health Connect app manually")
        }
    }

    Column(modifier = Modifier.fillMaxSize()) {
        TopAppBar(
            navigationIcon = {
                IconButton(onClick = onMenu) {
                    Icon(Icons.Filled.Menu, contentDescription = "Menu")
                }
            },
            title = { Text(section.title) },
        )
        Column(
            modifier = Modifier
                .fillMaxSize()
                .verticalScroll(rememberScrollState())
                .padding(16.dp),
            verticalArrangement = Arrangement.spacedBy(12.dp),
        ) {
            when (section) {
                SettingSection.Server -> {
            Text(
                "One URL for chat + sync (reverse proxy routes /p/* to the gateway, /api/* to health sync).",
                style = MaterialTheme.typography.bodySmall,
            )
            OutlinedTextField(
                value = state.serverUrl,
                onValueChange = viewModel::onServerUrl,
                label = { Text("Server URL") },
                placeholder = { Text("http://192.168.1.10:8080") },
                singleLine = true,
                modifier = Modifier.fillMaxWidth(),
            )
            OutlinedTextField(
                value = state.password,
                onValueChange = viewModel::onPassword,
                label = { Text("Password (sync token)") },
                singleLine = true,
                visualTransformation = PasswordVisualTransformation(),
                modifier = Modifier.fillMaxWidth(),
            )
            /** Saves server fields (chat config per tab saves from its own page). */
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                Button(onClick = {
                    val api = ChatApi(context)
                    val prefs = context.getSharedPreferences(
                        AgentoApp.PREFS_NAME, android.content.Context.MODE_PRIVATE)
                    prefs.edit()
                        .putString("server_base_url", state.serverUrl.trim().trimEnd('/'))
                        .putString("app_password", state.password.trim())
                        .remove("api_base_url")
                        .remove("server_url")
                        .apply()
                    modelsResult = "Saved."
                    scope.launch {
                        api.fetchCatalog(catalogPath(api), refresh = true).fold(
                            onSuccess = { list ->
                                catalog = list
                                modelsResult = "Saved. Providers:\n" + list.joinToString("\n") { o ->
                                    "${o.label}: ${o.models.size} model(s)"
                                }
                            },
                            onFailure = { e -> modelsResult = "Saved. Catalog FAILED — ${e.message}" },
                        )
                    }
                }, modifier = Modifier.fillMaxWidth()) {
                    Text("Save server")
                }
            }
            if (catalog.isEmpty()) {
                Text(
                    "Providers not loaded — check Server URL + Password, then Save server.",
                    style = MaterialTheme.typography.bodySmall,
                )
            }
            if (modelsResult.isNotEmpty()) {
                Text(modelsResult, style = MaterialTheme.typography.bodySmall)
            }
                }
                // #61: the "Chat backend" subsection is removed (provider +
                // model are picked on each tab's own page; nothing to show).
                SettingSection.Appearance -> {
            Text("Theme", style = MaterialTheme.typography.bodySmall)
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                listOf(
                    ThemeStore.SYSTEM to "System",
                    ThemeStore.LIGHT to "Light",
                    ThemeStore.DARK to "Dark",
                ).forEach { (value, text) ->
                    Row(
                        verticalAlignment = Alignment.CenterVertically,
                        modifier = Modifier.selectable(
                            selected = themeMode == value,
                            onClick = { onTheme(value) },
                            role = Role.RadioButton,
                        ),
                    ) {
                        RadioButton(selected = themeMode == value, onClick = null)
                        Text(text, style = MaterialTheme.typography.bodyMedium)
                    }
                }
            }
                }
                SettingSection.Updates -> {
            AppUpdateSection()
                }
                SettingSection.Health -> {
            HealthStatusCard(state)

            /** Three-step gate: install Health Connect, grant permissions, then sync. */
            when {
                !state.healthAvailable -> {
                    Button(onClick = {
                        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU &&
                            ContextCompat.checkSelfPermission(context, Manifest.permission.POST_NOTIFICATIONS)
                            != PackageManager.PERMISSION_GRANTED
                        ) {
                            hcPermissionLauncher.launch(Manifest.permission.POST_NOTIFICATIONS)
                        }
                        HealthConnectManager(context).openHealthConnectSettings(context)
                    }) {
                        Text("Install / Update Health Connect")
                    }
                }
                !state.permissionsGranted -> {
                    Button(onClick = {
                        val mgr = HealthConnectManager(context)
                        val launched = mgr.requestPermissions(hcRequest)
                        if (!launched) {
                            viewModel.onSyncError("Permission screen unavailable — opening Health Connect app")
                            mgr.openHealthConnectSettings(context)
                        }
                    }) {
                        Text("Grant Health Connect permissions")
                    }
                    OutlinedButton(onClick = {
                        viewModel.onSyncError("Open Health Connect → Permissions → Agento → allow each")
                        HealthConnectManager(context).openHealthConnectSettings(context)
                    }) {
                        Text("Open Health Connect app")
                    }
                    OutlinedButton(onClick = {
                        val intent = android.content.Intent(
                            android.content.Intent.ACTION_VIEW,
                            android.net.Uri.parse(HealthConnectManager.playStoreUrl()),
                        )
                        runCatching { context.startActivity(intent) }
                    }) {
                        Text("Install / Update Health Connect (Play Store)")
                    }
                    if (state.healthPackageInfo.isNotEmpty()) {
                        Text(
                            "HC package: ${state.healthPackageInfo}",
                            style = MaterialTheme.typography.bodySmall,
                        )
                    }
                }
                else -> {
                    Button(
                        onClick = { viewModel.syncNow() },
                        enabled = !state.syncing,
                    ) {
                        Text(if (state.syncing) "Syncing…" else "Sync now")
                    }
                }
            }


            if (state.lastSyncAt.isNotEmpty()) {
                Text("Last sync: ${state.lastSyncAt}", style = MaterialTheme.typography.bodySmall)
            }
            if (state.lastResult.isNotEmpty()) {
                Text(
                    state.lastResult,
                    style = MaterialTheme.typography.bodyMedium,
                    color = if (state.lastResult.startsWith("FAILED")) {
                        MaterialTheme.colorScheme.error
                    } else {
                        MaterialTheme.colorScheme.onSurface
                    },
                )
            }
                }
                SettingSection.Notifications -> {
                    NotificationsSection()
                }
                SettingSection.Backup -> {
                    SettingsBackupSection(onImported = {
                        viewModel.refresh()
                        settingsRefresh++
                    })
                }
            }
        }
    }
}

/** Hermes notifications hub (issue #58).
 *
 * 1) Scheduler status: the gateway config has no scheduler/cron section, so
 *    Hermes currently has NO cronjobs — there is nothing server-side whose
 *    completion could notify. This text says exactly that.
 * 2) Completion notifications: when a chat reply finishes while the app is
 *    in the background, Agento posts a local notification (toggle below).
 */
@Composable
private fun NotificationsSection() {
    val context = LocalContext.current
    var enabled by remember { mutableStateOf(ChatNotifications.isEnabled(context)) }
    var canPost by remember { mutableStateOf(ChatNotifications.canPost(context)) }
    var status by remember { mutableStateOf("") }

    LaunchedEffect(Unit) {
        canPost = ChatNotifications.canPost(context)
    }

    /** Asks for the runtime notification grant (Android 13+). */
    val permLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestPermission()
    ) { granted ->
        canPost = granted
        status = if (granted) {
            "Allowed — reply alerts will post."
        } else {
            "Denied — enable notifications for Agento in system Settings."
        }
    }

    Card(modifier = Modifier.fillMaxWidth()) {
        Column(
            modifier = Modifier.fillMaxWidth().padding(12.dp),
            verticalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            Text(
                "Gateway scheduler: no cronjobs configured.",
                style = MaterialTheme.typography.titleSmall,
            )
            Text(
                "Hermes has no scheduled jobs, so there are no server-side " +
                    "completions to report. Instead, Agento can notify you " +
                    "when a chat reply finishes while the app is in the background.",
                style = MaterialTheme.typography.bodySmall,
            )
            Row(
                modifier = Modifier.fillMaxWidth(),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Text(
                    "Notify when a reply finishes",
                    style = MaterialTheme.typography.bodyMedium,
                    modifier = Modifier.weight(1f),
                )
                Switch(
                    checked = enabled,
                    onCheckedChange = {
                        enabled = it
                        ChatNotifications.setEnabled(context, it)
                        if (it && !ChatNotifications.canPost(context) &&
                            Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU
                        ) {
                            permLauncher.launch(Manifest.permission.POST_NOTIFICATIONS)
                        }
                    },
                )
            }
            if (enabled && !canPost) {
                Text(
                    "Notifications are blocked — alerts can't post until allowed.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.error,
                )
                OutlinedButton(onClick = {
                    if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
                        permLauncher.launch(Manifest.permission.POST_NOTIFICATIONS)
                    }
                }) {
                    Text("Allow notifications")
                }
            }
            OutlinedButton(
                onClick = {
                    ChatNotifications.sendTest(context)
                    status = "Test sent — check the notification shade."
                },
                modifier = Modifier.fillMaxWidth(),
            ) {
                Text("Send test notification")
            }
            if (status.isNotEmpty()) {
                Text(
                    status,
                    style = MaterialTheme.typography.bodySmall,
                    color = if (status.startsWith("Denied")) {
                        MaterialTheme.colorScheme.error
                    } else {
                        MaterialTheme.colorScheme.onSurface
                    },
                )
            }
        }
    }
}

/** Issue #16: manual settings export/import. SAF pickers need no storage
 * permission; the file the user keeps survives any reinstall gap that outlives
 * cloud backup. [onImported] reloads the on-screen fields from prefs. */
@Composable
private fun SettingsBackupSection(onImported: () -> Unit) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()

    var status by remember { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }

    /** Writes the export JSON to the user-picked location. */
    val exportPicker = rememberLauncherForActivityResult(
        ActivityResultContracts.CreateDocument("application/json")
    ) { uri ->
        if (uri == null) return@rememberLauncherForActivityResult
        busy = true
        status = "Exporting…"
        scope.launch {
            val result = withContext(Dispatchers.IO) {
                runCatching {
                    context.contentResolver.openOutputStream(uri)?.use { out ->
                        out.write(SettingsBackup.export(context).toString(2).toByteArray())
                    } ?: throw IllegalStateException("Could not open $uri for writing")
                }
            }
            status = result.fold(
                onSuccess = { "Exported — keep the file somewhere safe." },
                onFailure = { e -> "FAILED — ${e.message}" },
            )
            busy = false
        }
    }

    /** Reads a previously exported file and applies its known keys. */
    val importPicker = rememberLauncherForActivityResult(
        ActivityResultContracts.OpenDocument()
    ) { uri ->
        if (uri == null) return@rememberLauncherForActivityResult
        busy = true
        status = "Importing…"
        scope.launch {
            val result = withContext(Dispatchers.IO) {
                runCatching {
                    val text = context.contentResolver.openInputStream(uri)?.use { input ->
                        input.bufferedReader().readText()
                    } ?: throw IllegalStateException("Could not open $uri for reading")
                    SettingsBackup.importFrom(context, org.json.JSONObject(text)).getOrThrow()
                }
            }
            result.fold(
                onSuccess = { n ->
                    status = "Imported $n setting(s) — fields reloaded."
                    onImported()
                },
                onFailure = { e -> status = "FAILED — ${e.message}" },
            )
            busy = false
        }
    }

    Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
        OutlinedButton(
            onClick = { exportPicker.launch("agento-settings.json") },
            enabled = !busy,
            modifier = Modifier.weight(1f),
        ) {
            Text("Export settings")
        }
        OutlinedButton(
            onClick = { importPicker.launch(arrayOf("application/json")) },
            enabled = !busy,
            modifier = Modifier.weight(1f),
        ) {
            Text("Import settings")
        }
    }
    if (status.isNotEmpty()) {
        Text(
            status,
            style = MaterialTheme.typography.bodySmall,
            color = if (status.startsWith("FAILED")) {
                MaterialTheme.colorScheme.error
            } else {
                MaterialTheme.colorScheme.onSurface
            },
        )
    }
}

/** Issue #6: check-then-install updater against GitHub Releases on main. */
@Composable
private fun AppUpdateSection() {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()

    val (installedName, installedCode) = remember {
        UpdateManager.currentVersion(context)
    }
    var status by remember { mutableStateOf("") }
    var latest by remember { mutableStateOf<AppRelease?>(null) }
    var busy by remember { mutableStateOf(false) }
    var progress by remember { mutableStateOf(-1f) }

    /** Returns from the "install unknown apps" toggle — install if allowed now. */
    val unknownSourcesReturn = rememberLauncherForActivityResult(
        ActivityResultContracts.StartActivityForResult()
    ) {
        if (UpdateManager.canInstallUnknownApps(context)) {
            status = "Allowed — tap Download & install again."
        } else {
            status = "Still not allowed — enable \"Allow from this source\", then retry."
        }
    }

    Text(
        "Installed: $installedName ($installedCode)",
        style = MaterialTheme.typography.bodySmall,
    )
    /** Checks /releases/latest and diffs the tag against the installed build. */
    Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
        Button(
            onClick = {
                busy = true
                progress = -1f
                status = "Checking…"
                scope.launch {
                    UpdateManager.fetchLatest()
                        .onSuccess { rel ->
                            latest = if (UpdateManager.isNewer(rel.tag, installedName, installedCode)) {
                                status = "Update available: ${rel.name}"
                                rel
                            } else {
                                status = "Up to date (${rel.tag})."
                                null
                            }
                        }
                        .onFailure { e ->
                            latest = null
                            status = "FAILED — ${e.message}"
                        }
                    busy = false
                }
            },
            enabled = !busy,
            modifier = Modifier.weight(1f),
        ) {
            Text("Check for updates")
        }
        Button(
            onClick = {
                val rel = latest ?: return@Button
                if (!UpdateManager.canInstallUnknownApps(context)) {
                    runCatching { unknownSourcesReturn.launch(UpdateManager.unknownSourcesIntent(context)) }
                    status = "Allow \"install unknown apps\", then tap again."
                    return@Button
                }
                busy = true
                progress = 0f
                status = "Downloading ${rel.tag}…"
                scope.launch {
                    val dest = java.io.File(
                        java.io.File(context.cacheDir, "updates"),
                        "agento-${rel.tag}.apk",
                    )
                    UpdateManager.download(rel.apkUrl, dest) { p -> progress = p }
                        .onSuccess { apk ->
                            status = "Downloaded — opening installer…"
                            runCatching {
                                context.startActivity(UpdateManager.installIntent(context, apk))
                            }.onFailure { e ->
                                status = "FAILED — could not open installer: ${e.message}"
                            }
                        }
                        .onFailure { e ->
                            status = "FAILED — ${e.message}"
                        }
                    busy = false
                }
            },
            enabled = !busy && latest != null,
            modifier = Modifier.weight(1f),
        ) {
            Text("Download & install")
        }
    }
    if (busy && progress >= 0f) {
        LinearProgressIndicator(
            progress = { progress.coerceIn(0f, 1f) },
            modifier = Modifier.fillMaxWidth(),
        )
    }
    if (status.isNotEmpty()) {
        Text(
            status,
            style = MaterialTheme.typography.bodySmall,
            color = if (status.startsWith("FAILED")) {
                MaterialTheme.colorScheme.error
            } else {
                MaterialTheme.colorScheme.onSurface
            },
        )
    }
}

/** Summarizes Health Connect install vs. permission state for Settings. */
@Composable
private fun HealthStatusCard(state: UiState) {
    Card(modifier = Modifier.fillMaxWidth()) {
        Column(
            modifier = Modifier
                .fillMaxWidth()
                .padding(16.dp),
            verticalArrangement = Arrangement.spacedBy(4.dp),
        ) {
            val healthText = when {
                !state.healthAvailable -> "Health Connect not installed"
                state.healthUpdateRequired -> "Health Connect update required"
                else -> "Health Connect ready"
            }
            val permText = if (state.permissionsGranted) {
                "Permissions granted"
            } else {
                "Permissions not granted"
            }
            Text(healthText, style = MaterialTheme.typography.bodyLarge)
            Text(permText, style = MaterialTheme.typography.bodyMedium)
        }
    }
}
