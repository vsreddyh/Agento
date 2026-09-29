@file:OptIn(ExperimentalMaterial3Api::class)

package com.vishnu.agento

import android.Manifest
import android.app.Activity
import android.app.AlarmManager
import android.content.ActivityNotFoundException
import android.content.Context
import android.appwidget.AppWidgetManager
import android.content.ComponentName
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import android.provider.Settings
import android.speech.RecognizerIntent
import androidx.activity.ComponentActivity
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.compose.setContent
import androidx.activity.result.contract.ActivityResultContracts
import androidx.activity.viewModels
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.interaction.DragInteraction
import androidx.compose.foundation.interaction.MutableInteractionSource
import androidx.compose.foundation.interaction.PressInteraction
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.grid.GridCells
import androidx.compose.foundation.lazy.grid.LazyVerticalGrid
import androidx.compose.foundation.lazy.grid.items
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.itemsIndexed
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.selection.toggleable
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.Send
import androidx.compose.material.icons.filled.AccessTime
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.ArrowDownward
import androidx.compose.material.icons.filled.Assignment
import androidx.compose.material.icons.filled.Build
import androidx.compose.material.icons.filled.CheckCircle
import androidx.compose.material.icons.filled.DateRange
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.Edit
import androidx.compose.material.icons.filled.Undo
import androidx.compose.material.icons.filled.RadioButtonUnchecked
import androidx.compose.material.icons.filled.Repeat
import androidx.compose.material.icons.filled.Sort
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.Cloud
import androidx.compose.material.icons.filled.ContentCopy
import androidx.compose.material.icons.filled.Description
import androidx.compose.material.icons.filled.DarkMode
import androidx.compose.material.icons.filled.Dashboard
import androidx.compose.material.icons.filled.DataUsage
import androidx.compose.material.icons.filled.Equalizer
import androidx.compose.material.icons.filled.ExpandLess
import androidx.compose.material.icons.filled.ExpandMore
import androidx.compose.material.icons.filled.Extension
import androidx.compose.material.icons.filled.Favorite
import androidx.compose.material.icons.filled.Folder
import androidx.compose.material.icons.filled.Groups
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
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.text.input.VisualTransformation
import androidx.compose.ui.text.style.TextDecoration
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.core.content.ContextCompat
import androidx.health.connect.client.PermissionController
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.ViewModel
import androidx.lifecycle.ViewModelProvider
import androidx.lifecycle.compose.LifecycleEventEffect
import androidx.lifecycle.viewmodel.compose.viewModel
import com.mikepenz.markdown.m3.Markdown
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.ensureActive
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.collect
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import java.util.Locale

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
 * Order is God, Story, Resume and Portfolio (#53); the resumes profile shows as
 * "Resume and Portfolio" (#54) but the tab key (prefs, files, profile path) is
 * unchanged so backend mapping never breaks. */
private enum class Destination(val title: String) {
    God("God"),
    Story("Story"),
    Portfolio("Resume and Portfolio"),
    Tasks("Projects"),
    TaskManager("Task Manager"),
    Storage("Storage"),
    Scheduler("Scheduler"),
    Skills("Skills"),
    Tools("Tools"),
    Settings("Settings"),
}

private fun Destination.icon() = when (this) {
    Destination.Story -> Icons.Filled.MenuBook
    Destination.Portfolio -> Icons.Filled.Description
    Destination.God -> Icons.Filled.Star
    Destination.Tasks -> Icons.Filled.List
    Destination.TaskManager -> Icons.Filled.Assignment
    Destination.Storage -> Icons.Filled.Folder
    Destination.Scheduler -> Icons.Filled.Schedule
    Destination.Skills -> Icons.Filled.Extension
    Destination.Tools -> Icons.Filled.Build
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
    Widget("Home-screen widget"),
    Notifications("Notifications"),
    Storage("Storage"),
    Usage("Usage"),
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

    /**
     * Widget live-session requests (#85). Generation counter (not boolean)
     * so retaps while the app is open retrigger: singleTop delivers them
     * via onNewIntent, and the God tab consumes each generation once.
     */
    private val liveGen = mutableIntStateOf(0)
    private val liveTab = mutableStateOf("god")

    /**
     * Task widget tap (#101). Generation counter like liveGen so retaps
     * while the app is open renavigate: singleTop delivers them via
     * onNewIntent and the Task Manager tab consumes each generation once.
     */
    private val tasksGen = mutableIntStateOf(0)

    /**
     * Deep-link into one task's detail sheet (widget row tap, reminder
     * tap). Carried alongside tasksGen; TaskManagerScreen consumes and
     * clears it once the list loads.
     */
    private val tasksTargetId = mutableStateOf<String?>(null)

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        setIntent(intent)
        if (intent.action == LiveWidget.ACTION_LIVE) {
            liveTab.value = intent.getStringExtra(LiveWidget.EXTRA_TAB) ?: "god"
            liveGen.intValue++
        }
        if (intent.action == TaskWidget.ACTION_TASKS) {
            tasksGen.intValue++
            intent.getStringExtra(TaskWidget.EXTRA_TASK_ID)?.let {
                tasksTargetId.value = it
            }
        }
    }

    /** Adaptive sidebar navigation (#18, #64); theme from prefs (#28). */
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        // Widget live tap: same intent redelivered on rotation/recreation
        // must not start a second session — only a fresh launch counts.
        if (savedInstanceState == null && intent?.action == LiveWidget.ACTION_LIVE && liveGen.intValue == 0) {
            liveTab.value = intent.getStringExtra(LiveWidget.EXTRA_TAB) ?: "god"
            liveGen.intValue = 1
        }
        // Task widget tap: same redelivery guard — only a fresh launch counts.
        if (savedInstanceState == null && intent?.action == TaskWidget.ACTION_TASKS && tasksGen.intValue == 0) {
            tasksGen.intValue = 1
            intent.getStringExtra(TaskWidget.EXTRA_TASK_ID)?.let {
                tasksTargetId.value = it
            }
        }
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
                    // Widget live tap (#85): land on the requested tab; its
                    // ChatTab consumes the liveGen generation and starts
                    // the session. Retaps renavigate (singleTop) and refire.
                    LaunchedEffect(liveGen.intValue) {
                        if (liveGen.intValue > 0) {
                            dest = when (liveTab.value) {
                                "story" -> Destination.Story
                                "resumes" -> Destination.Portfolio
                                else -> Destination.God
                            }
                        }
                    }
                    // Widget task tap (#101): land on the Task Manager screen.
                    LaunchedEffect(tasksGen.intValue) {
                        if (tasksGen.intValue > 0) {
                            dest = Destination.TaskManager
                        }
                    }
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
                    // Built-in TTS: one engine shared by God/Story/Resume and Portfolio,
                    // shut down with the activity. Auto-read is a single
                    // global toggle (prefs `tts_auto`) in each chat's bar.
                    val tts = remember { ChatTts(context) }
                    DisposableEffect(Unit) { onDispose { tts.shutdown() } }
                    var autoSpeak by remember {
                        mutableStateOf(prefs.getBoolean("tts_auto", false))
                    }
                    // Bumped whenever a model/effort pick saves, so the
                    // drawer subtitles below recompute without waiting for
                    // a navigation (remember(dest, section) alone goes stale).
                    var drawerTick by remember { mutableIntStateOf(0) }
                    val tabModels = remember(dest, section, drawerTick) {
                        // Drawer subtitles: model plus its effort pick, so the
                        // active reasoning level is visible without reopening
                        // the picker. Blank effort = server default (medium).
                        fun sub(tab: String): String {
                            val m = (prefs.getString("model_$tab", "") ?: "").trim()
                            if (m.isEmpty()) return ""
                            // Clamped like effortFor: a pick saved for another
                            // model never leaks into this model's subtitle.
                            val e = (prefs.getString("effort_${tab}_$m", "") ?: "").trim()
                                .ifEmpty { (prefs.getString("effort_$tab", "") ?: "").trim() }
                                .lowercase()
                                .takeIf { it in EffortCatalog.optionsFor(m) }.orEmpty()
                            return if (e.isEmpty()) m else "$m · $e"
                        }
                        mapOf(
                            "god" to sub("god"),
                            "story" to sub("story"),
                            "resumes" to sub("resumes"),
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
                                    // Only the requested tab sees the gen:
                                    // per-tab consumed counters would
                                    // otherwise auto-start sessions on
                                    // tabs the user merely switches to.
                                    autoLiveGen = if (liveTab.value == "god") liveGen.intValue else 0,
                                    onAutoSpeak = {
                                        autoSpeak = it
                                        prefs.edit().putBoolean("tts_auto", it).apply()
                                    },
                                    onMenu = { scope.launch { drawerState.open() } },
                                    onConfigChanged = { drawerTick++ },
                                )
                                Destination.Story -> ChatTab(
                                    app = application, tab = "story", title = "Story", wc = wc,
                                    tts = tts, autoSpeak = autoSpeak,
                                    autoLiveGen = if (liveTab.value == "story") liveGen.intValue else 0,
                                    onAutoSpeak = {
                                        autoSpeak = it
                                        prefs.edit().putBoolean("tts_auto", it).apply()
                                    },
                                    onMenu = { scope.launch { drawerState.open() } },
                                    onConfigChanged = { drawerTick++ },
                                )
                                Destination.Portfolio -> ChatTab(
                                    app = application, tab = "resumes", title = "Resume and Portfolio", wc = wc,
                                    tts = tts, autoSpeak = autoSpeak,
                                    autoLiveGen = if (liveTab.value == "resumes") liveGen.intValue else 0,
                                    onAutoSpeak = {
                                        autoSpeak = it
                                        prefs.edit().putBoolean("tts_auto", it).apply()
                                    },
                                    onMenu = { scope.launch { drawerState.open() } },
                                    onConfigChanged = { drawerTick++ },
                                )
                                Destination.Tasks -> TasksScreen(
                                    wc = wc,
                                    onMenu = { scope.launch { drawerState.open() } },
                                )
                                Destination.TaskManager -> TaskManagerScreen(
                                    wc = wc,
                                    onMenu = { scope.launch { drawerState.open() } },
                                    deepLinkId = tasksTargetId.value,
                                    onDeepLinkConsumed = { tasksTargetId.value = null },
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
                                Destination.Tools -> ToolsScreen(
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
    // Scrollable (#105): 9 destinations plus the settings subsections
    // overflow short screens — without this the bottom items are cut off
    // with no way to reach them.
    Column(modifier = Modifier.verticalScroll(rememberScrollState())) {
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
    // Scrollable like the drawer (#105): the rail overflows the same way
    // on short landscape screens.
    Column(modifier = Modifier.verticalScroll(rememberScrollState())) {
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
    autoLiveGen: Int = 0,
    onConfigChanged: () -> Unit = {},
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
        title = title, tab = tab, modelLabel = state.model.ifBlank { "" },
        setupNeeded = setupNeeded, state = state, wc = wc, snackbar = snackbar,
        tts = tts, autoSpeak = autoSpeak, onAutoSpeak = onAutoSpeak,
        onMenu = onMenu, autoLiveGen = autoLiveGen,
        onModel = { showModel = true },
        onHistory = { showThreads = true },
        onCheckConnection = vm::checkReachability,
        onPending = vm::onPending, onSend = vm::send, onStop = vm::stop,
        onNew = vm::newConversation, onRetry = vm::retry,
        onRegenerate = vm::regenerate, onContinue = vm::continueTurn,
        onReplace = vm::replaceAndResend, onFork = vm::forkAndResend,
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
            onChanged = { vm.refreshConfig(); onConfigChanged() },
            onClose = { showModel = false })
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

/** Server task states for the Task Manager filter (match `GET /api/tasks`). */
private enum class ServerTaskFilter(val state: String, val title: String) {
    Open("open", "Open"),
    Done("done", "Done"),
    All("all", "All"),
}

/** Client-side sort for the task list (server returns one state at a time,
 * unsorted). Due puts undated tasks last; Created is newest first. */
private enum class ServerTaskSort(val title: String) {
    Start("Start time"),
    Name("Name"),
    Created("Newest"),
    Estimate("Estimate"),
}

/**
 * Sort key for "when should this begin": the start moment, with the due
 * time as a tie-break so two tasks starting together still order by their
 * deadlines. Tasks with no usable due time sort last instead of first —
 * an undated task is not the most urgent thing you have.
 */
private fun ServerTask.startSortKey(): Long =
    startMillisOrNull() ?: Long.MAX_VALUE

/**
 * Overdue means the moment has passed, not just the date: a task due at
 * 09:00 today at 14:00 is overdue, and the reminder engine already nags it
 * every 15 minutes. Matching that here keeps the list's red rows and its
 * Overdue group telling the same story. ISO YYYY-MM-DD compares
 * lexicographically; blank or malformed dates never count.
 *
 * [now] is passed in rather than read here so a row's colour and the group
 * it sits in are decided from the same instant — at a bucket boundary two
 * rows must not land on opposite sides of it.
 */
private fun ServerTask.isOverdue(
    today: String,
    now: java.time.LocalDateTime,
): Boolean {
    if (!isOpen() || !dueDate.isIsoDate()) return false
    if (dueDate < today) return true
    if (dueDate != today) return false
    // Compared in millis, not via whole minutes: a task 30 seconds past due
    // is overdue now, and must match the nag that is already firing.
    val at = dueMillisOrNull(dueDate, dueTime) ?: return false
    return at <= now.atZone(IST).toInstant().toEpochMilli()
}

/**
 * Buckets for the grouped task list, in display order.
 *
 * Today is split by how long is left rather than shown as one "Today" pile:
 * what people actually ask about a task is "how soon?", and the six
 * horizons below answer it without opening anything. They stop at the end
 * of today on purpose — a task due in three hours is not "tomorrow", and
 * pretending otherwise hides it. Days from tomorrow on keep plain day
 * groups, where a time-of-day split adds nothing.
 *
 * Done tasks in the All view collect in Completed at the bottom.
 */
private enum class DueBucket(val title: String) {
    Overdue("Overdue"),
    Current("Current"),
    ThisHour("This hour"),
    NextHour("Next hour"),
    In2To3Hours("Next 2-3 hours"),
    In3To6Hours("Next 3-6 hours"),
    In6To12Hours("Next 6-12 hours"),
    LaterToday("Later today"),
    Tomorrow("Tomorrow"),
    ThisWeek("This week"),
    Later("Later"),
    NoDate("No due date"),
    Completed("Completed"),
}

/**
 * The group this task belongs to.
 *
 * Everything ahead of the deadline is measured to the task's **start**
 * time (`due - estimated_minutes`), not its due time, because the question
 * a list answers is "when should I begin?", and that is the moment the
 * "Start now" reminder fires. A task due in three hours with a one-hour
 * estimate belongs in the next hour or two, not three: it is what you
 * should be starting, not what you must finish by.
 *
 * Past the start time and short of the deadline is **Current** — the window
 * in which the work is meant to happen. Past the deadline it is
 * **Overdue**, which is also what the reminder engine calls it (it nags
 * every 15 minutes), so the list and the alerts never disagree.
 *
 * A today task with no time at all lands in Later today: nothing is known
 * about *when*, and "this hour" would be a guess.
 */
private fun ServerTask.dueBucket(
    today: java.time.LocalDate,
    now: java.time.LocalDateTime,
): DueBucket {
    if (!isOpen()) return DueBucket.Completed
    if (!dueDate.isIsoDate()) return DueBucket.NoDate
    val s = dueDate
    val nowMillis = now.atZone(IST).toInstant().toEpochMilli()
    val dueAt = dueMillisOrNull()
    if (dueAt == null) {
        // A date with no time (rows predating mandatory due_time): the day
        // is known, so it keeps its day group rather than being reported as
        // undated. A today one has no idea *when*, so Later today is the
        // honest answer — "This hour" would be a guess.
        return when {
            s < today.toString() -> DueBucket.Overdue
            s == today.toString() -> DueBucket.LaterToday
            s == today.plusDays(1).toString() -> DueBucket.Tomorrow
            s <= today.plusDays(7).toString() -> DueBucket.ThisWeek
            else -> DueBucket.Later
        }
    }
    // One shared start rule (TasksApi.startMillisOrNull), so the list and
    // the reminder engine cannot drift apart on when a task "starts".
    val startAt = startMillisOrNull() ?: dueAt
    val startDay = java.time.Instant.ofEpochMilli(startAt)
        .atZone(IST).toLocalDate().toString()
    val t = today.toString()
    return when {
        // Past the deadline: overdue, which is also what the reminder
        // engine calls it. Compared in millis, so a task 30 seconds past due
        // is overdue now rather than up to a minute later.
        dueAt <= nowMillis -> DueBucket.Overdue
        // The start moment has arrived and the deadline has not: this is
        // what should be under way now.
        startAt <= nowMillis -> DueBucket.Current
        // Which day a task belongs to is the day it *starts* on, not the
        // day it is due: due tomorrow 00:30 with a one-hour estimate is
        // something to start tonight, and tonight is today.
        startDay < t -> DueBucket.Overdue
        startDay == t -> {
            val left = startAt - nowMillis
            when {
                // Every name is the range it actually covers: this hour, the
                // hour after, 2-3h, 3-6h, 6-12h, and everything still left
                // today. No two names overlap, so a reader never has to
                // guess which bucket a row came from.
                left < 60L * 60_000 -> DueBucket.ThisHour
                left < 120L * 60_000 -> DueBucket.NextHour
                left < 180L * 60_000 -> DueBucket.In2To3Hours
                left < 360L * 60_000 -> DueBucket.In3To6Hours
                left < 720L * 60_000 -> DueBucket.In6To12Hours
                else -> DueBucket.LaterToday
            }
        }
        // Days from tomorrow on keep plain day groups, where a
        // time-of-day split adds nothing.
        startDay == today.plusDays(1).toString() -> DueBucket.Tomorrow
        startDay <= today.plusDays(7).toString() -> DueBucket.ThisWeek
        else -> DueBucket.Later
    }
}

private fun List<ServerTask>.sortedByMode(mode: ServerTaskSort): List<ServerTask> =
    when (mode) {
        ServerTaskSort.Start -> sortedWith(
            compareBy<ServerTask> { it.startSortKey() }
                .thenBy({ it.dueMillisOrNull() ?: Long.MAX_VALUE })
                .thenBy({ it.name.lowercase(Locale.ROOT) }),
        )
        ServerTaskSort.Name -> sortedBy { it.name.lowercase(Locale.ROOT) }
        ServerTaskSort.Created -> sortedByDescending { it.createdAt }
        ServerTaskSort.Estimate -> sortedWith(
            compareByDescending<ServerTask> { it.estimatedMinutes > 0 }
                .thenByDescending { it.estimatedMinutes },
        )
    }

/** Task Manager: the user's own tasks from the shared `tasks` collection
 * (the same rows the assistant manages over MCP). Full CRUD here; the
 * assistant stays a second writer through chat, same as before. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun TaskManagerScreen(
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
            onSuccess = {
                tasks = it
                if (!scheduledOnce) {
                    scheduledOnce = true
                    TaskReminders.refresh(context)
                }
            },
            onFailure = { e -> error = serverDetail(e.message ?: e.javaClass.simpleName) },
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
                        "${visible.size} of ${tasks.size}",
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
            onDelete = {
                selected = null
                deleting = open
            },
        )
    }
}

/** Editor draft for a server task (id empty = new). Text fields stay strings
 * so half-typed input (e.g. minutes, the repeat count) survives; parsed on
 * save. The repeat count is a string for the same reason. */
private data class ServerTaskDraft(
    val id: String = "",
    val name: String = "",
    val description: String = "",
    val dueDate: String = "",
    val dueTime: String = "",
    val estimatedMinutes: String = "",
    val repeatEvery: String = "",
    val repeatUnit: String = "days",
    val repeatCustom: Boolean = false,
    val repeatRule: String = "",
    val parallelable: Boolean = false,
)

private fun ServerTask.toDraft() = ServerTaskDraft(
    id = id,
    name = name,
    description = description,
    dueDate = dueDate,
    dueTime = dueTime,
    estimatedMinutes = estimatedMinutes.toString(),
    repeatEvery = if (repeatEvery > 0) repeatEvery.toString() else "",
    repeatUnit = repeatUnit.ifEmpty { "days" },
    repeatCustom = repeatCustom,
    repeatRule = repeatRule,
    parallelable = parallelable,
)

/** Repeat units offered by the editor, matching the server's vocabulary. */
private val REPEAT_UNITS = listOf("days", "weeks", "months", "years")

/** Bounds of the structured count, kept in step with the server. */
private const val REPEAT_EVERY_MIN = 1
private const val REPEAT_EVERY_MAX = 28

/** The draft's recurrence as (every, unit, customText), or null when it is
 * inconsistent — a custom condition with no words, or a count outside
 * 1-28, or a half-typed number. Save stays disabled until it parses. */
private fun ServerTaskDraft.repeatOrNull(): Triple<Int, String, String>? {
    if (repeatCustom) {
        val text = repeatRule.trim()
        return if (text.isEmpty()) null else Triple(0, "", text)
    }
    val typed = repeatEvery.trim()
    if (typed.isEmpty()) {
        // No cadence: one-shot.
        return Triple(0, "", "")
    }
    val every = typed.toIntOrNull() ?: return null
    if (every < REPEAT_EVERY_MIN || every > REPEAT_EVERY_MAX) return null
    if (repeatUnit !in REPEAT_UNITS) return null
    return Triple(every, repeatUnit, "")
}

/** Flat task row: checkbox toggles complete/reopen, tap opens the
 * detail sheet. Name + one friendly due line; description, estimate,
 * repeat, and timestamps live on the detail page. Overdue open tasks
 * render the due line in error color; done rows dim with a
 * strikethrough. Deliberately cardless — the section stacks the rows
 * with inset dividers. */
@Composable
private fun ServerTaskRow(
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
private fun ServerTaskDetailSheet(
    task: ServerTask,
    overdue: Boolean,
    today: java.time.LocalDate,
    actionsEnabled: Boolean,
    onDismiss: () -> Unit,
    onEdit: () -> Unit,
    onToggle: () -> Unit,
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
                    else -> task.startParts()
                        ?.let { (d, t) -> friendlyDue(d, t, today) }
                        ?.takeIf { it.isNotEmpty() }
                        // A zero estimate has no start of its own.
                        ?.takeIf { it != friendlyDue(task.dueDate, task.dueTime, today) }
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
            OutlinedButton(
                onClick = onDelete,
                enabled = actionsEnabled,
                colors = ButtonDefaults.outlinedButtonColors(
                    contentColor = MaterialTheme.colorScheme.error,
                ),
                modifier = Modifier.fillMaxWidth(),
            ) {
                Icon(Icons.Filled.Delete, contentDescription = null)
                Spacer(modifier = Modifier.width(4.dp))
                Text("Delete")
            }
        }
    }
}

/** One icon + label + value entry in the detail sheet. Stacked
 * (label above value) so narrow screens never wrap labels mid-word. */
@Composable
private fun DetailLine(
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
private fun ServerTaskDialog(
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
private fun FormLabel(text: String) {
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
private fun DueDatePickerDialog(
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
private fun DueTimePickerDialog(
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

private fun taskRank(status: String): Int =
    TASK_STATUSES.indexOf(status).let { if (it < 0) 2 else it }

/** Maps legacy free-text statuses onto the fixed set (#55). */
private fun normalizeStatus(raw: String): String = when (raw.trim().lowercase(Locale.ROOT)) {
    "doing", "ongoing", "in progress", "in_progress" -> "Ongoing"
    "paused", "pause", "pasued" -> "Paused"
    "done", "complete", "completed" -> "Done"
    else -> "Todo"
}

/** One-time read of the retired local tasks.json (#104): name/status/note
 * triples for server import. Lenient — a corrupt file imports as empty.
 * Capped at 500 to match the server MaxLimit used by the migration calls. */
private fun loadLegacyTasks(context: android.content.Context): List<Triple<String, String, String>> {
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

private enum class TaskSort(val title: String) {
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
private fun TasksScreen(wc: WindowClass, onMenu: () -> Unit = {}) {
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
                prefs.edit().putBoolean("projects_migrated", true).apply()
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
                    prefs.edit().putBoolean("projects_migrated", true).apply()
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
private fun TaskCard(
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
private fun TaskDialog(
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

/** One skill row: name + description/category + On/Off badge. Tap expands
 * the full description (#123); collapsed text caps at 3 lines. Expansion
 * is keyed by profile+name so switching assistants never leaks open rows. */
@Composable
private fun SkillCard(profile: String, s: SkillInfo) {
    var open by remember(profile, s.name) { mutableStateOf(false) }
    Card(
        onClick = { open = !open },
        modifier = Modifier.fillMaxWidth(),
    ) {
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
                        maxLines = if (open) Int.MAX_VALUE else 3,
                        overflow = TextOverflow.Ellipsis,
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
            Icon(
                if (open) Icons.Filled.ExpandLess else Icons.Filled.ExpandMore,
                contentDescription = if (open) "Collapse" else "Expand",
                tint = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.size(20.dp),
            )
        }
    }
}

/** One toolset row: label + name + description/tool list + state badges.
 * Tap expands the full description and the complete tool list (#123);
 * collapsed caps at the 10-tool summary. Off toolsets render an Off badge
 * (nothing is filtered); an explicit `configured: false` adds a
 * "Not configured" badge, invisible otherwise. Expansion is keyed by
 * profile+name so switching assistants never leaks open rows. */
@Composable
private fun ToolsetCard(profile: String, ts: ToolsetInfo) {
    var open by remember(profile, ts.name) { mutableStateOf(false) }
    Card(
        onClick = { open = !open },
        modifier = Modifier.fillMaxWidth(),
    ) {
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
                val blurb = if (open) {
                    listOfNotNull(
                        ts.description.ifEmpty { null },
                        if (ts.tools.isEmpty()) null
                        else "Tools (${ts.tools.size}): ${ts.tools.joinToString(", ")}",
                    ).joinToString("\n")
                } else {
                    ts.description.ifEmpty {
                        if (ts.tools.isEmpty()) "" else
                            "${ts.tools.size} tool(s): " +
                                ts.tools.take(10).joinToString(", ") +
                                if (ts.tools.size > 10) "…" else ""
                    }
                }
                if (blurb.isNotEmpty()) {
                    Text(
                        blurb,
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        maxLines = if (open) Int.MAX_VALUE else 4,
                        overflow = TextOverflow.Ellipsis,
                    )
                }
            }
            Column(horizontalAlignment = Alignment.End) {
                when (ts.enabled) {
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
                    // Unknown state shows no badge rather than Off.
                    null -> { }
                }
                if (ts.configured == false) {
                    Text(
                        "Not configured",
                        style = MaterialTheme.typography.labelMedium,
                        color = MaterialTheme.colorScheme.error,
                    )
                }
                Icon(
                    if (open) Icons.Filled.ExpandLess else Icons.Filled.ExpandMore,
                    contentDescription = if (open) "Collapse" else "Expand",
                    tint = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.size(20.dp),
                )
            }
        }
    }
}

/** One derived MCP server row: name + tool list. Tap expands the full
 * tool list (#123); collapsed caps at the 8-tool summary. Expansion is
 * keyed by profile+name so switching assistants never leaks open rows. */
@Composable
private fun McpCard(profile: String, server: McpServer) {
    var open by remember(profile, server.name) { mutableStateOf(false) }
    Card(
        onClick = { open = !open },
        modifier = Modifier.fillMaxWidth(),
    ) {
        Column(modifier = Modifier.fillMaxWidth().padding(12.dp)) {
            Row(
                modifier = Modifier.fillMaxWidth(),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Text(
                    server.name,
                    style = MaterialTheme.typography.bodyLarge,
                    modifier = Modifier.weight(1f),
                )
                Icon(
                    if (open) Icons.Filled.ExpandLess else Icons.Filled.ExpandMore,
                    contentDescription = if (open) "Collapse" else "Expand",
                    tint = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.size(20.dp),
                )
            }
            Text(
                if (server.tools.isEmpty()) "No tools listed"
                else if (open) "${server.tools.size} tool(s): ${server.tools.joinToString(", ")}"
                else "${server.tools.size} tool(s): ${server.tools.take(8).joinToString(", ")}" +
                    if (server.tools.size > 8) "…" else "",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                maxLines = if (open) Int.MAX_VALUE else 3,
                overflow = TextOverflow.Ellipsis,
            )
        }
    }
}

/** Read-only skills inventory per assistant — Default vs Custom — with
 * Tasks-style search filtering, origin FilterChips and a sort dropdown.
 * Origin rule: in-the-box Hermes skills carry a category, project skills
 * don't — so non-blank category means default.
 */
@Composable
private fun SkillsScreen(wc: WindowClass, onMenu: () -> Unit = {}) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    var profile by remember { mutableStateOf("god") }
    var skills by remember { mutableStateOf<List<SkillInfo>>(emptyList()) }
    var skillsError by remember { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }
    var loaded by remember { mutableStateOf(false) }
    // Cancelled + replaced on every load() so rapid profile taps can't
    // let a stale response win; only the latest job may clear busy.
    var loadJob by remember { mutableStateOf<Job?>(null) }

    fun load() {
        loadJob?.cancel()
        busy = true
        skillsError = ""
        loaded = false
        val path = ChatApi(context).pathFor(profile)
        var job: Job? = null
        job = scope.launch {
            try {
                val sr = ServerApi(context).listSkills(path)
                // A cancel landing mid-await must not leave stale state.
                ensureActive()
                if (loadJob == job) {
                    skills = sr.getOrDefault(emptyList())
                    skillsError = sr.exceptionOrNull()?.let {
                        it.message ?: it.javaClass.simpleName
                    } ?: ""
                    loaded = true
                }
            } finally {
                if (loadJob == job) busy = false
            }
        }
        loadJob = job
    }

    LaunchedEffect(profile) { load() }

    val tabs = listOf("god" to "God", "story" to "Story", "resumes" to "Resume and Portfolio")

    // Search + origin filter + sort (Tasks-style).
    var query by remember { mutableStateOf("") }
    var skillsOrigin by remember { mutableStateOf(SkillsOrigin.All) }
    var skillsSort by remember { mutableStateOf(SkillsSort.NameAz) }
    var skillsSortOpen by remember { mutableStateOf(false) }

    fun sortSkills(list: List<SkillInfo>): List<SkillInfo> = when (skillsSort) {
        SkillsSort.NameZa -> list.sortedByDescending { it.name.lowercase(Locale.ROOT) }
        SkillsSort.Category -> list.sortedWith(
            // Empty category sorts last so custom skills trail defaults.
            compareBy({ it.category.ifEmpty { "\uFFFF" }.lowercase(Locale.ROOT) }, { it.name.lowercase(Locale.ROOT) })
        )
        SkillsSort.NameAz -> list.sortedBy { it.name.lowercase(Locale.ROOT) }
    }
    // Origin rule (see ServerApi): bundled Hermes skills carry a category,
    // project skills don't — so non-blank category means default. If the
    // server omits categories entirely, everything counts as default
    // rather than silently emptying the Default section.
    val anyCategorized = remember(skills) { skills.any { it.category.isNotBlank() } }
    val defaultSkills = remember(skills, query, skillsSort, anyCategorized) {
        sortSkills(skills.filter { (it.isDefault() || !anyCategorized) && it.matches(query) })
    }
    val customSkills = remember(skills, query, skillsSort, anyCategorized) {
        // Mirrors the defaultSkills fallback: with no categories anywhere,
        // everything is default, so Custom stays empty (never duplicated).
        if (!anyCategorized) emptyList()
        else sortSkills(skills.filter { !it.isDefault() && it.matches(query) })
    }
    val shownDefaultSkills = if (skillsOrigin == SkillsOrigin.Custom) emptyList() else defaultSkills
    val shownCustomSkills = if (skillsOrigin == SkillsOrigin.Default) emptyList() else customSkills

    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text("Skills") },
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
                    placeholder = { Text("Search skills…") },
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
                LazyColumn(
                    modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp),
                    verticalArrangement = Arrangement.spacedBy(8.dp),
                    contentPadding = PaddingValues(vertical = 8.dp),
                ) {
                    if (skills.isEmpty() && skillsError.isNotEmpty() && loaded) {
                        item {
                            ErrorCard(raw = skillsError, onRetry = { load() })
                        }
                    }
                    if (loaded && skills.isEmpty() && skillsError.isEmpty()) {
                        item {
                            EmptyState(
                                icon = Icons.Filled.Extension,
                                title = "Nothing listed",
                                subtitle = "This assistant reports no skills.",
                                actionLabel = "Refresh",
                                onAction = { load() },
                            )
                        }
                    }
                    // ── Skills (Default vs Custom) ──
                    item {
                        Text(
                            "Skills",
                            style = MaterialTheme.typography.titleMedium,
                            modifier = Modifier.padding(horizontal = 4.dp, vertical = 4.dp),
                        )
                    }
                    // Totals header (#123): inventory size as a rough
                    // context-load proxy for this assistant.
                    if (loaded && skillsError.isEmpty()) {
                        item {
                            val totalDefault =
                                if (!anyCategorized) skills.size
                                else skills.count { it.isDefault() }
                            HintLine(
                                "${skills.size} skills · " +
                                    "$totalDefault default · " +
                                    "${skills.size - totalDefault} custom"
                            )
                        }
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
                            SkillCard(profile, s)
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
                            SkillCard(profile, s)
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
                }
            }
        }
    }
}

/** Read-only tools inventory per assistant — Default vs Custom MCP — with
 * Tasks-style search filtering, origin FilterChips and a sort dropdown.
 * Origin rule: in-the-box Hermes toolsets are default, everything else
 * (in-repo MCP servers, `mcp-*`) is custom. Split out of SkillsScreen
 * into its own sidebar section (#106).
 */
@Composable
private fun ToolsScreen(wc: WindowClass, onMenu: () -> Unit = {}) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    var profile by remember { mutableStateOf("god") }
    var toolsets by remember { mutableStateOf<List<ToolsetInfo>>(emptyList()) }
    var toolsError by remember { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }
    var loaded by remember { mutableStateOf(false) }
    // Cancelled + replaced on every load() so rapid profile taps can't
    // let a stale response win; only the latest job may clear busy.
    var loadJob by remember { mutableStateOf<Job?>(null) }

    fun load() {
        loadJob?.cancel()
        busy = true
        toolsError = ""
        loaded = false
        val path = ChatApi(context).pathFor(profile)
        var job: Job? = null
        job = scope.launch {
            try {
                val tr = ServerApi(context).listToolsets(path)
                // A cancel landing mid-await must not leave stale state.
                ensureActive()
                if (loadJob == job) {
                    toolsets = tr.getOrDefault(emptyList())
                    toolsError = tr.exceptionOrNull()?.let {
                        it.message ?: it.javaClass.simpleName
                    } ?: ""
                    loaded = true
                }
            } finally {
                if (loadJob == job) busy = false
            }
        }
        loadJob = job
    }

    LaunchedEffect(profile) { load() }

    val tabs = listOf("god" to "God", "story" to "Story", "resumes" to "Resume and Portfolio")
    // Everything stays visible (#123, consistent with Skills): explicitly-off
    // toolsets render an Off badge instead of being dropped, and unknown
    // toggle state (null) shows no badge — so flag-less server shapes never
    // blank the section.
    val listedToolsets = toolsets
    // Dedupe: explicit mcp-* toolset rows already render the server, so
    // derived rows parsed from their mcp__<server>__* tools are fallback
    // only (otherwise each server shows twice: 4 servers -> 8 rows).
    val mcp = remember(listedToolsets) {
        dedupMcpServers(listedToolsets, mcpServersFrom(listedToolsets))
    }

    // Search + origin filter + sort (Tasks-style).
    var query by remember { mutableStateOf("") }
    var toolsOrigin by remember { mutableStateOf(ToolsOrigin.All) }
    var toolsSort by remember { mutableStateOf(ToolsSort.NameAz) }
    var toolsSortOpen by remember { mutableStateOf(false) }

    fun sortToolsets(list: List<ToolsetInfo>): List<ToolsetInfo> = when (toolsSort) {
        ToolsSort.NameZa -> list.sortedByDescending { it.label.ifEmpty { it.name }.lowercase(Locale.ROOT) }
        ToolsSort.MostTools -> list.sortedWith(
            compareByDescending<ToolsetInfo> { it.tools.size }
                .thenBy { it.label.ifEmpty { it.name }.lowercase(Locale.ROOT) }
        )
        ToolsSort.NameAz -> list.sortedBy { it.label.ifEmpty { it.name }.lowercase(Locale.ROOT) }
    }
    val defaultTools = remember(listedToolsets, query, toolsSort) {
        sortToolsets(listedToolsets.filter { !it.isCustomMcp() && it.matches(query) })
    }
    val customMcpToolsets = remember(listedToolsets, query, toolsSort) {
        sortToolsets(listedToolsets.filter { it.isCustomMcp() && it.matches(query) })
    }
    val customMcpServers = remember(mcp, query, toolsSort) {
        val filtered = mcp.filter { it.matches(query) }
        if (toolsSort == ToolsSort.NameZa) {
            filtered.sortedByDescending { it.name.lowercase(Locale.ROOT) }
        } else {
            filtered.sortedWith(
                compareBy({ if (toolsSort == ToolsSort.MostTools) -it.tools.size else 0 },
                    { it.name.lowercase(Locale.ROOT) })
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
                title = { Text("Tools") },
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
                    placeholder = { Text("Search tools…") },
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
                LazyColumn(
                    modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp),
                    verticalArrangement = Arrangement.spacedBy(8.dp),
                    contentPadding = PaddingValues(vertical = 8.dp),
                ) {
                    if (listedToolsets.isEmpty() && toolsError.isNotEmpty() && loaded) {
                        item {
                            ErrorCard(raw = toolsError, onRetry = { load() })
                        }
                    }
                    if (loaded && listedToolsets.isEmpty() && toolsError.isEmpty()) {
                        item {
                            EmptyState(
                                icon = Icons.Filled.Build,
                                title = "Nothing listed",
                                subtitle = "This assistant reports no toolsets.",
                                actionLabel = "Refresh",
                                onAction = { load() },
                            )
                        }
                    }
                    // ── Tools (Default vs Custom MCP) ──
                    item {
                        Text(
                            "Tools",
                            style = MaterialTheme.typography.titleMedium,
                            modifier = Modifier.padding(horizontal = 4.dp, vertical = 4.dp),
                        )
                    }
                    // Totals header (#123): inventory size as a rough
                    // context-load proxy for this assistant.
                    if (loaded && toolsError.isEmpty()) {
                        item {
                            HintLine(
                                "${listedToolsets.size} toolsets · " +
                                    "${listedToolsets.sumOf { it.tools.size }} tools · " +
                                    "${mcp.size} MCP servers"
                            )
                        }
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
                            ToolsetCard(profile, ts)
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
                            ToolsetCard(profile, ts)
                        }
                        items(shownCustomMcpServers, key = { "ms:" + it.name }) { server ->
                            McpCard(profile, server)
                        }
                    }
                    if (loaded && listedToolsets.isNotEmpty()
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

/** Short HH:mm (plus date when not today); empty for unknown timestamps.
 * Month abbreviations pin to English so output never varies by device
 * locale. */
private fun shortTime(ts: Long): String {
    if (ts <= 0) return ""
    return try {
        val zdt = java.time.Instant.ofEpochMilli(ts).atZone(IST)
        val today = java.time.LocalDate.now(IST)
        if (zdt.toLocalDate() == today) {
            zdt.format(java.time.format.DateTimeFormatter.ofPattern("HH:mm"))
        } else {
            zdt.format(java.time.format.DateTimeFormatter.ofPattern("d MMM HH:mm", Locale.ENGLISH))
        }
    } catch (e: Exception) {
        ""
    }
}

/** Formats a stored `Instant.now().toString()` stamp (UTC ISO) for display
 * in IST (#124); blank stays blank, unparseable input falls back to raw
 * rather than hiding information. Month abbreviations pin to English. */
private fun formatSyncTime(raw: String): String {
    if (raw.isBlank()) return ""
    return runCatching {
        java.time.Instant.parse(raw.trim()).atZone(IST)
            .format(java.time.format.DateTimeFormatter.ofPattern("d MMM HH:mm", Locale.ENGLISH))
    }.getOrDefault(raw)
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
    var effort by remember { mutableStateOf("") }
    var catalog by remember { mutableStateOf<List<ProviderOption>>(emptyList()) }
    var loading by remember { mutableStateOf(true) }
    var error by remember { mutableStateOf("") }
    LaunchedEffect(tab) {
        loading = true
        error = ""
        val prefs = context.getSharedPreferences(AgentoApp.PREFS_NAME, android.content.Context.MODE_PRIVATE)
        provider = (prefs.getString("provider_$tab", "") ?: "").trim()
        model = (prefs.getString("model_$tab", "") ?: "").trim()
        effort = ChatApi(context).effortFor(tab, model).ifEmpty { EffortCatalog.defaultFor(model) }
        val api = ChatApi(context)
        catalog = runCatching {
            api.fetchCatalog(catalogPath(api)).getOrThrow()
        }.getOrElse { e ->
            error = e.message ?: e.javaClass.simpleName
            emptyList()
        }
        loading = false
    }
    fun save(p: String, m: String, e: String) {
        val api = ChatApi(context)
        api.setChatConfig(api.baseUrl(), api.password(), tab, p, m, e)
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
                    save(provider, model, effort)
                },
            )
            Spacer(modifier = Modifier.height(8.dp))
            OptionMenu(
                label = "Model",
                shown = model.ifEmpty { "Choose a model…" },
                options = modelOptionsFor(options, provider).map { m -> m to m },
                onPick = {
                    model = it
                    // Each model has its own effort vocabulary: restore this
                    // model's saved pick, else its sensible default.
                    effort = ChatApi(context).effortFor(tab, model)
                        .ifEmpty { EffortCatalog.defaultFor(model) }
                    error = ""
                    save(provider, model, effort)
                },
            )
            Spacer(modifier = Modifier.height(8.dp))
            // Reasoning effort: options depend on the selected model
            // (toggle families offer none/high, graded families offer levels).
            // The shown value is clamped: a pick restored for another model
            // never displays as valid here.
            val effortOptions = EffortCatalog.optionsFor(model)
            val shownEffort = effort.takeIf { it in effortOptions }
                ?: EffortCatalog.defaultFor(model)
            OptionMenu(
                label = "Effort",
                shown = shownEffort,
                options = effortOptions.map { e -> e to EffortCatalog.labelFor(e) },
                onPick = {
                    effort = it
                    error = ""
                    save(provider, model, effort)
                },
            )
            if (model.isBlank()) {
                Spacer(modifier = Modifier.height(4.dp))
                HintLine("Pick a model to see its effort levels.")
            }
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

/** Per-thread usage header (#121): thread total + last-turn context size
 * (the prompt of the latest reported turn — the full history is resent, so
 * prompt size IS the context pressure gauge) + counted/unreported turns.
 * The server line reconciles against the gateway session total (covers
 * turns served to other devices); the baseline line explains large
 * first-turn prompts (SOUL.md + skills + tools load before turn 1). */
@Composable
private fun ThreadUsageHeader(usage: ThreadUsage, server: SessionTotals?) {
    if (usage.counted == 0 && usage.unreported == 0) return
    Card(
        modifier = Modifier.fillMaxWidth(),
        colors = CardDefaults.cardColors(
            containerColor = MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.6f)
        ),
    ) {
        Column(modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp, vertical = 8.dp)) {
            Text(
                listOfNotNull(
                    if (usage.total > 0) {
                        "Thread ~${formatTokens(usage.total)} tok"
                    } else {
                        null
                    },
                    if (usage.lastPrompt > 0) {
                        "context ~${formatTokens(usage.lastPrompt)}"
                    } else {
                        null
                    },
                    if (usage.counted > 0) {
                        if (usage.counted == 1) "1 counted turn" else "${usage.counted} counted turns"
                    } else {
                        null
                    },
                    if (usage.unreported > 0) {
                        if (usage.unreported == 1) "1 without report" else "${usage.unreported} without reports"
                    } else {
                        null
                    },
                ).joinToString(" · ").ifEmpty { "No usage reported yet." },
                style = MaterialTheme.typography.labelSmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            val serverTotal = server?.total ?: 0L
            if (serverTotal > 0 && serverTotal != usage.total) {
                Text(
                    "Server total ~${formatTokens(serverTotal)} tok (all devices)",
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            if (usage.firstPrompt > 0) {
                Text(
                    "First-turn context ~${formatTokens(usage.firstPrompt)} (baseline + first message)",
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }
    }
}

/** Streaming chat surface (#52: conversational bubbles); auto-scrolls on
 * new tokens, delegates I/O to callbacks. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun ChatScreen(
    title: String,
    tab: String,
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
    onRegenerate: () -> Unit = {},
    onContinue: () -> Unit = {},
    onReplace: (Int, String) -> Unit = { _, _ -> },
    onFork: (Int, String) -> Unit = { _, _ -> },
    autoLiveGen: Int = 0,
) {
    val listState = rememberLazyListState()
    // Stick-to-bottom follows the user (#122): auto-scroll only while the
    // viewport is pinned to the latest message. A manual scroll-up unpins
    // (Jump-to-latest appears); reaching the bottom re-pins.
    var stick by remember(tab) { mutableStateOf(true) }
    // Unpin on user drags only (#122): programmatic smooth-scrolls also
    // raise isScrollInProgress, so gating on it would let every auto-scroll
    // unpin itself mid-stream. DragInteraction.Start fires for touch drags.
    LaunchedEffect(listState) {
        listState.interactionSource.interactions.collect {
            if (it is DragInteraction.Start) stick = false
        }
    }
    // Message-list head offset: the usage header item (when shown) shifts
    // message indices by one — every scroll target accounts for it.
    val headCount = if (state.messages.any { it.role != "user" && it.content.isNotBlank() }) 1 else 0
    // New turns / streamed tokens follow only while pinned. The key covers
    // content and reasoning length alike so thinking-only streams follow.
    val lastMsg = state.messages.lastOrNull()
    val lastLen = (lastMsg?.content?.length ?: 0) + (lastMsg?.reasoning?.length ?: 0)
    LaunchedEffect(state.messages.size, lastLen) {
        if (stick && state.messages.isNotEmpty()) {
            listState.animateScrollToItem(headCount + state.messages.size - 1)
        }
    }
    // Re-pin when the user scrolls back to the latest message.
    LaunchedEffect(listState.isScrollInProgress, state.messages.size, lastLen) {
        if (!listState.isScrollInProgress && state.messages.isNotEmpty()) {
            val last = listState.layoutInfo.visibleItemsInfo.lastOrNull()
            if (last != null && last.index >= headCount + state.messages.size - 1) stick = true
        }
    }
    // Inline user-message editor (#122): message index under edit, its
    // draft, and a pending replace awaiting drop-turns confirmation.
    var editingIndex by remember(tab) { mutableStateOf<Int?>(null) }
    var editDraft by remember(tab) { mutableStateOf("") }
    var confirmReplace by remember(tab) { mutableStateOf<Pair<Int, String>?>(null) }
    // Edit state belongs to one thread: a switch or a fresh thread closes it.
    LaunchedEffect(state.threadId) {
        editingIndex = null
        confirmReplace = null
    }
    val clipboard = LocalClipboardManager.current
    val scope = rememberCoroutineScope()
    val speakingKey by tts.speakingKey.collectAsState()
    // Live mode (#85): chain the existing one-shot voice input + TTS
    // readout into a continuous talk-listen-talk loop. The system
    // recognizer handles silence cutoff per turn; LiveInterrupt only
    // detects talk-over during readout (no transcription).
    // Live state is keyed by tab: whatever the when(dest) branch reuse
    // semantics are, a session can never bleed into another tab's screen.
    var liveMode by remember(tab) { mutableStateOf(false) }
    var silentRounds by remember(tab) { mutableIntStateOf(0) }
    // Transient recognizer faults (busy/audio/client overlap) retry without
    // costing a silent round; capped so a wedged recognizer still ends loudly.
    var transientRetries by remember(tab) { mutableIntStateOf(0) }
    // Rotation silently kills a live session (remember(tab) state is lost
    // and the rotation guard deliberately doesn't restart it). This flag
    // survives recreation so the user gets told instead of silence.
    var liveLostOnRotate by rememberSaveable(tab) { mutableStateOf(false) }
    LaunchedEffect(Unit) {
        if (liveLostOnRotate) {
            liveLostOnRotate = false
            snackbar.showSnackbar("Live session ended on rotation.")
        }
    }
    // Hands-free interrupt (#85): background mic watches for speech while
    // the readout plays; on trigger just cut TTS — the speakingKey effect
    // below starts the recognizer. Needs RECORD_AUDIO; without it the loop
    // still runs tap-to-talk.
    val context = LocalContext.current
    val interrupt = remember(tab) { LiveInterrupt({ scope.launch { tts.stop() } }) }
    var micArmed by remember(tab) { mutableStateOf(interrupt.hasPermission(context)) }
    // In-app recognizer for live mode (#85): headless, so our overlay
    // (status + End button) stays visible — the system dialog would cover it.
    val liveRec = remember(tab) { LiveRecognizer(context) }
    var livePartial by remember(tab) { mutableStateOf("") }
    var pendingLiveStart by remember(tab) { mutableStateOf(false) }
    val micPermission = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestPermission(),
    ) { granted ->
        micArmed = granted
        // Granted + pending start is consumed by an effect after startLive;
        // denied clears the pending start with a hint.
        if (!granted) {
            pendingLiveStart = false
            scope.launch {
                snackbar.showSnackbar("Live mode needs microphone access.")
            }
        }
    }
    DisposableEffect(tab) {
        onDispose {
            interrupt.stop(); liveRec.destroy(); tts.stop()
            // #107: leaving the tab ends the session silently. A rotation
            // recreates the activity (isChangingConfigurations) and keeps
            // the "ended on rotation" notice; a plain tab switch clears it
            // so re-entering never replays the end prompt.
            if ((context as? Activity)?.isChangingConfigurations != true) {
                liveLostOnRotate = false
            }
        }
    }
    /** Every send-type action cuts speech first. */
    fun stopThen(action: () -> Unit): () -> Unit = { tts.stop(); action() }
    fun speakMessage(key: String, text: String): Boolean {
        if (!tts.toggle(key, text)) {
            scope.launch { snackbar.showSnackbar("Voice output unavailable.") }
            return false
        }
        return true
    }
    fun endLive(reason: String? = null) {
        liveMode = false
        silentRounds = 0
        transientRetries = 0
        liveLostOnRotate = false
        livePartial = ""
        pendingLiveStart = false
        // Stop the detector now — don't rely on the liveMode=false effect
        // round-trip to get around to it.
        interrupt.stop()
        liveRec.cancel()
        tts.stop()
        if (reason != null) scope.launch { snackbar.showSnackbar(reason) }
    }
    // Reply/TTS loop effects are defined after startVoice below (they call it).
    // Built-in Android speech-to-text (RecognizerIntent — no extra
    // permission or dependency). Shared by God/Story/Resume and Portfolio: all three
    // tabs render this one ChatScreen, so one mic covers all chats.
    val voiceLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.StartActivityForResult()
    ) { result ->
        val heard = if (result.resultCode == Activity.RESULT_OK) {
            result.data
                ?.getStringArrayListExtra(RecognizerIntent.EXTRA_RESULTS)
                ?.firstOrNull()?.trim().orEmpty()
        } else {
            ""
        }
        // One-shot mic only: live sessions listen headless via LiveRecognizer,
        // so there is no live branch here (its retry path is onNoSpeech/onRetry).
        if (heard.isNotEmpty()) {
            val cur = state.pending
            onPending(if (cur.isBlank()) heard else "${cur.trimEnd()} $heard")
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
    /** One in-app listen cycle for the live loop (overlay stays visible). */
    fun liveListen() {
        liveRec.listen()
    }
    /** Begins (or resumes) the live session: kill turn, arm, listen. */
    fun startLive() {
        if (!liveRec.available()) {
            endLive("Voice recognition unavailable on this device.")
            return
        }
        pendingLiveStart = false
        liveRec.cancel()
        tts.stop()
        if (state.streaming) onStop()
        liveMode = true
        liveLostOnRotate = true
        silentRounds = 0
        transientRetries = 0
        livePartial = ""
        micArmed = true
        val handoff = speakingKey != null || state.streaming
        if (!handoff) liveListen()
    }
    // One-shot mic results flow through voiceLauncher below; only live
    // cycles arrive here. Reassigned each composition so closures stay fresh.
    fun bindLiveListener() {
        liveRec.listener = object : LiveRecognizer.Listener {
            override fun onBegin() {
                transientRetries = 0
            }
            override fun onPartial(text: String) {
                livePartial = text
            }
            override fun onResult(heard: String) {
                livePartial = ""
                transientRetries = 0
                if (!liveMode) {
                    onPending(heard)
                    return
                }
                if (heard.trimEnd('.', '!', '?').equals("stop", ignoreCase = true)) {
                    endLive()
                } else {
                    silentRounds = 0
                    onPending(heard)
                    onSend()
                }
            }
            override fun onNoSpeech() {
                if (!liveMode) return
                livePartial = ""
                transientRetries = 0
                silentRounds++
                if (silentRounds >= 3) endLive("Live session ended (no speech).")
                else liveListen()
            }
            override fun onRetry() {
                if (!liveMode) return
                transientRetries++
                if (transientRetries > 5) {
                    endLive("Voice recognition unavailable — live session ended.")
                } else {
                    liveListen()
                }
            }
            override fun onFatal(message: String) {
                if (liveMode) endLive(message)
            }
        }
    }
    // Auto-read: when a reply finishes cleanly while the toggle is on, speak it.
    // In live mode the reply is always spoken (independent of the toggle).
    var wasStreaming by remember(tab) { mutableStateOf(false) }
    LaunchedEffect(state.streaming) {
        if (wasStreaming && !state.streaming) {
            if (liveMode) {
                if (state.error.isEmpty()) {
                    silentRounds = 0
                    val last = state.messages.lastOrNull()
                    if (last != null && last.role == "assistant" && last.content.isNotBlank()) {
                        if (!speakMessage(ttsKeyFor(last), last.content)) {
                            // Engine dead: speakingKey never sets, so the
                            // loop effect can't re-listen — end it loudly.
                            endLive("Live session ended (voice output unavailable).")
                        }
                    } else {
                        liveListen()
                    }
                } else {
                    endLive("Live session ended (reply failed).")
                }
            } else if (autoSpeak && state.error.isEmpty()) {
                val last = state.messages.lastOrNull()
                if (last != null && last.role == "assistant" && last.content.isNotBlank()) {
                    speakMessage(ttsKeyFor(last), last.content)
                }
            }
        }
        wasStreaming = state.streaming
    }
    // Widget live request (#85): effect placed after startVoice (it calls
    // it); each generation starts the session once. Same steps as the
    // toggle (kill turn, arm, listen; handoff rule for the first listen).
    // Saveable (#107): exiting the tab and re-entering must not re-fire
    // the same generation and pop the End prompt again and again.
    var consumedLiveGen by rememberSaveable(tab) { mutableIntStateOf(0) }
    LaunchedEffect(autoLiveGen) {
        if (autoLiveGen > consumedLiveGen) {
            consumedLiveGen = autoLiveGen
            if (!interrupt.hasPermission(context)) {
                pendingLiveStart = true
                micPermission.launch(Manifest.permission.RECORD_AUDIO)
            } else {
                startLive()
            }
        }
    }
    // Granted mic permission with a pending start fires the session.
    LaunchedEffect(micArmed) {
        if (micArmed && pendingLiveStart) startLive()
    }
    // Bind recognizer callbacks (after all local funs).
    bindLiveListener()
    // Live loop driver: when the spoken reply fully finishes, listen again.
    // While it plays, the interrupt detector listens for talk-over.
    var wasSpeaking by remember(tab) { mutableStateOf(false) }
    LaunchedEffect(speakingKey, liveMode, micArmed) {
        if (liveMode && speakingKey != null && micArmed) {
            // start() false = no mic after all: degrade visibly to
            // tap-to-talk instead of retrying silently every re-listen.
            if (!interrupt.start(this)) micArmed = false
        } else {
            interrupt.stop()
        }
        if (wasSpeaking && speakingKey == null && liveMode) {
            liveListen()
        }
        wasSpeaking = speakingKey != null
    }
    // Surface send failures as a toast too (the inline card keeps details).
    LaunchedEffect(state.error) {
        if (state.error.isNotEmpty()) {
            snackbar.showSnackbar(friendlyError(state.error).title)
        }
    }
    Scaffold(
        snackbarHost = { SnackbarHost(snackbar) },
        // Jump-to-latest (#122): the stock FAB slot, shown once a manual
        // scroll-up unpins the viewport; tapping re-pins and drops to the
        // newest message. No extra nesting around the message list.
        floatingActionButton = {
            if (!stick && state.messages.isNotEmpty()) {
                SmallFloatingActionButton(
                    onClick = {
                        stick = true
                        scope.launch {
                            listState.animateScrollToItem(
                                headCount + state.messages.size - 1
                            )
                        }
                    },
                    containerColor = MaterialTheme.colorScheme.secondaryContainer,
                ) {
                    Icon(
                        Icons.Filled.ArrowDownward,
                        contentDescription = "Jump to latest",
                    )
                }
            }
        },
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
                    if (state.streaming && state.activeTools.isNotEmpty()) {
                        val shown = state.activeTools.take(3).joinToString(", ") +
                            if (state.activeTools.size > 3) {
                                " +${state.activeTools.size - 3} more"
                            } else ""
                        val label = state.activeToolLabel.trim()
                        Text(
                            "Using $shown…" + if (label.isNotEmpty()) " · $label" else "",
                            style = MaterialTheme.typography.labelSmall,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                            maxLines = 1,
                            overflow = TextOverflow.Ellipsis,
                            modifier = Modifier.fillMaxWidth()
                                .padding(horizontal = 16.dp, vertical = 2.dp),
                        )
                    }
                    // Queue-or-send (#122): the composer stays live while
                    // streaming. Parking a send here auto-fires it on Done;
                    // a queued send returns to the composer on failure/stop.
                    if (state.streaming && state.queued.isBlank() && state.pending.isNotBlank()) {
                        Row(
                            modifier = Modifier.fillMaxWidth(),
                            horizontalArrangement = Arrangement.End,
                        ) {
                            TextButton(onClick = onSend) { Text("Queue send") }
                        }
                    }
                    if (state.streaming && state.queued.isNotBlank()) {
                        Text(
                            "Queued — sends when the reply finishes.",
                            style = MaterialTheme.typography.labelSmall,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                            modifier = Modifier.fillMaxWidth()
                                .padding(horizontal = 16.dp, vertical = 2.dp),
                        )
                    }
                    Row(
                        modifier = Modifier.fillMaxWidth().padding(12.dp),
                        verticalAlignment = Alignment.CenterVertically,
                    ) {
                        OutlinedTextField(
                            value = state.pending,
                            onValueChange = onPending,
                            placeholder = {
                                Text(
                                    if (liveMode) "Live session — speak now…"
                                    else "Ask ${title.lowercase(Locale.ROOT)} anything…",
                                )
                            },
                            modifier = Modifier.weight(1f),
                            maxLines = 4,
                            keyboardOptions = KeyboardOptions(imeAction = ImeAction.Send),
                            keyboardActions = KeyboardActions(onSend = { stopThen(onSend)() }),
                            shape = RoundedCornerShape(24.dp),
                        )
                        Spacer(modifier = Modifier.width(8.dp))
                        // Live mode (#85): continuous talk-listen-talk using the
                        // same recognizer + TTS. Enabling mid-turn stops it first.
                        if (liveMode) {
                            FilledTonalIconButton(onClick = { endLive() }) {
                                Icon(Icons.Filled.Equalizer, contentDescription = "End live session")
                            }
                        } else {
                            IconButton(
                                onClick = {
                                    // Mic permission gates the session: the
                                    // in-app recognizer cannot run without it.
                                    if (!interrupt.hasPermission(context)) {
                                        pendingLiveStart = true
                                        micPermission.launch(Manifest.permission.RECORD_AUDIO)
                                    } else {
                                        startLive()
                                    }
                                },
                                enabled = state.ready,
                            ) {
                                Icon(Icons.Filled.Equalizer, contentDescription = "Start live session")
                            }
                        }
                        IconButton(
                            // Live: cut speech, drop the current listen, start
                            // fresh (no system dialog exists to stack).
                            onClick = if (liveMode) {
                                {
                                    tts.stop()
                                    liveRec.cancel()
                                    if (speakingKey == null) liveListen()
                                }
                            } else {
                                stopThen(::startVoice)
                            },
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
                        // Per-thread usage header (#121): device-observed sums
                        // from per-message reports, reconciled against the
                        // server session total (the multi-device truth).
                        if (state.messages.any { it.role != "user" && it.content.isNotBlank() }) {
                            item {
                                ThreadUsageHeader(
                                    usage = threadUsageOf(state.messages),
                                    server = state.serverTokens,
                                )
                            }
                        }
                        if (state.messages.isEmpty() && !setupNeeded) {
                            item {
                                EmptyState(
                                    icon = Icons.Filled.MenuBook,
                                    title = "Start the conversation",
                                    subtitle = "Ask anything — past conversations stay under the history button.",
                                )
                            }
                        }
                        itemsIndexed(state.messages) { index, msg ->
                            val isUser = msg.role == "user"
                            val isLast = index == state.messages.lastIndex
                            // Replace resend drops turns when anything
                            // non-blank follows the edited message.
                            val dropsTurns = state.messages.drop(index + 1).any { it.content.isNotBlank() }
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
                                            // Edit & resubmit (#122): pencil opens an
                                            // inline editor (Replace / Fork).
                                            if (isUser && !state.streaming) {
                                                IconButton(
                                                    onClick = {
                                                        editingIndex = index
                                                        editDraft = msg.content
                                                    },
                                                    modifier = Modifier.size(28.dp),
                                                ) {
                                                    Icon(Icons.Filled.Edit, contentDescription = "Edit and resend")
                                                }
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
                                                // Regenerate (#122, v1 replace): resends
                                                // history minus this reply. Offered on
                                                // the latest assistant message only.
                                                if (isLast && !state.streaming) {
                                                    IconButton(
                                                        onClick = stopThen(onRegenerate),
                                                        modifier = Modifier.size(28.dp),
                                                    ) {
                                                        Icon(Icons.Filled.Refresh, contentDescription = "Regenerate reply")
                                                    }
                                                }
                                            }
                                        }
                                        Spacer(modifier = Modifier.height(2.dp))
                                        if (isUser) {
                                            if (editingIndex == index) {
                                                // Inline editor (#122): Replace drops
                                                // later turns (confirmed first),
                                                // Fork copies them to a new thread.
                                                OutlinedTextField(
                                                    value = editDraft,
                                                    onValueChange = { editDraft = it },
                                                    modifier = Modifier.fillMaxWidth(),
                                                    maxLines = 6,
                                                )
                                                Spacer(modifier = Modifier.height(4.dp))
                                                Row(
                                                    horizontalArrangement = Arrangement.spacedBy(8.dp),
                                                    verticalAlignment = Alignment.CenterVertically,
                                                ) {
                                                    Button(
                                                        onClick = {
                                                            if (dropsTurns) {
                                                                confirmReplace = index to editDraft
                                                            } else {
                                                                onReplace(index, editDraft)
                                                                editingIndex = null
                                                            }
                                                        },
                                                        enabled = editDraft.isNotBlank(),
                                                    ) { Text("Replace") }
                                                    OutlinedButton(
                                                        onClick = {
                                                            onFork(index, editDraft)
                                                            editingIndex = null
                                                        },
                                                        enabled = editDraft.isNotBlank(),
                                                    ) { Text("Fork") }
                                                    TextButton(onClick = { editingIndex = null }) {
                                                        Text("Cancel")
                                                    }
                                                }
                                            } else {
                                                SelectionContainer {
                                                    Text(
                                                        msg.content.ifEmpty { "…" },
                                                        style = MaterialTheme.typography.bodyMedium,
                                                    )
                                                }
                                            }
                                        } else {
                                            // Reasoning trace (#122): collapsible
                                            // Thinking section above the reply.
                                            if (msg.reasoning.isNotEmpty()) {
                                                // Keyed on position + timestamp:
                                                // two rapid turns can share a ms.
                                                var thinkingOpen by remember(index, msg.ts) { mutableStateOf(false) }
                                                TextButton(
                                                    onClick = { thinkingOpen = !thinkingOpen },
                                                    contentPadding = PaddingValues(0.dp),
                                                ) {
                                                    Text(
                                                        if (thinkingOpen) "Hide thinking" else "Thinking",
                                                        style = MaterialTheme.typography.labelSmall,
                                                    )
                                                    Icon(
                                                        if (thinkingOpen) Icons.Filled.ExpandLess
                                                        else Icons.Filled.ExpandMore,
                                                        contentDescription = null,
                                                        modifier = Modifier.size(16.dp),
                                                    )
                                                }
                                                if (thinkingOpen) {
                                                    SelectionContainer {
                                                        Text(
                                                            msg.reasoning,
                                                            style = MaterialTheme.typography.bodySmall,
                                                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                                                        )
                                                    }
                                                    Spacer(modifier = Modifier.height(4.dp))
                                                }
                                            }
                                            // Assistant text is selectable (#122);
                                            // code-block copy buttons are deferred
                                            // (mikepenz components override needs
                                            // a version-pinned API check) — the
                                            // header copy keeps the full source.
                                            SelectionContainer {
                                                Markdown(
                                                    content = msg.content.ifEmpty { "…" },
                                                    modifier = Modifier.fillMaxWidth(),
                                                )
                                            }
                                            if (msg.interrupted) {
                                                Spacer(modifier = Modifier.height(4.dp))
                                                Text(
                                                    "Stopped — reply incomplete.",
                                                    style = MaterialTheme.typography.labelSmall,
                                                    color = MaterialTheme.colorScheme.error,
                                                )
                                            }
                                            // Persisted tool/skill usage for this
                                            // reply (live frames + post-turn fetch),
                                            // plus the turn's token report (#121).
                                            // No cost line: the gateway reports
                                            // estimated_cost_usd 0.0 / unknown,
                                            // so nothing is rendered until
                                            // pricing is real.
                                            val usedLine = listOf(
                                                // Per-message model attribution (#122)
                                                // leads the meta line, then tokens
                                                // (#121), then tools/skills.
                                                (msg.model.ifEmpty { modelLabel })
                                                    .takeIf { it.isNotBlank() },
                                                if (msg.total > 0) {
                                                    "~${formatTokens(msg.total)} tok"
                                                } else if (msg.content.isNotEmpty()) {
                                                    // Explicit gap (#121): a reply with no
                                                    // usable report — failed/interrupted
                                                    // turns and pre-tracking history
                                                    // alike, matching the header's
                                                    // "without reports" count.
                                                    "no usage reported"
                                                } else {
                                                    null
                                                },
                                                msg.tools.takeIf { it.isNotEmpty() }
                                                    ?.let { "Tools: ${it.joinToString(", ")}" },
                                                msg.skills.takeIf { it.isNotEmpty() }
                                                    ?.let { "Skills: ${it.joinToString(", ")}" },
                                            ).filterNotNull().joinToString(" · ")
                                            if (usedLine.isNotEmpty()) {
                                                Spacer(modifier = Modifier.height(4.dp))
                                                Text(
                                                    usedLine,
                                                    style = MaterialTheme.typography.labelSmall,
                                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                                    maxLines = 2,
                                                    overflow = TextOverflow.Ellipsis,
                                                )
                                            }
                                            // Stopped-state actions (#122): the latest
                                            // interrupted reply offers an honest
                                            // Continue (fresh "Continue" turn) plus
                                            // Regenerate (replace semantics).
                                            if (msg.interrupted && isLast && !state.streaming) {
                                                Spacer(modifier = Modifier.height(4.dp))
                                                Row(
                                                    horizontalArrangement = Arrangement.spacedBy(8.dp),
                                                    verticalAlignment = Alignment.CenterVertically,
                                                ) {
                                                    OutlinedButton(onClick = stopThen(onContinue)) {
                                                        Text("Continue")
                                                    }
                                                    TextButton(onClick = stopThen(onRegenerate)) {
                                                        Text("Regenerate")
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
        }
        // Replace confirmation (#122): resending from an edited message
        // drops every later turn — confirmed here, Fork needs none.
        val confirm = confirmReplace
        if (confirm != null) {
            val (idx, text) = confirm
            val dropped = state.messages.drop(idx + 1).count { it.content.isNotBlank() }
            AlertDialog(
                onDismissRequest = { confirmReplace = null },
                title = { Text("Resend from edited message?") },
                text = {
                    Text(
                        if (dropped == 1) {
                            "This drops 1 later message and resends."
                        } else {
                            "This drops $dropped later messages and resends."
                        }
                    )
                },
                confirmButton = {
                    TextButton(onClick = {
                        onReplace(idx, text)
                        confirmReplace = null
                        editingIndex = null
                    }) { Text("Replace") }
                },
                dismissButton = {
                    TextButton(onClick = { confirmReplace = null }) { Text("Cancel") }
                },
            )
        }
        // Live overlay (#85): status + transcript + an End button that stays
        // reachable — the in-app recognizer has no system dialog to cover it.
        if (liveMode) {
            LiveOverlay(
                speaking = speakingKey != null,
                thinking = state.streaming,
                partial = livePartial,
                onEnd = { endLive() },
            )
        }
    }
}

/** Live voice overlay (#85): status + live transcript + a big End button.
 * Drawn in the Scaffold BoxScope over the chat, so it stays tappable —
 * the in-app recognizer shows no system dialog to cover it. Taps outside
 * the card fall through to the chat behind (typing mid-live is allowed). */
@Composable
private fun LiveOverlay(
    speaking: Boolean,
    thinking: Boolean,
    partial: String,
    onEnd: () -> Unit,
) {
    val status = when {
        speaking -> "Speaking…"
        thinking -> "Thinking…"
        else -> "Listening…"
    }
    Box(
        modifier = Modifier.fillMaxSize()
            .background(MaterialTheme.colorScheme.scrim.copy(alpha = 0.45f)),
        contentAlignment = Alignment.Center,
    ) {
        Card(
            modifier = Modifier.fillMaxWidth().padding(horizontal = 32.dp),
        ) {
            Column(
                modifier = Modifier.fillMaxWidth().padding(24.dp),
                horizontalAlignment = Alignment.CenterHorizontally,
            ) {
                Icon(
                    Icons.Filled.Equalizer,
                    contentDescription = null,
                    tint = MaterialTheme.colorScheme.primary,
                    modifier = Modifier.size(40.dp),
                )
                Spacer(modifier = Modifier.height(12.dp))
                Text(status, style = MaterialTheme.typography.titleLarge)
                if (partial.isNotEmpty()) {
                    Spacer(modifier = Modifier.height(8.dp))
                    Text(
                        "“$partial”",
                        style = MaterialTheme.typography.bodyLarge,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
                Spacer(modifier = Modifier.height(20.dp))
                Button(
                    onClick = onEnd,
                    colors = ButtonDefaults.buttonColors(
                        containerColor = MaterialTheme.colorScheme.error,
                    ),
                    modifier = Modifier.fillMaxWidth().height(56.dp),
                ) {
                    Text("End live session", style = MaterialTheme.typography.titleMedium)
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
    // Bumped on every resume so the hub's widget count reflects a
    // placement added (or removed) while the app was in the background —
    // same reason the widget section re-reads.
    var resumeTick by remember { mutableIntStateOf(0) }
    LifecycleEventEffect(Lifecycle.Event.ON_RESUME) { resumeTick++ }
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
                        val usageStatus = remember(context, settingsRefresh) {
                            val all = UsageStore.loadAll(context)
                            val total = all.values.fold(0L) { acc, t -> acc + t.total }
                            val turns = all.values.fold(0L) { acc, t -> acc + t.turns }
                            if (turns == 0L) "Not tracked yet"
                            else "${formatTokens(total)} tokens · $turns turns"
                        }
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
                            // Re-read on resume, keyed above.
                            widgetStatus = remember(context, resumeTick) {
                                val placed = taskWidgetIds(context).size
                                when (placed) {
                                    0 -> "Not added"
                                    1 -> "1 on home screen"
                                    else -> "$placed on home screen"
                                }
                            },
                            usageStatus = usageStatus,
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
                        WidgetDiagnosticsCard()
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
                            HintLine("Last sync: ${formatSyncTime(state.lastSyncAt)}")
                        }
                        if (state.lastResult.isNotEmpty() && state.lastResult != "syncing…") {
                            if (state.lastResult.startsWith("FAILED")) {
                                ErrorCard(raw = state.lastResult.removePrefix("FAILED — ").removePrefix("FAILED "))
                            } else {
                                HintLine(state.lastResult)
                            }
                        }
                    }
                    SettingSection.Widget -> {
                        WidgetSettingsSection()
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
                    }
                    SettingSection.Usage -> {
                        UsageSection()
                    }
                }
            }
        }
    }
}

/** Real usage: per-assistant token counts reported by the server.
 * Each completed turn's stream carries a `usage` object (see
 * [parseTokenUsage]); totals accumulate on-device from those reports, so
 * turns from before this update — and failed turns, which report zeros —
 * contribute nothing. Cached is the of-prompt cached subset. Cost is
 * deliberately absent: the gateway reports estimated_cost_usd 0.0 /
 * unknown, so nothing renders until pricing is real. */
@Composable
private fun UsageSection() {
    val context = LocalContext.current
    var rows by remember { mutableStateOf<List<UsageRow>>(emptyList()) }
    var loaded by remember { mutableStateOf(false) }

    LaunchedEffect(Unit) {
        val data = withContext(Dispatchers.IO) {
            UsageStore.clearLegacy(context)
            listOf(
                "god" to "God",
                "story" to "Story",
                "resumes" to "Resume and Portfolio",
            ).map { (tab, label) ->
                val threads = ChatThreads.load(context, tab)
                val t = UsageStore.load(context, tab)
                // Replies with no usable report (failed/interrupted turns,
                // or history from before per-message tracking) surface as
                // explicit gaps instead of silent skips.
                val unreported = threads.flatMap { it.messages }.count {
                    it.role != "user" && it.content.isNotBlank() &&
                        it.total <= 0 && it.prompt <= 0 && it.completion <= 0
                }
                UsageRow(
                    label = label,
                    conversations = threads.size,
                    messages = threads.sumOf { it.messages.size },
                    prompt = t.prompt,
                    completion = t.completion,
                    total = t.total,
                    cached = t.cached,
                    turns = t.turns,
                    unreported = unreported,
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
    val totalTokens = rows.sumOf { it.total }
    val countedTurns = rows.sumOf { it.turns }
    SectionCard(
        title = "Total",
        subtitle = "Server-reported tokens across all assistants, counted on this device.",
    ) {
        Text(
            "${formatTokens(totalTokens)} tokens · " +
                "${rows.sumOf { it.messages }} messages · " +
                "${rows.sumOf { it.conversations }} conversations",
            style = MaterialTheme.typography.bodyLarge,
        )
    }
    rows.forEach { r ->
        SectionCard(title = r.label) {
            Text(
                "${formatTokens(r.total)} tokens · " +
                    "${r.messages} messages · ${r.conversations} conversations",
                style = MaterialTheme.typography.bodyMedium,
            )
            HintLine(
                if (r.turns > 0) {
                    listOfNotNull(
                        "Prompt ${formatTokens(r.prompt)}",
                        "completion ${formatTokens(r.completion)}",
                        if (r.cached > 0) "cached ${formatTokens(r.cached)}" else null,
                        "${r.turns} counted turns",
                        if (r.unreported > 0) "${r.unreported} without reports" else null,
                    ).joinToString(" · ")
                } else {
                    "No token counts reported yet — chat once to start tracking."
                }
            )
        }
    }
    if (countedTurns == 0L) {
        HintLine("Nothing counted yet. Turns from before this update have no reports and can't be backfilled.")
    } else {
        HintLine("Only turns with server reports count — failed turns report zeros and are skipped.")
    }
}

private data class UsageRow(
    val label: String,
    val conversations: Int,
    val messages: Int,
    val prompt: Long,
    val completion: Long,
    val total: Long,
    val cached: Long,
    val turns: Long,
    val unreported: Int,
)

private fun formatTokens(tokens: Long): String {
    return when {
        tokens >= 1_000_000 -> "%.1fM".format(Locale.US, tokens / 1_000_000.0)
        tokens >= 1_000 -> "%.1fk".format(Locale.US, tokens / 1_000.0)
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
    widgetStatus: String,
    usageStatus: String,
    appVersion: String,
    onPick: (SettingSection) -> Unit,
) {
    val rows = remember(serverStatus, healthStatus, themeStatus, notificationsStatus, widgetStatus, usageStatus, appVersion) {
        listOf(
            HubRow(SettingSection.Connection, serverStatus),
            HubRow(SettingSection.Health, healthStatus),
            HubRow(SettingSection.Appearance, themeStatus),
            HubRow(SettingSection.Widget, widgetStatus),
            HubRow(SettingSection.Notifications, notificationsStatus),
            HubRow(SettingSection.Storage, "Backup"),
            HubRow(SettingSection.Usage, usageStatus),
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
    SettingSection.Widget -> Icons.Filled.Dashboard
    SettingSection.Notifications -> Icons.Filled.Notifications
    SettingSection.Storage -> Icons.Filled.Folder
    SettingSection.Usage -> Icons.Filled.DataUsage
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
            // Rendered as Markdown (not raw text) so changelog headings and
            // lists never show their `#`/`-` markers in the app. Full notes:
            // truncating raw Markdown mid-token can leave unclosed syntax
            // and render broken output.
            Markdown(
                content = rel.notes,
                modifier = Modifier.fillMaxWidth(),
            )
        }
        if (status.isNotEmpty()) {
            if (failed) ErrorCard(raw = status) else HintLine(status)
        }
    }
}

/** Task-widget placement settings (Settings → Home-screen widget).
 *
 * The widget's own header controls never rendered on some launchers
 * (#137), so what it shows is changed from the app. This used to sit
 * under the task rows in the Task Manager, which is a poor home for it:
 * it configures the home screen, not the task list. Each placement is
 * configured on its own, because the settings are stored per widget id.
 */
@Composable
private fun WidgetSettingsSection() {
    val context = LocalContext.current
    // Re-read on resume: the user can add or drop a placement (or come
    // back from the launcher) while this screen sits in the back stack,
    // and a stale list here is worse than useless.
    var tick by remember { mutableIntStateOf(0) }
    LifecycleEventEffect(Lifecycle.Event.ON_RESUME) { tick++ }
    // Filtered against each id's own provider info: the voice widget's
    // placement must never be listed (or configured) as a task widget.
    val widgetIds = remember(context, tick) {
        taskWidgetIds(context).toList()
    }
    val canPin = remember(context) {
        AppWidgetManager.getInstance(context).isRequestPinAppWidgetSupported
    }
    val config = rememberLauncherForActivityResult(
        ActivityResultContracts.StartActivityForResult(),
    ) { tick++ }

    SectionCard(
        title = "Home-screen widget",
        subtitle = "What the task widget shows: which tasks, how dense the rows are, and whether due dates and scrolling are on. Every placement keeps its own settings.",
    ) {
        if (widgetIds.isEmpty()) {
            Text(
                "No task widget on your home screen yet.",
                style = MaterialTheme.typography.bodyMedium,
            )
            Text(
                "Long-press an empty spot on the home screen, then Widgets → Agento — or use the button below.",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            // Only where the launcher supports pinning: on the ones that
            // don't, requestPinAppWidget is a silent no-op, so a button
            // that does nothing would be worse than no button. The
            // instructions above still apply there.
            if (canPin) {
                OutlinedButton(
                    onClick = {
                        AppWidgetManager.getInstance(context)
                            .requestPinAppWidget(
                                ComponentName(context, TaskWidget::class.java), null, null)
                    },
                ) { Text("Add the widget") }
            }
        } else {
            widgetIds.forEachIndexed { i, id ->
                if (i > 0) {
                    HorizontalDivider(modifier = Modifier.padding(vertical = 8.dp))
                }
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Column(modifier = Modifier.weight(1f)) {
                        Text(
                            "Widget ${i + 1}",
                            style = MaterialTheme.typography.bodyMedium,
                        )
                        Text(
                            buildString {
                                append(TaskWidget.viewFor(context, id).title)
                                append(" tasks · ")
                                append(TaskWidget.densityFor(context, id).title)
                                append(" rows")
                                if (!TaskWidget.showDueFor(context, id)) append(" · no due dates")
                                if (!TaskWidget.scrollableFor(context, id)) {
                                    append(" · up to ${TaskWidget.STATIC_ROW_LIMIT} rows")
                                }
                            },
                            style = MaterialTheme.typography.bodySmall,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                    Spacer(modifier = Modifier.width(12.dp))
                    OutlinedButton(
                        onClick = {
                            config.launch(
                                Intent(context, TaskWidgetConfigActivity::class.java)
                                    .putExtra(AppWidgetManager.EXTRA_APPWIDGET_ID, id)
                            )
                        },
                    ) { Text("Configure") }
                }
            }
        }
        Text(
            "On some launchers the widget's own buttons never appear — this screen is the reliable way to change these.",
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}

/** Task-widget health + one-tap log copy (Settings → About), so a broken
 * widget can be diagnosed without adb: placements, cached counts, last
 * fetch errors, plus recent log lines from our own process. */
@Composable
private fun WidgetDiagnosticsCard() {
    val context = LocalContext.current
    val clipboard = LocalClipboardManager.current
    val scope = rememberCoroutineScope()
    var summary by remember { mutableStateOf("") }
    var copying by remember { mutableStateOf(false) }
    var copied by remember { mutableStateOf(false) }
    var saving by remember { mutableStateOf(false) }
    var saved by remember { mutableStateOf("") }
    var clearing by remember { mutableStateOf(false) }
    var cleared by remember { mutableStateOf("") }
    // Prefs/AppWidgetManager reads stay off Main (same as the log dump).
    LaunchedEffect(Unit) {
        summary = withContext(Dispatchers.IO) { TaskWidget.diagnostics(context) }
    }
    SectionCard(
        title = "Widget diagnostics",
        subtitle = "Task-widget state and recent log. If the home-screen widget errors, save the report and upload it — it includes system log, where a host-side widget failure shows up.",
    ) {
        if (summary.isNotEmpty()) {
            SelectionContainer {
                Text(summary, style = MaterialTheme.typography.bodySmall)
            }
        }
        Button(
            onClick = {
                copying = true
                copied = false
                scope.launch {
                    try {
                        val fresh = withContext(Dispatchers.IO) { TaskWidget.diagnostics(context) }
                        summary = fresh
                        // Same content as the file export (minus the long
                        // log tail): copying must not silently miss the
                        // host-side lines the subtitle promises.
                        val mine = withContext(Dispatchers.IO) {
                            TaskWidget.dumpLog(interestingLines = 60, tailLines = 15)
                        }
                        val sys = withContext(Dispatchers.IO) {
                            TaskWidget.dumpLog(
                                allProcesses = true,
                                interestingLines = 120, tailLines = 0)
                        }
                        clipboard.setText(
                            AnnotatedString(
                                ("$fresh\n--- log (this app) ---\n$mine\n" +
                                    "--- log (system, recent) ---\n$sys").take(6000)
                            )
                        )
                        copied = true
                    } catch (e: Exception) {
                        summary = "diagnostics failed: ${e.message ?: e.javaClass.simpleName}"
                    } finally {
                        copying = false
                    }
                }
            },
            enabled = !copying && !saving && !clearing,
            modifier = Modifier.fillMaxWidth(),
        ) {
            Text(if (copying) "Copying…" else "Copy diagnostics")
        }
        OutlinedButton(
            onClick = {
                saving = true
                saved = ""
                scope.launch {
                    runCatching {
                        val report = withContext(Dispatchers.IO) { TaskWidget.buildReport(context) }
                        val file = withContext(Dispatchers.IO) {
                            saveDiagnosticsFile(context, report)
                        }
                        val uri = androidx.core.content.FileProvider.getUriForFile(
                            context, "${context.packageName}.fileprovider", file)
                        // Grant via ClipData on both intents: the flag on
                        // the inner intent alone isn't reliably forwarded
                        // by createChooser() on all API levels.
                        val share = android.content.Intent(android.content.Intent.ACTION_SEND).apply {
                            type = "text/plain"
                            clipData = android.content.ClipData.newUri(
                                context.contentResolver, "diagnostics", uri)
                            putExtra(android.content.Intent.EXTRA_STREAM, uri)
                            addFlags(android.content.Intent.FLAG_GRANT_READ_URI_PERMISSION)
                        }
                        val chooser = android.content.Intent.createChooser(share, "Share diagnostics").apply {
                            addFlags(android.content.Intent.FLAG_GRANT_READ_URI_PERMISSION)
                        }
                        context.startActivity(chooser)
                        saved = "Saved ${file.name} — share it to upload."
                    }.onFailure { e ->
                        saved = "Save failed: ${e.message ?: e.javaClass.simpleName}"
                    }
                    saving = false
                }
            },
            enabled = !copying && !saving && !clearing,
            modifier = Modifier.fillMaxWidth(),
        ) {
            Text(if (saving) "Saving…" else "Save report to file")
        }
        OutlinedButton(
            onClick = {
                clearing = true
                scope.launch {
                    cleared = withContext(Dispatchers.IO) { TaskWidget.clearSystemLog() }
                    clearing = false
                }
            },
            enabled = !copying && !saving && !clearing,
            modifier = Modifier.fillMaxWidth(),
        ) {
            Text(if (clearing) "Clearing…" else "Clear system log")
        }
        if (copied) HintLine("Copied — paste it in chat.")
        if (saved.isNotEmpty()) HintLine(saved)
        if (cleared.isNotEmpty()) HintLine(cleared)
    }
}

/** Writes a diagnostics report under cache/diagnostics for upload.
 * Millis-precision names avoid same-second collisions; only the newest
 * 5 are kept so retries never grow the cache unbounded. */
private fun saveDiagnosticsFile(context: Context, text: String): java.io.File {
    val dir = java.io.File(context.cacheDir, "diagnostics").apply { mkdirs() }
    val stamp = java.time.LocalDateTime.now(IST)
        .format(java.time.format.DateTimeFormatter.ofPattern("yyyyMMdd-HHmmss-SSS"))
    val file = java.io.File(dir, "agento-diagnostics-$stamp.txt").apply { writeText(text) }
    dir.listFiles()
        ?.sortedByDescending { it.lastModified() }
        ?.drop(5)
        ?.forEach { runCatching { it.delete() } }
    return file
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
