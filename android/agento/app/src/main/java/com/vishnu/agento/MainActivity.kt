@file:OptIn(ExperimentalMaterial3Api::class)

package com.vishnu.agento

import android.content.Intent
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.viewModels
import androidx.compose.foundation.clickable
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.AccountBalanceWallet
import androidx.compose.material.icons.filled.Assignment
import androidx.compose.material.icons.filled.Folder
import androidx.compose.material.icons.filled.List
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import androidx.core.content.edit
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.compose.LifecycleEventEffect
import java.util.Locale
import kotlinx.coroutines.launch

/** Sidebar destinations for the thin phone client (#493, #571): Tasks/Projects,
 * Task Manager, Files, Money and Settings. Chat and the agent-management
 * screens are gone. */
// #584 TEMP-REMOVE (∃-Mail): no mail entry until the feature returns.
private enum class Destination(val title: String) {
    Tasks("Projects"),
    TaskManager("Task Manager"),
    Storage("Files"),
    // Reports("∃-Mail"),
    Money("Money"),
    Settings("Settings"),
}

private fun Destination.icon() = when (this) {
    Destination.Tasks -> Icons.Filled.List
    Destination.TaskManager -> Icons.Filled.Assignment
    Destination.Storage -> Icons.Filled.Folder
    // #584 TEMP-REMOVE (∃-Mail): Destination.Reports -> Icons.Filled.Mail
    Destination.Money -> Icons.Filled.AccountBalanceWallet
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
                    // #584 TEMP-REMOVE (∃-Mail): drawer badge + startup fetch.
                    // ∃-Mail drawer badge (unread count, #365): fetched once
                    // at startup and refreshed by the inbox screen itself on
                    // every load (open, star, delete) via onUnreadChanged —
                    // refresh-on-open, no poll, no push in v1. Null = unknown
                    // (never configured or not yet fetched), so no badge.
                    // var reportsUnread by remember { mutableStateOf<Int?>(null) }
                    // LaunchedEffect(Unit) {
                    //     ReportsApi(context).list().fold(
                    //         onSuccess = { reportsUnread = it.count { r -> !r.read } },
                    //         onFailure = { },
                    //     )
                    // }
                    fun go(d: Destination, s: SettingSection? = null) {
                        dest = d
                        section = s
                        scope.launch { drawerState.close() }
                    }
                    // Sidebar update check (#591): the persisted pending
                    // release, refreshed on resume and after every manual
                    // check — the launch-time worker already ran through
                    // UpdateCheck.run, so the sidebar reflects it instead of
                    // triggering its own.
                    var updateLabel by remember {
                        mutableStateOf(
                            UpdateCheck.pendingRelease(context)?.let(UpdateCheck::pendingUpdateLabel),
                        )
                    }
                    var updateChecking by remember { mutableStateOf(false) }
                    fun refreshUpdateLabel() {
                        updateLabel = UpdateCheck.pendingRelease(context)
                            ?.let(UpdateCheck::pendingUpdateLabel)
                    }
                    LifecycleEventEffect(Lifecycle.Event.ON_RESUME) {
                        refreshUpdateLabel()
                    }
                    // No one-shot worker-completion hook: WorkManager reports
                    // completion through ListenableFuture, which is not on
                    // this module's compile classpath, so first paint can
                    // show the previous launch's snapshot until the next
                    // resume or manual check converges it. Best-effort by
                    // design — the check itself still runs on launch.
                    // Update-check failure toasts (#591): a silent stop
                    // reads as either "up to date" or a dead button.
                    val toasts = remember(scope) { ToastCenter(scope) }
                    // Manual tap while a check is in flight is a no-op (no
                    // parallel checks).
                    fun checkUpdatesNow() {
                        if (updateChecking) return
                        updateChecking = true
                        scope.launch {
                            try {
                                val result = UpdateCheck.run(context, notify = false)
                                result.onFailure { e ->
                                    val fe = friendlyError(e.message ?: "")
                                    toasts.toast(
                                        "${fe.title} — ${fe.nextStep}",
                                        ToastKind.Error,
                                    )
                                }
                                // Refresh from the snapshot on every completed
                                // run: a clean fetch clears a stale pending
                                // line in-session instead of waiting for the
                                // next resume, and a failure keeps the old
                                // label (the release is still pending — only
                                // the re-check failed).
                                if (result.isSuccess) {
                                    updateLabel = UpdateCheck.pendingRelease(context)
                                        ?.let(UpdateCheck::pendingUpdateLabel)
                                }
                                // Navigate only on a fresh find: a failed run
                                // must not open About on a stale pending.
                                if (result.isSuccess &&
                                    UpdateCheck.pendingRelease(context) != null
                                ) {
                                    go(Destination.Settings, SettingSection.About)
                                }
                            } finally {
                                updateChecking = false
                            }
                        }
                    }
                    fun openUpdates() = go(Destination.Settings, SettingSection.About)
                    Box(modifier = Modifier.fillMaxSize()) {
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
                                // #584 TEMP-REMOVE (∃-Mail).
                                // Destination.Reports -> ReportsScreen(
                                //     onMenu = { scope.launch { drawerState.open() } },
                                //     onUnreadChanged = { reportsUnread = it },
                                // )
                                Destination.Money -> MoneyScreen(
                                    onMenu = { scope.launch { drawerState.open() } },
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
                                            // #584 TEMP-REMOVE (∃-Mail).
                                            // reportsUnread = reportsUnread,
                                            updateLabel = updateLabel,
                                            updateChecking = updateChecking,
                                            onCheckUpdates = ::checkUpdatesNow,
                                            onOpenUpdates = ::openUpdates,
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
                                            // #584 TEMP-REMOVE (∃-Mail).
                                            // reportsUnread = reportsUnread,
                                            onDest = { go(it) },
                                            modifier = Modifier.weight(1f),
                                        )
                                        RailUpdateButton(
                                            updateLabel = updateLabel,
                                            checking = updateChecking,
                                            onCheck = ::checkUpdatesNow,
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
                                            // #584 TEMP-REMOVE (∃-Mail).
                                            // reportsUnread = reportsUnread,
                                            updateLabel = updateLabel,
                                            updateChecking = updateChecking,
                                            onCheckUpdates = ::checkUpdatesNow,
                                            onOpenUpdates = ::openUpdates,
                                            permanent = true,
                                            onDest = { go(it) },
                                        )
                                    },
                                ) { Box { navContent() } }
                            }
                        }
                        ToastHost(center = toasts, modifier = Modifier.fillMaxWidth())
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
    // #584 TEMP-REMOVE (∃-Mail).
    // reportsUnread: Int? = null,
    updateLabel: String?,
    updateChecking: Boolean,
    onCheckUpdates: () -> Unit,
    onOpenUpdates: () -> Unit,
) {
    @Composable
    fun content() {
        Column(modifier = Modifier.fillMaxHeight()) {
            DrawerList(dest, onDest, modifier = Modifier.weight(1f))
            UpdateCheckFooter(
                updateLabel = updateLabel,
                checking = updateChecking,
                onCheck = onCheckUpdates,
                onOpen = onOpenUpdates,
            )
        }
    }
    if (permanent) {
        PermanentDrawerSheet(modifier = Modifier.widthIn(max = 280.dp)) {
            content()
        }
    } else {
        ModalDrawerSheet {
            content()
        }
    }
}

@Composable
private fun DrawerList(
    dest: Destination,
    onDest: (Destination) -> Unit,
    // #584 TEMP-REMOVE (∃-Mail).
    // reportsUnread: Int? = null,
    modifier: Modifier = Modifier,
) {
    // Scrollable (#105): destinations overflow short screens — without
    // this the bottom items are cut off with no way to reach them.
    Column(modifier = modifier.verticalScroll(rememberScrollState())) {
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
                // #584 TEMP-REMOVE (∃-Mail unread badge, #365).
                // badge = {
                //     val n = reportsUnread
                //     if (d == Destination.Reports && n != null && n > 0) {
                //         Badge { Text(if (n > 99) "99+" else "$n") }
                //     }
                // },
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
    // #584 TEMP-REMOVE (∃-Mail).
    // reportsUnread: Int? = null,
    modifier: Modifier = Modifier,
) {
    // Scrollable like the drawer (#105): the rail overflows the same way
    // on short landscape screens.
    Column(modifier = modifier.verticalScroll(rememberScrollState())) {
        Destination.entries.forEach { d ->
            NavigationRailItem(
                selected = dest == d,
                onClick = { onDest(d) },
                icon = {
                    Icon(d.icon(), contentDescription = d.title)
                    // #584 TEMP-REMOVE (∃-Mail unread badge, #365): wrapped
                    // the icon in a BadgedBox while reports existed.
                    // val unread = reportsUnread
                    // if (d == Destination.Reports && unread != null && unread > 0) {
                    //     BadgedBox(badge = { Badge { Text(if (unread > 99) "99+" else "$unread") } }) {
                    //         Icon(d.icon(), contentDescription = d.title)
                    //     }
                    // } else {
                    //     Icon(d.icon(), contentDescription = d.title)
                    // }
                },
                label = { Text(d.title) },
            )
        }
    }
}

/** Sidebar bottom: a manual update check plus the pending-release line
 * (#591). The line opens Settings → About like the notification tap; the
 * button re-checks (and opens About when the check finds something). The
 * rail variant is icon-only with a dot while a release pends. */
@Composable
private fun UpdateCheckFooter(
    updateLabel: String?,
    checking: Boolean,
    onCheck: () -> Unit,
    onOpen: () -> Unit,
) {
    Column(modifier = Modifier.fillMaxWidth().padding(vertical = 4.dp)) {
        HorizontalDivider()
        Row(verticalAlignment = Alignment.CenterVertically) {
            IconButton(onClick = onCheck) {
                SyncButtonIcon(
                    spinning = checking,
                    idleDescription = "Check for updates",
                    spinningDescription = "Checking for updates",
                )
            }
            Text(
                "Check for updates",
                style = MaterialTheme.typography.bodyMedium,
                modifier = Modifier.weight(1f)
                    .clickable(onClick = onCheck),
            )
        }
        if (updateLabel != null) {
            Text(
                "Update available :- $updateLabel",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.primary,
                modifier = Modifier
                    .fillMaxWidth()
                    .clickable(onClick = onOpen)
                    .padding(horizontal = 16.dp, vertical = 4.dp),
            )
        }
    }
}

/** Rail twin of the footer: icon-only, with a dot while a release pends. */
@Composable
private fun RailUpdateButton(
    updateLabel: String?,
    checking: Boolean,
    onCheck: () -> Unit,
) {
    BadgedBox(badge = { if (updateLabel != null) Badge() }) {
        IconButton(onClick = onCheck) {
            SyncButtonIcon(
                spinning = checking,
                idleDescription = "Check for updates",
                spinningDescription = "Checking for updates",
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

