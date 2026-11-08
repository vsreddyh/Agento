@file:OptIn(ExperimentalMaterial3Api::class)

package com.vishnu.agento

import android.content.Intent
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.viewModels
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Assignment
import androidx.compose.material.icons.filled.Folder
import androidx.compose.material.icons.filled.List
import androidx.compose.material.icons.filled.Mail
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import androidx.core.content.edit
import java.util.Locale
import kotlinx.coroutines.launch

/** Sidebar destinations for the thin phone client (#493): Tasks/Projects,
 * Task Manager, Files, ∃-Mail and Settings. Chat and the agent-management
 * screens are gone. */
private enum class Destination(val title: String) {
    Tasks("Projects"),
    TaskManager("Task Manager"),
    Storage("Files"),
    Reports("∃-Mail"),
    Settings("Settings"),
}

private fun Destination.icon() = when (this) {
    Destination.Tasks -> Icons.Filled.List
    Destination.TaskManager -> Icons.Filled.Assignment
    Destination.Storage -> Icons.Filled.Folder
    Destination.Reports -> Icons.Filled.Mail
    Destination.Settings -> Icons.Filled.Settings
}

/** Settings subsections; each gets its own hub row + screen (#60, #418).
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

/** Single-activity host; health state lives here. */
class MainActivity : ComponentActivity() {

    private val healthModel: MainViewModel by viewModels()

    /**
     * Task widget tap (#101). Generation counter so retaps
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
     * Update-available notification tap (#366). Generation counter like
     * tasksGen so retaps renavigate; lands on Settings → About (where
     * the App updates section lives).
     */
    private val updateGen = mutableIntStateOf(0)

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        setIntent(intent)
        if (intent.action == TaskWidget.ACTION_TASKS) {
            tasksGen.intValue++
            intent.getStringExtra(TaskWidget.EXTRA_TASK_ID)?.let {
                tasksTargetId.value = it
            }
        }
        if (intent.action == UpdateCheck.ACTION_UPDATE) {
            updateGen.intValue++
        }
    }

    /** Adaptive sidebar navigation (#18, #64); theme from prefs (#28). */
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        // Task widget tap: same redelivery guard — only a fresh launch counts.
        if (savedInstanceState == null && intent?.action == TaskWidget.ACTION_TASKS && tasksGen.intValue == 0) {
            tasksGen.intValue = 1
            intent.getStringExtra(TaskWidget.EXTRA_TASK_ID)?.let {
                tasksTargetId.value = it
            }
        }
        // Update-available notification tap: same redelivery guard.
        if (savedInstanceState == null && intent?.action == UpdateCheck.ACTION_UPDATE && updateGen.intValue == 0) {
            updateGen.intValue = 1
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
                    var dest by remember { mutableStateOf(Destination.Tasks) }
                    // Widget task tap (#101): land on the Task Manager screen.
                    LaunchedEffect(tasksGen.intValue) {
                        if (tasksGen.intValue > 0) {
                            dest = Destination.TaskManager
                        }
                    }
                    // Null section = the settings hub overview; a non-null
                    // section opens that subsection directly.
                    var section by remember { mutableStateOf<SettingSection?>(null) }
                    // Update-available notification tap (#366): land on
                    // Settings → About, where the App updates section lives.
                    // After `section` is declared: the effect assigns it.
                    LaunchedEffect(updateGen.intValue) {
                        if (updateGen.intValue > 0) {
                            dest = Destination.Settings
                            section = SettingSection.About
                        }
                    }
                    val drawerState = rememberDrawerState(DrawerValue.Closed)
                    val scope = rememberCoroutineScope()
                    // ∃-Mail drawer badge (unread count, #365): fetched once
                    // at startup and refreshed by the inbox screen itself on
                    // every load (open, star, delete) via onUnreadChanged —
                    // refresh-on-open, no poll, no push in v1. Null = unknown
                    // (never configured or not yet fetched), so no badge.
                    var reportsUnread by remember { mutableStateOf<Int?>(null) }
                    LaunchedEffect(Unit) {
                        ReportsApi(context).list().fold(
                            onSuccess = { reportsUnread = it.count { r -> !r.read } },
                            onFailure = { },
                        )
                    }
                    fun go(d: Destination, s: SettingSection? = null) {
                        dest = d
                        section = s
                        scope.launch { drawerState.close() }
                    }
                    BoxWithConstraints(modifier = Modifier.fillMaxSize()) {
                        val wc = windowClassFor(maxWidth)
                        val navContent: @Composable () -> Unit = {
                            when (dest) {
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
                                Destination.Reports -> ReportsScreen(
                                    onMenu = { scope.launch { drawerState.open() } },
                                    onUnreadChanged = { reportsUnread = it },
                                )
                                Destination.Settings -> SettingsScreen(
                                    healthModel,
                                    section = section,
                                    wc = wc,
                                    onMenu = { scope.launch { drawerState.open() } },
                                    onPick = { section = it },
                                    // Back arrow on subsection screens (#418):
                                    // without this the arrow renders but does
                                    // nothing, leaving no return path to the
                                    // hub (the drawer has no subsection rows
                                    // anymore).
                                    onBack = { section = null },
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
                                            dest = dest,
                                            reportsUnread = reportsUnread,
                                            onDest = { go(it) },
                                        )
                                    },
                                ) { Box { navContent() } }
                            }
                            WindowClass.Medium -> {
                                Row(modifier = Modifier.fillMaxSize()) {
                                    NavigationRail {
                                        RailContent(
                                            dest = dest,
                                            reportsUnread = reportsUnread,
                                            onDest = { go(it) },
                                        )
                                    }
                                    Box(modifier = Modifier.weight(1f)) { navContent() }
                                }
                            }
                            WindowClass.Expanded -> {
                                PermanentNavigationDrawer(
                                    drawerContent = {
                                        DrawerContent(
                                            dest = dest,
                                            reportsUnread = reportsUnread,
                                            permanent = true,
                                            onDest = { go(it) },
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
    onDest: (Destination) -> Unit,
    permanent: Boolean = false,
    reportsUnread: Int? = null,
) {
    if (permanent) {
        PermanentDrawerSheet(modifier = Modifier.widthIn(max = 280.dp)) {
            DrawerList(dest, onDest, reportsUnread)
        }
    } else {
        ModalDrawerSheet {
            DrawerList(dest, onDest, reportsUnread)
        }
    }
}

@Composable
private fun DrawerList(
    dest: Destination,
    onDest: (Destination) -> Unit,
    reportsUnread: Int? = null,
) {
    // Scrollable (#105): destinations overflow short screens — without
    // this the bottom items are cut off with no way to reach them.
    Column(modifier = Modifier.verticalScroll(rememberScrollState())) {
        Text(
            "Agento",
            style = MaterialTheme.typography.titleLarge,
            modifier = Modifier.padding(16.dp),
        )
        // Settings is a plain destination now (#418): it opens the hub
        // (section = null via go's default) and the hub rows own
        // subsection navigation. No chevron, no subsection rows.
        Destination.entries.forEach { d ->
            NavigationDrawerItem(
                label = {
                    Text(d.title, maxLines = 1)
                },
                // ∃-Mail unread badge (#365): refresh-on-open, no poll.
                // Null/zero = nothing to surface, so no badge.
                badge = {
                    val n = reportsUnread
                    if (d == Destination.Reports && n != null && n > 0) {
                        Badge { Text(if (n > 99) "99+" else "$n") }
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

/** Compact rail for medium windows (icon-only). */
@Composable
private fun RailContent(
    dest: Destination,
    onDest: (Destination) -> Unit,
    reportsUnread: Int? = null,
) {
    // Scrollable like the drawer (#105): the rail overflows the same way
    // on short landscape screens.
    Column(modifier = Modifier.verticalScroll(rememberScrollState())) {
        Destination.entries.forEach { d ->
            // Rail has no badge slot (unlike the drawer item above), so
            // the ∃-Mail unread count wraps the icon instead. Same
            // refresh-on-open rule: null/zero shows no badge.
            val unread = reportsUnread
            NavigationRailItem(
                selected = dest == d,
                onClick = { onDest(d) },
                icon = {
                    if (d == Destination.Reports && unread != null && unread > 0) {
                        BadgedBox(badge = { Badge { Text(if (unread > 99) "99+" else "$unread") } }) {
                            Icon(d.icon(), contentDescription = d.title)
                        }
                    } else {
                        Icon(d.icon(), contentDescription = d.title)
                    }
                },
                label = { Text(d.title) },
            )
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

