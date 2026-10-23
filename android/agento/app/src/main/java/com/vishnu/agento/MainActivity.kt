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
import androidx.compose.material.icons.filled.ExpandLess
import androidx.compose.material.icons.filled.ExpandMore
import androidx.compose.material.icons.filled.Extension
import androidx.compose.material.icons.filled.Folder
import androidx.compose.material.icons.filled.List
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
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.compose.LifecycleEventEffect
import androidx.core.content.edit
import java.util.Locale
import kotlinx.coroutines.launch

/** Sidebar destinations; Chat maps to the single gateway profile (`chat`, #289). */
private enum class Destination(val title: String) {
    Chat("Chat"),
    Tasks("Projects"),
    TaskManager("Task Manager"),
    Storage("Files"),
    Scheduler("Scheduler"),
    Skills("Skills"),
    Tools("Tools"),
    Settings("Settings"),
}

private fun Destination.icon() = when (this) {
    Destination.Chat -> Icons.Filled.Star
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
 * Backup, About): connection setup first, feature areas next, device data
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
     * via onNewIntent, and the Chat tab consumes each generation once.
     */
    private val liveGen = mutableIntStateOf(0)

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

    /**
     * Cron notification tap (#289). Generation counter like tasksGen so
     * retaps renavigate; SchedulerScreen consumes the job id and clears it
     * once the list loads.
     */
    private val schedulerGen = mutableIntStateOf(0)
    private val schedulerTargetId = mutableStateOf<String?>(null)

    /**
     * Chat-thread deep-link (task-instruction tap, #287). Generation counter
     * like tasksGen so retaps renavigate; ChatTab consumes the thread id
     * and clears it once the ViewModel is ready.
     */
    private val chatGen = mutableIntStateOf(0)
    private val chatThreadId = mutableStateOf<String?>(null)
    private val chatAutosend = mutableStateOf(false)

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        setIntent(intent)
        if (intent.action == LiveWidget.ACTION_LIVE) {
            liveGen.intValue++
        }
        if (intent.action == TaskWidget.ACTION_TASKS) {
            tasksGen.intValue++
            intent.getStringExtra(TaskWidget.EXTRA_TASK_ID)?.let {
                tasksTargetId.value = it
            }
        }
        if (intent.action == CronIntents.ACTION_CRON) {
            schedulerGen.intValue++
            intent.getStringExtra(CronIntents.EXTRA_JOB_ID)?.let {
                schedulerTargetId.value = it
            }
        }
        if (intent.action == InstructIntents.ACTION_CHAT) {
            chatGen.intValue++
            chatThreadId.value = intent.getStringExtra(InstructIntents.EXTRA_THREAD_ID)
            chatAutosend.value = intent.getBooleanExtra(InstructIntents.EXTRA_AUTOSEND, false)
        }
    }

    /** Adaptive sidebar navigation (#18, #64); theme from prefs (#28). */
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        // Widget live tap: same intent redelivered on rotation/recreation
        // must not start a second session — only a fresh launch counts.
        if (savedInstanceState == null && intent?.action == LiveWidget.ACTION_LIVE && liveGen.intValue == 0) {
            liveGen.intValue = 1
        }
        // Task widget tap: same redelivery guard — only a fresh launch counts.
        if (savedInstanceState == null && intent?.action == TaskWidget.ACTION_TASKS && tasksGen.intValue == 0) {
            tasksGen.intValue = 1
            intent.getStringExtra(TaskWidget.EXTRA_TASK_ID)?.let {
                tasksTargetId.value = it
            }
        }
        // Cron notification tap: same redelivery guard.
        if (savedInstanceState == null && intent?.action == CronIntents.ACTION_CRON && schedulerGen.intValue == 0) {
            schedulerGen.intValue = 1
            intent.getStringExtra(CronIntents.EXTRA_JOB_ID)?.let {
                schedulerTargetId.value = it
            }
        }
        // Task-instruction tap: same redelivery guard.
        if (savedInstanceState == null && intent?.action == InstructIntents.ACTION_CHAT && chatGen.intValue == 0) {
            chatGen.intValue = 1
            chatThreadId.value = intent?.getStringExtra(InstructIntents.EXTRA_THREAD_ID)
            chatAutosend.value = intent?.getBooleanExtra(InstructIntents.EXTRA_AUTOSEND, false) == true
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
                    var dest by remember { mutableStateOf(Destination.Chat) }
                    // Widget live tap (#85): land on the Chat tab; its
                    // ChatTab consumes the liveGen generation and starts
                    // the session. Retaps renavigate (singleTop) and refire.
                    LaunchedEffect(liveGen.intValue) {
                        if (liveGen.intValue > 0) {
                            // Single tab since #289: any stored tab key
                            // (including retired god/story/resumes) lands
                            // on Chat.
                            dest = Destination.Chat
                        }
                    }
                    // Widget task tap (#101): land on the Task Manager screen.
                    LaunchedEffect(tasksGen.intValue) {
                        if (tasksGen.intValue > 0) {
                            dest = Destination.TaskManager
                        }
                    }
                    // Cron notification tap (#289): land on the Scheduler screen.
                    LaunchedEffect(schedulerGen.intValue) {
                        if (schedulerGen.intValue > 0) {
                            dest = Destination.Scheduler
                        }
                    }
                    // Task-instruction tap (#287): land on the Chat tab; the
                    // ChatTab consumes the thread id (and auto-sends) once
                    // its ViewModel is ready. Retaps renavigate (singleTop).
                    LaunchedEffect(chatGen.intValue) {
                        if (chatGen.intValue > 0) {
                            dest = Destination.Chat
                        }
                    }
                    // Null section = the settings hub overview; a non-null
                    // section opens that subsection directly.
                    var section by remember { mutableStateOf<SettingSection?>(null) }
                    var settingsOpen by remember { mutableStateOf(false) }
                    val drawerState = rememberDrawerState(DrawerValue.Closed)
                    val scope = rememberCoroutineScope()
                    // Resume tick for cross-screen freshness: a flip in
                    // Settings → Behaviour must re-read on return here.
                    var mainResume by remember { mutableIntStateOf(0) }
                    LifecycleEventEffect(Lifecycle.Event.ON_RESUME) { mainResume++ }
                    // Per-assistant model subtitles for the drawer: refreshed
                    // on every navigation so model picks show up immediately.
                    // One reader for the drawer, so the subtitle honours the
                    // same retired-key fallbacks and clamping as the request.
                    val chatApi = remember { ChatApi(context) }
                    // Built-in TTS: one engine for the Chat tab,
                    // shut down with the activity. Auto-read is a single
                    // global toggle (prefs `tts_auto`) in each chat's bar,
                    // also surfaced in Settings → Behaviour: keyed on the
                    // destination, the settings section AND resume, so a
                    // flip in either direction re-reads on return.
                    val tts = remember { ChatTts(context) }
                    DisposableEffect(Unit) { onDispose { tts.shutdown() } }
                    var autoSpeak by remember(dest, section, mainResume) {
                        mutableStateOf(BehaviourSettings.ttsAuto(context))
                    }
                    // Bumped whenever a model/effort pick saves, so the
                    // drawer subtitles below recompute without waiting for
                    // a navigation (remember(dest, section) alone goes stale).
                    var drawerTick by remember { mutableIntStateOf(0) }
                    val tabModels = remember(dest, section, drawerTick) {
                        // Drawer subtitles: model plus the level that will
                        // actually run, so the active reasoning depth is
                        // visible without reopening the picker. An unset pick
                        // shows the model default — the same level the picker
                        // displays and the request carries (#361).
                        fun sub(tab: String): String {
                            val m = chatApi.modelFor(tab)
                            if (m.isEmpty()) return ""
                            // effortFor reads the same per-model/tab memory as
                            // the request, retired god keys included; an unset
                            // pick shows the level that will actually run.
                            val e = chatApi.effortFor(tab, m)
                                .ifEmpty { EffortCatalog.defaultFor(m) }
                            return "$m · $e"
                        }
                        mapOf(
                            "chat" to sub("chat"),
                        )
                    }
                    fun drawerModel(d: Destination): String? = when (d) {
                        Destination.Chat -> tabModels["chat"]
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
                                Destination.Chat -> ChatTab(
                                    app = application, tab = "chat", title = "Chat", wc = wc,
                                    tts = tts, autoSpeak = autoSpeak,
                                    // Only the requested tab sees the gen:
                                    // per-tab consumed counters would
                                    // otherwise auto-start sessions on
                                    // tabs the user merely switches to.
                                    autoLiveGen = liveGen.intValue,
                                    targetThreadId = chatThreadId.value,
                                    targetAutosend = chatAutosend.value,
                                    autosendGen = chatGen.intValue,
                                    onTargetConsumed = {
                                        chatThreadId.value = null
                                        chatAutosend.value = false
                                    },
                                    onAutoSpeak = {
                                        autoSpeak = it
                                        BehaviourSettings.setTtsAuto(context, it)
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
                                    deepLinkJobId = schedulerTargetId.value,
                                    onDeepLinkConsumed = { schedulerTargetId.value = null },
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
    // Scrollable (#105): 8 destinations plus the settings subsections
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

