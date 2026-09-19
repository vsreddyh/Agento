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
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
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

/** Bottom-nav destinations; first three map 1:1 to gateway profiles. */
private enum class Destination(val title: String) {
    Story("Story"),
    Resumes("Resumes"),
    God("God"),
    Settings("Settings"),
}

/** Single-activity host; health state lives here, chat state per tab. */
class MainActivity : ComponentActivity() {

    private val healthModel: MainViewModel by viewModels()

    /** Inflates bottom nav; each destination hosts an independent tab. */
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContent {
            MaterialTheme {
                var dest by remember { mutableStateOf(Destination.Story) }
                Scaffold(
                    bottomBar = {
                        NavigationBar {
                            Destination.entries.forEach { d ->
                                NavigationBarItem(
                                    selected = dest == d,
                                    onClick = { dest = d },
                                    label = { Text(d.title) },
                                    icon = {},
                                )
                            }
                        }
                    },
                ) { padding ->
                    Box(modifier = Modifier.padding(padding)) {
                        when (dest) {
                            Destination.Story -> ChatTab(app = application, tab = "story", title = "Story")
                            Destination.Resumes -> ChatTab(app = application, tab = "resumes", title = "Resumes")
                            Destination.God -> ChatTab(app = application, tab = "god", title = "God")
                            Destination.Settings -> SettingsScreen(healthModel)
                        }
                    }
                }
            }
        }
    }
}

/** Scopes one chat ViewModel per tab key so drafts/history survive tab switches. */
@Composable
private fun ChatTab(app: android.app.Application, tab: String, title: String) {
    val factory = remember(tab) { ChatViewModelFactory(app, tab) }
    // Keyed per tab — otherwise all three tabs would share one ViewModel.
    val vm: ChatViewModel = viewModel(key = "chat_$tab", factory = factory)
    val state by vm.state
    LaunchedEffect(Unit) { vm.refreshConfig() }
    val subtitle = (if (state.model.isEmpty()) "gateway default" else state.model) +
        " · " + state.provider.id
    ChatScreen(title = title, model = subtitle, state = state,
        onPending = vm::onPending, onSend = vm::send, onStop = vm::stop, onNew = vm::newConversation)
}

/** Streaming chat surface; auto-scrolls on new tokens, delegates I/O to callbacks. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun ChatScreen(
    title: String,
    model: String,
    state: ChatUiState,
    onPending: (String) -> Unit,
    onSend: () -> Unit,
    onStop: () -> Unit,
    onNew: () -> Unit,
) {
    val listState = rememberLazyListState()
    LaunchedEffect(state.messages.size, state.messages.lastOrNull()?.content?.length) {
        if (state.messages.isNotEmpty()) listState.animateScrollToItem(state.messages.size - 1)
    }
    Column(modifier = Modifier.fillMaxSize()) {
        TopAppBar(
            title = { Text("$title · $model") },
            actions = {
                TextButton(onClick = onNew, enabled = !state.streaming) { Text("New") }
            },
        )
        if (state.error.isNotEmpty()) {
            Text(
                state.error,
                color = MaterialTheme.colorScheme.error,
                style = MaterialTheme.typography.bodySmall,
                modifier = Modifier.padding(horizontal = 16.dp, vertical = 4.dp),
            )
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
                        "No messages yet. Ask anything — history stays in this tab until New.",
                        style = MaterialTheme.typography.bodyMedium,
                    )
                }
            }
            items(state.messages) { msg ->
                Card(
                    modifier = Modifier.fillMaxWidth(),
                    colors = if (msg.role == "user") {
                        CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.primaryContainer)
                    } else {
                        CardDefaults.cardColors()
                    },
                ) {
                    Text(
                        msg.content.ifEmpty { "…" },
                        modifier = Modifier.padding(12.dp),
                        style = MaterialTheme.typography.bodyMedium,
                    )
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

/** Per-tab provider/model/path picker; blanks fall back to gateway defaults. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun TabLlmConfig(
    tabTitle: String,
    provider: LlmProvider,
    onProvider: (LlmProvider) -> Unit,
    model: String,
    onModel: (String) -> Unit,
    path: String,
    onPath: (String) -> Unit,
) {
    var expanded by remember { mutableStateOf(false) }
    Text("$tabTitle tab", style = MaterialTheme.typography.titleSmall)
    ExposedDropdownMenuBox(
        expanded = expanded,
        onExpandedChange = { expanded = it },
    ) {
        OutlinedTextField(
            value = provider.id,
            onValueChange = {},
            readOnly = true,
            label = { Text("Provider") },
            trailingIcon = { ExposedDropdownMenuDefaults.TrailingIcon(expanded = expanded) },
            modifier = Modifier.fillMaxWidth().menuAnchor(),
        )
        ExposedDropdownMenu(
            expanded = expanded,
            onDismissRequest = { expanded = false },
        ) {
            LlmProvider.entries.forEach { p ->
                DropdownMenuItem(
                    text = { Text(p.id) },
                    onClick = { onProvider(p); expanded = false },
                )
            }
        }
    }
    OutlinedTextField(
        value = model,
        onValueChange = onModel,
        label = { Text("Model (blank = gateway default)") },
        placeholder = { Text("muse-spark-1.2-free") },
        singleLine = true,
        modifier = Modifier.fillMaxWidth(),
    )
    OutlinedTextField(
        value = path,
        onValueChange = onPath,
        label = { Text("Profile path (blank = default below)") },
        placeholder = {
            Text(if (tabTitle == "God") "/p/default" else "/p/" + tabTitle.lowercase())
        },
        singleLine = true,
        modifier = Modifier.fillMaxWidth(),
    )
}

/** Settings hub for chat backend plus Health Connect sync; prefs load once on entry. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun SettingsScreen(viewModel: MainViewModel) {
    val state by viewModel.state
    val context = LocalContext.current
    val scope = rememberCoroutineScope()

    var apiBase by remember { mutableStateOf("") }
    var apiKey by remember { mutableStateOf("") }
    // Per-tab LLM config: provider + model (+ profile path override).
    var providerStory by remember { mutableStateOf(LlmProvider.OPENCODE) }
    var modelStory by remember { mutableStateOf("") }
    var pathStory by remember { mutableStateOf("") }
    var providerResumes by remember { mutableStateOf(LlmProvider.OPENCODE) }
    var modelResumes by remember { mutableStateOf("") }
    var pathResumes by remember { mutableStateOf("") }
    var providerGod by remember { mutableStateOf(LlmProvider.OPENCODE) }
    var modelGod by remember { mutableStateOf("") }
    var pathGod by remember { mutableStateOf("") }
    var modelsResult by remember { mutableStateOf("") }
    // Bumped after a settings import so the fields below reload from prefs.
    var settingsRefresh by remember { mutableStateOf(0) }

    /** Preloads persisted chat + sync prefs into compose state for editing. */
    LaunchedEffect(settingsRefresh) {
        val prefs = context.getSharedPreferences(AgentoApp.PREFS_NAME, android.content.Context.MODE_PRIVATE)
        // serverUrl/authToken fields below are the health-sync ones (unchanged keys).
        apiBase = prefs.getString("api_base_url", "") ?: ""
        apiKey = prefs.getString("api_key", "") ?: ""
        providerStory = LlmProvider.fromId(prefs.getString("provider_story", "") ?: "")
        modelStory = prefs.getString("model_story", "") ?: ""
        pathStory = prefs.getString("path_story", "") ?: ""
        providerResumes = LlmProvider.fromId(prefs.getString("provider_resumes", "") ?: "")
        modelResumes = prefs.getString("model_resumes", "") ?: ""
        pathResumes = prefs.getString("path_resumes", "") ?: ""
        providerGod = LlmProvider.fromId(prefs.getString("provider_god", "") ?: "")
        modelGod = prefs.getString("model_god", "") ?: ""
        pathGod = prefs.getString("path_god", "") ?: ""
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

        Text("Chat backend", style = MaterialTheme.typography.titleMedium)
        OutlinedTextField(
            value = apiBase,
            onValueChange = { apiBase = it },
            label = { Text("API base URL") },
            placeholder = { Text("http://192.168.1.10:8642") },
            singleLine = true,
            modifier = Modifier.fillMaxWidth(),
        )
        OutlinedTextField(
            value = apiKey,
            onValueChange = { apiKey = it },
            label = { Text("API key (gateway, shared)") },
            singleLine = true,
            modifier = Modifier.fillMaxWidth(),
        )
        TabLlmConfig(
            tabTitle = "Story",
            provider = providerStory,
            onProvider = { providerStory = it },
            model = modelStory,
            onModel = { modelStory = it },
            path = pathStory,
            onPath = { pathStory = it },
        )
        TabLlmConfig(
            tabTitle = "Resumes",
            provider = providerResumes,
            onProvider = { providerResumes = it },
            model = modelResumes,
            onModel = { modelResumes = it },
            path = pathResumes,
            onPath = { pathResumes = it },
        )
        TabLlmConfig(
            tabTitle = "God",
            provider = providerGod,
            onProvider = { providerGod = it },
            model = modelGod,
            onModel = { modelGod = it },
            path = pathGod,
            onPath = { pathGod = it },
        )
        /** Probes each distinct profile path so one gateway serves all tabs. */
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            Button(onClick = {
                val api = ChatApi(context)
                api.setChatConfig(apiBase, apiKey, "story", providerStory, modelStory, pathStory)
                api.setChatConfig(apiBase, apiKey, "resumes", providerResumes, modelResumes, pathResumes)
                api.setChatConfig(apiBase, apiKey, "god", providerGod, modelGod, pathGod)
                modelsResult = "Saved."
            }, modifier = Modifier.weight(1f)) {
                Text("Save chat config")
            }
            OutlinedButton(onClick = {
                val api = ChatApi(context)
                api.setChatConfig(apiBase, apiKey, "story", providerStory, modelStory, pathStory)
                api.setChatConfig(apiBase, apiKey, "resumes", providerResumes, modelResumes, pathResumes)
                api.setChatConfig(apiBase, apiKey, "god", providerGod, modelGod, pathGod)
                modelsResult = "Checking…"
                scope.launch {
                    val paths = listOf(
                        api.pathFor("story"), api.pathFor("resumes"), api.pathFor("god"),
                    ).distinct()
                    val parts = mutableListOf<String>()
                    for (p in paths) {
                        val r = api.listModels(p)
                        parts.add("$p: " + r.fold(
                            onSuccess = { ids -> if (ids.isEmpty()) "(none)" else ids.joinToString() },
                            onFailure = { e -> "FAILED — ${e.message}" },
                        ))
                    }
                    modelsResult = parts.joinToString("\n")
                }
            }, modifier = Modifier.weight(1f)) {
                Text("Check models")
            }
        }
        if (modelsResult.isNotEmpty()) {
            Text(modelsResult, style = MaterialTheme.typography.bodySmall)
        }

        Text("App updates", style = MaterialTheme.typography.titleMedium)
        AppUpdateSection()

        Text("Health sync", style = MaterialTheme.typography.titleMedium)
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

        OutlinedTextField(
            value = state.serverUrl,
            onValueChange = viewModel::onServerUrl,
            label = { Text("Sync server URL") },
            placeholder = { Text("http://192.168.1.10:8001") },
            singleLine = true,
            modifier = Modifier.fillMaxWidth(),
        )
        OutlinedTextField(
            value = state.authToken,
            onValueChange = viewModel::onAuthToken,
            label = { Text("Sync auth token") },
            singleLine = true,
            modifier = Modifier.fillMaxWidth(),
        )
        Button(onClick = viewModel::saveConfig, modifier = Modifier.fillMaxWidth()) {
            Text("Save sync config")
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

        Text("Settings backup", style = MaterialTheme.typography.titleMedium)
        SettingsBackupSection(onImported = {
            viewModel.refresh()
            settingsRefresh++
        })
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

    Text(
        "Upgrades and reinstalls keep settings automatically; export a copy " +
            "to survive a reinstall after a long gap.",
        style = MaterialTheme.typography.bodySmall,
    )
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
