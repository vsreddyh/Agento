@file:OptIn(ExperimentalMaterial3Api::class)

package com.vishnu.agento

import android.Manifest
import android.app.Activity
import android.content.ActivityNotFoundException
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import android.speech.RecognizerIntent
import androidx.activity.ComponentActivity
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.compose.setContent
import androidx.activity.result.contract.ActivityResultContracts
import androidx.activity.viewModels
import androidx.compose.foundation.clickable
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.grid.GridCells
import androidx.compose.foundation.lazy.grid.LazyVerticalGrid
import androidx.compose.foundation.lazy.grid.items
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.Send
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.Cloud
import androidx.compose.material.icons.filled.ContentCopy
import androidx.compose.material.icons.filled.Description
import androidx.compose.material.icons.filled.DarkMode
import androidx.compose.material.icons.filled.ExpandLess
import androidx.compose.material.icons.filled.ExpandMore
import androidx.compose.material.icons.filled.Extension
import androidx.compose.material.icons.filled.Folder
import androidx.compose.material.icons.filled.Favorite
import androidx.compose.material.icons.filled.History
import androidx.compose.material.icons.filled.Info
import androidx.compose.material.icons.filled.List
import androidx.compose.material.icons.filled.Menu
import androidx.compose.material.icons.filled.MenuBook
import androidx.compose.material.icons.filled.Mic
import androidx.compose.material.icons.filled.MoreVert
import androidx.compose.material.icons.filled.Notifications
import androidx.compose.material.icons.filled.PlayArrow
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material.icons.filled.Schedule
import androidx.compose.material.icons.filled.Search
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material.icons.filled.Star
import androidx.compose.material.icons.filled.Stop
import androidx.compose.material.icons.filled.Tune
import androidx.compose.material.icons.filled.VolumeOff
import androidx.compose.material.icons.filled.VolumeUp
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.text.input.VisualTransformation
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.core.content.ContextCompat
import androidx.health.connect.client.PermissionController
import androidx.lifecycle.ViewModel
import androidx.lifecycle.ViewModelProvider
import androidx.lifecycle.viewmodel.compose.viewModel
import com.mikepenz.markdown.m3.Markdown
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.async
import kotlinx.coroutines.ensureActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.supervisorScope
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
    Scheduler("Scheduler"),
    Skills("Skills"),
    Settings("Settings"),
}

private fun Destination.icon() = when (this) {
    Destination.Story -> Icons.Filled.MenuBook
    Destination.Portfolio -> Icons.Filled.Description
    Destination.God -> Icons.Filled.Star
    Destination.Tasks -> Icons.Filled.List
    Destination.Storage -> Icons.Filled.Folder
    Destination.Scheduler -> Icons.Filled.Schedule
    Destination.Skills -> Icons.Filled.Extension
    Destination.Settings -> Icons.Filled.Settings
}

/** Settings subsections; each gets its own drawer entry + screen (#60).
 * Taxonomy follows Android conventions (General/Notifications/Data & sync,
 * Storage, About): connection setup first, feature areas next, device data
 * and app info last. */
private enum class SettingSection(val title: String) {
    Connection("Connection"),
    Health("Health sync"),
    Appearance("Appearance"),
    Notifications("Notifications"),
    Storage("Storage"),
    About("About"),
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

    /** Adaptive sidebar navigation (#18, #64); theme from prefs (#28). */
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
            AgentoTheme(dark = dark) {
                // #59: paint the Material background over the full window so
                // dark mode never shows the light window background through.
                Surface(
                    modifier = Modifier.fillMaxSize(),
                    color = MaterialTheme.colorScheme.background,
                ) {
                    var dest by remember { mutableStateOf(Destination.God) }
                    // Null section = the settings hub overview; a non-null
                    // section opens that subsection directly.
                    var section by remember { mutableStateOf<SettingSection?>(null) }
                    var settingsOpen by remember { mutableStateOf(false) }
                    val drawerState = rememberDrawerState(DrawerValue.Closed)
                    val scope = rememberCoroutineScope()
                    // Per-assistant model subtitles for the drawer: refreshed
                    // on every navigation so model picks show up immediately.
                    val prefs = context.getSharedPreferences(
                        AgentoApp.PREFS_NAME, android.content.Context.MODE_PRIVATE)
                    // Built-in TTS: one engine shared by God/Story/Portfolio,
                    // shut down with the activity. Auto-read is a single
                    // global toggle (prefs `tts_auto`) in each chat's bar.
                    val tts = remember { ChatTts(context) }
                    DisposableEffect(Unit) { onDispose { tts.shutdown() } }
                    var autoSpeak by remember {
                        mutableStateOf(prefs.getBoolean("tts_auto", false))
                    }
                    val tabModels = remember(dest, section) {
                        mapOf(
                            "god" to (prefs.getString("model_god", "") ?: "").trim(),
                            "story" to (prefs.getString("model_story", "") ?: "").trim(),
                            "resumes" to (prefs.getString("model_resumes", "") ?: "").trim(),
                        )
                    }
                    fun drawerModel(d: Destination): String? = when (d) {
                        Destination.God -> tabModels["god"]
                        Destination.Story -> tabModels["story"]
                        Destination.Portfolio -> tabModels["resumes"]
                        else -> null
                    }
                    fun go(d: Destination, s: SettingSection? = null) {
                        dest = d
                        section = s
                        if (d != Destination.Settings) settingsOpen = false
                        scope.launch { drawerState.close() }
                    }
                    BoxWithConstraints(modifier = Modifier.fillMaxSize()) {
                        val wc = windowClassFor(maxWidth)
                        val navContent: @Composable () -> Unit = {
                            when (dest) {
                                Destination.God -> ChatTab(
                                    app = application, tab = "god", title = "God", wc = wc,
                                    tts = tts, autoSpeak = autoSpeak,
                                    onAutoSpeak = {
                                        autoSpeak = it
                                        prefs.edit().putBoolean("tts_auto", it).apply()
                                    },
                                    onMenu = { scope.launch { drawerState.open() } },
                                )
                                Destination.Story -> ChatTab(
                                    app = application, tab = "story", title = "Story", wc = wc,
                                    tts = tts, autoSpeak = autoSpeak,
                                    onAutoSpeak = {
                                        autoSpeak = it
                                        prefs.edit().putBoolean("tts_auto", it).apply()
                                    },
                                    onMenu = { scope.launch { drawerState.open() } },
                                )
                                Destination.Portfolio -> ChatTab(
                                    app = application, tab = "resumes", title = "Portfolio", wc = wc,
                                    tts = tts, autoSpeak = autoSpeak,
                                    onAutoSpeak = {
                                        autoSpeak = it
                                        prefs.edit().putBoolean("tts_auto", it).apply()
                                    },
                                    onMenu = { scope.launch { drawerState.open() } },
                                )
                                Destination.Tasks -> TasksScreen(
                                    wc = wc,
                                    onMenu = { scope.launch { drawerState.open() } },
                                )
                                Destination.Storage -> StorageScreen(
                                    onMenu = { scope.launch { drawerState.open() } },
                                )
                                Destination.Scheduler -> SchedulerScreen(
                                    onMenu = { scope.launch { drawerState.open() } },
                                )
                                Destination.Skills -> SkillsScreen(
                                    wc = wc,
                                    onMenu = { scope.launch { drawerState.open() } },
                                )
                                Destination.Settings -> SettingsScreen(
                                    healthModel,
                                    section = section,
                                    wc = wc,
                                    onMenu = { scope.launch { drawerState.open() } },
                                    onPick = { section = it },
                                    themeMode = themeMode,
                                    onTheme = {
                                        themeMode = it
                                        ThemeStore.save(context, it)
                                    },
                                )
                            }
                        }
                        when (wc) {
                            WindowClass.Compact -> {
                                ModalNavigationDrawer(
                                    drawerState = drawerState,
                                    drawerContent = {
                                        DrawerContent(
                                            dest = dest, section = section,
                                            settingsOpen = settingsOpen,
                                            modelFor = ::drawerModel,
                                            onDest = { go(it) },
                                            onSection = { go(Destination.Settings, it) },
                                            onToggleSettings = {
                                                dest = Destination.Settings
                                                section = null
                                                settingsOpen = !settingsOpen
                                            },
                                        )
                                    },
                                ) { Box { navContent() } }
                            }
                            WindowClass.Medium -> {
                                Row(modifier = Modifier.fillMaxSize()) {
                                    NavigationRail {
                                        RailContent(
                                            dest = dest, section = section,
                                            settingsOpen = settingsOpen,
                                            onDest = { go(it) },
                                            onSection = { go(Destination.Settings, it) },
                                            onToggleSettings = {
                                                dest = Destination.Settings
                                                section = null
                                                settingsOpen = !settingsOpen
                                            },
                                        )
                                    }
                                    Box(modifier = Modifier.weight(1f)) { navContent() }
                                }
                            }
                            WindowClass.Expanded -> {
                                PermanentNavigationDrawer(
                                    drawerContent = {
                                        DrawerContent(
                                            dest = dest, section = section,
                                            settingsOpen = settingsOpen,
                                            modelFor = ::drawerModel,
                                            permanent = true,
                                            onDest = { go(it) },
                                            onSection = { go(Destination.Settings, it) },
                                            onToggleSettings = {
                                                dest = Destination.Settings
                                                section = null
                                                settingsOpen = !settingsOpen
                                            },
                                        )
                                    },
                                ) { Box { navContent() } }
                            }
                        }
                    }
                }
            }
        }
    }
}

/** Drawer body shared by modal + permanent drawers. */
@Composable
private fun DrawerContent(
    dest: Destination,
    section: SettingSection?,
    settingsOpen: Boolean,
    onDest: (Destination) -> Unit,
    onSection: (SettingSection) -> Unit,
    onToggleSettings: () -> Unit,
    permanent: Boolean = false,
    modelFor: (Destination) -> String? = { null },
) {
    if (permanent) {
        PermanentDrawerSheet(modifier = Modifier.widthIn(max = 280.dp)) {
            DrawerList(dest, section, settingsOpen, onDest, onSection, onToggleSettings, modelFor)
        }
    } else {
        ModalDrawerSheet {
            DrawerList(dest, section, settingsOpen, onDest, onSection, onToggleSettings, modelFor)
        }
    }
}

@Composable
private fun DrawerList(
    dest: Destination,
    section: SettingSection?,
    settingsOpen: Boolean,
    onDest: (Destination) -> Unit,
    onSection: (SettingSection) -> Unit,
    onToggleSettings: () -> Unit,
    modelFor: (Destination) -> String? = { null },
) {
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
                onClick = onToggleSettings,
                modifier = Modifier.padding(horizontal = 8.dp),
            )
            // #60: settings subsections live in the
            // sidebar; each opens its own screen.
            if (settingsOpen) {
                SettingSection.entries.forEach { s ->
                    NavigationDrawerItem(
                        label = { Text(s.title) },
                        selected = dest == Destination.Settings && section == s,
                        onClick = { onSection(s) },
                        modifier = Modifier.padding(horizontal = 8.dp)
                            .padding(start = 24.dp),
                    )
                }
            }
        } else {
            val sub = modelFor(d)
            NavigationDrawerItem(
                label = {
                    Column {
                        Text(d.title, maxLines = 1)
                        if (sub != null) {
                            Text(
                                sub.ifEmpty { "Not set up" },
                                style = MaterialTheme.typography.labelSmall,
                                color = if (sub.isEmpty()) {
                                    MaterialTheme.colorScheme.error
                                } else {
                                    MaterialTheme.colorScheme.onSurfaceVariant
                                },
                                maxLines = 1,
                            )
                        }
                    }
                },
                icon = { Icon(d.icon(), contentDescription = null) },
                selected = dest == d,
                onClick = { onDest(d) },
                modifier = Modifier.padding(horizontal = 8.dp),
            )
        }
    }
}

/** Compact rail for medium windows (icon-only + expandable settings). */
@Composable
private fun RailContent(
    dest: Destination,
    section: SettingSection?,
    settingsOpen: Boolean,
    onDest: (Destination) -> Unit,
    onSection: (SettingSection) -> Unit,
    onToggleSettings: () -> Unit,
) {
    Destination.entries.forEach { d ->
        if (d == Destination.Settings) {
            NavigationRailItem(
                selected = dest == d,
                onClick = onToggleSettings,
                icon = { Icon(d.icon(), contentDescription = d.title) },
                label = { Text(d.title) },
            )
            if (settingsOpen) {
                SettingSection.entries.forEach { s ->
                    NavigationRailItem(
                        selected = dest == Destination.Settings && section == s,
                        onClick = { onSection(s) },
                        icon = { },
                        label = { Text(s.title) },
                    )
                }
            }
        } else {
            NavigationRailItem(
                selected = dest == d,
                onClick = { onDest(d) },
                icon = { Icon(d.icon(), contentDescription = d.title) },
                label = { Text(d.title) },
            )
        }
    }
}

/** Scopes one chat ViewModel per tab key so drafts/history survive tab switches. */
@Composable
private fun ChatTab(
    app: android.app.Application,
    tab: String,
    title: String,
    wc: WindowClass,
    tts: ChatTts,
    autoSpeak: Boolean,
    onAutoSpeak: (Boolean) -> Unit,
    onMenu: () -> Unit,
) {
    val factory = remember(tab) { ChatViewModelFactory(app, tab) }
    // Keyed per tab — otherwise all three tabs would share one ViewModel.
    val vm: ChatViewModel = viewModel(key = "chat_$tab", factory = factory)
    val state by vm.state
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    LaunchedEffect(Unit) { vm.refreshConfig() }
    var showModel by remember { mutableStateOf(false) }
    var showThreads by remember { mutableStateOf(false) }
    val snackbar = remember { SnackbarHostState() }
    // #64: never show raw "not set" — the header shows the model name when
    // configured and a setup prompt otherwise.
    val setupNeeded = state.model.isBlank() || state.provider.isBlank()
    ChatScreen(
        title = title, modelLabel = state.model.ifBlank { "" },
        setupNeeded = setupNeeded, state = state, wc = wc, snackbar = snackbar,
        tts = tts, autoSpeak = autoSpeak, onAutoSpeak = onAutoSpeak,
        onMenu = onMenu,
        onModel = { showModel = true },
        onHistory = { showThreads = true },
        onCheckConnection = vm::checkReachability,
        onPending = vm::onPending, onSend = vm::send, onStop = vm::stop,
        onNew = vm::newConversation, onRetry = vm::retry,
    )
    if (showThreads) {
        ThreadSheet(
            title = title,
            threads = state.threads,
            activeId = state.threadId,
            onSearch = vm::searchThreads,
            onSwitch = { tts.stop(); vm.switchThread(it); showThreads = false },
            onNew = { tts.stop(); vm.newConversation() },
            onRename = vm::renameThread,
            onDelete = {
                tts.stop()
                vm.deleteThread(it)
                scope.launch { snackbar.showSnackbar("Conversation deleted.") }
            },
            onExport = { id ->
                val text = vm.exportMarkdown(id)
                if (text.isNotEmpty()) {
                    val intent = android.content.Intent(android.content.Intent.ACTION_SEND).apply {
                        type = "text/plain"
                        putExtra(android.content.Intent.EXTRA_TEXT, text)
                    }
                    runCatching {
                        context.startActivity(android.content.Intent.createChooser(intent, "Share conversation"))
                    }
                }
            },
            onClose = { showThreads = false },
        )
    }
    if (showModel) {
        TabModelSheet(app = app, tab = tab, title = title,
            onChanged = vm::refreshConfig, onClose = { showModel = false })
    }
}

/** Conversation switcher: search, jump, rename, delete, export. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun ThreadSheet(
    title: String,
    threads: List<ThreadSummary>,
    activeId: String,
    onSearch: (String) -> List<ThreadSummary>,
    onSwitch: (String) -> Unit,
    onNew: () -> Unit,
    onRename: (String, String) -> Unit,
    onDelete: (String) -> Unit,
    onExport: (String) -> Unit,
    onClose: () -> Unit,
) {
    var query by remember { mutableStateOf("") }
    var renaming by remember { mutableStateOf<ThreadSummary?>(null) }
    var deleting by remember { mutableStateOf<ThreadSummary?>(null) }
    ModalBottomSheet(onDismissRequest = onClose) {
        Column(modifier = Modifier.fillMaxWidth().padding(16.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text(
                    "$title conversations",
                    style = MaterialTheme.typography.titleMedium,
                    modifier = Modifier.weight(1f),
                )
                FilledTonalButton(onClick = onNew) { Text("New") }
            }
            Spacer(modifier = Modifier.height(8.dp))
            OutlinedTextField(
                value = query,
                onValueChange = { query = it },
                placeholder = { Text("Search conversations…") },
                leadingIcon = { Icon(Icons.Filled.Search, contentDescription = null) },
                singleLine = true,
                modifier = Modifier.fillMaxWidth(),
            )
            Spacer(modifier = Modifier.height(8.dp))
            val shown = remember(query, threads) { onSearch(query) }
            if (shown.isEmpty()) {
                Text(
                    if (query.isBlank()) "No conversations yet." else "No matches.",
                    style = MaterialTheme.typography.bodyMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(vertical = 16.dp),
                )
            } else {
                LazyColumn(
                    verticalArrangement = Arrangement.spacedBy(4.dp),
                    contentPadding = PaddingValues(bottom = 16.dp),
                ) {
                    items(shown, key = { it.id }) { t ->
                        var menu by remember(t.id) { mutableStateOf(false) }
                        Card(
                            onClick = { onSwitch(t.id) },
                            modifier = Modifier.fillMaxWidth(),
                            colors = if (t.id == activeId) {
                                CardDefaults.cardColors(
                                    containerColor = MaterialTheme.colorScheme.primaryContainer
                                )
                            } else CardDefaults.cardColors(),
                        ) {
                            Row(
                                modifier = Modifier.fillMaxWidth().padding(12.dp),
                                verticalAlignment = Alignment.CenterVertically,
                            ) {
                                Column(modifier = Modifier.weight(1f)) {
                                    Text(t.title, style = MaterialTheme.typography.bodyLarge, maxLines = 1)
                                    val ts = shortTime(t.updatedAt)
                                    Text(
                                        listOfNotNull(
                                            if (t.count == 1) "1 message" else "${t.count} messages",
                                            ts.ifEmpty { null },
                                        ).joinToString(" · "),
                                        style = MaterialTheme.typography.bodySmall,
                                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                                    )
                                }
                                Box {
                                    IconButton(onClick = { menu = true }) {
                                        Icon(Icons.Filled.MoreVert, contentDescription = "Conversation menu")
                                    }
                                    DropdownMenu(expanded = menu, onDismissRequest = { menu = false }) {
                                        DropdownMenuItem(
                                            text = { Text("Rename") },
                                            onClick = { menu = false; renaming = t },
                                        )
                                        DropdownMenuItem(
                                            text = { Text("Share / export") },
                                            onClick = { menu = false; onExport(t.id) },
                                        )
                                        DropdownMenuItem(
                                            text = { Text("Delete") },
                                            onClick = { menu = false; deleting = t },
                                        )
                                    }
                                }
                            }
                        }
                    }
                }
            }
        }
    }
    val renameTarget = renaming
    if (renameTarget != null) {
        var name by remember(renameTarget.id) { mutableStateOf(renameTarget.title) }
        AlertDialog(
            onDismissRequest = { renaming = null },
            title = { Text("Rename conversation") },
            text = {
                OutlinedTextField(
                    value = name,
                    onValueChange = { name = it },
                    label = { Text("Title") },
                    singleLine = true,
                    modifier = Modifier.fillMaxWidth(),
                )
            },
            confirmButton = {
                TextButton(onClick = {
                    onRename(renameTarget.id, name)
                    renaming = null
                }) { Text("Save") }
            },
            dismissButton = {
                TextButton(onClick = { renaming = null }) { Text("Cancel") }
            },
        )
    }
    val deleteTarget = deleting
    if (deleteTarget != null) {
        AlertDialog(
            onDismissRequest = { deleting = null },
            title = { Text("Delete conversation?") },
            text = { Text("“${deleteTarget.title}” and its messages will be removed from this device. This can't be undone.") },
            confirmButton = {
                TextButton(onClick = {
                    onDelete(deleteTarget.id)
                    deleting = null
                }) { Text("Delete") }
            },
            dismissButton = {
                TextButton(onClick = { deleting = null }) { Text("Cancel") }
            },
        )
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

/** Task board: fixed statuses, filters + sorts, persisted locally (#34, #55). */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun TasksScreen(wc: WindowClass, onMenu: () -> Unit = {}) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    val snackbar = remember { SnackbarHostState() }
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

    fun persist(next: List<TaskItem>, toast: String? = null) {
        tasks = next
        scope.launch(Dispatchers.IO) { TaskStore.save(context, next) }
        if (toast != null) scope.launch { snackbar.showSnackbar(toast) }
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

    Scaffold(
        snackbarHost = { SnackbarHost(snackbar) },
        topBar = {
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
                if (!loaded) {
                    Box(modifier = Modifier.fillMaxWidth().padding(32.dp), contentAlignment = Alignment.Center) {
                        CircularProgressIndicator()
                    }
                } else if (visible.isEmpty()) {
                    EmptyState(
                        icon = Icons.Filled.List,
                        title = if (tasks.isEmpty()) "No tasks yet" else "Nothing matches this filter",
                        subtitle = if (tasks.isEmpty()) {
                            "Capture your first to-do — it stays on this device."
                        } else {
                            "Try a different status filter."
                        },
                        actionLabel = if (tasks.isEmpty()) "New task" else null,
                        onAction = if (tasks.isEmpty()) {
                            { editing = TaskItem(id = TaskStore.newId(), status = "Todo") }
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
                                onCycle = {
                                    persist(tasks.map {
                                        if (it.id == t.id) it.copy(
                                            status = cycleStatus(it),
                                            updatedAt = ChatThreads.now(),
                                        ) else it
                                    }, Toasts.TASK_SAVED)
                                },
                                onEdit = { editing = t.copy(status = normalizeStatus(t.status)) },
                                onDelete = {
                                    persist(tasks.filterNot { it.id == t.id }, Toasts.TASK_DELETED)
                                },
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
                                onCycle = {
                                    persist(tasks.map {
                                        if (it.id == t.id) it.copy(
                                            status = cycleStatus(it),
                                            updatedAt = ChatThreads.now(),
                                        ) else it
                                    }, Toasts.TASK_SAVED)
                                },
                                onEdit = { editing = t.copy(status = normalizeStatus(t.status)) },
                                onDelete = {
                                    persist(tasks.filterNot { it.id == t.id }, Toasts.TASK_DELETED)
                                },
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
            isNew = tasks.none { it.id == draft.id },
            onDismiss = { editing = null },
            onSave = { saved ->
                val next = if (tasks.any { it.id == saved.id }) {
                    tasks.map { if (it.id == saved.id) saved else it }
                } else {
                    listOf(saved) + tasks
                }
                persist(next, Toasts.TASK_SAVED)
                editing = null
            },
        )
    }
}

/** One task card: title + note + colored status chip + timestamp + menu. */
@Composable
private fun TaskCard(
    task: TaskItem,
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
                val ts = shortTime(task.updatedAt)
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
            TextButton(
                onClick = {
                    onSave(initial.copy(
                        name = name.trim(),
                        status = status,
                        note = note.trim(),
                        updatedAt = ChatThreads.now(),
                    ))
                },
                enabled = name.isNotBlank(),
            ) { Text("Save") }
        },
        dismissButton = {
            TextButton(onClick = onDismiss) { Text("Cancel") }
        },
    )
}

/** VPS exports browser with breadcrumbs + mobile downloads (#26). */
@Composable
private fun StorageScreen(onMenu: () -> Unit = {}) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    val snackbar = remember { SnackbarHostState() }
    var path by remember { mutableStateOf("") }
    var dirs by remember { mutableStateOf<List<RemoteEntry>>(emptyList()) }
    var files by remember { mutableStateOf<List<RemoteEntry>>(emptyList()) }
    var error by remember { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }

    fun load(p: String) {
        busy = true
        error = ""
        scope.launch {
            StorageApi(context).list(p).fold(
                onSuccess = { (cur, d, f) ->
                    path = cur.ifEmpty { "" }
                    dirs = d
                    files = f
                },
                onFailure = { e -> error = e.message ?: e.javaClass.simpleName },
            )
            busy = false
        }
    }

    LaunchedEffect(Unit) { load("") }

    val crumbs = remember(path) {
        if (path.isEmpty()) emptyList()
        else path.split("/").scan("") { acc, part ->
            if (acc.isEmpty()) part else "$acc/$part"
        }.drop(1)
    }

    Scaffold(
        snackbarHost = { SnackbarHost(snackbar) },
        topBar = {
            TopAppBar(
                title = { Text("Storage", maxLines = 1) },
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
                    IconButton(onClick = { load(path) }, enabled = !busy) {
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
            Column(modifier = Modifier.contentWidth(WindowClass.Compact)) {
                if (path.isNotEmpty()) {
                    Row(
                        modifier = Modifier.fillMaxWidth()
                            .horizontalScroll(rememberScrollState())
                            .padding(horizontal = 12.dp, vertical = 4.dp),
                        verticalAlignment = Alignment.CenterVertically,
                    ) {
                        TextButton(onClick = { load("") }) { Text("Files") }
                        crumbs.forEach { c ->
                            Text(" / ", color = MaterialTheme.colorScheme.onSurfaceVariant)
                            TextButton(onClick = { load(c) }) {
                                Text(c.substringAfterLast("/"), maxLines = 1)
                            }
                        }
                    }
                }
                if (busy) {
                    LinearProgressIndicator(modifier = Modifier.fillMaxWidth())
                }
                if (error.isNotEmpty()) {
                    ErrorCard(raw = error, onRetry = { load(path) },
                        modifier = Modifier.padding(12.dp))
                }
                if (!busy && dirs.isEmpty() && files.isEmpty() && error.isEmpty()) {
                    EmptyState(
                        icon = Icons.Filled.Folder,
                        title = "This folder is empty",
                        subtitle = "Drop files into the VPS exports folder to see them here.",
                        actionLabel = "Refresh",
                        onAction = { load(path) },
                    )
                }
                LazyColumn(
                    modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp),
                    verticalArrangement = Arrangement.spacedBy(8.dp),
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
                                Icon(Icons.Filled.Description, contentDescription = null)
                                Spacer(modifier = Modifier.width(8.dp))
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
                                        scope.launch { snackbar.showSnackbar("Saving ${f.name}…") }
                                    }.onFailure { e ->
                                        scope.launch {
                                            snackbar.showSnackbar(
                                                friendlyError(e.message ?: "").title
                                            )
                                        }
                                    }
                                }) { Text("Save") }
                            }
                        }
                    }
                }
            }
        }
    }
}

/** Gateway scheduler: native cron jobs over the Jobs API (same auth as
 * chat, through the proxy's /p/ route — no server changes needed). */
@Composable
private fun SchedulerScreen(onMenu: () -> Unit = {}) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    val snackbar = remember { SnackbarHostState() }
    var jobs by remember { mutableStateOf<List<CronJob>>(emptyList()) }
    var error by remember { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }
    var loaded by remember { mutableStateOf(false) }
    var editing by remember { mutableStateOf<CronJob?>(null) }
    var creating by remember { mutableStateOf(false) }
    var deleting by remember { mutableStateOf<CronJob?>(null) }

    fun path(): String = ChatApi(context).pathFor("god")

    fun load(silent: Boolean = false) {
        if (!silent) { busy = true; error = "" }
        scope.launch {
            JobsApi(context).list(path()).fold(
                onSuccess = { jobs = it; loaded = true },
                onFailure = { e ->
                    error = e.message ?: e.javaClass.simpleName
                    loaded = true
                },
            )
            busy = false
        }
    }

    fun mutate(work: suspend () -> Result<Unit>, okToast: String) {
        scope.launch {
            work().fold(
                onSuccess = {
                    load(silent = true)
                    snackbar.showSnackbar(okToast)
                },
                onFailure = { e ->
                    snackbar.showSnackbar(friendlyError(e.message ?: "").title)
                },
            )
        }
    }

    LaunchedEffect(Unit) { load() }

    Scaffold(
        snackbarHost = { SnackbarHost(snackbar) },
        topBar = {
            TopAppBar(
                title = { Text("Scheduler") },
                navigationIcon = {
                    IconButton(onClick = onMenu) {
                        Icon(Icons.Filled.Menu, contentDescription = "Menu")
                    }
                },
                actions = {
                    IconButton(onClick = { creating = true }) {
                        Icon(Icons.Filled.Add, contentDescription = "New job")
                    }
                    IconButton(onClick = { load() }, enabled = !busy) {
                        Icon(Icons.Filled.Refresh, contentDescription = "Refresh")
                    }
                },
            )
        },
    ) { padding ->
        Column(modifier = Modifier.fillMaxSize().padding(padding)) {
            if (busy) LinearProgressIndicator(modifier = Modifier.fillMaxWidth())
            if (error.isNotEmpty() && jobs.isEmpty()) {
                ErrorCard(raw = error, onRetry = { load() },
                    modifier = Modifier.padding(12.dp))
            }
            if (loaded && jobs.isEmpty() && error.isEmpty()) {
                EmptyState(
                    icon = Icons.Filled.Schedule,
                    title = "No scheduled jobs",
                    subtitle = "Jobs run prompts on a schedule, even when the app is closed.",
                    actionLabel = "New job",
                    onAction = { creating = true },
                )
            }
            LazyColumn(
                modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp),
                verticalArrangement = Arrangement.spacedBy(8.dp),
                contentPadding = PaddingValues(vertical = 8.dp),
            ) {
                items(jobs, key = { it.id }) { job ->
                    var menu by remember(job.id) { mutableStateOf(false) }
                    Card(modifier = Modifier.fillMaxWidth()) {
                        Column(modifier = Modifier.fillMaxWidth().padding(12.dp)) {
                            Row(verticalAlignment = Alignment.CenterVertically) {
                                Column(modifier = Modifier.weight(1f)) {
                                    Text(
                                        job.name.ifEmpty { "(unnamed job)" },
                                        style = MaterialTheme.typography.bodyLarge,
                                        maxLines = 1,
                                    )
                                    if (job.schedule.isNotEmpty()) {
                                        Text(
                                            job.schedule,
                                            style = MaterialTheme.typography.bodySmall,
                                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                                        )
                                    }
                                }
                                val paused = job.paused
                                AssistChip(
                                    onClick = {
                                        if (paused == true) {
                                            mutate({ JobsApi(context).resume(path(), job.id) }, "Job resumed.")
                                        } else {
                                            mutate({ JobsApi(context).pause(path(), job.id) }, "Job paused.")
                                        }
                                    },
                                    label = { Text(when (paused) { true -> "Paused"; false -> "Active"; null -> "—" }) },
                                )
                                Box {
                                    IconButton(onClick = { menu = true }) {
                                        Icon(Icons.Filled.MoreVert, contentDescription = "Job menu")
                                    }
                                    DropdownMenu(expanded = menu, onDismissRequest = { menu = false }) {
                                        DropdownMenuItem(
                                            text = { Text("Run now") },
                                            onClick = {
                                                menu = false
                                                mutate({ JobsApi(context).runNow(path(), job.id) }, "Job started.")
                                            },
                                        )
                                        DropdownMenuItem(
                                            text = { Text("Edit") },
                                            onClick = { menu = false; editing = job },
                                        )
                                        DropdownMenuItem(
                                            text = { Text("Delete") },
                                            onClick = { menu = false; deleting = job },
                                        )
                                    }
                                }
                            }
                            if (job.prompt.isNotEmpty()) {
                                Text(
                                    job.prompt,
                                    style = MaterialTheme.typography.bodySmall,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                    maxLines = 3,
                                )
                            }
                            val meta = listOfNotNull(
                                job.nextRun.takeIf { it.isNotEmpty() }?.let { "Next: $it" },
                                job.lastRun.takeIf { it.isNotEmpty() }?.let { "Last: $it" },
                                job.delivery.takeIf { it.isNotEmpty() }?.let { "To: $it" },
                            ).joinToString(" · ")
                            if (meta.isNotEmpty()) {
                                Text(
                                    meta,
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

    if (creating) {
        JobDialog(
            initial = null,
            onDismiss = { creating = false },
            onSave = { name, prompt, schedule, delivery ->
                creating = false
                mutate(
                    { JobsApi(context).create(path(), name, prompt, schedule, delivery) },
                    "Job created.",
                )
            },
        )
    }
    val editTarget = editing
    if (editTarget != null) {
        JobDialog(
            initial = editTarget,
            onDismiss = { editing = null },
            onSave = { name, prompt, schedule, delivery ->
                editing = null
                mutate(
                    { JobsApi(context).update(path(), editTarget.id, name, prompt, schedule, delivery) },
                    "Job updated.",
                )
            },
        )
    }
    val deleteTarget = deleting
    if (deleteTarget != null) {
        AlertDialog(
            onDismissRequest = { deleting = null },
            title = { Text("Delete job?") },
            text = { Text("“${deleteTarget.name.ifEmpty { deleteTarget.schedule.ifEmpty { "Unnamed job" } }}” will stop running. This can't be undone.") },
            confirmButton = {
                TextButton(onClick = {
                    deleting = null
                    mutate({ JobsApi(context).delete(path(), deleteTarget.id) }, "Job deleted.")
                }) { Text("Delete") }
            },
            dismissButton = {
                TextButton(onClick = { deleting = null }) { Text("Cancel") }
            },
        )
    }
}

/** Create/edit dialog for one scheduled job. */
@Composable
private fun JobDialog(
    initial: CronJob?,
    onDismiss: () -> Unit,
    onSave: (name: String, prompt: String, schedule: String, delivery: String) -> Unit,
) {
    var name by remember { mutableStateOf(initial?.name ?: "") }
    var prompt by remember { mutableStateOf(initial?.prompt ?: "") }
    var schedule by remember { mutableStateOf(initial?.schedule ?: "") }
    var delivery by remember { mutableStateOf(initial?.delivery ?: "local") }
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(if (initial == null) "New job" else "Edit job") },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                OutlinedTextField(
                    value = name,
                    onValueChange = { name = it },
                    label = { Text("Name (optional)") },
                    singleLine = true,
                    modifier = Modifier.fillMaxWidth(),
                )
                OutlinedTextField(
                    value = prompt,
                    onValueChange = { prompt = it },
                    label = { Text("Prompt") },
                    modifier = Modifier.fillMaxWidth(),
                    maxLines = 4,
                )
                OutlinedTextField(
                    value = schedule,
                    onValueChange = { schedule = it },
                    label = { Text("Schedule (cron, e.g. 0 9 * * *)") },
                    singleLine = true,
                    modifier = Modifier.fillMaxWidth(),
                )
                OutlinedTextField(
                    value = delivery,
                    onValueChange = { delivery = it },
                    label = { Text("Deliver to (local, telegram, …)") },
                    singleLine = true,
                    modifier = Modifier.fillMaxWidth(),
                )
            }
        },
        confirmButton = {
            TextButton(
                onClick = { onSave(name.trim(), prompt.trim(), schedule.trim(), delivery.trim()) },
                enabled = prompt.isNotBlank() && schedule.isNotBlank(),
            ) { Text("Save") }
        },
        dismissButton = {
            TextButton(onClick = onDismiss) { Text("Cancel") }
        },
    )
}

/** Origin filter for the Skills section (default = bundled Hermes). */
private enum class SkillsOrigin(val title: String) {
    All("All"),
    Default("Default"),
    Custom("Custom"),
}

/** Sort order for the Skills section lists. */
private enum class SkillsSort(val title: String) {
    NameAz("Name A–Z"),
    NameZa("Name Z–A"),
    Category("Category"),
}

/** Origin filter for the Tools section (custom = project MCP servers). */
private enum class ToolsOrigin(val title: String) {
    All("All"),
    Default("Default"),
    CustomMcp("Custom MCP"),
}

/** Sort order for the Tools section lists. */
private enum class ToolsSort(val title: String) {
    NameAz("Name A–Z"),
    NameZa("Name Z–A"),
    MostTools("Most tools"),
}

/** One skill row: name + description/category + On/Off badge. */
@Composable
private fun SkillCard(s: SkillInfo) {
    Card(modifier = Modifier.fillMaxWidth()) {
        Row(
            modifier = Modifier.fillMaxWidth().padding(12.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Column(modifier = Modifier.weight(1f)) {
                Text(s.name, style = MaterialTheme.typography.bodyLarge)
                val blurb = s.description.ifEmpty { s.category }
                if (blurb.isNotEmpty()) {
                    Text(
                        blurb,
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        maxLines = 3,
                    )
                }
            }
            when (s.enabled) {
                true -> Text(
                    "On",
                    style = MaterialTheme.typography.labelMedium,
                    color = MaterialTheme.colorScheme.primary,
                )
                false -> Text(
                    "Off",
                    style = MaterialTheme.typography.labelMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                null -> { }
            }
        }
    }
}

/** One toolset row: label + name + description/tool list + On badge. */
@Composable
private fun ToolsetCard(ts: ToolsetInfo) {
    Card(modifier = Modifier.fillMaxWidth()) {
        Row(
            modifier = Modifier.fillMaxWidth().padding(12.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Column(modifier = Modifier.weight(1f)) {
                Text(ts.label.ifEmpty { ts.name },
                    style = MaterialTheme.typography.bodyLarge)
                if (ts.label.isNotEmpty()) {
                    Text(
                        ts.name,
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
                val blurb = ts.description.ifEmpty {
                    if (ts.tools.isEmpty()) "" else
                        "${ts.tools.size} tool(s): " +
                            ts.tools.take(10).joinToString(", ") +
                            if (ts.tools.size > 10) "…" else ""
                }
                if (blurb.isNotEmpty()) {
                    Text(
                        blurb,
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        maxLines = 4,
                    )
                }
            }
            // Off items are filtered above; unknown
            // state shows no badge rather than Off.
            when (ts.enabled) {
                true -> Text(
                    "On",
                    style = MaterialTheme.typography.labelMedium,
                    color = MaterialTheme.colorScheme.primary,
                )
                else -> { }
            }
        }
    }
}

/** One derived MCP server row: name + tool list. */
@Composable
private fun McpCard(server: McpServer) {
    Card(modifier = Modifier.fillMaxWidth()) {
        Column(modifier = Modifier.fillMaxWidth().padding(12.dp)) {
            Text(server.name, style = MaterialTheme.typography.bodyLarge)
            Text(
                "${server.tools.size} tool(s): ${server.tools.take(8).joinToString(", ")}" +
                    if (server.tools.size > 8) "…" else "",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
}

/** Read-only skills + tools inventory per assistant, in two sections —
 * Skills (Default vs Custom) and Tools (Default vs Custom MCP) — each with
 * Tasks-style search filtering, origin FilterChips and a sort dropdown.
 * Origin rule: in-the-box Hermes skills/toolsets are default, everything
 * else (project skills, money/cookbook/health-check MCP, `mcp-*`) is custom.
 */
@Composable
private fun SkillsScreen(wc: WindowClass, onMenu: () -> Unit = {}) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    var profile by remember { mutableStateOf("god") }
    var skills by remember { mutableStateOf<List<SkillInfo>>(emptyList()) }
    var toolsets by remember { mutableStateOf<List<ToolsetInfo>>(emptyList()) }
    var skillsError by remember { mutableStateOf("") }
    var toolsError by remember { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }
    var loaded by remember { mutableStateOf(false) }
    // Cancelled + replaced on every load() so rapid profile taps can't
    // let a stale response win; only the latest job may clear busy.
    var loadJob by remember { mutableStateOf<Job?>(null) }

    fun load() {
        loadJob?.cancel()
        busy = true
        skillsError = ""
        toolsError = ""
        loaded = false
        val path = ChatApi(context).pathFor(profile)
        var job: Job? = null
        job = scope.launch {
            try {
                supervisorScope {
                    val s = async { ServerApi(context).listSkills(path) }
                    val t = async { ServerApi(context).listToolsets(path) }
                    // Await into locals first: a cancel landing between the
                    // two awaits must not leave half-stale state behind.
                    val sr = s.await()
                    val tr = t.await()
                    ensureActive()
                    if (loadJob == job) {
                        skills = sr.getOrDefault(emptyList())
                        toolsets = tr.getOrDefault(emptyList())
                        skillsError = sr.exceptionOrNull()?.let {
                            it.message ?: it.javaClass.simpleName
                        } ?: ""
                        toolsError = tr.exceptionOrNull()?.let {
                            it.message ?: it.javaClass.simpleName
                        } ?: ""
                        loaded = true
                    }
                }
            } finally {
                if (loadJob == job) busy = false
            }
        }
        loadJob = job
    }

    LaunchedEffect(profile) { load() }

    val tabs = listOf("god" to "God", "story" to "Story", "resumes" to "Portfolio")
    // UI hides explicitly-off toolsets only; unknown toggle state (null)
    // stays visible so flag-less server shapes never blank the section.
    val visibleToolsets = remember(toolsets) { toolsets.filter { it.enabled != false } }
    val mcp = remember(visibleToolsets) { mcpServersFrom(visibleToolsets) }

    // Per-section search + origin filter + sort (Tasks-style).
    var query by remember { mutableStateOf("") }
    var skillsOrigin by remember { mutableStateOf(SkillsOrigin.All) }
    var skillsSort by remember { mutableStateOf(SkillsSort.NameAz) }
    var skillsSortOpen by remember { mutableStateOf(false) }
    var toolsOrigin by remember { mutableStateOf(ToolsOrigin.All) }
    var toolsSort by remember { mutableStateOf(ToolsSort.NameAz) }
    var toolsSortOpen by remember { mutableStateOf(false) }

    fun sortSkills(list: List<SkillInfo>): List<SkillInfo> = when (skillsSort) {
        SkillsSort.NameZa -> list.sortedByDescending { it.name.lowercase() }
        SkillsSort.Category -> list.sortedWith(
            compareBy({ it.category.ifEmpty { "\uFFFF" }.lowercase() }, { it.name.lowercase() })
        )
        SkillsSort.NameAz -> list.sortedBy { it.name.lowercase() }
    }
    // Origin rule (see ServerApi): bundled Hermes skills carry a category,
    // project skills don't — so non-blank category means default.
    val defaultSkills = remember(skills, query, skillsSort) {
        sortSkills(skills.filter { it.isDefault() && it.matches(query) })
    }
    val customSkills = remember(skills, query, skillsSort) {
        sortSkills(skills.filter { !it.isDefault() && it.matches(query) })
    }
    val shownDefaultSkills = if (skillsOrigin == SkillsOrigin.Custom) emptyList() else defaultSkills
    val shownCustomSkills = if (skillsOrigin == SkillsOrigin.Default) emptyList() else customSkills

    fun sortToolsets(list: List<ToolsetInfo>): List<ToolsetInfo> = when (toolsSort) {
        ToolsSort.NameZa -> list.sortedByDescending { it.label.ifEmpty { it.name }.lowercase() }
        ToolsSort.MostTools -> list.sortedWith(
            compareByDescending<ToolsetInfo> { it.tools.size }
                .thenBy { it.label.ifEmpty { it.name }.lowercase() }
        )
        ToolsSort.NameAz -> list.sortedBy { it.label.ifEmpty { it.name }.lowercase() }
    }
    val defaultTools = remember(visibleToolsets, query, toolsSort) {
        sortToolsets(visibleToolsets.filter { !it.isCustomMcp() && it.matches(query) })
    }
    val customMcpToolsets = remember(visibleToolsets, query, toolsSort) {
        sortToolsets(visibleToolsets.filter { it.isCustomMcp() && it.matches(query) })
    }
    val customMcpServers = remember(mcp, query, toolsSort) {
        val filtered = mcp.filter { it.matches(query) }
        if (toolsSort == ToolsSort.NameZa) {
            filtered.sortedByDescending { it.name.lowercase() }
        } else {
            filtered.sortedWith(
                compareBy({ if (toolsSort == ToolsSort.MostTools) -it.tools.size else 0 },
                    { it.name.lowercase() })
            )
        }
    }
    val shownDefaultTools = if (toolsOrigin == ToolsOrigin.CustomMcp) emptyList() else defaultTools
    val shownCustomMcpToolsets =
        if (toolsOrigin == ToolsOrigin.Default) emptyList() else customMcpToolsets
    val shownCustomMcpServers =
        if (toolsOrigin == ToolsOrigin.Default) emptyList() else customMcpServers

    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text("Skills & tools") },
                navigationIcon = {
                    IconButton(onClick = onMenu) {
                        Icon(Icons.Filled.Menu, contentDescription = "Menu")
                    }
                },
                actions = {
                    IconButton(onClick = { load() }, enabled = !busy) {
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
                ) {
                    tabs.forEach { (key, label) ->
                        FilterChip(
                            selected = profile == key,
                            onClick = { profile = key },
                            label = { Text(label) },
                        )
                    }
                }
                OutlinedTextField(
                    value = query,
                    onValueChange = { query = it },
                    placeholder = { Text("Search skills & tools…") },
                    leadingIcon = { Icon(Icons.Filled.Search, contentDescription = null) },
                    trailingIcon = if (query.isEmpty()) null else ({
                        IconButton(onClick = { query = "" }) {
                            Icon(Icons.Filled.Close, contentDescription = "Clear search")
                        }
                    }),
                    singleLine = true,
                    modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp, vertical = 4.dp),
                )
                if (busy && !loaded) {
                    Box(modifier = Modifier.fillMaxWidth().padding(32.dp), contentAlignment = Alignment.Center) {
                        CircularProgressIndicator()
                    }
                }
                // Hoisted above the list so memoization isn't position-keyed.
                val skillsHint = remember(skillsError) {
                    "Skills unavailable (${friendlyError(skillsError).title}) — " +
                        "tools below still work."
                }
                val toolsHint = remember(toolsError) {
                    "Tools unavailable (${friendlyError(toolsError).title}) — " +
                        "skills above still work."
                }
                LazyColumn(
                    modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp),
                    verticalArrangement = Arrangement.spacedBy(8.dp),
                    contentPadding = PaddingValues(vertical = 8.dp),
                ) {
                    // Fully-empty screen: one error card with retry, joining both
                    // raws when both sources failed so Details keeps everything.
                    val bothEmptyError = listOf(toolsError, skillsError)
                        .filter { it.isNotEmpty() }
                        .joinToString("\n\n")
                    if (skills.isEmpty() && visibleToolsets.isEmpty()
                        && bothEmptyError.isNotEmpty() && loaded
                    ) {
                        item {
                            ErrorCard(raw = bothEmptyError, onRetry = { load() })
                        }
                    }
                    if (loaded && skills.isEmpty() && visibleToolsets.isEmpty()
                        && skillsError.isEmpty() && toolsError.isEmpty()
                    ) {
                        item {
                            EmptyState(
                                icon = Icons.Filled.Extension,
                                title = "Nothing listed",
                                subtitle = "This assistant reports no skills or toolsets.",
                                actionLabel = "Refresh",
                                onAction = { load() },
                            )
                        }
                    }
                    // ── SECTION 1: Skills (Default vs Custom) ──
                    item {
                        Text(
                            "Skills",
                            style = MaterialTheme.typography.titleMedium,
                            modifier = Modifier.padding(horizontal = 4.dp, vertical = 4.dp),
                        )
                    }
                    item {
                        Row(
                            modifier = Modifier.fillMaxWidth()
                                .horizontalScroll(rememberScrollState()),
                            horizontalArrangement = Arrangement.spacedBy(8.dp),
                            verticalAlignment = Alignment.CenterVertically,
                        ) {
                            SkillsOrigin.entries.forEach { o ->
                                FilterChip(
                                    selected = skillsOrigin == o,
                                    onClick = { skillsOrigin = o },
                                    label = { Text(o.title) },
                                )
                            }
                            Box {
                                TextButton(onClick = { skillsSortOpen = true }) {
                                    Text("Sort: ${skillsSort.title}")
                                }
                                DropdownMenu(
                                    expanded = skillsSortOpen,
                                    onDismissRequest = { skillsSortOpen = false },
                                ) {
                                    SkillsSort.entries.forEach { s ->
                                        DropdownMenuItem(
                                            text = { Text(s.title) },
                                            onClick = { skillsSort = s; skillsSortOpen = false },
                                        )
                                    }
                                }
                            }
                        }
                    }
                    if (shownDefaultSkills.isNotEmpty()) {
                        item {
                            Text(
                                "Default skills (${shownDefaultSkills.size})",
                                style = MaterialTheme.typography.titleSmall,
                                modifier = Modifier.padding(horizontal = 4.dp),
                            )
                        }
                        items(shownDefaultSkills, key = { "ds:" + it.name }) { s ->
                            SkillCard(s)
                        }
                    }
                    if (shownCustomSkills.isNotEmpty()) {
                        item {
                            Text(
                                "Custom skills (${shownCustomSkills.size})",
                                style = MaterialTheme.typography.titleSmall,
                                modifier = Modifier.padding(horizontal = 4.dp),
                            )
                        }
                        items(shownCustomSkills, key = { "cs:" + it.name }) { s ->
                            SkillCard(s)
                        }
                    }
                    // Partial failure with the other side intact: slim hint only
                    // (the full-empty card above already covers both-empty).
                    if (skills.isEmpty() && skillsError.isNotEmpty()
                        && visibleToolsets.isNotEmpty() && loaded
                    ) {
                        item {
                            HintLine(skillsHint)
                        }
                    }
                    if (loaded && skills.isNotEmpty()
                        && shownDefaultSkills.isEmpty() && shownCustomSkills.isEmpty()
                        && skillsError.isEmpty()
                    ) {
                        item {
                            HintLine("No skills match this search or filter.")
                        }
                    }
                    // ── SECTION 2: Tools (Default vs Custom MCP) ──
                    item {
                        Text(
                            "Tools",
                            style = MaterialTheme.typography.titleMedium,
                            modifier = Modifier.padding(horizontal = 4.dp, vertical = 4.dp),
                        )
                    }
                    item {
                        Row(
                            modifier = Modifier.fillMaxWidth()
                                .horizontalScroll(rememberScrollState()),
                            horizontalArrangement = Arrangement.spacedBy(8.dp),
                            verticalAlignment = Alignment.CenterVertically,
                        ) {
                            ToolsOrigin.entries.forEach { o ->
                                FilterChip(
                                    selected = toolsOrigin == o,
                                    onClick = { toolsOrigin = o },
                                    label = { Text(o.title) },
                                )
                            }
                            Box {
                                TextButton(onClick = { toolsSortOpen = true }) {
                                    Text("Sort: ${toolsSort.title}")
                                }
                                DropdownMenu(
                                    expanded = toolsSortOpen,
                                    onDismissRequest = { toolsSortOpen = false },
                                ) {
                                    ToolsSort.entries.forEach { s ->
                                        DropdownMenuItem(
                                            text = { Text(s.title) },
                                            onClick = { toolsSort = s; toolsSortOpen = false },
                                        )
                                    }
                                }
                            }
                        }
                    }
                    if (shownDefaultTools.isNotEmpty()) {
                        item {
                            Text(
                                "Default tools (${shownDefaultTools.size})",
                                style = MaterialTheme.typography.titleSmall,
                                modifier = Modifier.padding(horizontal = 4.dp),
                            )
                        }
                        items(shownDefaultTools, key = { "dt:" + it.name }) { ts ->
                            ToolsetCard(ts)
                        }
                    }
                    if (shownCustomMcpToolsets.isNotEmpty() || shownCustomMcpServers.isNotEmpty()) {
                        item {
                            Text(
                                "Custom MCP (${shownCustomMcpToolsets.size + shownCustomMcpServers.size})",
                                style = MaterialTheme.typography.titleSmall,
                                modifier = Modifier.padding(horizontal = 4.dp),
                            )
                        }
                        item {
                            Text(
                                "Read-only — servers are configured on the gateway.",
                                style = MaterialTheme.typography.bodySmall,
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                            )
                        }
                        items(shownCustomMcpToolsets, key = { "cm:" + it.name }) { ts ->
                            ToolsetCard(ts)
                        }
                        items(shownCustomMcpServers, key = { "ms:" + it.name }) { server ->
                            McpCard(server)
                        }
                    }
                    if (visibleToolsets.isEmpty() && toolsError.isNotEmpty()
                        && skills.isNotEmpty() && loaded
                    ) {
                        item {
                            HintLine(toolsHint)
                        }
                    }
                    if (loaded && visibleToolsets.isNotEmpty()
                        && shownDefaultTools.isEmpty()
                        && shownCustomMcpToolsets.isEmpty() && shownCustomMcpServers.isEmpty()
                        && toolsError.isEmpty()
                    ) {
                        item {
                            HintLine("No tools match this search or filter.")
                        }
                    }
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

/** Stable TTS key per message (timestamp + content hash; ms precision
 * alone could theoretically collide across regenerate/retry). */
private fun ttsKeyFor(msg: ChatMessage): String = "${msg.ts}:${msg.content.hashCode()}"

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
    var provider by remember { mutableStateOf("") }
    var model by remember { mutableStateOf("") }
    var catalog by remember { mutableStateOf<List<ProviderOption>>(emptyList()) }
    var loading by remember { mutableStateOf(true) }
    var error by remember { mutableStateOf("") }
    LaunchedEffect(tab) {
        loading = true
        error = ""
        val prefs = context.getSharedPreferences(AgentoApp.PREFS_NAME, android.content.Context.MODE_PRIVATE)
        provider = (prefs.getString("provider_$tab", "") ?: "").trim()
        model = (prefs.getString("model_$tab", "") ?: "").trim()
        val api = ChatApi(context)
        catalog = runCatching {
            api.fetchCatalog(catalogPath(api)).getOrThrow()
        }.getOrElse { e ->
            error = e.message ?: e.javaClass.simpleName
            emptyList()
        }
        loading = false
    }
    fun save(p: String, m: String) {
        val api = ChatApi(context)
        api.setChatConfig(api.baseUrl(), api.password(), tab, p, m)
        onChanged()
    }
    ModalBottomSheet(onDismissRequest = onClose) {
        Column(modifier = Modifier.fillMaxWidth().padding(16.dp)) {
            Text("$title model", style = MaterialTheme.typography.titleMedium)
            Text(
                "Each assistant keeps its own model. Pick a provider first, then a model.",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            Spacer(modifier = Modifier.height(12.dp))
            val options = providerOptionsFor(catalog, provider)
            val shownProvider = options.firstOrNull { it.slug == provider }
                ?.let { providerDisplay(it.slug, it.label) }
                ?: provider.ifEmpty { "Choose a provider…" }
            OptionMenu(
                label = "Provider",
                shown = shownProvider,
                options = options.map { o -> o.slug to providerDisplay(o.slug, o.label) },
                onPick = {
                    if (it != provider) model = ""
                    provider = it
                    error = ""
                    save(provider, model)
                },
            )
            Spacer(modifier = Modifier.height(8.dp))
            OptionMenu(
                label = "Model",
                shown = model.ifEmpty { "Choose a model…" },
                options = modelOptionsFor(options, provider).map { m -> m to m },
                onPick = {
                    model = it
                    error = ""
                    save(provider, model)
                },
            )
            Spacer(modifier = Modifier.height(8.dp))
            when {
                loading -> {
                    Row(verticalAlignment = Alignment.CenterVertically) {
                        CircularProgressIndicator(modifier = Modifier.size(20.dp))
                        Spacer(modifier = Modifier.width(8.dp))
                        Text(
                            "Loading providers…",
                            style = MaterialTheme.typography.bodySmall,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                }
                error.isNotEmpty() -> {
                    ErrorCard(raw = error)
                    Spacer(modifier = Modifier.height(4.dp))
                    HintLine("Check Server URL + Password in Settings, then reopen this picker.")
                }
                catalog.isEmpty() -> {
                    HintLine("No providers found — check Settings → Server.")
                }
                provider.isNotBlank() && modelOptionsFor(options, provider).isEmpty() -> {
                    HintLine("This provider has no models listed — try Reload below.")
                }
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
    modelLabel: String,
    setupNeeded: Boolean,
    state: ChatUiState,
    wc: WindowClass,
    snackbar: SnackbarHostState,
    tts: ChatTts,
    autoSpeak: Boolean,
    onAutoSpeak: (Boolean) -> Unit,
    onMenu: () -> Unit,
    onModel: () -> Unit,
    onHistory: () -> Unit,
    onCheckConnection: () -> Unit,
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
    val scope = rememberCoroutineScope()
    val speakingKey by tts.speakingKey.collectAsState()
    // Leaving the tab cuts speech (one shared engine for all three tabs).
    DisposableEffect(Unit) { onDispose { tts.stop() } }
    /** Every send-type action cuts speech first. */
    fun stopThen(action: () -> Unit): () -> Unit = { tts.stop(); action() }
    fun speakMessage(key: String, text: String) {
        if (!tts.toggle(key, text)) {
            scope.launch { snackbar.showSnackbar("Voice output unavailable.") }
        }
    }
    // Auto-read: when a reply finishes cleanly while the toggle is on, speak it.
    var wasStreaming by remember { mutableStateOf(false) }
    LaunchedEffect(state.streaming) {
        if (wasStreaming && !state.streaming && autoSpeak && state.error.isEmpty()) {
            val last = state.messages.lastOrNull()
            if (last != null && last.role == "assistant" && last.content.isNotBlank()) {
                speakMessage(ttsKeyFor(last), last.content)
            }
        }
        wasStreaming = state.streaming
    }
    // Built-in Android speech-to-text (RecognizerIntent — no extra
    // permission or dependency). Shared by God/Story/Portfolio: all three
    // tabs render this one ChatScreen, so one mic covers all chats.
    val voiceLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.StartActivityForResult()
    ) { result ->
        if (result.resultCode == Activity.RESULT_OK) {
            val heard = result.data
                ?.getStringArrayListExtra(RecognizerIntent.EXTRA_RESULTS)
                ?.firstOrNull()?.trim().orEmpty()
            if (heard.isNotEmpty()) {
                val cur = state.pending
                onPending(if (cur.isBlank()) heard else "${cur.trimEnd()} $heard")
            }
        }
    }
    fun startVoice() {
        val intent = Intent(RecognizerIntent.ACTION_RECOGNIZE_SPEECH).apply {
            putExtra(
                RecognizerIntent.EXTRA_LANGUAGE_MODEL,
                RecognizerIntent.LANGUAGE_MODEL_FREE_FORM,
            )
            putExtra(RecognizerIntent.EXTRA_PROMPT, "Speak now")
            putExtra(RecognizerIntent.EXTRA_MAX_RESULTS, 1)
        }
        // No resolveActivity pre-check: on Android 11+ it needs a
        // <queries> declaration to see the recognizer, and the launch
        // try/catch below already covers a missing handler.
        try {
            voiceLauncher.launch(intent)
        } catch (e: ActivityNotFoundException) {
            scope.launch { snackbar.showSnackbar("No voice input app found.") }
        }
    }
    // Surface send failures as a toast too (the inline card keeps details).
    LaunchedEffect(state.error) {
        if (state.error.isNotEmpty()) {
            snackbar.showSnackbar(friendlyError(state.error).title)
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
                title = {
                    Column {
                        Text(title, maxLines = 1)
                        if (setupNeeded) {
                            Text(
                                "Choose a model to start",
                                style = MaterialTheme.typography.labelMedium,
                                color = MaterialTheme.colorScheme.primary,
                                maxLines = 1,
                                overflow = TextOverflow.Ellipsis,
                            )
                        } else {
                            Text(
                                modelLabel,
                                style = MaterialTheme.typography.labelMedium,
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                                maxLines = 1,
                                overflow = TextOverflow.Ellipsis,
                            )
                        }
                    }
                },
                actions = {
                    IconButton(onClick = onModel, enabled = !state.streaming) {
                        Icon(Icons.Filled.Tune, contentDescription = "Model")
                    }
                    IconButton(onClick = onHistory, enabled = !state.streaming) {
                        Icon(Icons.Filled.History, contentDescription = "Conversations")
                    }
                    // Auto-read: speak each finished reply aloud.
                    IconButton(onClick = { onAutoSpeak(!autoSpeak) }) {
                        Icon(
                            if (autoSpeak) Icons.Filled.VolumeUp else Icons.Filled.VolumeOff,
                            contentDescription = if (autoSpeak) {
                                "Auto-read on"
                            } else {
                                "Auto-read off"
                            },
                        )
                    }
                    // + starts a new conversation; older ones are kept.
                    IconButton(onClick = stopThen(onNew), enabled = !state.streaming) {
                        Icon(Icons.Filled.Add, contentDescription = "New conversation")
                    }
                },
            )
        },
        bottomBar = {
            // imePadding: belt-and-braces above adjustResize — a no-op when
            // the window already resized, but lifts the input above the
            // keyboard on any soft-input mode that pans instead.
            Surface(tonalElevation = 2.dp, modifier = Modifier.imePadding()) {
                Column {
                    if (state.streaming) {
                        LinearProgressIndicator(modifier = Modifier.fillMaxWidth())
                    }
                    Row(
                        modifier = Modifier.fillMaxWidth().padding(12.dp),
                        verticalAlignment = Alignment.CenterVertically,
                    ) {
                        OutlinedTextField(
                            value = state.pending,
                            onValueChange = onPending,
                            placeholder = { Text("Ask ${title.lowercase()} anything…") },
                            modifier = Modifier.weight(1f),
                            maxLines = 4,
                            keyboardOptions = KeyboardOptions(imeAction = ImeAction.Send),
                            keyboardActions = KeyboardActions(onSend = { stopThen(onSend)() }),
                            shape = RoundedCornerShape(24.dp),
                        )
                        Spacer(modifier = Modifier.width(8.dp))
                        IconButton(
                            onClick = ::startVoice,
                            enabled = !state.streaming,
                        ) {
                            Icon(Icons.Filled.Mic, contentDescription = "Voice input")
                        }
                        if (state.streaming) {
                            FilledTonalIconButton(onClick = onStop) {
                                Icon(Icons.Filled.Close, contentDescription = "Stop")
                            }
                        } else {
                            FilledIconButton(
                                onClick = stopThen(onSend),
                                enabled = state.pending.isNotBlank() && state.ready,
                            ) {
                                Icon(Icons.AutoMirrored.Filled.Send, contentDescription = "Send")
                            }
                        }
                    }
                }
            }
        },
    ) { padding ->
        Column(
            modifier = Modifier.fillMaxSize().padding(padding),
            horizontalAlignment = Alignment.CenterHorizontally,
        ) {
            Column(modifier = Modifier.contentWidth(wc).weight(1f)) {
                if (setupNeeded) {
                    Card(
                        onClick = onModel,
                        modifier = Modifier.fillMaxWidth().padding(12.dp),
                        colors = CardDefaults.cardColors(
                            containerColor = MaterialTheme.colorScheme.primaryContainer
                        ),
                    ) {
                        Column(modifier = Modifier.padding(16.dp)) {
                            Text(
                                "Set up $title",
                                style = MaterialTheme.typography.titleSmall,
                                color = MaterialTheme.colorScheme.onPrimaryContainer,
                            )
                            Text(
                                "Pick a provider and model to start chatting.",
                                style = MaterialTheme.typography.bodyMedium,
                                color = MaterialTheme.colorScheme.onPrimaryContainer,
                            )
                        }
                    }
                }
                if (state.error.isNotEmpty()) {
                    ErrorCard(
                        raw = state.error,
                        onRetry = if (!state.streaming) stopThen(onRetry) else null,
                        modifier = Modifier.padding(horizontal = 12.dp, vertical = 4.dp),
                    )
                }
                // Offline banner: local history still works, sends will fail.
                if (state.online == false && state.error.isEmpty()) {
                    Card(
                        modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp, vertical = 4.dp),
                        colors = CardDefaults.cardColors(
                            containerColor = MaterialTheme.colorScheme.secondaryContainer
                        ),
                    ) {
                        Row(
                            modifier = Modifier.fillMaxWidth().padding(12.dp),
                            verticalAlignment = Alignment.CenterVertically,
                        ) {
                            Text(
                                "You're offline — showing saved conversations.",
                                style = MaterialTheme.typography.bodyMedium,
                                color = MaterialTheme.colorScheme.onSecondaryContainer,
                                modifier = Modifier.weight(1f),
                            )
                            TextButton(onClick = onCheckConnection) { Text("Retry") }
                        }
                    }
                }
                if (!state.ready) {
                    Box(modifier = Modifier.fillMaxWidth().padding(32.dp), contentAlignment = Alignment.Center) {
                        CircularProgressIndicator()
                    }
                } else {
                    LazyColumn(
                        state = listState,
                        modifier = Modifier.weight(1f).fillMaxWidth().padding(horizontal = 12.dp),
                        verticalArrangement = Arrangement.spacedBy(8.dp),
                        contentPadding = PaddingValues(vertical = 8.dp),
                    ) {
                        if (state.messages.isEmpty() && !setupNeeded) {
                            item {
                                EmptyState(
                                    icon = Icons.Filled.MenuBook,
                                    title = "Start the conversation",
                                    subtitle = "Ask anything — past conversations stay under the history button.",
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
                                    modifier = Modifier.fillMaxWidth(bubbleFraction(wc)),
                                    shape = RoundedCornerShape(16.dp),
                                    color = if (isUser) {
                                        MaterialTheme.colorScheme.primaryContainer
                                    } else {
                                        MaterialTheme.colorScheme.surfaceVariant
                                    },
                                    tonalElevation = if (isUser) 0.dp else 1.dp,
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
                                                val key = ttsKeyFor(msg)
                                                val speaking = speakingKey == key
                                                // Disabled mid-stream: the key is content-based,
                                                // so a partial snapshot would speak stale text.
                                                IconButton(
                                                    onClick = { speakMessage(key, msg.content) },
                                                    enabled = !state.streaming,
                                                    modifier = Modifier.size(28.dp),
                                                ) {
                                                    Icon(
                                                        if (speaking) Icons.Filled.Stop else Icons.Filled.PlayArrow,
                                                        contentDescription = if (speaking) {
                                                            "Stop reading"
                                                        } else {
                                                            "Read aloud"
                                                        },
                                                    )
                                                }
                                                IconButton(
                                                    onClick = { clipboard.setText(AnnotatedString(msg.content)) },
                                                    modifier = Modifier.size(28.dp),
                                                ) {
                                                    Icon(Icons.Filled.ContentCopy, contentDescription = "Copy")
                                                }
                                            }
                                        }
                                        Spacer(modifier = Modifier.height(2.dp))
                                        if (isUser) {
                                            SelectionContainer {
                                                Text(
                                                    msg.content.ifEmpty { "…" },
                                                    style = MaterialTheme.typography.bodyMedium,
                                                )
                                            }
                                        } else {
                                            // Assistant messages render Markdown
                                            // (code blocks, lists); copy keeps the source.
                                            Markdown(
                                                content = msg.content.ifEmpty { "…" },
                                                modifier = Modifier.fillMaxWidth(),
                                            )
                                        }
                                    }
                                }
                            }
                        }
                    }
                }
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
            if (options.isEmpty()) {
                DropdownMenuItem(
                    text = { Text("No options — reload or check Settings") },
                    onClick = { expanded = false },
                    enabled = false,
                )
            } else {
                options.forEach { (value, text) ->
                    DropdownMenuItem(
                        text = { Text(text) },
                        onClick = { onPick(value); expanded = false },
                    )
                }
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
private fun SettingsScreen(
    viewModel: MainViewModel,
    section: SettingSection?,
    wc: WindowClass = WindowClass.Compact,
    onMenu: () -> Unit = {},
    onPick: (SettingSection) -> Unit = {},
    themeMode: String = ThemeStore.SYSTEM,
    onTheme: (String) -> Unit = {},
) {
    val state by viewModel.state
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    val snackbar = remember { SnackbarHostState() }
    var error by remember { mutableStateOf("") }
    // Live picker inventory (providers + their models); empty until loaded.
    var catalog by remember { mutableStateOf<List<ProviderOption>>(emptyList()) }
    // Bumped after a settings import so the fields below reload from prefs.
    var settingsRefresh by remember { mutableStateOf(0) }

    /** Preloads the live provider/model catalog (pickers live per tab now). */
    LaunchedEffect(settingsRefresh) {
        error = ""
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
            viewModel.onSyncError("Permission screen returned without a grant")
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
                title = { Text(section?.title ?: "Settings") },
            )
        },
    ) { padding ->
        Column(
            modifier = Modifier.fillMaxSize().padding(padding),
            horizontalAlignment = Alignment.CenterHorizontally,
        ) {
            Column(
                modifier = Modifier
                    .contentWidth(wc)
                    .verticalScroll(rememberScrollState())
                    .padding(16.dp),
                verticalArrangement = Arrangement.spacedBy(12.dp),
            ) {
                when (section) {
                    null -> {
                        SettingsHub(
                            serverStatus = if (state.serverUrl.isBlank()) {
                                "Not set up"
                            } else if (catalog.isNotEmpty()) {
                                "Connected"
                            } else {
                                state.serverUrl
                            },
                            healthStatus = when {
                                !state.healthAvailable -> "Not installed"
                                !state.permissionsGranted -> "Setup needed"
                                state.syncing -> "Syncing…"
                                state.lastSyncAt.isNotEmpty() -> "Synced"
                                else -> "Ready"
                            },
                            themeStatus = when (themeMode) {
                                ThemeStore.LIGHT -> "Light"
                                ThemeStore.DARK -> "Dark"
                                else -> "System"
                            },
                            notificationsStatus = if (ChatNotifications.isEnabled(context)) "On" else "Off",
                            appVersion = remember(context) {
                                UpdateManager.currentVersion(context).first
                            },
                            onPick = onPick,
                        )
                    }
                    SettingSection.Connection -> {
                        SectionCard(
                            title = "Connection",
                            subtitle = "One URL for chat + sync. The proxy routes chat to the gateway and health data to the sync service.",
                        ) {
                            OutlinedTextField(
                                value = state.serverUrl,
                                onValueChange = viewModel::onServerUrl,
                                label = { Text("Server URL") },
                                placeholder = { Text("http://192.168.1.10:8080") },
                                singleLine = true,
                                keyboardOptions = KeyboardOptions(imeAction = ImeAction.Next),
                                modifier = Modifier.fillMaxWidth(),
                            )
                            var showPassword by remember { mutableStateOf(false) }
                            OutlinedTextField(
                                value = state.password,
                                onValueChange = viewModel::onPassword,
                                label = { Text("Password") },
                                singleLine = true,
                                visualTransformation = if (showPassword) {
                                    VisualTransformation.None
                                } else {
                                    PasswordVisualTransformation()
                                },
                                trailingIcon = {
                                    TextButton(onClick = { showPassword = !showPassword }) {
                                        Text(if (showPassword) "Hide" else "Show")
                                    }
                                },
                                keyboardOptions = KeyboardOptions(imeAction = ImeAction.Done),
                                keyboardActions = KeyboardActions(onDone = { /* saved via button */ }),
                                modifier = Modifier.fillMaxWidth(),
                            )
                            /** Saves server fields (chat config per tab saves from its own page). */
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
                                error = ""
                                scope.launch { snackbar.showSnackbar(Toasts.SERVER_SAVED) }
                                scope.launch {
                                    api.fetchCatalog(catalogPath(api), refresh = true).fold(
                                        onSuccess = { list -> catalog = list },
                                        onFailure = { e ->
                                            error = e.message ?: e.javaClass.simpleName
                                        },
                                    )
                                }
                            }, modifier = Modifier.fillMaxWidth()) {
                                Text("Save server")
                            }
                            var testing by remember { mutableStateOf(false) }
                            var report by remember { mutableStateOf<ChatApi.ConnectionReport?>(null) }
                            OutlinedButton(
                                onClick = {
                                    testing = true
                                    report = null
                                    error = ""
                                    scope.launch {
                                        val api = ChatApi(context)
                                        val r = api.testConnection(catalogPath(api)).getOrNull()
                                        report = r
                                        if (r == null) {
                                            error = "Server URL not configured"
                                        }
                                        testing = false
                                    }
                                },
                                enabled = !testing,
                                modifier = Modifier.fillMaxWidth(),
                            ) {
                                Text(if (testing) "Testing…" else "Test connection")
                            }
                            if (testing) {
                                LinearProgressIndicator(modifier = Modifier.fillMaxWidth())
                            }
                            val rep = report
                            if (rep != null) {
                                if (rep.gatewayOk && rep.syncOk) {
                                    HintLine("Connected — chat (${rep.providers} provider(s), ${rep.models} model(s)) and sync both reachable.")
                                    catalog = catalog.ifEmpty {
                                        LlmProvider.entries.map {
                                            ProviderOption(it.id, it.id, emptyList())
                                        }
                                    }
                                } else {
                                    if (!rep.gatewayOk) {
                                        ErrorCard(raw = rep.gatewayError.ifEmpty { "Chat backend unreachable" })
                                    }
                                    if (!rep.syncOk) {
                                        ErrorCard(raw = rep.syncError.ifEmpty { "Sync backend unreachable" })
                                    }
                                    if (rep.gatewayOk && !rep.syncOk) {
                                        HintLine("Chat works, but sync is unreachable — health data won't upload.")
                                    }
                                }
                            }
                        }
                        if (error.isNotEmpty()) {
                            ErrorCard(raw = error)
                        } else if (catalog.isNotEmpty()) {
                            val count = catalog.sumOf { it.models.size }
                            HintLine("Connected — ${catalog.size} provider(s), $count model(s). Pick a model on each assistant's page.")
                        } else {
                            HintLine("No providers loaded yet — save the server above first.")
                        }
                    }
                    // #61: the "Chat backend" subsection is removed (provider +
                    // model are picked on each tab's own page; nothing to show).
                    SettingSection.Appearance -> {
                        SectionCard(
                            title = "Theme",
                            subtitle = "Follow the system or pick a fixed look.",
                        ) {
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
                    }
                    SettingSection.About -> {
                        AppUpdateSection()
                    }
                    SettingSection.Health -> {
                        HealthStatusCard(state)

                        /** Three-step gate: install Health Connect, grant permissions, then sync. */
                        when {
                            !state.healthAvailable -> {
                                SectionCard(
                                    title = "Install Health Connect",
                                    subtitle = "Health sync needs the Health Connect app before anything else.",
                                ) {
                                    Button(
                                        onClick = {
                                            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU &&
                                                ContextCompat.checkSelfPermission(context, Manifest.permission.POST_NOTIFICATIONS)
                                                != PackageManager.PERMISSION_GRANTED
                                            ) {
                                                hcPermissionLauncher.launch(Manifest.permission.POST_NOTIFICATIONS)
                                            }
                                            HealthConnectManager(context).openHealthConnectSettings(context)
                                        },
                                        modifier = Modifier.fillMaxWidth(),
                                    ) {
                                        Text("Install / update Health Connect")
                                    }
                                }
                            }
                            !state.permissionsGranted -> {
                                SectionCard(
                                    title = "Allow health data",
                                    subtitle = "Agento can only sync what you approve in Health Connect.",
                                ) {
                                    Button(
                                        onClick = {
                                            val mgr = HealthConnectManager(context)
                                            val launched = mgr.requestPermissions(hcRequest)
                                            if (!launched) {
                                                viewModel.onSyncError("Permission screen unavailable")
                                                mgr.openHealthConnectSettings(context)
                                            }
                                        },
                                        modifier = Modifier.fillMaxWidth(),
                                    ) {
                                        Text("Grant Health Connect permissions")
                                    }
                                    OutlinedButton(
                                        onClick = {
                                            viewModel.onSyncError("Permissions still needed")
                                            HealthConnectManager(context).openHealthConnectSettings(context)
                                        },
                                        modifier = Modifier.fillMaxWidth(),
                                    ) {
                                        Text("Open Health Connect app")
                                    }
                                    OutlinedButton(
                                        onClick = {
                                            val intent = android.content.Intent(
                                                android.content.Intent.ACTION_VIEW,
                                                android.net.Uri.parse(HealthConnectManager.playStoreUrl()),
                                            )
                                            runCatching { context.startActivity(intent) }
                                        },
                                        modifier = Modifier.fillMaxWidth(),
                                    ) {
                                        Text("Get Health Connect (Play Store)")
                                    }
                                    if (state.healthPackageInfo.isNotEmpty()) {
                                        HintLine("Health Connect: ${state.healthPackageInfo}")
                                    }
                                }
                            }
                            else -> {
                                SectionCard(
                                    title = "Sync",
                                    subtitle = "Push the latest health data to your server now. Automatic sync runs hourly.",
                                ) {
                                    Button(
                                        onClick = { viewModel.syncNow() },
                                        enabled = !state.syncing,
                                        modifier = Modifier.fillMaxWidth(),
                                    ) {
                                        Text(if (state.syncing) "Syncing…" else "Sync now")
                                    }
                                    if (state.syncing) {
                                        LinearProgressIndicator(modifier = Modifier.fillMaxWidth())
                                    }
                                }
                            }
                        }


                        if (state.lastSyncAt.isNotEmpty()) {
                            HintLine("Last sync: ${state.lastSyncAt}")
                        }
                        if (state.lastResult.isNotEmpty() && state.lastResult != "syncing…") {
                            if (state.lastResult.startsWith("FAILED")) {
                                ErrorCard(raw = state.lastResult.removePrefix("FAILED — ").removePrefix("FAILED "))
                            } else {
                                HintLine(state.lastResult)
                            }
                        }
                    }
                    SettingSection.Notifications -> {
                        NotificationsSection()
                    }
                    SettingSection.Storage -> {
                        SettingsBackupSection(onImported = {
                            viewModel.refresh()
                            settingsRefresh++
                            scope.launch { snackbar.showSnackbar(Toasts.SAVED) }
                        })
                        UsageSection()
                    }
                }
            }
        }
    }
}

/** Local usage estimates: per-assistant traffic measured on-device.
 * The gateway exposes no aggregate endpoint, so tokens are approximated
 * from characters (≈4 chars/token) — good enough for a sense of scale. */
@Composable
private fun UsageSection() {
    val context = LocalContext.current
    var rows by remember { mutableStateOf<List<UsageRow>>(emptyList()) }
    var loaded by remember { mutableStateOf(false) }

    LaunchedEffect(Unit) {
        val data = withContext(Dispatchers.IO) {
            val prefs = context.getSharedPreferences(
                AgentoApp.PREFS_NAME, android.content.Context.MODE_PRIVATE)
            listOf(
                Triple("god", "God", "default"),
                Triple("story", "Story", "story"),
                Triple("resumes", "Portfolio", "resumes"),
            ).map { (tab, label, _) ->
                val threads = ChatThreads.load(context, tab)
                UsageRow(
                    label = label,
                    conversations = threads.size,
                    messages = threads.sumOf { it.messages.size },
                    sentChars = prefs.getLong("usage_sent_$tab", 0L),
                    recvChars = prefs.getLong("usage_recv_$tab", 0L),
                )
            }
        }
        rows = data
        loaded = true
    }

    if (!loaded) {
        Box(modifier = Modifier.fillMaxWidth().padding(32.dp), contentAlignment = Alignment.Center) {
            CircularProgressIndicator()
        }
        return
    }
    val totalChars = rows.sumOf { it.sentChars + it.recvChars }
    SectionCard(
        title = "Total",
        subtitle = "Across all assistants, measured on this device.",
    ) {
        Text(
            "≈ ${formatTokens(totalChars)} tokens · " +
                "${rows.sumOf { it.messages }} messages · " +
                "${rows.sumOf { it.conversations }} conversations",
            style = MaterialTheme.typography.bodyLarge,
        )
    }
    rows.forEach { r ->
        SectionCard(title = r.label) {
            Text(
                "≈ ${formatTokens(r.sentChars + r.recvChars)} tokens · " +
                    "${r.messages} messages · ${r.conversations} conversations",
                style = MaterialTheme.typography.bodyMedium,
            )
            HintLine("Sent ≈ ${formatTokens(r.sentChars)} · received ≈ ${formatTokens(r.recvChars)}")
        }
    }
    HintLine("Estimates only — the server reports no totals, so these are character-based approximations.")
}

private data class UsageRow(
    val label: String,
    val conversations: Int,
    val messages: Int,
    val sentChars: Long,
    val recvChars: Long,
)

private fun formatTokens(chars: Long): String {
    val tokens = chars / 4
    return when {
        tokens >= 1_000_000 -> "%.1fM".format(tokens / 1_000_000.0)
        tokens >= 1_000 -> "%.1fk".format(tokens / 1_000.0)
        else -> "$tokens"
    }
}

/** Settings hub overview: every subsection with its live status, opening
 * the matching subscreen on tap (titles match 1:1 per platform convention). */
@Composable
private fun SettingsHub(
    serverStatus: String,
    healthStatus: String,
    themeStatus: String,
    notificationsStatus: String,
    appVersion: String,
    onPick: (SettingSection) -> Unit,
) {
    val rows = remember(serverStatus, healthStatus, themeStatus, notificationsStatus, appVersion) {
        listOf(
            HubRow(SettingSection.Connection, serverStatus),
            HubRow(SettingSection.Health, healthStatus),
            HubRow(SettingSection.Appearance, themeStatus),
            HubRow(SettingSection.Notifications, notificationsStatus),
            HubRow(SettingSection.Storage, "Backup and usage"),
            HubRow(SettingSection.About, appVersion),
        )
    }
    Card(modifier = Modifier.fillMaxWidth()) {
        Column {
            rows.forEachIndexed { i, row ->
                if (i > 0) HorizontalDivider()
                Row(
                    modifier = Modifier.fillMaxWidth()
                        .clickable(
                            role = Role.Button,
                            onClickLabel = "Open ${row.section.title}",
                            onClick = { onPick(row.section) },
                        )
                        .padding(horizontal = 16.dp, vertical = 12.dp),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    Icon(
                        row.section.hubIcon(),
                        contentDescription = null,
                        tint = MaterialTheme.colorScheme.primary,
                    )
                    Spacer(modifier = Modifier.width(16.dp))
                    Column(modifier = Modifier.weight(1f)) {
                        Text(row.section.title, style = MaterialTheme.typography.bodyLarge)
                        Text(
                            row.status,
                            style = MaterialTheme.typography.bodySmall,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                            maxLines = 1,
                        )
                    }
                    TextButton(onClick = { onPick(row.section) }) { Text("Open") }
                }
            }
        }
    }
    HintLine("Connection first — nothing else works until the server is set.")
}

private data class HubRow(val section: SettingSection, val status: String)

private fun SettingSection.hubIcon() = when (this) {
    SettingSection.Connection -> Icons.Filled.Cloud
    SettingSection.Health -> Icons.Filled.Favorite
    SettingSection.Appearance -> Icons.Filled.DarkMode
    SettingSection.Notifications -> Icons.Filled.Notifications
    SettingSection.Storage -> Icons.Filled.Folder
    SettingSection.About -> Icons.Filled.Info
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
    var failed by remember { mutableStateOf(false) }

    LaunchedEffect(Unit) {
        canPost = ChatNotifications.canPost(context)
    }

    /** Asks for the runtime notification grant (Android 13+). */
    val permLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestPermission()
    ) { granted ->
        canPost = granted
        failed = !granted
        status = if (granted) {
            "Allowed — reply alerts will post."
        } else {
            "Notifications are still blocked. Allow them for Agento in system Settings."
        }
    }

    SectionCard(
        title = "Reply alerts",
        subtitle = "The server runs no scheduled jobs, so there are no server-side completions to report. " +
            "Instead, Agento can notify you when a chat reply finishes while the app is in the background.",
    ) {
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
                    status = ""
                    failed = false
                    if (it && !ChatNotifications.canPost(context) &&
                        Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU
                    ) {
                        permLauncher.launch(Manifest.permission.POST_NOTIFICATIONS)
                    }
                },
            )
        }
        if (enabled && !canPost) {
            ErrorCard(raw = "Notifications are blocked at the system level.")
            OutlinedButton(onClick = {
                if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
                    permLauncher.launch(Manifest.permission.POST_NOTIFICATIONS)
                }
            }, modifier = Modifier.fillMaxWidth()) {
                Text("Allow notifications")
            }
        }
        OutlinedButton(
            onClick = {
                failed = false
                status = if (ChatNotifications.sendTest(context)) {
                    Toasts.TEST_SENT
                } else if (!ChatNotifications.isEnabled(context)) {
                    failed = true
                    "Turn the toggle on first, then test again."
                } else {
                    failed = true
                    "Notifications are blocked — allow them first, then test again."
                }
            },
            modifier = Modifier.fillMaxWidth(),
        ) {
            Text("Send test notification")
        }
        if (status.isNotEmpty()) {
            if (failed) ErrorCard(raw = status) else HintLine(status)
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
    var failed by remember { mutableStateOf(false) }
    var busy by remember { mutableStateOf(false) }

    /** Writes the export JSON to the user-picked location. */
    val exportPicker = rememberLauncherForActivityResult(
        ActivityResultContracts.CreateDocument("application/json")
    ) { uri ->
        if (uri == null) return@rememberLauncherForActivityResult
        busy = true
        status = ""
        failed = false
        scope.launch {
            val result = withContext(Dispatchers.IO) {
                runCatching {
                    context.contentResolver.openOutputStream(uri)?.use { out ->
                        out.write(SettingsBackup.export(context).toString(2).toByteArray())
                    } ?: throw IllegalStateException("Could not open the selected file for writing")
                }
            }
            status = result.fold(
                onSuccess = { Toasts.EXPORTED },
                onFailure = { e -> e.message ?: e.javaClass.simpleName },
            )
            failed = result.isFailure
            busy = false
        }
    }

    /** Reads a previously exported file and applies its known keys. */
    val importPicker = rememberLauncherForActivityResult(
        ActivityResultContracts.OpenDocument()
    ) { uri ->
        if (uri == null) return@rememberLauncherForActivityResult
        busy = true
        status = ""
        failed = false
        scope.launch {
            val result = withContext(Dispatchers.IO) {
                runCatching {
                    val text = context.contentResolver.openInputStream(uri)?.use { input ->
                        input.bufferedReader().readText()
                    } ?: throw IllegalStateException("Could not open the selected file for reading")
                    SettingsBackup.importFrom(context, org.json.JSONObject(text)).getOrThrow()
                }
            }
            result.fold(
                onSuccess = { n ->
                    status = "Restored $n setting(s)."
                    onImported()
                },
                onFailure = { e -> status = e.message ?: e.javaClass.simpleName },
            )
            failed = result.isFailure
            busy = false
        }
    }

    SectionCard(
        title = "Backup",
        subtitle = "Keep a copy of your settings and conversations somewhere safe. Restoring brings them back after a reinstall.",
    ) {
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
        if (busy) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                CircularProgressIndicator(modifier = Modifier.size(20.dp))
                Spacer(modifier = Modifier.width(8.dp))
                Text("Working…", style = MaterialTheme.typography.bodySmall)
            }
        }
        if (status.isNotEmpty()) {
            if (failed) ErrorCard(raw = status) else HintLine(status)
        }
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
    var failed by remember { mutableStateOf(false) }
    var latest by remember { mutableStateOf<AppRelease?>(null) }
    var busy by remember { mutableStateOf(false) }
    var progress by remember { mutableStateOf(-1f) }

    /** Returns from the "install unknown apps" toggle — install if allowed now. */
    val unknownSourcesReturn = rememberLauncherForActivityResult(
        ActivityResultContracts.StartActivityForResult()
    ) {
        if (UpdateManager.canInstallUnknownApps(context)) {
            failed = false
            status = "Allowed — tap Download & install again."
        } else {
            failed = true
            status = "Install permission still off. Turn on “Allow from this source”, then retry."
        }
    }

    SectionCard(
        title = "Version",
        subtitle = "Installed: $installedName ($installedCode)",
    ) {
        /** Checks /releases/latest and diffs the tag against the installed build. */
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            Button(
                onClick = {
                    busy = true
                    progress = -1f
                    status = ""
                    failed = false
                    scope.launch {
                        UpdateManager.fetchLatest()
                            .onSuccess { rel ->
                                latest = if (UpdateManager.isNewer(rel.tag, installedName, installedCode)) {
                                    status = "Update available: ${rel.name}"
                                    rel
                                } else {
                                    status = Toasts.UP_TO_DATE
                                    null
                                }
                            }
                            .onFailure { e ->
                                latest = null
                                failed = true
                                status = e.message ?: e.javaClass.simpleName
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
                        failed = true
                        status = "Allow “install unknown apps” for Agento, then tap again."
                        return@Button
                    }
                    busy = true
                    progress = 0f
                    status = ""
                    failed = false
                    scope.launch {
                        val dest = java.io.File(
                            java.io.File(context.cacheDir, "updates"),
                            "agento-${rel.tag}.apk",
                        )
                        UpdateManager.download(rel.apkUrl, dest) { p -> progress = p }
                            .onSuccess { apk ->
                                status = "Downloaded — opening the installer…"
                                runCatching {
                                    context.startActivity(UpdateManager.installIntent(context, apk))
                                }.onFailure { e ->
                                    failed = true
                                    status = e.message ?: e.javaClass.simpleName
                                }
                            }
                            .onFailure { e ->
                                failed = true
                                status = e.message ?: e.javaClass.simpleName
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
        } else if (busy) {
            LinearProgressIndicator(modifier = Modifier.fillMaxWidth())
        }
        val rel = latest
        if (rel != null && rel.notes.isNotBlank()) {
            Text(
                rel.notes.take(400),
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
        if (status.isNotEmpty()) {
            if (failed) ErrorCard(raw = status) else HintLine(status)
        }
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
                state.healthUpdateRequired -> "Health Connect needs an update"
                else -> "Health Connect ready"
            }
            val permText = if (state.permissionsGranted) {
                "Permissions granted"
            } else {
                "Permissions not granted yet"
            }
            Text(healthText, style = MaterialTheme.typography.titleSmall)
            Text(
                permText,
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
}
