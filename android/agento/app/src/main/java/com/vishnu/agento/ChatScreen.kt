@file:OptIn(ExperimentalMaterial3Api::class)

package com.vishnu.agento

/**
 * Chat UI: the per-tab container, thread switcher, streaming surface and its sheets (#163, last of four moves).
 *
 * Moved verbatim out of MainActivity.kt; the only changes are
 * visibility (`private` to `internal`, same package) and the import
 * list, which carries exactly what this file uses.
 */
import android.Manifest
import android.app.Activity
import android.content.ActivityNotFoundException
import android.content.Context
import android.content.Intent
import android.provider.Settings
import android.speech.RecognizerIntent
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.background
import androidx.compose.foundation.interaction.DragInteraction
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.grid.items
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.itemsIndexed
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.Send
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.ArrowDownward
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.ContentCopy
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.Edit
import androidx.compose.material.icons.filled.Equalizer
import androidx.compose.material.icons.filled.ExpandLess
import androidx.compose.material.icons.filled.ExpandMore
import androidx.compose.material.icons.filled.History
import androidx.compose.material.icons.filled.List
import androidx.compose.material.icons.filled.Menu
import androidx.compose.material.icons.filled.MenuBook
import androidx.compose.material.icons.filled.Mic
import androidx.compose.material.icons.filled.MoreVert
import androidx.compose.material.icons.filled.PlayArrow
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material.icons.filled.Search
import androidx.compose.material.icons.filled.Settings
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
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.lifecycle.ViewModel
import androidx.lifecycle.ViewModelProvider
import androidx.lifecycle.viewmodel.compose.viewModel
import com.mikepenz.markdown.m3.Markdown
import java.util.Locale
import kotlinx.coroutines.flow.collect
import kotlinx.coroutines.launch
/** Builds per-tab chat ViewModels so story/resumes/god keep isolated history. */
class ChatViewModelFactory(
    internal val app: android.app.Application,
    internal val tab: String,
) : ViewModelProvider.Factory {
    @Suppress("UNCHECKED_CAST")
    override fun <T : ViewModel> create(modelClass: Class<T>): T {
        return ChatViewModel(app, tab) as T
    }
}

/** Scopes one chat ViewModel per tab key so drafts/history survive tab switches. */
@Composable
internal fun ChatTab(
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
internal fun ThreadSheet(
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

/** Stable TTS key per message (timestamp + content hash; ms precision
 * alone could theoretically collide across regenerate/retry). */
internal fun ttsKeyFor(msg: ChatMessage): String = "${msg.ts}:${msg.content.hashCode()}"

/**
 * Per-tab provider/model picker (#18: model selection lives on each
 * profile's own page). Saves immediately on pick; blanks mean not set.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
internal fun TabModelSheet(
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
 * The server line reconciles against the server-side conversation total (covers
 * turns served to other devices); the baseline line explains large
 * first-turn prompts (SOUL.md + skills + tools load before turn 1). */
@Composable
internal fun ThreadUsageHeader(usage: ThreadUsage, server: SessionTotals?) {
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
internal fun ChatScreen(
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
internal fun LiveOverlay(
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
internal fun OptionMenu(
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
internal fun catalogPath(api: ChatApi): String =
    listOf(api.pathFor("story"), api.pathFor("resumes"), api.pathFor("god")).distinct().first()

/** Display name for a provider slug. The wire value stays the gateway slug
 * (`opencode-go` per the gateway config); the catalog label is preferred. */
internal fun providerDisplay(slug: String, label: String): String =
    if (label == slug) slug else "$label ($slug)"

/** Dropdown options for a tab: live catalog, else known slugs; the saved
 * value is always kept so legacy/unknown slugs are never lost. */
internal fun providerOptionsFor(catalog: List<ProviderOption>, saved: String): List<ProviderOption> {
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
internal fun modelOptionsFor(
    options: List<ProviderOption>,
    provider: String,
): List<String> =
    if (provider.isBlank()) emptyList()
    else options.firstOrNull { it.slug == provider }?.models.orEmpty()
