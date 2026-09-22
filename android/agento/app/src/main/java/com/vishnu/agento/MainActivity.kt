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
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.ContentCopy
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.Description
import androidx.compose.material.icons.filled.History
import androidx.compose.material.icons.filled.List
import androidx.compose.material.icons.filled.Menu
import androidx.compose.material.icons.filled.MenuBook
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

/** Sidebar destinations; first three map 1:1 to gateway profiles (#18). */
private enum class Destination(val title: String) {
    Story("Story"),
    Resumes("Resumes"),
    God("God"),
    Tasks("Tasks"),
    Settings("Settings"),
}

private fun Destination.icon() = when (this) {
    Destination.Story -> Icons.Filled.MenuBook
    Destination.Resumes -> Icons.Filled.Description
    Destination.God -> Icons.Filled.Star
    Destination.Tasks -> Icons.Filled.List
    Destination.Settings -> Icons.Filled.Settings
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
                var dest by remember { mutableStateOf(Destination.Story) }
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
                    },
                ) {
                    Box {
                        when (dest) {
                            Destination.Story -> ChatTab(
                                app = application, tab = "story", title = "Story",
                                onMenu = { scope.launch { drawerState.open() } },
                            )
                            Destination.Resumes -> ChatTab(
                                app = application, tab = "resumes", title = "Resumes",
                                onMenu = { scope.launch { drawerState.open() } },
                            )
                            Destination.God -> ChatTab(
                                app = application, tab = "god", title = "God",
                                onMenu = { scope.launch { drawerState.open() } },
                            )
                            Destination.Tasks -> TasksScreen()
                            Destination.Settings -> SettingsScreen(
                                healthModel,
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
    var showThreads by remember { mutableStateOf(false) }
    var showModel by remember { mutableStateOf(false) }
    val subtitle = (if (state.model.isEmpty()) "not set" else state.model) +
        " · " + state.provider.ifEmpty { "not set" }
    ChatScreen(
        title = title, model = subtitle, state = state,
        onMenu = onMenu,
        onThreads = { showThreads = true },
        onModel = { showModel = true },
        onPending = vm::onPending, onSend = vm::send, onStop = vm::stop,
        onNew = vm::newConversation, onRetry = vm::retry,
    )
    if (showThreads) {
        ThreadSheet(
            threads = state.threads,
            activeId = state.activeThreadId,
            onSelect = { vm.switchThread(it); showThreads = false },
            onDelete = vm::deleteThread,
            onNew = { vm.newConversation(); showThreads = false },
            onClose = { showThreads = false },
        )
    }
    if (showModel) {
        TabModelSheet(app = app, tab = tab, title = title,
            onChanged = vm::refreshConfig, onClose = { showModel = false })
    }
}

/** Task table: name + status + note rows, persisted locally (#34). */
@Composable
private fun TasksScreen() {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    var tasks by remember { mutableStateOf<List<TaskItem>>(emptyList()) }
    var editing by remember { mutableStateOf<TaskItem?>(null) }
    var loaded by remember { mutableStateOf(false) }

    LaunchedEffect(Unit) {
        tasks = withContext(Dispatchers.IO) { TaskStore.load(context) }
        loaded = true
    }

    fun persist(next: List<TaskItem>) {
        tasks = next
        scope.launch(Dispatchers.IO) { TaskStore.save(context, next) }
    }

    fun cycleStatus(t: TaskItem): String = when (t.status.lowercase()) {
        "todo" -> "doing"
        "doing" -> "done"
        else -> "todo"
    }

    Column(modifier = Modifier.fillMaxSize()) {
        TopAppBar(
            title = { Text("Tasks") },
            actions = {
                TextButton(onClick = {
                    editing = TaskItem(id = TaskStore.newId())
                }) { Text("Add") }
            },
        )
        if (!loaded) {
            Text(
                "Loading…",
                style = MaterialTheme.typography.bodyMedium,
                modifier = Modifier.padding(16.dp),
            )
        } else if (tasks.isEmpty()) {
            Text(
                "No tasks yet. Tap Add to create the first row.",
                style = MaterialTheme.typography.bodyMedium,
                modifier = Modifier.padding(16.dp),
            )
        }
        LazyColumn(
            modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp),
            verticalArrangement = Arrangement.spacedBy(8.dp),
            contentPadding = PaddingValues(vertical = 8.dp),
        ) {
            items(tasks, key = { it.id }) { t ->
                Card(modifier = Modifier.fillMaxWidth()) {
                    Column(modifier = Modifier.padding(12.dp)) {
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            Text(
                                t.name.ifEmpty { "(untitled)" },
                                style = MaterialTheme.typography.titleSmall,
                                modifier = Modifier.weight(1f),
                            )
                            AssistChip(
                                onClick = { persist(tasks.map {
                                    if (it.id == t.id) it.copy(status = cycleStatus(it)) else it
                                }) },
                                label = { Text(t.status.ifEmpty { "todo" }) },
                            )
                        }
                        if (t.note.isNotEmpty()) {
                            Spacer(modifier = Modifier.height(4.dp))
                            Text(t.note, style = MaterialTheme.typography.bodyMedium)
                        }
                        Row(
                            modifier = Modifier.fillMaxWidth(),
                            horizontalArrangement = Arrangement.End,
                        ) {
                            TextButton(onClick = { editing = t }) { Text("Edit") }
                            IconButton(onClick = {
                                persist(tasks.filterNot { it.id == t.id })
                            }) {
                                Icon(Icons.Filled.Delete, contentDescription = "Delete")
                            }
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

/** Add/edit dialog for one task row. */
@Composable
private fun TaskDialog(
    initial: TaskItem,
    isNew: Boolean,
    onDismiss: () -> Unit,
    onSave: (TaskItem) -> Unit,
) {
    var name by remember(initial.id) { mutableStateOf(initial.name) }
    var status by remember(initial.id) { mutableStateOf(initial.status.ifEmpty { "todo" }) }
    var note by remember(initial.id) { mutableStateOf(initial.note) }
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
                OutlinedTextField(
                    value = status,
                    onValueChange = { status = it },
                    label = { Text("Status") },
                    singleLine = true,
                    modifier = Modifier.fillMaxWidth(),
                )
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
                    status = status.trim().ifEmpty { "todo" },
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

/** Thread switcher: past conversations survive New and restarts (#17). */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun ThreadSheet(
    threads: List<Pair<String, String>>,
    activeId: String,
    onSelect: (String) -> Unit,
    onDelete: (String) -> Unit,
    onNew: () -> Unit,
    onClose: () -> Unit,
) {
    ModalBottomSheet(onDismissRequest = onClose) {
        Column(modifier = Modifier.fillMaxWidth().padding(16.dp)) {
            Text("Conversations", style = MaterialTheme.typography.titleMedium)
            Spacer(modifier = Modifier.height(8.dp))
            LazyColumn(modifier = Modifier.heightIn(max = 320.dp)) {
                items(threads, key = { it.first }) { (id, title) ->
                    Row(
                        modifier = Modifier.fillMaxWidth(),
                        verticalAlignment = Alignment.CenterVertically,
                    ) {
                        TextButton(
                            onClick = { onSelect(id) },
                            modifier = Modifier.weight(1f),
                            contentPadding = PaddingValues(8.dp),
                        ) {
                            Text(
                                (if (id == activeId) "● " else "") + title,
                                maxLines = 1,
                                modifier = Modifier.fillMaxWidth(),
                            )
                        }
                        if (threads.size > 1) {
                            IconButton(onClick = { onDelete(id) }) {
                                Icon(Icons.Filled.Delete, contentDescription = "Delete")
                            }
                        }
                    }
                }
            }
            Spacer(modifier = Modifier.height(8.dp))
            Button(onClick = onNew, modifier = Modifier.fillMaxWidth()) {
                Icon(Icons.Filled.Add, contentDescription = null)
                Spacer(modifier = Modifier.width(8.dp))
                Text("New conversation")
            }
            Spacer(modifier = Modifier.height(16.dp))
        }
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

/** Streaming chat surface; auto-scrolls on new tokens, delegates I/O to callbacks. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun ChatScreen(
    title: String,
    model: String,
    state: ChatUiState,
    onMenu: () -> Unit,
    onThreads: () -> Unit,
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
                IconButton(onClick = onThreads, enabled = !state.streaming) {
                    Icon(Icons.Filled.History, contentDescription = "Conversations")
                }
                IconButton(onClick = onModel, enabled = !state.streaming) {
                    Icon(Icons.Filled.Tune, contentDescription = "Model")
                }
                TextButton(onClick = onNew, enabled = !state.streaming) { Text("New") }
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
                        "No messages yet. Ask anything — threads keep history until you delete them.",
                        style = MaterialTheme.typography.bodyMedium,
                    )
                }
            }
            items(state.messages) { msg ->
                val isUser = msg.role == "user"
                Card(
                    modifier = Modifier.fillMaxWidth(),
                    colors = if (isUser) {
                        CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.primaryContainer)
                    } else {
                        CardDefaults.cardColors()
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

/** One labeled section card so Settings reads as groups, not a wall (#17). */
@Composable
private fun SettingsSection(title: String, content: @Composable ColumnScope.() -> Unit) {
    Text(title, style = MaterialTheme.typography.titleMedium)
    Card(modifier = Modifier.fillMaxWidth()) {
        Column(
            modifier = Modifier.fillMaxWidth().padding(12.dp),
            verticalArrangement = Arrangement.spacedBy(8.dp),
            content = content,
        )
    }
}

/** Settings hub, grouped into sections (#17); model pickers moved to each
 * tab's own page (#18). Theme toggle lives under Appearance (#28). */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun SettingsScreen(
    viewModel: MainViewModel,
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

    Column(
        modifier = Modifier
            .fillMaxSize()
            .verticalScroll(rememberScrollState())
            .padding(16.dp),
        verticalArrangement = Arrangement.spacedBy(12.dp),
    ) {
        Text("Settings", style = MaterialTheme.typography.headlineMedium)

        SettingsSection("Server") {
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

        SettingsSection("Chat backend") {
            Text(
                "Provider + model are picked on each tab's own page (top bar ⋮ model button).",
                style = MaterialTheme.typography.bodySmall,
            )
        }

        SettingsSection("Appearance") {
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

        SettingsSection("App updates") {
            AppUpdateSection()
        }

        SettingsSection("Health sync") {
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

        SettingsSection("Settings backup") {
            SettingsBackupSection(onImported = {
                viewModel.refresh()
                settingsRefresh++
            })
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
