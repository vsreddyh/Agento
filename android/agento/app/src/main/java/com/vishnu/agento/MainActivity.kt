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
        " · " + state.provider.ifEmpty { "gateway default" }
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

/** Per-tab provider/model/path picker; blanks fall back to gateway defaults.
 * Provider/model options come from the live gateway catalog
 * (`GET /api/model/options`); the saved value is always kept selectable so a
 * legacy or unknown slug is never lost, and blank always means default. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun TabLlmConfig(
    tabTitle: String,
    provider: String,
    onProvider: (String) -> Unit,
    providerOptions: List<ProviderOption>,
    model: String,
    onModel: (String) -> Unit,
    modelOptions: List<String>,
    path: String,
    onPath: (String) -> Unit,
) {
    /** Read-only option menu (saved values stay intact; picks write the slug). */
    @Composable
    fun OptionMenu(
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

    Text("$tabTitle tab", style = MaterialTheme.typography.titleSmall)
    val shownProvider = providerOptions.firstOrNull { it.slug == provider }
        ?.let { if (it.label == it.slug) it.slug else "${it.label} (${it.slug})" }
        ?: provider.ifEmpty { "(gateway default)" }
    OptionMenu(
        label = "Provider",
        shown = shownProvider,
        options = listOf("" to "(gateway default)") +
            providerOptions.map { o ->
                o.slug to (if (o.label == o.slug) o.slug else "${o.label} (${o.slug})")
            },
        onPick = onProvider,
    )
    OptionMenu(
        label = "Model (blank = gateway default)",
        shown = model.ifEmpty { "(gateway default)" },
        options = modelOptions.map { m -> m to m.ifEmpty { "(gateway default)" } },
        onPick = onModel,
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

/** Providers are gateway-global; one catalog fetch covers all tabs. */
private fun catalogPath(api: ChatApi): String =
    listOf(api.pathFor("story"), api.pathFor("resumes"), api.pathFor("god")).distinct().first()

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

/** Model options for a tab: blank (gateway default) + the selected
 * provider's catalog models + the saved value. */
private fun modelOptionsFor(
    options: List<ProviderOption>,
    provider: String,
    saved: String,
): List<String> = buildList {
    add("")
    addAll(options.firstOrNull { it.slug == provider }?.models.orEmpty())
    if (saved.isNotBlank() && saved !in this) add(saved)
}

/** Settings hub for chat backend plus Health Connect sync; prefs load once on entry. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun SettingsScreen(viewModel: MainViewModel) {
    val state by viewModel.state
    val context = LocalContext.current
    val scope = rememberCoroutineScope()

    // Per-tab LLM config: provider slug + model (blank = gateway default).
    var providerStory by remember { mutableStateOf("") }
    var modelStory by remember { mutableStateOf("") }
    var pathStory by remember { mutableStateOf("") }
    var providerResumes by remember { mutableStateOf("") }
    var modelResumes by remember { mutableStateOf("") }
    var pathResumes by remember { mutableStateOf("") }
    var providerGod by remember { mutableStateOf("") }
    var modelGod by remember { mutableStateOf("") }
    var pathGod by remember { mutableStateOf("") }
    var modelsResult by remember { mutableStateOf("") }
    // Live picker inventory (providers + their models); empty until loaded.
    var catalog by remember { mutableStateOf<List<ProviderOption>>(emptyList()) }
    // Bumped after a settings import so the fields below reload from prefs.
    var settingsRefresh by remember { mutableStateOf(0) }

    /** Preloads persisted chat prefs into compose state, then pulls the live
     * provider/model catalog for the dropdowns (silent on failure — the
     * offline fallback list + saved values keep the pickers usable). */
    LaunchedEffect(settingsRefresh) {
        val prefs = context.getSharedPreferences(AgentoApp.PREFS_NAME, android.content.Context.MODE_PRIVATE)
        // serverUrl/password live in the viewModel (single shared fields,
        // legacy keys fall back inside MainViewModel.refresh on init).
        providerStory = (prefs.getString("provider_story", "") ?: "").trim()
        modelStory = (prefs.getString("model_story", "") ?: "").trim()
        pathStory = (prefs.getString("path_story", "") ?: "").trim()
        providerResumes = (prefs.getString("provider_resumes", "") ?: "").trim()
        modelResumes = (prefs.getString("model_resumes", "") ?: "").trim()
        pathResumes = (prefs.getString("path_resumes", "") ?: "").trim()
        providerGod = (prefs.getString("provider_god", "") ?: "").trim()
        modelGod = (prefs.getString("model_god", "") ?: "").trim()
        pathGod = (prefs.getString("path_god", "") ?: "").trim()
        val api = ChatApi(context)
        catalog = runCatching {
            api.fetchCatalog(catalogPath(api)).getOrThrow()
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

        Text("Server", style = MaterialTheme.typography.titleMedium)
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
        Text("Chat backend", style = MaterialTheme.typography.titleMedium)
        val storyProviders = providerOptionsFor(catalog, providerStory)
        val resumesProviders = providerOptionsFor(catalog, providerResumes)
        val godProviders = providerOptionsFor(catalog, providerGod)
        TabLlmConfig(
            tabTitle = "Story",
            provider = providerStory,
            onProvider = { providerStory = it },
            providerOptions = storyProviders,
            model = modelStory,
            onModel = { modelStory = it },
            modelOptions = modelOptionsFor(storyProviders, providerStory, modelStory),
            path = pathStory,
            onPath = { pathStory = it },
        )
        TabLlmConfig(
            tabTitle = "Resumes",
            provider = providerResumes,
            onProvider = { providerResumes = it },
            providerOptions = resumesProviders,
            model = modelResumes,
            onModel = { modelResumes = it },
            modelOptions = modelOptionsFor(resumesProviders, providerResumes, modelResumes),
            path = pathResumes,
            onPath = { pathResumes = it },
        )
        TabLlmConfig(
            tabTitle = "God",
            provider = providerGod,
            onProvider = { providerGod = it },
            providerOptions = godProviders,
            model = modelGod,
            onModel = { modelGod = it },
            modelOptions = modelOptionsFor(godProviders, providerGod, modelGod),
            path = pathGod,
            onPath = { pathGod = it },
        )
        /** Saves config, then reloads the live provider/model catalog. */
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            Button(onClick = {
                val api = ChatApi(context)
                api.setChatConfig(state.serverUrl, state.password, "story", providerStory, modelStory, pathStory)
                api.setChatConfig(state.serverUrl, state.password, "resumes", providerResumes, modelResumes, pathResumes)
                api.setChatConfig(state.serverUrl, state.password, "god", providerGod, modelGod, pathGod)
                modelsResult = "Saved."
            }, modifier = Modifier.weight(1f)) {
                Text("Save chat config")
            }
            OutlinedButton(onClick = {
                val api = ChatApi(context)
                api.setChatConfig(state.serverUrl, state.password, "story", providerStory, modelStory, pathStory)
                api.setChatConfig(state.serverUrl, state.password, "resumes", providerResumes, modelResumes, pathResumes)
                api.setChatConfig(state.serverUrl, state.password, "god", providerGod, modelGod, pathGod)
                modelsResult = "Loading providers…"
                scope.launch {
                    api.fetchCatalog(catalogPath(api), refresh = true).fold(
                        onSuccess = { list ->
                            catalog = list
                            modelsResult = list.joinToString("\n") { o ->
                                "${o.label}: ${o.models.size} model(s)"
                            }
                        },
                        onFailure = { e -> modelsResult = "FAILED — ${e.message}" },
                    )
                }
            }, modifier = Modifier.weight(1f)) {
                Text("Reload catalog")
            }
        }
        if (catalog.isEmpty()) {
            Text(
                "Providers not loaded — check Server URL + Password, then Reload catalog.",
                style = MaterialTheme.typography.bodySmall,
            )
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
