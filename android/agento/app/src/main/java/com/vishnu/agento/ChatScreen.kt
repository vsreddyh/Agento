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
import android.content.ContextWrapper
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.provider.Settings
import android.speech.RecognizerIntent
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.Image
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
import androidx.compose.material.icons.filled.Approval
import androidx.compose.material.icons.filled.ArrowDownward
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.ContentCopy
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.Edit
import androidx.compose.material.icons.filled.Equalizer
import androidx.compose.material.icons.filled.ExpandLess
import androidx.compose.material.icons.filled.ExpandMore
import androidx.compose.material.icons.filled.History
import androidx.compose.material.icons.filled.Image
import androidx.compose.material.icons.filled.List
import androidx.compose.material.icons.filled.Menu
import androidx.compose.material.icons.filled.MenuBook
import androidx.compose.material.icons.filled.Mic
import androidx.compose.material.icons.filled.MoreVert
import androidx.compose.material.icons.filled.PlayArrow
import androidx.compose.material.icons.filled.Psychology
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
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.semantics.LiveRegionMode
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.liveRegion
import androidx.compose.ui.semantics.role
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.semantics.stateDescription
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.core.content.ContextCompat
import androidx.core.content.FileProvider
import androidx.lifecycle.ViewModel
import androidx.lifecycle.ViewModelProvider
import androidx.lifecycle.viewmodel.compose.viewModel
import com.mikepenz.markdown.m3.Markdown
import java.io.File
import java.util.Locale
import java.util.concurrent.atomic.AtomicInteger
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.NonCancellable
import kotlinx.coroutines.flow.collect
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

/** Pending count as both the badge and the banner show it: one cap, one
 * wording, so the two can never disagree above 99. */
private fun pendingCountLabel(count: Int): String = if (count > 99) "99+" else "$count"

/** Unwraps a possibly-wrapped Context to its Activity, if any (#339). */
private tailrec fun Context.findActivity(): Activity? = when (this) {
    is Activity -> this
    is ContextWrapper -> baseContext.findActivity()
    else -> null
}

/**
 * One staged or sent photo (#318): the remembered ~256px thumbnail, or an
 * icon placeholder when the payload won't decode (a corrupt payload
 * degrades to a missing thumbnail, never a crash).
 */
@Composable
private fun AttachmentThumb(
    base64: String,
    label: String,
    modifier: Modifier = Modifier,
    slot: Int = 0,
) {
    // Keyed (length, hash, slot), not the string (#339): remember's
    // equality check would memcmp ~1.5 M chars on every recomposition
    // (hashCode walks once — the JVM caches it), and the slot keeps two
    // identical payloads from aliasing one thumbnail. Decoded off-main:
    // ~1 MB Base64.decode + decodeByteArray during composition janks
    // low-end scroll, so produceState moves it to IO (placeholder shows
    // until it lands) and awaitDispose recycles on change/unmount.
    val thumbKey = Triple(base64.length, base64.hashCode(), slot)
    // No manual recycle (#339): awaitDispose { recycle() } can pull pixels
    // from under a late outgoing draw ("trying to use a recycled bitmap").
    // ~256px thumbnails are GC-cheap via the native allocation registry —
    // prompt freeing is not worth a crash class.
    val produced by produceState<android.graphics.Bitmap?>(null, thumbKey) {
        value = withContext(Dispatchers.IO) {
            runCatching { decodeThumbnail(base64) }.getOrNull()
        }
    }
    // Local, not the delegate (#339): smart cast is impossible on a
    // delegated property, so snapshot it once per composition.
    val bmp = produced
    if (bmp != null) {
        Image(
            bitmap = bmp.asImageBitmap(),
            contentDescription = label,
            modifier = modifier.clip(RoundedCornerShape(8.dp)),
            contentScale = ContentScale.Crop,
        )
    } else {
        Box(
            modifier = modifier
                .clip(RoundedCornerShape(8.dp))
                .background(MaterialTheme.colorScheme.surfaceVariant),
            contentAlignment = Alignment.Center,
        ) {
            Icon(Icons.Filled.Image, contentDescription = label)
        }
    }
}

/** Builds the Chat ViewModel so the single tab keeps isolated history. */
class ChatViewModelFactory(
    internal val app: android.app.Application,
    internal val tab: String,
) : ViewModelProvider.Factory {
    @Suppress("UNCHECKED_CAST")
    override fun <T : ViewModel> create(modelClass: Class<T>): T {
        return ChatViewModel(app, tab) as T
    }
}

/** Scopes the chat ViewModel to the tab key so drafts/history survive recomposition. */
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
    targetThreadId: String? = null,
    targetAutosend: Boolean = false,
    autosendGen: Int = 0,
    onTargetConsumed: () -> Unit = {},
) {
    val factory = remember(tab) { ChatViewModelFactory(app, tab) }
    // Keyed by tab — the ViewModel survives recomposition.
    val vm: ChatViewModel = viewModel(key = "chat_$tab", factory = factory)
    val state by vm.state
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    LaunchedEffect(Unit) { vm.refreshConfig() }
    // Task-instruction deep-link (#287): open the instructed thread once
    // the ViewModel has loaded. Consumed on confirmed open (not
    // optimistically): an early clear plus an auto-cancelled card would
    // orphan the thread as an unsent draft on a ready-race miss.
    LaunchedEffect(targetThreadId, autosendGen, state.ready) {
        if (targetThreadId != null && state.ready) {
            vm.openThread(targetThreadId, targetAutosend, onTargetConsumed)
        }
    }
    var showModel by remember { mutableStateOf(false) }
    var showThreads by remember { mutableStateOf(false) }
    // Thread id awaiting skill naming (#314): the dialog collects a name +
    // description, then the transcript becomes a draft for review in Skills.
    var savingSkillId by remember { mutableStateOf<String?>(null) }
    val toasts = remember(scope) { ToastCenter(scope) }
    // Photo attachments (#318): gallery picks need no permission (the picker
    // returns read access to just the chosen images); the camera needs
    // CAMERA plus a FileProvider Uri staged before TakePicture launches.
    var cameraUri by rememberSaveable { mutableStateOf<Uri?>(null) }
    // File path, not the File (#339 review): rotation while the camera app
    // is foreground destroys this composition, and only saveable state
    // survives — a lost Uri/File silently drops the capture (TakePicture
    // returns with nothing to decode). Uri is Parcelable; the path is a
    // String; both restore.
    var cameraPath by rememberSaveable { mutableStateOf<String?>(null) }
    // True while a camera batch is decoding (#339): launchCamera's
    // delete-previous must skip a file with an in-flight decode, or a
    // rapid re-tap deletes it mid-read and turns a good capture into a
    // "moved or deleted" error. Plain remember is right — a rotation
    // during decode cancels the batch anyway, so no restore is needed.
    var cameraBusy by remember { mutableStateOf(false) }
    // Startup sweep (#339): a process kill between the TakePicture write
    // and the decode leaves a capture with no owner — per-batch finally
    // covers cancel/leave-tab, not death. The cache dir holds ONLY these
    // captures, so clearing it here bounds the litter to zero with no
    // bookkeeping. Once per composition-creation, not per recomposition —
    // and sparing the restored capture: rotation/death while the camera
    // app is foreground restores cameraPath, and sweeping its file would
    // turn the returning capture into a "moved or deleted" error.
    LaunchedEffect(Unit) {
        runCatching {
            val keep = cameraPath
            context.cacheDir.resolve("camera").takeIf { it.exists() }?.listFiles()
                ?.filter { it.absolutePath != keep }
                ?.forEach { runCatching { it.delete() } }
        }
    }
    // One decode at a time, in pick order: each photo lands (or its named
    // error toasts) as it finishes, so a slow second image never blocks the
    // first one's thumbnail. Failures name the fix (too large, unreadable)
    // rather than dropping the pick silently. [cleanup] is the camera's
    // temp file for a capture batch (deleted once decoded; gallery Uris
    // are not ours, so gallery batches pass none) — passed explicitly so
    // a concurrent gallery batch can never delete a capture still decoding.
    // [onBatchDone] fires on Main after the batch (camera uses it to drop
    // its in-flight flag).
    fun processUris(uris: List<Uri>, cleanup: File? = null, onBatchDone: () -> Unit = {}) {        // Done fires on early returns too (#339): the camera sets its
        // in-flight flag before calling, so a cap refusal without the
        // callback would stick the flag and leak the capture forever.
        // The capture itself is deleted here too — the finally below only
        // runs when the batch body starts.
        if (uris.isEmpty()) {
            runCatching { cleanup?.takeIf { it.exists() }?.delete() }
            onBatchDone()
            return
        }
        // Unified on vm.notify (#339): the same cap refused here and
        // mid-batch below must have one lifetime — a composition toast is
        // lost on rotation while a state notice survives it. Count read
        // live from the ViewModel, not the last recomposition (#339).
        val slots = MAX_IMAGES_PER_TURN - vm.pendingAttachmentCount()
        if (slots <= 0) {
            // One cap string + Info, shared with the backstop (#339).
            vm.notify(capRefused(), ToastKind.Info)
            // Same leak as above: refusal skips the body, so the capture
            // is deleted here, not in finally.
            runCatching { cleanup?.takeIf { it.exists() }?.delete() }
            onBatchDone()
            return
        }
        val take = uris.take(slots)
        if (take.size < uris.size) {
            vm.notify(capRefused(), ToastKind.Info)
        }
        // Hoisted ABOVE the launch (#339): the resolver is all the batch
        // needs, and declaring it inside would still capture the Activity
        // in the closure until that line runs — narrowing the leak, not
        // removing it.
        val appContext = context.applicationContext
        scope.launch(Dispatchers.IO) {
            try {
                for (u in take) {
                    // Re-check room per pick (#339): two rapid batches both
                    // snapshot free slots at tap time, so without this both
                    // decode fully and the loser drops AFTER the expensive
                    // part. addPendingAttachments stays as backstop.
                    val room = withContext(Dispatchers.Main) { vm.hasAttachmentRoom() }
                    if (!room) {
                        withContext(Dispatchers.Main) {
                            vm.notify(capRefused(), ToastKind.Info)
                        }
                        break
                    }
                    // Application context, hoisted above the launch (#339):
                    // the closure never sees the Activity, so no rotation
                    // can leak it for the batch. The resolver is all this
                    // needs.
                    val result = prepareAttachment(appContext, u)
                    withContext(Dispatchers.Main) {
                        // State notices, not composition toasts (#339): a
                        // decode failure must survive rotation like the cap
                        // refusals do — same lifetime for every attach error.
                        result
                            .onSuccess { vm.addPendingAttachments(listOf(it)) }
                            .onFailure { vm.notify(it.message ?: "Couldn't attach that photo.") }
                    }
                }
            } finally {
                // In finally, not after the loop (#339 review): leaving the
                // tab mid-decode cancels this coroutine, and a trailing
                // delete would be skipped — littering one capture per
                // cancel. File IO is fine here; no state is touched. The
                // completion callback runs on Main for the same reason —
                // NonCancellable so a cancelled batch still drops the flag
                // (otherwise every later launch skips its cleanup forever).
                runCatching { cleanup?.takeIf { it.exists() }?.delete() }
                withContext(Dispatchers.Main + NonCancellable) { onBatchDone() }
            }
        }
    }
    val galleryLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.GetMultipleContents(),
    ) { uris -> processUris(uris) }
    val cameraLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.TakePicture(),
    ) { ok ->
        val u = cameraUri
        val f = cameraPath?.let(::File)
        // Uri nulls now (single-delivery, never reused); the PATH stays
        // until the batch completes (#339) so a rotation mid-decode
        // restores a non-null keep for the startup sweep — nulling it here
        // would let the sweep delete the file being read.
        cameraUri = null
        if (ok && u != null) {
            cameraBusy = true
            processUris(listOf(u), f) {
                cameraBusy = false
                cameraPath = null
            }
        } else {
            runCatching { f?.takeIf { it.exists() }?.delete() }
            cameraPath = null
        }
    }
    fun launchCamera() {
        val dir = File(context.cacheDir, "camera")
        // Checked, not assumed (#339): full storage fails mkdirs, and a
        // bare TakePicture failure (ok=false) would leave the user with
        // no hint why no camera appeared. State notice, not a composition
        // toast: same rotation-proof lifetime as every other attach error.
        if (!dir.exists() && !dir.mkdirs()) {
            vm.notify("Couldn't start the camera — storage unavailable.")
            return
        }
        // Skip the delete while a capture is decoding (#339): a rapid
        // re-tap would delete the in-flight file mid-read and turn a good
        // capture into a "moved or deleted" error. The in-flight batch's
        // own cleanup deletes its file when it lands.
        if (!cameraBusy) {
            runCatching { cameraPath?.let(::File)?.takeIf { it.exists() }?.delete() }
        }
        // UUID, not millis (#339): a rapid double-tap can mint the same
        // millisecond twice and point two captures at one file.
        val file = File(dir, "capture-${java.util.UUID.randomUUID()}.jpg")
        val uri = runCatching {
            // packageName, not APPLICATION_ID (#339): BuildConfig is not
            // generated in this module, and the two are identical while
            // there are no flavors or applicationIdSuffixes (verified in
            // build.gradle.kts) — the FileProvider authority in the
            // manifest is ${applicationId}, same value. If a flavor or a
            // .debug suffix ever lands, this call is the one that breaks.
            FileProvider.getUriForFile(context, "${context.packageName}.fileprovider", file)
        }.getOrNull() ?: run {
            vm.notify("Couldn't start the camera.")
            return
        }
        cameraUri = uri
        cameraPath = file.absolutePath
        cameraLauncher.launch(uri)
    }
    val cameraPermission = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestPermission(),
    ) { granted ->
        if (granted) {
            launchCamera()
        } else if (context.findActivity()?.let {
                androidx.core.app.ActivityCompat.shouldShowRequestPermissionRationale(
                    it, Manifest.permission.CAMERA,
                )
            } == false
        ) {
            // Permanent denial ("Don't allow again"): the request UI will
            // never show again, so a re-toast loops forever (#339). The
            // only path forward is app Settings — opened directly since
            // the user tapped Take photo and there is nowhere else to go.
            vm.notify("Camera access is off — opening Settings to re-enable.", ToastKind.Info)
            runCatching {
                context.startActivity(
                    Intent(
                        Settings.ACTION_APPLICATION_DETAILS_SETTINGS,
                        Uri.fromParts("package", context.packageName, null),
                    ).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK),
                )
            }
        } else {
            // State notice like every other attach refusal (#339).
            vm.notify("Camera permission is needed to take photos.")
        }
    }
    fun requestCamera() {
        if (ContextCompat.checkSelfPermission(context, Manifest.permission.CAMERA) ==
            PackageManager.PERMISSION_GRANTED
        ) {
            launchCamera()
        } else {
            cameraPermission.launch(Manifest.permission.CAMERA)
        }
    }
    // ViewModel notices (attach caps) surface as toasts, not the error
    // banner: the banner's Retry resends a TURN, which is wrong for a
    // staging problem. Consumed after showing so rotation never replays it.
    // Keyed on the generation too (#339): an identical repeat notice must
    // re-fire rather than dedupe into a dead second tap. Kind rides the
    // state so info confirmations and error refusals share one plumbing
    // with the same rotation-proof lifetime.
    LaunchedEffect(state.notice, state.noticeGen) {
        if (state.notice.isNotEmpty()) {
            toasts.toast(state.notice, state.noticeKind)
            vm.consumeNotice()
        }
    }
    // #64: never show raw "not set" — the header shows the model name when
    // configured and a setup prompt otherwise.
    val setupNeeded = state.model.isBlank() || state.provider.isBlank()
    ChatScreen(
        title = title, tab = tab, modelLabel = state.model.ifBlank { "" },
        setupNeeded = setupNeeded, state = state, wc = wc, toasts = toasts,
        tts = tts, autoSpeak = autoSpeak, onAutoSpeak = onAutoSpeak,
        onMenu = onMenu, autoLiveGen = autoLiveGen,
        onModel = { showModel = true },
        onHistory = { showThreads = true },
        onCheckConnection = vm::checkReachability,
        onPending = vm::onPending, onSend = vm::send, onStop = vm::stop,
        onSteer = vm::steer, onRuns = vm::openRuns,
        onCloseRuns = vm::closeRuns, onCancelRun = vm::cancelRun,
        onRefreshRuns = vm::refreshRuns, onApprovals = vm::openApprovals,
        onCloseApprovals = vm::closeApprovals,
        onApprove = { vm.decideApproval(it, true) },
        onDeny = { vm.decideApproval(it, false) },
        onRefreshApprovals = vm::refreshApprovals,
        onMemory = vm::openMemory,
        onCloseMemory = vm::closeMemory,
        onConfirmMemory = vm::confirmMemory,
        onEditMemory = vm::editMemory,
        onDeleteMemory = vm::deleteMemory,
        onToggleMemoryPause = vm::toggleMemoryPause,
        onCreateMemory = vm::createMemory,
        onForgetMemory = vm::forgetMemory,
        onRefreshMemory = vm::refreshMemory,
        onNew = vm::newConversation, onRetry = vm::retry,
        onRegenerate = vm::regenerate, onContinue = vm::continueTurn,
        onReplace = vm::replaceAndResend, onFork = vm::forkAndResend,
        onPickPhotos = { galleryLauncher.launch("image/*") },
        onTakePhoto = ::requestCamera,
        onRemovePhoto = vm::removePendingAttachment,
        onLiveStagedPhotos = {
            vm.notify(
                "Heard you — photo still staged, tap send to include it.",
                ToastKind.Info,
            )
        },
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
                toasts.toast("Conversation deleted.", ToastKind.Success)
            },
            onSaveSkill = { savingSkillId = it },
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
    val savingId = savingSkillId
    if (savingId != null) {
        var skillName by remember(savingId) { mutableStateOf("") }
        var skillDescription by remember(savingId) { mutableStateOf("") }
        var saving by remember(savingId) { mutableStateOf(false) }
        var saveError by remember(savingId) { mutableStateOf("") }
        AlertDialog(
            // No dismiss mid-save: the callback toasts on landing, and a
            // cancelled dialog must not celebrate a save nobody saw start.
            onDismissRequest = { if (!saving) savingSkillId = null },
            title = { Text("Save as skill") },
            text = {
                Column(modifier = Modifier.fillMaxWidth()) {
                    Text(
                        "The transcript becomes a draft for review in Skills — " +
                            "nothing runs until you review, test and activate it.",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                    if (saveError.isNotBlank()) {
                        Text(
                            saveError,
                            style = MaterialTheme.typography.bodySmall,
                            color = MaterialTheme.colorScheme.error,
                            modifier = Modifier.fillMaxWidth().padding(top = 8.dp),
                        )
                    }
                    OutlinedTextField(
                        value = skillName,
                        onValueChange = { skillName = it },
                        label = { Text("Name (lowercase, hyphens)") },
                        singleLine = true,
                        modifier = Modifier.fillMaxWidth().padding(top = 8.dp),
                    )
                    OutlinedTextField(
                        value = skillDescription,
                        onValueChange = { skillDescription = it },
                        label = { Text("Description") },
                        modifier = Modifier.fillMaxWidth().padding(top = 8.dp),
                    )
                }
            },
            confirmButton = {
                TextButton(
                    onClick = {
                        saving = true
                        saveError = ""
                        vm.saveThreadAsSkill(savingId, skillName, skillDescription) { err, truncated ->
                            saving = false
                            if (err == null) {
                                savingSkillId = null
                                toasts.toast(
                                    if (truncated) "Draft saved, long thread clipped — review it in Skills."
                                    else "Draft saved — review it in Skills.",
                                    ToastKind.Success,
                                )
                            } else {
                                saveError = err
                            }
                        }
                    },
                    // Trimmed: "   " passes isNotBlank but sends "".
                    enabled = !saving && skillName.trim().isNotBlank() &&
                        skillDescription.trim().isNotBlank(),
                ) {
                    Text(if (saving) "Saving…" else "Save draft")
                }
            },
            dismissButton = {
                TextButton(
                    onClick = { savingSkillId = null },
                    enabled = !saving,
                ) {
                    Text("Cancel")
                }
            },
        )
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
    onSaveSkill: (String) -> Unit,
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
                                            text = { Text("Save as skill") },
                                            onClick = { menu = false; onSaveSkill(t.id) },
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

/** One beat between detector start attempts (see driver retry below). */
private const val DETECT_RETRY_MS = 2_000L

/** Detector bookkeeping for the live loop driver (#317):
 * plain holder instead of state — only the driver effect touches it, and
 * the effect keys drive restarts, so publishing it would recompose for
 * bookkeeping the keys already encode. */
private class DetectCtl {
    var running = false
    var phase = -1
}

/** Monotonic milestone-cue key counter (#317): plain holder,
 * not state — only the milestone effect touches it. */
private class CueKeys {
    var n = 0
}

/**
 * Chat provider/model picker (#18: model selection lives on the chat
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
    toasts: ToastCenter,
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
    onSteer: () -> Unit = {},
    onRuns: () -> Unit = {},
    onCloseRuns: () -> Unit = {},
    onCancelRun: (String) -> Unit = {},
    onRefreshRuns: () -> Unit = {},
    onApprovals: () -> Unit = {},
    onCloseApprovals: () -> Unit = {},
    onApprove: (String) -> Unit = {},
    onDeny: (String) -> Unit = {},
    onRefreshApprovals: () -> Unit = {},
    onMemory: () -> Unit = {},
    onCloseMemory: () -> Unit = {},
    onConfirmMemory: (String) -> Unit = {},
    onEditMemory: (String, String, String, () -> Unit) -> Unit = { _, _, _, _ -> },
    onDeleteMemory: (String) -> Unit = {},
    onToggleMemoryPause: (String, Boolean) -> Unit = { _, _ -> },
    onCreateMemory: (String, String, () -> Unit) -> Unit = { _, _, _ -> },
    onForgetMemory: () -> Unit = {},
    onRefreshMemory: () -> Unit = {},
    onNew: () -> Unit,
    onRetry: () -> Unit,
    onRegenerate: () -> Unit = {},
    onContinue: () -> Unit = {},
    onReplace: (Int, String) -> Unit = { _, _ -> },
    onFork: (Int, String) -> Unit = { _, _ -> },
    /** Photo attachments (#318): gallery picker, camera capture, and
     * removing one staged photo. Wired by ChatTab (launchers live there). */
    onPickPhotos: () -> Unit = {},
    onTakePhoto: () -> Unit = {},
    onRemovePhoto: (Int) -> Unit = {},
    /** Live transcript heard text while photos are staged (#339): the text
     * parks in the composer for a confirming tap instead of auto-sending
     * with the photos. */
    onLiveStagedPhotos: () -> Unit = {},
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
    // Photo attach menu (#318): gallery vs camera. Plain dialog state —
    // the pickers themselves live in ChatTab (onPickPhotos/onTakePhoto).
    var showAttachMenu by remember(tab) { mutableStateOf(false) }
    // Edit state belongs to one thread: a switch or a fresh thread closes it.
    // The attach menu closes too (#339): it is keyed on tab, so without
    // this a thread switch leaves it open over the new thread's composer.
    LaunchedEffect(state.threadId) {
        editingIndex = null
        confirmReplace = null
        showAttachMenu = false
    }
    val clipboard = LocalClipboardManager.current
    val scope = rememberCoroutineScope()
    val speakingKey by tts.speakingKey.collectAsState()
    // TTS readiness as state (#317): the milestone effect
    // keys on this, so an engine that finishes init mid-turn re-fires the
    // effect and cues the tools that arrived while it was down.
    val ttsReady by tts.readyFlow.collectAsState()
    // Live mode (#85, call-mode #317): chain the existing one-shot voice
    // input + TTS readout into a continuous talk-listen-talk loop. The
    // system recognizer handles silence cutoff per turn; LiveInterrupt
    // detects talk-over during readout (cut speech) and during streaming
    // (cut the turn — barge-in), and tool milestones are spoken mid-task.
    // Live state is keyed by tab: whatever the when(dest) branch reuse
    // semantics are, a session can never bleed into another tab's screen.
    var liveMode by remember(tab) { mutableStateOf(false) }
    var silentRounds by remember(tab) { mutableIntStateOf(0) }
    // Transient recognizer faults (busy/audio/client overlap) retry without
    // costing a silent round; capped so a wedged recognizer still ends loudly.
    var transientRetries by remember(tab) { mutableIntStateOf(0) }
    // Last readout state for the driver's reply-finish re-listen (#317):
    // declared up here (not next to the driver) so endLive/startLive can
    // retire it — a stale true plus an idle restart would double-listen.
    var wasSpeaking by remember(tab) { mutableStateOf(false) }
    // Rotation silently kills a live session (remember(tab) state is lost
    // and the rotation guard deliberately doesn't restart it). This flag
    // survives recreation so the user gets told instead of silence.
    var liveLostOnRotate by rememberSaveable(tab) { mutableStateOf(false) }
    LaunchedEffect(Unit) {
        if (liveLostOnRotate) {
            liveLostOnRotate = false
            toasts.toast("Live session ended on rotation.")
        }
    }
    // Hands-free interrupt (#85) with barge-in (#317): background mic
    // watches for speech while the readout plays AND while the turn
    // streams. On trigger cut speech and, if a turn is running, cut it
    // too — the stream-end effect re-listens instead of speaking the
    // stopped partial. Needs RECORD_AUDIO; without it the loop still
    // runs tap-to-talk.
    val context = LocalContext.current
    // Barge-in claim (#317): set before stopping so the stream-end effect
    // below re-listens rather than speaking the stopped partial.
    var bargeCancel by remember(tab) { mutableStateOf(false) }
    // Tool milestones already spoken this turn (#317): each newly-appearing
    // tool counts as announced once; retired when the turn ends.
    var announcedTools by remember(tab) { mutableStateOf(listOf<String>()) }
    // Label already spoken alongside the tools above: the ViewModel keeps
    // the last non-empty label independently, so a label-only frame must
    // re-fire the milestone effect too (#317).
    var announcedLabel by remember(tab) { mutableStateOf("") }
    // Monotonic cue-key counter (#317):
    // plain holder, not state — only the milestone effect touches it, so
    // publishing would recompose the whole screen once per cue for
    // bookkeeping the toggle() key needs. Global, not per-tab: TTS is
    // hoisted at activity level, so per-tab counters restarting at 0 would
    // collide — toggle() on a live key stops instead of speaking. Never
    // resets within the activity lifetime (a destination switch drops it,
    // like rotation — safe because dispose stops TTS, so no cue survives
    // to collide with a restarted live-cue-1).
    val cueKeys = remember { CueKeys() }
    // Detector bookkeeping (#317):
    // plain holder, not state — only the driver effect writes it in the
    // steady state, and the effect keys drive restarts. Declared up here
    // (not next to the driver) so endLive/startLive can reset it alongside
    // the actual stop(): the holder must never claim "stopped" while the
    // recorder still holds the mic, or vice versa.
    val detectCtl = remember(tab) { DetectCtl() }
    // Session generation for stale detector triggers (#317): the detector
    // posts via scope.launch, so a trigger fired just before hang-up can
    // land after it. endLive/startLive bump this; onBargeIn only honors the
    // generation live at fire time (captured before the launch hop), so a
    // trigger can't leak across a hang-up→restart either. Global, not
    // per-tab: the trigger path (scope/latestBarge) is shared across tabs,
    // so per-tab counters restarting at 0 would collide cross-tab. (A
    // destination switch drops it, like rotation — safe because the
    // composition scope is cancelled, so no in-flight trigger survives to
    // meet a reset generation.)
    val liveGen = remember { AtomicInteger(0) }
    fun onBargeIn(firedGen: Int) {
        // Stale-trigger guard (#317): drop triggers from a previous session
        // generation, and anything landing after hang-up — neither may claim
        // and kill a turn the user started after hanging up.
        if (firedGen != liveGen.get()) return
        if (!liveMode) return
        tts.stop()
        if (state.streaming) {
            bargeCancel = true
            onStop()
        }
    }
    // Fresh callback for the once-per-tab detector (#317): the
    // remembered lambda reads the latest handler instead of the first
    // composition's closure over state/onStop.
    val latestBarge by rememberUpdatedState(::onBargeIn)
    val interrupt = remember(tab) { LiveInterrupt({ val g = liveGen.get(); scope.launch { latestBarge(g) } }) }
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
            toasts.toast("Live mode needs microphone access.", ToastKind.Error)
        }
    }
    DisposableEffect(tab) {
        onDispose {
            interrupt.stop(); liveRec.destroy(); tts.stop()
            // Same reset discipline as endLive (#317): the stopped recorder
            // must not leave the holder claiming "running" (else no restart
            // on return), a stale barge claim must not reroute the next
            // stream end, and the bumped generation retires in-flight
            // detector triggers so none leak into the other tab. NOTE: this
            // targets the leaving tab's slots and holds only while
            // remember(tab) stays keyed — if that keying ever collapses the
            // writes land on shared objects. Either way the external stops
            // and the global gen bump are the load-bearing lines; do not
            // "optimize out" the bump as redundant. The
            // milestone set is retired too: a turn settling while away never
            // runs the stream-end retire, so a kept set would suppress the
            // next turn's first cue for repeated tools. On a mid-turn return
            // the restarted effect re-announces the latest tool once — a
            // one-line orientation cue, not a replay — and diffing resumes
            // from the full set.
            detectCtl.running = false
            detectCtl.phase = -1
            bargeCancel = false
            announcedTools = emptyList()
            announcedLabel = ""
            liveGen.incrementAndGet()
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
            toasts.toast("Voice output unavailable.", ToastKind.Error)
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
        // Retire the reply-finish gate too (#317): a stale true plus an
        // idle restart would drive a second liveListen() with no turn.
        wasSpeaking = false
        // Retire any barge claim and milestone set with the session (#317):
        // a stale claim must not reroute the next turn's stream end.
        // Bump the session generation too: in-flight detector triggers from
        // this session die with it instead of landing in the next one.
        liveGen.incrementAndGet()
        bargeCancel = false
        announcedTools = emptyList()
        announcedLabel = ""
        // Stop the detector now — don't rely on the liveMode=false effect
        // round-trip to get around to it — and reset its holder alongside
        // (#317): the holder must not claim "running"
        // after the recorder was stopped out from under the driver effect.
        interrupt.stop()
        detectCtl.running = false
        detectCtl.phase = -1
        liveRec.cancel()
        tts.stop()
        if (reason != null) toasts.toast(reason, ToastKind.Error)
    }
    // Reply/TTS loop effects are defined after startVoice below (they call it).
    // Built-in Android speech-to-text (RecognizerIntent — no extra
    // permission or dependency) for the Chat tab.
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
            toasts.toast("No voice input app found.", ToastKind.Error)
        }
    }
    /** One in-app listen cycle for the live loop (overlay stays visible). */
    fun liveListen() {
        liveRec.listen()
    }
    /** Begins (or resumes) the live session: kill turn, arm, listen. */
    fun startLive() {
        // Consume any pending start first (#317): every exit below returns
        // before the old clear site, and a stuck true would auto-start a
        // session on the next micArmed flip that nobody asked for.
        pendingLiveStart = false
        // Model first (#317): the on-screen Start gates on state.ready and
        // the widget/auto paths must share it — arming with nothing to send
        // to just drops the first utterance (send() returns early !ready).
        if (!state.ready) {
            toasts.toast("Choose a model to start", ToastKind.Error)
            return
        }
        if (!liveRec.available()) {
            // No session to retire: toast and return directly — endLive()
            // would cut a non-live readout via tts.stop() as collateral.
            toasts.toast("Voice recognition unavailable on this device.", ToastKind.Error)
            return
        }
        // Permission first (#317): the kills below are destructive and the
        // check is cheap — a revoked mic must exit before touching the turn.
        // liveMode is armed only after this point for the same reason; the
        // next tap retries via the permission-launcher path.
        micArmed = interrupt.hasPermission(context)
        if (!micArmed) {
            // No permission, no loop — and no endLive(): there is no session
            // to retire, and it would cut a non-live readout as collateral.
            // Stay out and say why; the next tap retries via the launcher.
            toasts.toast("Live mode needs microphone access.", ToastKind.Error)
            return
        }
        // Mid-turn entry (#317): capture before onStop() clears
        // it. A startLive-killed turn must not be spoken back: claim it like
        // a barge so the stream-end effect re-listens instead of reading
        // the just-killed partial, and skip the direct liveListen() below —
        // the effect drives the first listen, so the recognizer starts
        // exactly once (a double startListening burns a transient retry).
        val killedTurn = state.streaming
        liveRec.cancel()
        tts.stop()
        if (state.streaming) onStop()
        liveMode = true
        liveLostOnRotate = true
        silentRounds = 0
        transientRetries = 0
        livePartial = ""
        // Retire the reply-finish gate for symmetry with endLive (#317):
        // the first listen here is driven explicitly below (or by the
        // stream-end effect for a killed turn), never by a stale flag.
        wasSpeaking = false
        // Fresh call state (#317): a previous session's barge claim or
        // milestone set must not leak into this one. The detector is
        // stopped and its holder reset too (#317) so the
        // next detecting phase recalibrates from clean state — a bare reset
        // without the stop would lie the other way (effect sees "stopped",
        // skips its own stop, and leaks a live recorder into the
        // recognizer's mic).
        interrupt.stop()
        detectCtl.running = false
        detectCtl.phase = -1
        // A killed turn arrives claimed (see above); a fresh start clears
        // any stale claim from the previous session and opens a new trigger
        // generation, so pre-start detector firings can't leak into it.
        liveGen.incrementAndGet()
        bargeCancel = killedTurn
        announcedTools = emptyList()
        announcedLabel = ""
        // No handoff wait (#317): any speech playing at entry was just cut
        // above, so the first listen is driven here directly — except for a
        // killed turn, whose stream-end effect re-listens instead of speaking
        // the partial. The driver's reply gate stays out either way:
        // wasSpeaking was retired above (reply) or never armed (cue), so
        // this is the single listen for every non-killed entry.
        if (!killedTurn) liveListen()
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
                    // Photos need a confirming tap (#339): live transcripts
                    // auto-send by design, but background speech firing a
                    // staged-photo turn with no confirmation is a
                    // wrong-image send. Text parks for the tap instead.
                    if (state.pendingAttachments.isEmpty()) {
                        onSend()
                    } else {
                        onLiveStagedPhotos()
                    }
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
                // A settled turn retires its milestone set either way (#317).
                announcedTools = emptyList()
                announcedLabel = ""
                if (bargeCancel) {
                    // Barge-in (#317): the user cut the turn — re-listen
                    // instead of speaking the stopped partial. Barge wins
                    // over a simultaneous stream error (#317): the
                    // user asked to talk, and the error toast still fires
                    // from the state.error effect.
                    bargeCancel = false
                    liveListen()
                } else if (state.error.isEmpty()) {
                    silentRounds = 0
                    val last = state.messages.lastOrNull()
                    if (last != null && last.role == "assistant" && last.content.isNotBlank()) {
                        if (!speakMessage(ttsKeyFor(last), last.content)) {
                            // Engine dead: speakingKey never sets, so the
                            // loop effect can't re-listen — end it loudly.
                            endLive("Live session ended (voice output unavailable).")
                        }
                    } else {
                        // Blank reply (#317): cut a still-playing milestone
                        // cue first, or it bleeds into the fresh listen with
                        // the detector off and no re-listen to save it.
                        tts.stop()
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
    // Live loop driver (#85, barge-in #317): the talk-over detector runs
    // while the reply plays (cut speech, existing) and while the turn
    // streams (cut the turn, new); it stays off for milestone cues and
    // while listening. The phase key restarts detection on every
    // transition so the floor recalibrates (quiet room vs readout bleed).
    // When the spoken reply fully finishes, listen again — but a cue
    // finishing mid-turn must not: the turn still owns the mic.
    // (wasSpeaking lives up top with the other retired-on-reset state.)
    val cueSpeaking = speakingKey?.startsWith("live-cue-") == true
    val detecting = liveMode && micArmed && !cueSpeaking &&
        (speakingKey != null || state.streaming)
    val detectPhase = when {
        !detecting -> 0
        speakingKey != null -> 1
        else -> 2
    }
    // Driver bookkeeping lives up top (#317) so session start/end can
    // reset it alongside the actual stop(). Restarts are skipped when
    // already running in the target phase, so a micArmed/liveMode toggle
    // with no phase change doesn't churn the AudioRecord. speakingKey and
    // streaming are keyed directly, not just via detectPhase, so a readout
    // end re-evaluates the re-listen gate even when the phase doesn't flip
    // (micArmed==false tap-to-talk degrades to silence after one reply
    // otherwise — detecting stays false throughout, detectPhase stays 0).
    LaunchedEffect(detectPhase, liveMode, micArmed, speakingKey, state.streaming) {
        val phaseChanged = detectPhase != detectCtl.phase
        detectCtl.phase = detectPhase
        if (detecting && (phaseChanged || !detectCtl.running)) {
            // Fresh start per phase (never a carried calibration): stop
            // first, the same rapid-restart shape LiveInterrupt is built for.
            // Started in the composition scope, not this effect's
            // (#317): an effect restart must not cancel
            // the old detector job out from under the new AudioRecord —
            // the explicit stop() below is the only lifetime manager.
            interrupt.stop()
            detectCtl.running = false
            // A start failure here does NOT clear micArmed (#317):
            // contention with a dying recorder reads the same as a dead
            // mic, and a sticky downgrade would end detection for the whole
            // session. One bounded retry runs below, then the next phase
            // change retries; a truly dead mic fails fast each time and
            // the loop runs tap-to-talk.
            detectCtl.running = interrupt.start(scope)
            if (!detectCtl.running) {
                // One bounded retry (#317): a long tool task sits in a
                // single phase, so an entry failure would leave the whole
                // task unmonitored until the next transition. Wait one
                // mic-release beat, then retry once — a phase change cancels
                // this effect first, and a session reset flips detectCtl
                // phase, so a stale retry can't arm a dead phase. NOTE: the
                // delay never gates the re-listen below — any transition that
                // would make it fire (readout/streaming/mode flip) also
                // flips an effect key via detectPhase and cancels this delay
                // first; if the delay completes, the gate reads unchanged. No stop()
                // precedes the retry: failed starts return before the job is
                // assigned, and start() reports true only for an already
                // active job, so a second stop() would only race the
                // recorder the retry is about to use.
                delay(DETECT_RETRY_MS)
                if (detecting && detectPhase == detectCtl.phase) {
                    detectCtl.running = interrupt.start(scope)
                }
            }
        } else if (!detecting && detectCtl.running) {
            interrupt.stop()
            detectCtl.running = false
        }
        if (wasSpeaking && speakingKey == null && liveMode && !state.streaming) {
            liveListen()
        }
        // Cues never arm the gate (#317): only a reply finish re-listens.
        // A cut cue would otherwise leave a stale true behind and the next
        // driver run would double the stream-end effect's own listen.
        wasSpeaking = speakingKey != null && !cueSpeaking
    }
    // Tool milestones (#317): while a live turn streams, announce newly
    // appearing tools once per change so a long task is audible. The cue
    // names the latest delta (#317: a batch seeds several tools
    // at once, but only one cue can play) — the whole batch still counts
    // as announced. Cues use live-cue-* keys: the driver above keeps the
    // detector off for them, and their completion never starts listening.
    LaunchedEffect(liveMode, state.streaming, state.activeTools, state.activeToolLabel, ttsReady) {
        if (!liveMode || !state.streaming || state.activeTools.isEmpty()) return@LaunchedEffect
        // Skip cues while TTS isn't up (#317): toggle()
        // would park the cue in `pending` and speak it late — possibly after
        // the turn — with no speakingKey while parked, so the detector stays
        // armed. Readiness is collected state, so a late init re-fires this
        // effect and the missed set still cues instead of being lost (the
        // early return below leaves announcedTools untouched on purpose).
        if (!ttsReady) return@LaunchedEffect
        val newTools = state.activeTools - announcedTools.toSet()
        val labelChanged = state.activeToolLabel != announcedLabel
        // Set-equality for tools (#317): a reorder with no new
        // member and no fresh label is not a milestone.
        if (newTools.isEmpty() && !labelChanged) return@LaunchedEffect
        announcedTools = state.activeTools.toList()
        announcedLabel = state.activeToolLabel
        val what = if (labelChanged && state.activeToolLabel.isNotBlank()) {
            state.activeToolLabel
        } else {
            // Fallback names the latest too (#317): the empty-delta
            // path needs a blank label, which the ViewModel's keep-prior
            // never produces mid-turn — defensive, and consistent if reached.
            "Using " + (newTools.lastOrNull() ?: state.activeTools.last())
                .substringAfterLast("__").replace('_', ' ')
        }
        cueKeys.n++
        // Engine-dead stays silent via the !ttsReady early return above (a
        // failed engine never becomes ready), not via the branch below —
        // the reply path reports TTS failure loudly at stream end instead
        // of killing a working call over a cue. The branch is defense for
        // a shutdown racing the readiness snapshot; intentionally a no-op.
        if (!tts.toggle("live-cue-${cueKeys.n}", what)) {
            // No-op by decision, not by accident.
        }
    }
    // Surface send failures as a toast too (the inline card keeps details).
    LaunchedEffect(state.error) {
        if (state.error.isNotEmpty()) {
            toasts.toast(friendlyError(state.error).title, ToastKind.Error)
        }
    }
    // The toast card floats top-end over the content (Box overlay, not
    // the bottom-anchored Scaffold slot) so it never shifts the layout.
    Box(modifier = Modifier.fillMaxSize()) {
    Scaffold(
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
                    // Runs (#311): the thread's server-side turns, in-flight
                    // and finished. Gated on a loaded thread: a blank thread
                    // would list every conversation's runs unscoped.
                    IconButton(
                        onClick = onRuns,
                        enabled = state.ready && state.threadId.isNotBlank(),
                    ) {
                        Icon(Icons.Filled.List, contentDescription = "Runs")
                    }
                    // Approvals (#313): held tool calls need an explicit
                    // decision, and a gate stalls the stream with no frames.
                    // Badged with the pending count; always enabled once
                    // ready — approvals are global, not per-thread.
                    BadgedBox(
                        badge = {
                            if (state.pendingApprovalCount > 0) {
                                Badge { Text(pendingCountLabel(state.pendingApprovalCount)) }
                            }
                        },
                    ) {
                        IconButton(
                            onClick = onApprovals,
                            enabled = state.ready,
                        ) {
                            Icon(Icons.Filled.Approval, contentDescription = "Approvals")
                        }
                    }
                    // Memory (#315): learned facts. Badged with pending
                    // proposals awaiting review; always enabled once ready —
                    // memory is per-profile, not per-thread.
                    BadgedBox(
                        badge = {
                            if (state.pendingMemoryCount > 0) {
                                Badge { Text(pendingCountLabel(state.pendingMemoryCount)) }
                            }
                        },
                    ) {
                        IconButton(
                            onClick = onMemory,
                            enabled = state.ready,
                        ) {
                            Icon(Icons.Filled.Psychology, contentDescription = "Memory")
                        }
                    }
                    // Auto-read: speak each finished reply aloud.
                    //
                    // role = Switch (#283): the icon's description already
                    // carries "on"/"off", but without a role TalkBack calls
                    // it a plain button and never says it toggles. The
                    // description stays the label — the role is what says
                    // "this is a toggle" — so the two are not redundant.
                    // stateDescription carries the value separately from the
                    // label, which is the slot a switch's state belongs in.
                    IconButton(
                        onClick = { onAutoSpeak(!autoSpeak) },
                        modifier = Modifier.semantics {
                            role = Role.Switch
                            stateDescription = if (autoSpeak) "On" else "Off"
                        },
                    ) {
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
                        // Polite live region (#283): a reply being generated
                        // was otherwise silent to TalkBack — the bar is
                        // decorative and the tool line below it is optional,
                        // so a screen-reader user got no signal at all that a
                        // turn was in flight.
                        LinearProgressIndicator(
                            modifier = Modifier
                                .fillMaxWidth()
                                .semantics { liveRegion = LiveRegionMode.Polite },
                        )
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
                                .padding(horizontal = 16.dp, vertical = 2.dp)
                                // Each tool the agent picks up is announced as
                                // it happens (#283), for the same reason.
                                .semantics { liveRegion = LiveRegionMode.Polite },
                        )
                    }
                    // Held approval (#313): a gate stalls the stream with no
                    // frames, which otherwise reads as thinking. The watch
                    // raises the count; the banner points at the sheet.
                    if (state.streaming && state.pendingApprovalCount > 0) {
                        Row(
                            modifier = Modifier.fillMaxWidth(),
                            horizontalArrangement = Arrangement.End,
                        ) {
                            // The gate-stalled-stream signal a screen-reader
                            // user most needs: same live region as the tool
                            // banners above.
                            TextButton(
                                onClick = onApprovals,
                                modifier = Modifier.semantics {
                                    liveRegion = LiveRegionMode.Polite
                                },
                            ) {
                                Text("Approval needed (${pendingCountLabel(state.pendingApprovalCount)})")
                            }
                        }
                    }
                    // Queue-or-send (#122): the composer stays live while
                    // streaming. Parking a send here auto-fires it on Done;
                    // a queued send returns to the composer on failure/stop.
                    // Steer (#311) sends the text into the running turn
                    // instead of behind it — shown independently of the
                    // queue, since steering matters most mid-turn. Needs a
                    // known run id: on older servers (or before headers
                    // arrive) there is nothing to steer into, so only Queue
                    // shows and send() takes the normal path.
                    if (state.streaming && state.pending.isNotBlank()) {
                        Row(
                            modifier = Modifier.fillMaxWidth(),
                            horizontalArrangement = Arrangement.End,
                        ) {
                            if (state.activeRunId.isNotBlank()) {
                                TextButton(onClick = onSteer) { Text("Steer") }
                            }
                            if (state.queued.isBlank()) {
                                TextButton(onClick = onSend) { Text("Queue send") }
                            }
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
                    // Staged photos (#318): thumbnails with per-photo remove.
                    // They send with the next turn even when the turn has no
                    // text; the count line states the per-turn cap up front.
                    if (state.pendingAttachments.isNotEmpty()) {
                        Column(modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp)) {
                            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                                state.pendingAttachments.forEachIndexed { i, a ->
                                    Box {
                                        AttachmentThumb(
                                            base64 = a.base64,
                                            label = "Staged photo ${i + 1} of " +
                                                "${state.pendingAttachments.size}",
                                            modifier = Modifier.size(64.dp),
                                            slot = i,
                                        )
                                        // 24dp is an overlay exception to the
                                        // standard-48dp rule (#283): the target
                                        // sits ON a 64dp thumbnail, so a full
                                        // 48dp button would cover the photo it
                                        // removes. Content description kept.
                                        IconButton(
                                            onClick = { onRemovePhoto(i) },
                                            modifier = Modifier.align(Alignment.TopEnd).size(24.dp),
                                        ) {
                                            Icon(
                                                Icons.Filled.Close,
                                                contentDescription = "Remove photo ${i + 1}",
                                                modifier = Modifier.size(16.dp),
                                            )
                                        }
                                    }
                                }
                            }
                            Text(
                                "${state.pendingAttachments.size}/$MAX_IMAGES_PER_TURN photos — " +
                                    "sent with your next message.",
                                style = MaterialTheme.typography.labelSmall,
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                                modifier = Modifier.padding(top = 2.dp),
                            )
                        }
                    }
                    Row(
                        modifier = Modifier.fillMaxWidth().padding(12.dp),
                        verticalAlignment = Alignment.CenterVertically,
                    ) {
                        // Photo attachments (#318): gallery or camera, staged
                        // thumbnails above, sent as image parts with the turn.
                        // Stays live while streaming — staged photos park in
                        // the composer until the next explicit send.
                        Box {
                            // Gated on ready like send (#339): staging work
                            // before the thread loads just parks photos that
                            // sit through the load into a "still loading"
                            // notice at send time.
                            IconButton(
                                onClick = { showAttachMenu = true },
                                enabled = state.ready,
                            ) {
                                Icon(Icons.Filled.Add, contentDescription = "Attach photo")
                            }
                            DropdownMenu(
                                expanded = showAttachMenu,
                                onDismissRequest = { showAttachMenu = false },
                            ) {
                                DropdownMenuItem(
                                    text = { Text("Choose photos") },
                                    onClick = { showAttachMenu = false; onPickPhotos() },
                                )
                                DropdownMenuItem(
                                    text = { Text("Take photo") },
                                    onClick = { showAttachMenu = false; onTakePhoto() },
                                )
                            }
                        }
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
                            // speakingKey is read pre-stop on purpose: the
                            // snapshot predates recomposition, so it still
                            // reports the readout playing at tap time — a tap
                            // mid-readout skips the direct listen and lets
                            // the driver's wasSpeaking transition drive it,
                            // while an idle tap listens immediately. Moving
                            // the null-check below would flip both branches.
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
                            FilledTonalIconButton(onClick = {
                                // Live stop cuts like a barge (#317):
                                // the user cut the turn to talk, so the loop
                                // re-listens instead of reading back the
                                // just-killed partial. Cuts a playing cue too
                                // — like onBargeIn, the loop must own the mic
                                // before it re-listens. Outside live mode the
                                // plain stop keeps its speak-the-partial path.
                                if (liveMode) {
                                    tts.stop()
                                    bargeCancel = true
                                }
                                onStop()
                            }) {
                                Icon(Icons.Filled.Close, contentDescription = "Stop")
                            }
                        } else {
                            FilledIconButton(
                                onClick = stopThen(onSend),
                                // Photos send without text (#318): an
                                // image-only turn is a real turn.
                                enabled = (state.pending.isNotBlank() ||
                                    state.pendingAttachments.isNotEmpty()) &&
                                    state.ready,
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
                            // Content includes photos (#318): dropping an
                            // image-only turn without confirming is silent
                            // data loss of the same class as text.
                            val dropsTurns = state.messages.drop(index + 1).any { messageHasContent(it) }
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
                                            // Author + time read as one unit
                                            // (#283): without the merge a
                                            // swipe-walker hears "Assistant"
                                            // and "14:32" as two detached
                                            // stops before the message.
                                            // Merging stops at the text —
                                            // the action buttons below stay
                                            // separate nodes, because folding
                                            // clickable children into a
                                            // merged parent would take the
                                            // buttons out of swipe order.
                                            Row(
                                                modifier = Modifier
                                                    .weight(1f)
                                                    .semantics(mergeDescendants = true) {},
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
                                            }
                                            // Edit & resubmit (#122): pencil opens an
                                            // inline editor (Replace / Fork).
                                            //
                                            // No size override (#283): a
                                            // 28dp Button pins the node and
                                            // coerces away IconButton's own
                                            // 48dp reservation, so the target
                                            // was 28dp. The standard 48dp
                                            // with a 24dp icon is what every
                                            // other icon button here uses.
                                            if (isUser && !state.streaming) {
                                                IconButton(
                                                    onClick = {
                                                        editingIndex = index
                                                        editDraft = msg.content
                                                    },
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
                                                // Copy, and regenerate on the
                                                // latest reply. Both lose their
                                                // size override for the same
                                                // reason as the pencil above
                                                // (#283).
                                                IconButton(
                                                    onClick = { clipboard.setText(AnnotatedString(msg.content)) },
                                                ) {
                                                    Icon(Icons.Filled.ContentCopy, contentDescription = "Copy")
                                                }
                                                // Regenerate (#122, v1 replace): resends
                                                // history minus this reply. Offered on
                                                // the latest assistant message only.
                                                if (isLast && !state.streaming) {
                                                    IconButton(
                                                        onClick = stopThen(onRegenerate),
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
                                                // Carried photos are invisible here
                                                // (#339): the field is text-only,
                                                // but Replace/Fork resends the
                                                // turn's photos with it — say so,
                                                // or an edit to "thanks" silently
                                                // resends images.
                                                if (msg.attachments.isNotEmpty()) {
                                                    HintLine(
                                                        attachmentMarker(msg.attachments.size) +
                                                            " — resends with the edited text.",
                                                    )
                                                }
                                                Spacer(modifier = Modifier.height(4.dp))
                                                Row(
                                                    horizontalArrangement = Arrangement.spacedBy(8.dp),
                                                    verticalAlignment = Alignment.CenterVertically,
                                                ) {
                                                    // Empty text allowed with photos
                                                    // (#339): the editor reaches
                                                    // image-only resends, which
                                                    // send() supports — the VM
                                                    // still refuses empty
                                                    // text-only resends.
                                                    val canResend = editDraft.isNotBlank() ||
                                                        msg.attachments.isNotEmpty()
                                                    Button(
                                                        onClick = {
                                                            if (dropsTurns) {
                                                                confirmReplace = index to editDraft
                                                            } else {
                                                                onReplace(index, editDraft)
                                                                editingIndex = null
                                                            }
                                                        },
                                                        enabled = canResend,
                                                    ) { Text("Replace") }
                                                    OutlinedButton(
                                                        onClick = {
                                                            onFork(index, editDraft)
                                                            editingIndex = null
                                                        },
                                                        enabled = canResend,
                                                    ) { Text("Fork") }
                                                    TextButton(onClick = { editingIndex = null }) {
                                                        Text("Cancel")
                                                    }
                                                }
                                            } else {
                                                // Sent photos (#318) render above the
                                                // text; an image-only turn shows no
                                                // "…" placeholder — the thumbnails
                                                // ARE the message.
                                                if (msg.attachments.isNotEmpty()) {
                                                    Row(
                                                        horizontalArrangement = Arrangement.spacedBy(8.dp),
                                                    ) {
                                                        msg.attachments.forEachIndexed { ai, a ->
                                                            AttachmentThumb(
                                                                base64 = a.base64,
                                                                label = "Attached photo ${ai + 1} of " +
                                                                    "${msg.attachments.size}",
                                                                modifier = Modifier.size(120.dp),
                                                                slot = ai,
                                                            )
                                                        }
                                                    }
                                                    if (msg.content.isNotBlank()) {
                                                        Spacer(modifier = Modifier.height(4.dp))
                                                    }
                                                }
                                                if (msg.content.isNotBlank() || msg.attachments.isEmpty()) {
                                                    SelectionContainer {
                                                        Text(
                                                            msg.content.ifEmpty { "…" },
                                                            style = MaterialTheme.typography.bodyMedium,
                                                        )
                                                    }
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
                                                    // The two causes mean opposite
                                                    // things to the reader, and one
                                                    // string cannot serve both:
                                                    // "Stopped" blames the user for a
                                                    // network failure they did not
                                                    // cause and cannot act on.
                                                    when (msg.interruptedBy) {
                                                        InterruptedBy.DROP ->
                                                            "Connection lost — reply may be incomplete."
                                                        InterruptedBy.USER ->
                                                            "Stopped — reply incomplete."
                                                    },
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
            // Content, not text (#339): image-only turns count as dropped
            // turns, like the dropsTurns gate above — otherwise the dialog
            // reports "drops 0 later messages" while discarding photos.
            val dropped = state.messages.drop(idx + 1).count { messageHasContent(it) }
            // Same invisibility as the inline editor above: the edited
            // turn's photos resend with the new text — named here so the
            // confirmation covers the full resend.
            val resendPhotos = state.messages.getOrNull(idx)?.attachments?.size ?: 0
            AlertDialog(
                onDismissRequest = { confirmReplace = null },
                title = { Text("Resend from edited message?") },
                text = {
                    Text(
                        (if (dropped == 1) {
                            "This drops 1 later message and resends."
                        } else {
                            "This drops $dropped later messages and resends."
                        }) + if (resendPhotos > 0) {
                            " " + attachmentMarker(resendPhotos) + " — resends too."
                        } else {
                            ""
                        },
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
        // Resumable runs (#311): the thread's server-side turns, in-flight
        // and finished, with cancel for the running ones.
        if (state.showRuns) {
            RunsDialog(
                runs = state.runs,
                loading = state.runsLoading,
                runsError = state.runsError,
                highlightId = state.activeRunId.ifBlank { state.busyRunId },
                onRefresh = onRefreshRuns,
                onCancel = onCancelRun,
                onClose = onCloseRuns,
            )
        }
        // Approval gates (#313): pending cards with approve/deny, audit below.
        if (state.showApprovals) {
            ApprovalsDialog(
                approvals = state.approvals,
                auditRows = state.auditRows,
                loading = state.approvalsLoading,
                error = state.approvalsError,
                degraded = state.approvalsDegraded,
                deciding = state.decidingApprovals,
                onApprove = onApprove,
                onDeny = onDeny,
                onRefresh = onRefreshApprovals,
                onClose = onCloseApprovals,
            )
        }
        // Per-profile memory (#315): proposals to confirm, rows to edit or
        // delete, topic pause toggles, and a composer for user items.
        if (state.showMemory) {
            MemoryDialog(
                items = state.memoryItems,
                paused = state.memoryPaused,
                topics = state.memoryTopics,
                warnings = state.memoryWarnings,
                loading = state.memoryLoading,
                error = state.memoryError,
                acting = state.actingMemory,
                onConfirm = onConfirmMemory,
                onEdit = onEditMemory,
                onDelete = onDeleteMemory,
                onTogglePause = onToggleMemoryPause,
                onCreate = onCreateMemory,
                onForget = onForgetMemory,
                onRefresh = onRefreshMemory,
                onClose = onCloseMemory,
            )
        }
    }
        ToastHost(center = toasts, modifier = Modifier.fillMaxWidth())
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

/** Providers are gateway-global; one catalog fetch covers the Chat tab. */
internal fun catalogPath(api: ChatApi): String =
    api.pathFor("chat")

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
