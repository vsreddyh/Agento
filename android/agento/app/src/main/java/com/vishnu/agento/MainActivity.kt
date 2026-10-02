@file:OptIn(ExperimentalMaterial3Api::class)

package com.vishnu.agento

import android.content.Context
import android.content.Intent
import android.os.Build
import android.os.Bundle
import android.provider.Settings
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.viewModels
import androidx.compose.foundation.background
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.grid.items
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Assignment
import androidx.compose.material.icons.filled.Build
import androidx.compose.material.icons.filled.Description
import androidx.compose.material.icons.filled.ExpandLess
import androidx.compose.material.icons.filled.ExpandMore
import androidx.compose.material.icons.filled.Extension
import androidx.compose.material.icons.filled.Folder
import androidx.compose.material.icons.filled.List
import androidx.compose.material.icons.filled.MenuBook
import androidx.compose.material.icons.filled.Notifications
import androidx.compose.material.icons.filled.Schedule
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material.icons.filled.Star
import androidx.compose.material.icons.filled.Undo
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import androidx.core.content.edit
import java.util.Locale
import kotlinx.coroutines.launch

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
            .edit {
                putString(KEY, mode)
            }
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
                                        prefs.edit {
                                            putBoolean("tts_auto", it)
                                        }
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
                                        prefs.edit {
                                            putBoolean("tts_auto", it)
                                        }
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
                                        prefs.edit {
                                            putBoolean("tts_auto", it)
                                        }
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

/** Human file size ("1.5 MB"); the storage screen's only formatter. */
internal fun humanSize(bytes: Long): String {
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
internal fun shortTime(ts: Long): String {
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
internal fun formatSyncTime(raw: String): String {
    if (raw.isBlank()) return ""
    return runCatching {
        java.time.Instant.parse(raw.trim()).atZone(IST)
            .format(java.time.format.DateTimeFormatter.ofPattern("d MMM HH:mm", Locale.ENGLISH))
    }.getOrDefault(raw)
}

