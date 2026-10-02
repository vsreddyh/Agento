@file:OptIn(ExperimentalMaterial3Api::class)

package com.vishnu.agento

/**
 * Settings UI: the hub, every section, and the widget diagnostics (#163,
 * third of four moves).
 *
 * Moved verbatim out of MainActivity.kt; the only changes are visibility
 * (`private` to `internal`, same package, so every caller keeps working)
 * and the import list, which carries exactly what this file uses. The two
 * helpers the settings still take from MainActivity (`catalogPath`,
 * `formatSyncTime`) were widened there for the same reason.
 */
import android.Manifest
import android.appwidget.AppWidgetManager
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import android.provider.Settings
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.Send
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.Build
import androidx.compose.material.icons.filled.Cloud
import androidx.compose.material.icons.filled.DarkMode
import androidx.compose.material.icons.filled.Dashboard
import androidx.compose.material.icons.filled.DataUsage
import androidx.compose.material.icons.filled.Favorite
import androidx.compose.material.icons.filled.Folder
import androidx.compose.material.icons.filled.Info
import androidx.compose.material.icons.filled.List
import androidx.compose.material.icons.filled.Menu
import androidx.compose.material.icons.filled.Notifications
import androidx.compose.material.icons.filled.Settings
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
import androidx.compose.ui.unit.dp
import androidx.core.content.ContextCompat
import androidx.core.content.edit
import androidx.core.net.toUri
import androidx.health.connect.client.PermissionController
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.compose.LifecycleEventEffect
import androidx.lifecycle.viewmodel.compose.viewModel
import com.mikepenz.markdown.m3.Markdown
import java.util.Locale
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
internal enum class SettingSection(val title: String) {
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

/** Settings hub (#60): subsections live in the sidebar and each gets its
 * own screen; model pickers live on each tab's own page (#18). Theme toggle
 * lives under Appearance (#28). */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
internal fun SettingsScreen(
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
    var settingsRefresh by remember { mutableIntStateOf(0) }

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
                                prefs.edit {
                                    putString("server_base_url", state.serverUrl.trim().trimEnd('/'))
                                    putString("app_password", state.password.trim())
                                    remove("api_base_url")
                                    remove("server_url")
                                }
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
                                                HealthConnectManager.playStoreUrl().toUri(),
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
internal fun UsageSection() {
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

internal data class UsageRow(
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

internal fun formatTokens(tokens: Long): String {
    return when {
        tokens >= 1_000_000 -> "%.1fM".format(Locale.US, tokens / 1_000_000.0)
        tokens >= 1_000 -> "%.1fk".format(Locale.US, tokens / 1_000.0)
        else -> "$tokens"
    }
}

/** Settings hub overview: every subsection with its live status, opening
 * the matching subscreen on tap (titles match 1:1 per platform convention). */
@Composable
internal fun SettingsHub(
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

internal data class HubRow(val section: SettingSection, val status: String)

internal fun SettingSection.hubIcon() = when (this) {
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
internal fun NotificationsSection() {
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
internal fun SettingsBackupSection(onImported: () -> Unit) {
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
internal fun AppUpdateSection() {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()

    val (installedName, installedCode) = remember {
        UpdateManager.currentVersion(context)
    }
    var status by remember { mutableStateOf("") }
    var failed by remember { mutableStateOf(false) }
    var latest by remember { mutableStateOf<AppRelease?>(null) }
    var busy by remember { mutableStateOf(false) }
    var progress by remember { mutableFloatStateOf(-1f) }

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
internal fun WidgetSettingsSection() {
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
internal fun WidgetDiagnosticsCard() {
    val context = LocalContext.current
    val clipboard = LocalClipboardManager.current
    val scope = rememberCoroutineScope()
    var summary by remember { mutableStateOf("") }
    // Read once per entry: prefs only, so it is cheap and safe on Main.
    var budget by remember { mutableStateOf<TaskReminders.Budget?>(null) }
    var copying by remember { mutableStateOf(false) }
    var copied by remember { mutableStateOf(false) }
    var saving by remember { mutableStateOf(false) }
    var saved by remember { mutableStateOf("") }
    var clearing by remember { mutableStateOf(false) }
    var cleared by remember { mutableStateOf("") }
    // Prefs/AppWidgetManager reads stay off Main (same as the log dump).
    LaunchedEffect(Unit) {
        summary = withContext(Dispatchers.IO) { TaskWidget.diagnostics(context) }
        budget = TaskReminders.budget(context)
    }
    SectionCard(
        title = "Widget diagnostics",
        subtitle = "Task-widget state, the reminder budget, and recent log. If the home-screen widget errors, save the report and upload it — it includes system log, where a host-side widget failure shows up.",
    ) {
        // The one thing in here that changes what the user should do: an
        // exhausted budget means some tasks have no alert armed at all, and
        // nothing else in the app would tell them (#165).
        budget?.let { b ->
            if (b.dropped > 0) {
                Text(
                    "Reminder budget exceeded — ${b.dropped} reminder(s) are not " +
                        "armed, so those tasks will not alert. ${b.armed} of " +
                        "${b.cap} slots used by ${b.tasks} timed task(s).",
                    style = MaterialTheme.typography.bodyMedium,
                    color = MaterialTheme.colorScheme.error,
                )
            }
        }
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
                        // Re-read with it: the banner above is derived from
                        // the same prefs, and a copy is a refresh.
                        budget = TaskReminders.budget(context)
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
internal fun saveDiagnosticsFile(context: Context, text: String): java.io.File {
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
internal fun HealthStatusCard(state: UiState) {
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
