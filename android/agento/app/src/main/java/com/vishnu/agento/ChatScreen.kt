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
import android.os.SystemClock
import android.provider.Settings
import android.util.Log
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
import androidx.lifecycle.viewModelScope
import androidx.lifecycle.viewmodel.compose.viewModel
import com.mikepenz.markdown.m3.Markdown
import java.io.File
import java.util.Locale
import java.util.concurrent.atomic.AtomicInteger
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineStart
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.NonCancellable
import kotlinx.coroutines.flow.collect
import kotlinx.coroutines.isActive
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

/** Pending count as both the badge and the banner show it: one cap, one
 * wording, so the two can never disagree above 99. */
private fun pendingCountLabel(count: Int): String = if (count > 99) "99+" else "$count"

/**
 * Chat header actions (#354): the single named list the header's action set
 * comes from. The bar keeps [chatHeaderVisibleActions] on screen and folds
 * [chatHeaderOverflowActions] behind the "More" menu, so a future action
 * cannot silently re-stuff the bar — adding it here without assigning it to
 * one of the two lists fails [ChatHeaderTest].
 */
internal enum class ChatHeaderAction {
    MODEL,
    CONVERSATIONS,
    RUNS,
    APPROVALS,
    MEMORY,
    AUTO_READ,
    NEW,
}

/** Always-visible header actions (#354): Model, Conversations, Auto-read, New. */
internal val chatHeaderVisibleActions: List<ChatHeaderAction> =
    listOf(
        ChatHeaderAction.MODEL,
        ChatHeaderAction.CONVERSATIONS,
        ChatHeaderAction.AUTO_READ,
        ChatHeaderAction.NEW,
    )

/** Header actions folded behind the "More" overflow menu (#354). */
internal val chatHeaderOverflowActions: List<ChatHeaderAction> =
    listOf(
        ChatHeaderAction.RUNS,
        ChatHeaderAction.APPROVALS,
        ChatHeaderAction.MEMORY,
    )

/** Every header action, exactly once: visible + overflow with no overlap. */
internal val chatHeaderAllActions: List<ChatHeaderAction> =
    chatHeaderVisibleActions + chatHeaderOverflowActions

/**
 * Combined badge for the "More" overflow icon (#354): the pending Approval
 * count plus the pending Memory count. Zero means no badge.
 */
internal fun chatHeaderOverflowBadgeCount(pendingApprovals: Int, pendingMemory: Int): Int =
    maxOf(0, pendingApprovals) + maxOf(0, pendingMemory)

/** Unwraps a possibly-wrapped Context to its Activity, if any. */
private tailrec fun Context.findActivity(): Activity? = when (this) {
    is Activity -> this
    is ContextWrapper -> baseContext.findActivity()
    else -> null
}

/** A camera intent older than this is treated as dead: no callback will ever
 * settle it, so the double-launch gate must not stick forever. Ten minutes
 * bounds a stuck gate far above any real camera session. MONOTONIC domain. */
private const val CAMERA_PENDING_STALE_MS = 10 * 60 * 1000L
/** The gallery picker's own stale window (#339): a picker session is long
 * (browsing photos, taking a call), so the camera's 10 minutes would
 * expire while it is still open. */
private const val GALLERY_PENDING_STALE_MS = 60 * 60 * 1000L

/** Sweep age-out for non-keep camera orphans: same duration, WALL domain —
 * file mtimes are wall-clock, so compare against currentTimeMillis(). A
 * separate constant despite the equal value: different clock domains. */
private const val CAMERA_SWEEP_STALE_MS = 10 * 60 * 1000L

/**
 * Head/tail content sample for thumbnail keys: a true prefix/suffix pair on
 * normal payloads, the whole string when it fits — the sample invariant
 * (prefix + suffix, no overlap) holds at every size.
 */
private fun String.sampleKey(half: Int): String =
    if (length <= half * 2) this else take(half) + takeLast(half)

/**
 * Process-lifetime IO scope for fire-and-forget file cleanup: deleteQuietly
 * must survive tab-leave, rotation AND composition death — so it launches
 * here, not on a composition scope.
 *
 * NEVER CANCELLED, BY DESIGN (#339): a cleanup that is cancelled by a
 * tab-leave would leak the file it was meant to delete. Named
 * `processCleanupScope` so the lifetime is explicit — do NOT launch real
 * work here (it would inherit a process-lifetime scope by accident). A
 * global scope IS the load-bearing choice: the ViewModel's scope dies with
 * the tab, and the composition's with the screen, and a delete cancelled
 * by either is a delete that never happened.
 */
private val processCleanupScope =
    kotlinx.coroutines.CoroutineScope(
        kotlinx.coroutines.SupervisorJob() + kotlinx.coroutines.Dispatchers.IO,
    )

/**
 * Bounded decoded-thumbnail memory: produceState remembers per composition,
 * so scrolling back re-decodes. The key IS the produce key ([ThumbKey]:
 * thread + id + length + head/tail sample, never the id alone) — a weaker
 * shadow could serve a stale bitmap. Legacy id-less payloads key on the
 * staged slot and can alias across threads (wrong thumb, never wrong send).
 * Bounded by BYTES (~8 MB), not entries — and dropped on a memory trim
 * (the ViewModel registers the ComponentCallbacks2 hook) and when the
 * ViewModel is cleared.
 */
internal class ThumbCache(maxKb: Int = 8 * 1024) {
    private val cache = object : android.util.LruCache<ThumbKey, android.graphics.Bitmap>(maxKb) {
        // allocationByteCount, not byteCount (#339): a hardware-config
        // bitmap can report 0 logical bytes while the real allocation is
        // larger, so byteCount alone would let the cache hold far more
        // than its byte bound. allocationByteCount covers both configs.
        override fun sizeOf(key: ThumbKey, value: android.graphics.Bitmap): Int =
            maxOf(1, value.allocationByteCount / 1024)
    }
    /** Threads whose thumbs are dead (#339): a deleted thread's id never
     * comes back, so a decode that finishes AFTER the eviction must not
     * re-pin its bitmap. Grows by one short id per deleted thread and is
     * NOT cleared by evictAll() — clearing it would reopen the very race
     * it closes (a late put re-pinning a dead thread's bitmap). Bounded in
     * practice by the ViewModel's lifetime: the cache dies with it. */
    private val deadThreads: MutableSet<String> =
        java.util.Collections.newSetFromMap(java.util.concurrent.ConcurrentHashMap())

    fun get(key: ThumbKey): android.graphics.Bitmap? = cache.get(key)
    fun put(key: ThumbKey, bmp: android.graphics.Bitmap) {
        // A put for a deleted thread is refused (#339): the eviction and
        // the decode race, and the cache must not resurrect the dead one.
        if (key.thread.isNotEmpty() && deadThreads.contains(key.thread)) return
        cache.put(key, bmp)
    }
    /** Explicit invalidation for staged-remove: the strip knows the exact
     * attachment leaving, so its bitmap frees immediately instead of waiting
     * for LRU pressure. Thread-delete evicts the whole thread instead. */
    fun remove(key: ThumbKey) {
        cache.remove(key)
    }
    /** Evicts one thread's keys (#339): a deleted thread's thumbs are dead
     * weight (up to 8 MB pinned by LRU pressure alone). The key TYPE carries
     * the thread, so this is a field read, not a shape cast. */
    fun removeThread(threadKey: String) {
        // Marked dead first (#339): a decode in flight would otherwise put
        // its bitmap back after the eviction.
        deadThreads.add(threadKey)
        for (key in cache.snapshot().keys) {
            if (key.thread == threadKey) cache.remove(key)
        }
    }
    fun evictAll() = cache.evictAll()
}

/**
 * One thumbnail cache key (#339): a named type, not a bare Triple, so the
 * eviction path reads the thread off the key instead of casting a shape it
 * hopes for. A key-shape change would then be a compile error here rather
 * than a silent no-op that pins a deleted thread's bitmaps.
 */
internal data class ThumbKey(
    /** Thread the key belongs to: every key carries one, so a deleted
     * thread's keys are exactly evictable. */
    val thread: String,
    /** Stable per-pick id, or the legacy staged slot. */
    val id: String,
    /** Payload length: a same-id payload of a different length is a
     * different photo. */
    val length: Int,
    /** Head/tail content sample: catches a same-id, same-length swap. */
    val sample: String,
)

/**
 * Cache-key construction for an attachment's thumbnail: the produce path
 * ([AttachmentThumb]) uses the returned key AS its remember value AND its
 * cache key — identical object, zero drift — and the staged-remove path
 * rebuilds it to evict the exact entry.
 *
 * The THREAD is in every key (#339): a stable id is unique, but without
 * the thread the key could not be evicted when its thread was deleted, and
 * the cache would pin dead thumbnails until LRU pressure. The cost is a
 * re-decode when the same photo shows in two threads (rare — a photo
 * belongs to one thread). Legacy id-less payloads key on the staged slot
 * instead of an id, so they are thread-scoped by construction.
 */
internal fun thumbCacheKey(stableId: String, legacySlot: Int, base64: String, threadKey: String = ""): ThumbKey =
    ThumbKey(
        thread = threadKey,
        id = if (stableId.isNotEmpty()) stableId else "s$legacySlot",
        length = base64.length,
        sample = base64.sampleKey(64),
    )

/**
 * One staged or sent photo: the remembered ~256px thumbnail, or an icon
 * placeholder when the payload won't decode — a corrupt payload degrades to
 * a missing thumbnail, never a crash.
 */
@Composable
private fun AttachmentThumb(
    base64: String,
    label: String,
    modifier: Modifier = Modifier,
    // Legacy-only for identity: the stable VALUE ignores this — it exists for
    // the id == "" fallback. Named legacySlot (not slot) so the stable value
    // can't accidentally consume it; a slot shift only rebuilds the legacy
    // sample string, never re-decodes.
    legacySlot: Int = 0,
    stableId: String = "",
    // Owning thread for the legacy cache key: id-less payloads key on slot,
    // which repeats per thread — so the legacy key carries it. Defaults empty
    // (previews, single-use callers with cache = null).
    threadKey: String = "",
    // Scroll-back memory, hoisted by the caller: null disables the cache
    // (previews, single-use). The thread is in EVERY key (#339) — a UUID
    // alone would be unique, but without the thread the key could not be
    // evicted when its thread was deleted, and the cache would pin the
    // dead thread's bitmaps. The cost is a re-decode when the same photo
    // shows in two threads (rare — a photo belongs to one thread).
    cache: ThumbCache? = null,
) {
    // Identity without walking the payload: the per-pick UUID is stable
    // across removes — the stable VALUE ignores slot, so photo 1 keeps its
    // thumbnail when photo 0 leaves (no re-decode, no flicker) — and distinct
    // across identical picks. Legacy payloads (id "") have no UUID, so their
    // 128-char sample can still alias — wrong thumb, no crash.
    // Decoded off-main: ~1 MB Base64.decode + decodeByteArray during
    // composition janks low-end scroll, so produceState moves it to IO. No
    // manual recycle: it can pull pixels from under a late outgoing draw.
    // Remembered, no per-recomposition alloc: the remember keys are the cheap
    // identity (UUID + slot + length + the content sample) and the value
    // builds ONLY when they change. The sample is a KEY too, not just part
    // of the cache key (#339): without it, a same-id/same-length payload
    // swap keeps the old key and serves the stale bitmap. A hand-edited
    // same-id/same-length payload never re-keys — accepted: display-only
    // anyway (the wire always sends the current bytes). ONE remember slot
    // with the branch inside, so a remove (indices shift 1→0) never
    // restarts produceState.
    val thumbKey = remember(stableId, legacySlot, base64.length, base64.sampleKey(64), threadKey) {
        thumbCacheKey(stableId, legacySlot, base64, threadKey)
    }
    val produced by produceState<android.graphics.Bitmap?>(null, thumbKey) {
        // Explicit try/catch, not runCatching: runCatching would swallow the
        // producer's own CancellationException into a null placeholder. Cache
        // lookup uses the produce key ITSELF — the identical key built
        // above, so a divergent key can never serve a stale bitmap.
        val cacheKey = thumbKey
        value = try {
            val hit = cache?.get(cacheKey)
            hit ?: withContext(Dispatchers.IO) {
                decodeThumbnail(base64)?.also { bmp ->
                    cache?.put(cacheKey, bmp)
                }
            }
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            null
        }
    }
    // Local, not the delegate: smart cast is impossible on a
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
    // Staged-toast episode marker: which staged count the "photo still
    // staged" notice already voiced for. Saveable, not plain remember: a
    // rotation mid-dictation would re-toast the identical notice.
    var liveStagedVoicedCount by rememberSaveable { mutableStateOf(0) }
    // Scroll-back thumbnail memory: AttachmentThumb's produceState remembers
    // per composition, so scrolling back re-decodes. VM-held (per-tab keyed,
    // rotation-surviving) — see ChatViewModel.thumbCache.
    val thumbCache = vm.thumbCache
    // New thread, new staging episode: the voice slate belongs to the old
    // thread's staged set. The thumb cache is NOT evicted here by design —
    // its keys are thread-scoped (both branches carry the thread), so a
    // dead thread's bitmaps are unreachable AND evictable (see
    // ThumbCache.removeThread).
    LaunchedEffect(state.threadId) {
        liveStagedVoicedCount = 0
    }
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
    // Photo attachments: gallery picks need no permission (the picker returns
    // read access to just the chosen images); the camera needs CAMERA plus a
    // FileProvider Uri staged before TakePicture launches.
    var cameraUri by rememberSaveable { mutableStateOf<Uri?>(null) }
    // File path, not the File: rotation while the camera app is foreground
    // destroys this composition and only saveable state survives — a lost
    // Uri/File silently drops the capture.
    var cameraPath by rememberSaveable { mutableStateOf<String?>(null) }
    // Confirm dialog before the Settings escape hatch: auto-opening Settings
    // on every tap would surprise-navigate on accidental taps. Saveable like
    // the camera path; closed on thread switch.
    var showCameraSettings by rememberSaveable { mutableStateOf(false) }
    // Owning thread of the outstanding capture: the callback runs post-switch,
    // so processUris must be told which thread the photo was taken for. A
    // stale owner can only mismatch, and mismatch drops with notice.
    var cameraThread by rememberSaveable { mutableStateOf<String?>(null) }
    // Owning thread of the outstanding gallery pick: same hole as camera — a
    // thread switch while the picker is open would stage into the new composer.
    var galleryThread by rememberSaveable { mutableStateOf<String?>(null) }
    // Thread-scoped transient UI, same as the editor/menu state inside
    // ChatScreen. Split across parent/child by render site — both fire on the
    // same key, so a new addition belongs in whichever effect owns its site.
    // Deliberately NOT cleared here: the camera/permission gates, owners and
    // paths — an in-flight TakePicture still needs cameraUri/cameraPath/
    // cameraThread to home or drop-with-notice, and clearing the intent gate
    // would let a new tap overwrite the in-flight capture.
    LaunchedEffect(state.threadId) { showCameraSettings = false }
    // Path of the capture currently decoding (not a boolean): the
    // delete-previous guard must compare FILE IDENTITY, so a rotation
    // (which cancels the batch) cannot let the next tap delete a file
    // that is still being read. Saveable, so the path outlives the
    // composition; a stale entry only delays one cleanup to the sweep.
    var cameraDecoding by rememberSaveable { mutableStateOf<String?>(null) }
    // True while a gallery picker is outstanding (#339): a second tap
    // before the first returns would open a second picker, and the two
    // results would interleave. Saveable timestamp, like the camera gate.
    // Same hardening as the camera gate (#339): a MONOTONIC clock
    // (elapsedRealtime — a wall-clock jump cannot age it out) and a
    // fresh-process clear (the sweep below), so a kill cannot leave a gate
    // with no callback to clear it.
    var galleryPendingSince by rememberSaveable { mutableStateOf(0L) }
    // True while a camera intent is outstanding: launchCamera overwrites the
    // shared Uri/path on every tap, so a second tap before the first returns
    // would make the first callback decode the wrong file. Saveable timestamp,
    // not a boolean: death must not stick the gate forever.
    var cameraPendingSince by rememberSaveable { mutableStateOf(0L) }
    // Separate gate for the permission-request window: the intent gate above
    // covers an outstanding TakePicture, this one an outstanding system
    // permission dialog — one timestamp for both meant a restored permission
    // arm blocked Take-photo taps with the wrong "already open" copy. Each
    // window arms and clears only its own.
    var cameraPermSince by rememberSaveable { mutableStateOf(0L) }
    // Asked-before flag for rationale routing: the API returns false for both
    // first-denial (some OEMs) and permanent denial, so without history a
    // first deny jumps straight to Settings. false+never-asked → toast;
    // false+asked → Settings. Saveable, but cleared on a fresh process.
    var cameraAskedBefore by rememberSaveable { mutableStateOf(false) }
    // Pre-request snapshot for rationale routing: cameraAskedBefore is set at
    // request time, so reading it in the callback is always true — route on
    // the snapshot (false + never-asked → toast is reachable again).
    var cameraWasAskedAtRequest by rememberSaveable { mutableStateOf(false) }
    /** True while a camera permission request is outstanding (unanswered dialog). */
    fun isCameraPermPending(): Boolean {
        if (cameraPermSince == 0L) return false
        val age = SystemClock.elapsedRealtime() - cameraPermSince
        return age >= 0 && age < CAMERA_PENDING_STALE_MS
    }
    // In-memory double-tap guard for the launchCamera IO window: the saveable
    // gate arms only in the Main hop AFTER IO, so a second tap landing
    // mid-hop would otherwise pass both checks. Deliberately NOT saveable —
    // a rotation cancels the IO work with the composition.
    val ioGuard = remember { java.util.concurrent.atomic.AtomicBoolean(false) }
    /** True while a camera intent launched recently enough to still return.
     * Monotonic clock: a wall-clock jump would age a 1-minute-old intent into
     * a 31-minute-old one. A negative age means reboot since arming. */
    fun isCameraPending(): Boolean {
        if (cameraPendingSince == 0L) return false
        val age = SystemClock.elapsedRealtime() - cameraPendingSince
        return age >= 0 && age < CAMERA_PENDING_STALE_MS
    }

    /** True while a gallery picker is outstanding (#339): a second tap
     * would open a second picker and the two results would interleave. Its
     * own bound: a picker session is long (browsing, a call), so the
     * camera's 10-minute window would expire while the picker is still
     * open. RESIDUAL, same shape as the camera gate: a picker left open
     * past the bound can still be re-opened by a second tap — the bound
     * only exists so a killed process cannot block the picker forever
     * (the fresh-process sweep clears it). */
    fun isGalleryPending(): Boolean {
        if (galleryPendingSince == 0L) return false
        val age = SystemClock.elapsedRealtime() - galleryPendingSince
        return age >= 0 && age < GALLERY_PENDING_STALE_MS
    }
    // Startup sweep: a process kill between the TakePicture write and the
    // decode leaves a capture with no owner — per-batch finally covers
    // cancel/leave-tab, not death. The cache dir holds ONLY these captures,
    // so clearing it here bounds the litter to zero. Sparing the restored
    // capture: sweeping the file cameraPath points at would turn the
    // returning capture into a "moved or deleted" error.
    val appCacheDir = context.applicationContext.cacheDir
    LaunchedEffect(Unit) {
        // Off-main: directory listing + deletes are disk IO, and this effect
        // runs on every first composition including post-rotation. The dir
        // resolves BEFORE the IO block, so no rotation can leak the Activity.
        // Fresh-process gate reset via the ViewModel: saveable state restores
        // on rotation AND on kill-with-bundle, so a local first-run flag can't
        // tell them apart — the VM dies with the process but survives rotation.
        // Snapshot on Main: cameraPath is snapshot state.
        val keep = cameraPath
        val freshProcess = vm.consumeSweepGateReset()
        // Permission gate ALWAYS clears on a fresh process: unlike a camera
        // intent (whose TakePicture result may still arrive after a
        // kill-with-camera-foreground), a system permission dialog never
        // survives process death. The intent gate below is pickier.
        if (freshProcess) {
            cameraPermSince = 0L
            // Asked-before state resets with it: a restored true would send
            // the first denial after a kill straight to Settings.
            cameraAskedBefore = false
            cameraWasAskedAtRequest = false
        }
        // Intent gate cleared only with no outstanding capture: a kill with
        // the camera foreground restores BOTH the timestamp and cameraPath,
        // and zeroing the gate would let a second Take-photo overwrite the
        // restored Uri/path mid-flight.
        if (freshProcess && keep == null) {
            cameraPendingSince = 0L
        }
        // The gallery gate clears on a fresh process too (#339): the OS
        // never redelivers a GetMultipleContents result to a new process,
        // so a restored gate would block the picker for its whole window on
        // the device that just died.
        if (freshProcess) {
            galleryPendingSince = 0L
        }
        // Age out orphans, keep the live capture UNCONDITIONALLY: a restored
        // keep older than the bound is still the file the returning camera may
        // be writing to. Fresh-process path deletes every non-keep file
        // UNCONDITIONALLY (after a kill they are ownerless by construction);
        // the rotation path keeps the age gate — a young non-keep file there
        // can be an older flight still decoding. Wall-clock risk: a clock jump
        // can age a young non-keep past the gate (the live capture is spared).
        // Compared as absolute Files, not raw strings: a relative path would
        // silently un-keep the live capture.
        val keepAbs = keep?.let { java.io.File(it).absolutePath }
        val sweepDir = appCacheDir.resolve("camera")
        withContext(Dispatchers.IO) {
            runCatching {
                val now = System.currentTimeMillis()
                sweepDir.takeIf { it.exists() }?.listFiles()
                    ?.filter {
                        it.absolutePath != keepAbs &&
                            (freshProcess || now - it.lastModified() > CAMERA_SWEEP_STALE_MS)
                    }
                    ?.forEach { runCatching { it.delete() } }
            }
        }
    }
    // One decode at a time, in pick order: each photo lands (or its named
    // error toasts) as it finishes, so a slow second image never blocks the
    // first one's thumbnail. [cleanup] is the camera's temp file for a capture
    // batch — passed explicitly so a concurrent gallery batch can never delete
    // a capture still decoding. [homeThread] is the owning thread; a mismatch
    // at any Main hop drops the batch with notice instead of staging thread-A
    // photos into thread B.
    // Best-effort file cleanup, shared by every delete below: disk IO on Main
    // picker/camera callbacks, so fire-and-forget. It must survive tab-leave,
    // rotation AND composition death, so it launches on the process-lifetime
    // cleanupScope (never cancelled by design — see its declaration).
    fun deleteQuietly(f: File?) {
        if (f == null) return
        processCleanupScope.launch {
            // Logged, not swallowed (#339): a failed delete (a lock, a full
            // disk) leaves the file behind until the startup sweep, and
            // without a line in logcat the orphan has no explanation.
            runCatching { f.takeIf { it.exists() }?.delete() }
                .onFailure { Log.w("ChatAttachments", "cleanup delete failed: ${f.name}", it) }
        }
    }
    fun processUris(
        uris: List<Uri>,
        cleanup: File? = null,
        onBatchDone: () -> Unit = {},
        homeThread: String = vm.threadIdNow(),
    ) {
        // Done fires on early returns too: the camera sets its in-flight flag
        // before calling, so a cap refusal without the callback would stick
        // the flag and leak the capture.
        if (uris.isEmpty()) {
            deleteQuietly(cleanup)
            onBatchDone()
            return
        }
        // Unified on vm.notify: the same cap refused here and mid-batch must
        // have one lifetime — a composition toast is lost on rotation while a
        // state notice survives it.
        val slots = MAX_IMAGES_PER_TURN - vm.pendingAttachmentCount()
        if (slots <= 0) {
            // One cap string + Info, shared with the backstop.
            vm.notify(capRefused(slots), ToastKind.Info)
            // Refusal skips the body, so the capture is deleted here, not
            // in finally.
            deleteQuietly(cleanup)
            onBatchDone()
            return
        }
        // Deduped at tap time: a double-selected image otherwise runs two
        // full decodes and spends both turn slots. Uri-equality only — exact
        // dedupe would need a post-read content hash, i.e. the cost dedupe
        // exists to avoid.
        val unique = uris.distinct()
        // Duplicates collapse into one copy — said out loud, but DEFERRED: the
        // tap can't know whether the deduped set still overflows, so the loop
        // voices the dedupe at the end, only when no cap toast fired.
        val hadDupes = unique.size < uris.size
        val capToasted = java.util.concurrent.atomic.AtomicBoolean(false)
        // Hoisted ABOVE the launch: the resolver is all the batch needs, and
        // declaring it inside would still capture the Activity in the closure.
        val appContext = context.applicationContext
        // Settled inline when the scope is already dead: a navigate-away +
        // result race can deliver picker output after the composition scope
        // died — launching then is a no-op whose body never runs, sticking
        // cameraDecoding. Undispatched for the same reason: the body starts
        // synchronously, so the try/finally is entered before any cancel.
        if (!vm.viewModelScope.isActive) {
            deleteQuietly(cleanup)
            onBatchDone()
            return
        }
        // Try-entry proof for the TOCTOU below. Atomic like every other
        // cross-thread flag here: the write lands on the IO thread and the
        // read on Main.
        val batchStarted = java.util.concurrent.atomic.AtomicBoolean(false)
        // The ViewModel's scope, NOT the composition's (#339): a rotation
        // recreates the composition and cancelled the batch, so the rest
        // of a valid multi-pick was dropped with a "pick again" notice.
        // The ViewModel outlives the rotation, and the staged photos live
        // there too — the batch finishes and the view catches up.
        vm.viewModelScope.launch(Dispatchers.IO, CoroutineStart.UNDISPATCHED) {
            // First statement: try-entry proof. Combined with UNDISPATCHED,
            // the post-launch check below can tell "body never ran" from
            // "body ran" — and settle the never-ran case inline.
            batchStarted.set(true)
            // Landed count for the cancel notice below: only a batch that
            // staged something before dying has anything to report.
            val landed = java.util.concurrent.atomic.AtomicInteger(0)
            // Consecutive-failure abort: failures yield to the next Uri by
            // design (a bad pick must not cost a later good one its slot),
            // but 50 corrupt URIs would each burn a full prepareAttachment
            // (up to 32 MB read + decodes + EXIF). Three in a row aborts the
            // rest with a named notice.
            val consecutiveFailures = java.util.concurrent.atomic.AtomicInteger(0)
            val batchAborted = java.util.concurrent.atomic.AtomicBoolean(false)
            try {
                // No pre-truncation: a failed pick must not cost a later
                // good pick its slot — the room re-check below stops the
                // loop exactly when full.
                for (u in unique) {
                    // Home + room re-check per pick: two rapid batches both
                    // snapshot free slots at tap time, so without this both
                    // decode fully and the loser drops AFTER the expensive
                    // part. addPendingAttachments stays as backstop.
                    val gate = withContext(Dispatchers.Main) {
                        vm.hasAttachmentRoom() to vm.isThreadCurrent(homeThread)
                    }
                    val room = gate.first
                    if (!gate.second) {
                        withContext(Dispatchers.Main) {
                            vm.notify(
                                "Thread switched — photos dropped, pick again.",
                                ToastKind.Info,
                            )
                        }
                        break
                    }
                    if (!room) {
                        // Toasted on the ACTUAL drop, not predicted at tap
                        // time: corrupt-first picks can free the slots a
                        // tap-time prediction assumed lost. First !room ends
                        // the loop, so this fires at most once per batch.
                        // Flag set AFTER the toast in the same Main hop.
                        withContext(Dispatchers.Main) {
                            // Free slots re-read live: room is a boolean,
                            // but the cap wording needs the count.
                            vm.notify(
                                capRefused(MAX_IMAGES_PER_TURN - vm.pendingAttachmentCount()),
                                ToastKind.Info,
                            )
                            capToasted.set(true)
                        }
                        break
                    }
                    // Application context, hoisted above the launch: the
                    // closure never sees the Activity, so no rotation can
                    // leak it for the batch.
                    val result = prepareAttachment(appContext, u)
                    withContext(Dispatchers.Main) {
                        // Home re-checked AFTER the decode: the pre-decode gate
                        // and this hop bracket ~100ms of blocking IO per photo
                        // — a thread switch landing inside would stage thread-A
                        // photos into thread B's composer. Drops with notice.
                        if (!vm.isThreadCurrent(homeThread)) {
                            vm.notify(
                                "Thread switched — photos dropped, pick again.",
                                ToastKind.Info,
                            )
                            return@withContext
                        }
                        // State notices, not composition toasts: a decode
                        // failure must survive rotation like the cap refusals
                        // do — same lifetime for every attach error. Landed
                        // counts actual growth, not attempts.
                        result
                            .onSuccess {
                                val before = vm.pendingAttachmentCount()
                                // Stages into the HOME thread (the one the
                                // pick was made for), not "whatever is current"
                                // (#339): the check above re-reads the live
                                // thread and drops with a notice when the
                                // user switched mid-batch, so a photo can
                                // never land in a conversation the pick was
                                // not made for.
                                vm.addPendingAttachments(listOf(it))
                                if (vm.pendingAttachmentCount() > before) landed.incrementAndGet()
                                consecutiveFailures.set(0)
                            }
                            .onFailure {
                                vm.notify(it.message ?: "Couldn't attach that photo.")
                                if (consecutiveFailures.incrementAndGet() >= MAX_CONSECUTIVE_ATTACH_FAILURES) {
                                    vm.notify(
                                        "Stopped after repeated unreadable photos — the rest were skipped.",
                                        ToastKind.Info,
                                    )
                                    batchAborted.set(true)
                                }
                            }
                    }
                    if (batchAborted.get()) break
                }
                // Deferred duplicate voice: the tap can't know whether the
                // deduped set still overflows, so the notice waits for the
                // loop outcome — voiced only when something staged AND no
                // cap toast fired.
                // Landed notice (#339): the batch outlives the tab (it runs
                // in the ViewModel's scope), so photos can land in a
                // composer the user is not looking at — say so instead of
                // letting them appear silently on return.
                if (landed.get() > 0 && !capToasted.get()) {
                    withContext(Dispatchers.Main) {
                        val n = landed.get()
                        vm.notify(
                            when {
                                hadDupes -> "Duplicate photos attached once."
                                n == 1 -> "Photo added to the composer."
                                else -> "$n photos added to the composer."
                            },
                            ToastKind.Info,
                        )
                    }
                }
            } catch (e: CancellationException) {
                // Cancelled mid-batch (the tab's ViewModel went away — the
                // app closed or the tab was discarded; a ROTATION no longer
                // cancels it, the batch runs in the ViewModel's scope):
                // staged picks stay, undecoded ones vanish — said once,
                // including the zero-landed abort, because silence reads as a
                // dead tap. Immediately rethrown: swallowing cancel breaks
                // structured concurrency.
                withContext(Dispatchers.Main + NonCancellable) {
                    vm.notify(
                        if (landed.get() == 0) {
                            "Photo pick interrupted — pick again."
                        } else if (landed.get() == 1) {
                            "Stopped staging photos — 1 photo kept in the composer."
                        } else {
                            "Stopped staging photos — ${landed.get()} photos kept in the composer."
                        },
                        ToastKind.Info,
                    )
                }
                throw e
            } finally {
                // In finally, not after the loop: leaving the tab mid-decode
                // cancels this coroutine, and a trailing delete would be
                // skipped — littering one capture per cancel. The completion
                // callback runs on Main, NonCancellable, so a cancelled batch
                // still drops the flag. Dead-composition write is INTENTIONAL.
                runCatching { cleanup?.takeIf { it.exists() }?.delete() }
                withContext(Dispatchers.Main + NonCancellable) { onBatchDone() }
            }
        }
        // Never-ran settle: with UNDISPATCHED above, a live scope runs the
        // body synchronously inside launch() — so batchStarted.get() == false
        // proves the scope died in the TOCTOU window and the finally never
        // ran. Settle inline on Main.
        if (!batchStarted.get()) {
            deleteQuietly(cleanup)
            onBatchDone()
        }
    }
    val galleryLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.GetMultipleContents(),
    ) { uris ->
        // Intent settled — the launch gate below re-arms for the next tap.
        galleryPendingSince = 0L
        processUris(uris, homeThread = galleryThread ?: vm.threadIdNow())
        galleryThread = null
    }
    val cameraLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.TakePicture(),
    ) { ok ->
        val u = cameraUri
        val f = cameraPath?.let(::File)
        // Intent settled — the launch gate below re-arms for the next tap.
        cameraPendingSince = 0L
        // Uri nulls now (single-delivery, never reused); the PATH stays
        // until the batch completes so a rotation mid-decode restores a
        // non-null keep for the startup sweep.
        cameraUri = null
        // ok=false is cancel AND camera-error alike (indistinguishable by
        // contract), so silence is correct for cancels — a toast on every
        // false would nag every backing-out. ok=true with a lost Uri is the
        // third case: unreachable by construction, but a good capture deleted
        // in silence would be the worst outcome.
        if (ok && u != null) {
            cameraDecoding = f?.absolutePath
            processUris(listOf(u), f, {
                // Only the OWNING batch retires the slot (#339): a second
                // capture may already be decoding, and clearing the shared
                // flag would let the next tap delete the file that batch is
                // still reading.
                if (cameraDecoding == f?.absolutePath) cameraDecoding = null
                // Conditional clear: a rapid re-tap overwrites the path with
                // the second capture while the first still decodes —
                // blanking unconditionally would strand the second flight
                // with keep == null, letting a rotation sweep delete the file
                // being read. Only the owning batch retires the path.
                if (f != null && cameraPath == f.absolutePath) cameraPath = null
            }, cameraThread ?: vm.threadIdNow())
            cameraThread = null
        } else {
            // Shared dead-scope-safe cleanup (inline when the scope died).
            deleteQuietly(f)
            cameraPath = null
            // Owner clears with the path: the else branch previously left
            // cameraThread set — harmless (next launch overwrites), but a
            // rotation restores a stale owner with no outstanding intent
            // that the next reader must then prove dead.
            cameraThread = null
            // The third case above (ok + lost Uri): a capture deleted with
            // no notice would be silent data loss, so the impossible edge
            // voices while the cancel case stays silent.
            if (ok) vm.notify("Couldn't keep that capture — try again.")
        }
    }
    fun launchCamera() {
        // Single outstanding intent + hardware gate first (both Main-safe,
        // both synchronous reads): overlapping launches and camera-less
        // devices refuse here, before any work starts. Two gates, two windows:
        // the saveable timestamp covers the outstanding intent (survives
        // rotation AND death), while ioGuard covers the IO prep window below.
        if (isCameraPending() || ioGuard.get()) {
            vm.notify(
                if (isCameraPending()) "Camera is already open — finish it first."
                else "Camera is starting — one moment.",
                ToastKind.Info,
            )
            return
        }
        // Hardware gate, idempotent: requestCamera refuses first so no
        // permission prompt fires on camera-less devices, and the same guard
        // holds AT the launch. Read off the application context — the tap
        // must not touch the Activity at all.
        val appTapContext = context.applicationContext
        if (!appTapContext.packageManager.hasSystemFeature(PackageManager.FEATURE_CAMERA_ANY)) {
            vm.notify("No camera on this device — use Choose photos instead.", ToastKind.Info)
            return
        }
        // File prep off-main: mkdirs, exists/delete, and the FileProvider
        // query are disk/package-manager IO on the tap handler's Main thread.
        // Everything below runs in one IO hop; state writes + launcher fire
        // hop back to Main at the end. Checked, not assumed: full storage fails
        // mkdirs, and a bare TakePicture failure (ok=false) would leave the
        // user with no hint why no camera appeared. isDirectory, not exists: a
        // stray FILE at this path would fail downstream generically.
        // Arm the IO-window guard synchronously at tap: the check above runs
        // on Main, and this closes the double-tap window across the IO hop.
        // The SAVEABLE gate stays disarmed until the Main hop after IO —
        // arming it here would stick across rotation. ioGuard is deliberately
        // in-memory: a rotation cancels the IO work with the composition.
        ioGuard.set(true)
        // Homed LIVE at tap time like the gallery path: reading
        // state.threadId in the Main hop after the IO hop widens the lag
        // window, homing the capture to a thread the user already left.
        cameraThread = vm.threadIdNow()
        // Composition-scoped on purpose (#339): this is the camera's FILE
        // PREP (mkdirs + FileProvider uri), a few milliseconds of IO. A
        // cancel here (tab left mid-hop) drops the half-made capture, which
        // the finally deletes — nothing user-visible is lost, unlike the
        // decode batch, which is why that one moved to the ViewModel scope.
        scope.launch(Dispatchers.IO) {
            try {
                val dir = File(appTapContext.cacheDir, "camera")
                if (!dir.isDirectory && !dir.mkdirs()) {
                    withContext(Dispatchers.Main) {
                        // Gate untouched: it arms only in the success Main hop
                        // below; the try/finally drops ioGuard.
                        // Home dropped too (#339): no capture follows, and a
                        // stale owner would home the NEXT one to a thread
                        // the user already left.
                        cameraThread = null
                        vm.notify("Couldn't start the camera — storage unavailable.")
                    }
                    return@launch
                }
                // UUID, not millis: a rapid double-tap can mint the same
                // millisecond twice and point two captures at one file.
                val file = File(dir, "capture-${java.util.UUID.randomUUID()}.jpg")
                val uri = runCatching {
                    // packageName tracks applicationId at runtime:
                    // Context.getPackageName() returns the runtime applicationId
                    // including any suffix, matching the manifest's
                    // ${applicationId} authority.
                    FileProvider.getUriForFile(appTapContext, "${appTapContext.packageName}.fileprovider", file)
                }.getOrNull()
                withContext(Dispatchers.Main) {
                    if (uri == null) {
                        // Same: gate never armed on this path — notify and
                        // let the finally drop ioGuard. The homed thread
                        // goes too (#339): no capture follows this launch.
                        cameraThread = null
                        vm.notify("Couldn't start the camera.")
                        // Shared helper (dead-scope-safe), not an inline
                        // exists()/delete() on Main.
                        deleteQuietly(file)
                        return@withContext
                    }
                    // Delete-before-replace, liveness re-checked HERE: the
                    // pre-IO snapshot this replaces could go stale if the
                    // in-flight batch finished mid-hop. On Main both reads
                    // are fresh: skip only while this exact capture is
                    // actively decoding (identity, not a stale boolean).
                    if (cameraDecoding != cameraPath) {
                        cameraPath?.let(::File)?.takeIf { it.exists() }?.let { deleteQuietly(it) }
                    }
                    cameraUri = uri
                    cameraPath = file.absolutePath
                    // Owner homed at tap time (see launchCamera above): the
                    // callback reads the tap-time owner, never current state.
                    // Armed at launch, cleared in the callback: from here on the
                    // saveable gate owns the outstanding intent (survives
                    // rotation AND death). ioGuard's job ends here — a second
                    // tap now reads "already open", not silence.
                    // Guarded: if launch() itself throws (no camera activity,
                    // broken FileProvider on an odd OEM), no callback ever fires
                    // — so the gate resets and the orphan file deletes here.
                    cameraPendingSince = SystemClock.elapsedRealtime()
                    ioGuard.set(false)
                    runCatching {
                        cameraLauncher.launch(uri)
                    }.onFailure {
                        cameraPendingSince = 0L
                        cameraPath = null
                        deleteQuietly(file)
                        vm.notify("Couldn't start the camera.")
                    }
                }
            } finally {
                // IO-window guard release, cancel-safe: a rotation or
                // tab-leave mid-hop cancels this coroutine before the Main
                // hop runs — but ioGuard was armed at tap and must drop, or
                // every later tap in THIS composition reads "starting"
                // forever.
                ioGuard.set(false)
                // The homed owner (cameraThread) deliberately SURVIVES here:
                // it is the tap-time owner the result callback homes the
                // capture with. A cancel leaves it stale, which is
                // harmless — the next launchCamera overwrites it at tap.
            }
        }
    }
    // Fresh Activity at callback time: the launcher registration must not
    // pin the pre-rotation Activity for rationale routing or the Settings
    // intent — updated-state always holds the latest composition's Activity.
    val appPermContext = context.applicationContext
    val currentActivity = rememberUpdatedState(context.findActivity())
    val cameraPermission = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestPermission(),
    ) { granted ->
        // Request settled — the permission gate clears on every outcome;
        // only the launch path re-arms the *intent* gate (on grant).
        cameraPermSince = 0L
        if (granted) {
            launchCamera()
        } else if (currentActivity.value?.let {
                // Unknown (null Activity) defaults to Settings: without an
                // Activity there is no rationale UI to consult, and the
                // re-toast branch would loop forever.
                androidx.core.app.ActivityCompat.shouldShowRequestPermissionRationale(
                    it, Manifest.permission.CAMERA,
                )
            } != true && cameraWasAskedAtRequest
        ) {
            // Permanent denial ("Don't allow again"): the request UI will
            // never show again, so a re-toast loops forever. The only path
            // forward is app Settings — offered as a confirm dialog
            // (below), never auto-opened.
            showCameraSettings = true
        } else {
            // State notice like every other attach refusal.
            vm.notify("Camera permission is needed to take photos.")
        }
    }
    fun requestCamera() {
        // Hardware gate first: on a camera-less device (kept installable by
        // uses-feature required=false) a permission prompt before the refusal
        // is a pointless dialog — refuse directly. Application context for
        // both resolves: the tap handler never touches the Activity.
        val appReqContext = context.applicationContext
        if (!appReqContext.packageManager.hasSystemFeature(PackageManager.FEATURE_CAMERA_ANY)) {
            vm.notify("No camera on this device — use Choose photos instead.", ToastKind.Info)
            return
        }
        if (ContextCompat.checkSelfPermission(appReqContext, Manifest.permission.CAMERA) ==
            PackageManager.PERMISSION_GRANTED
        ) {
            launchCamera()
        } else if (isCameraPermPending()) {
            // Double-request gate: a second tap while the permission dialog
            // is outstanding would otherwise fire two permission requests
            // (OEM-dependent second-request behavior). Voiced, not silent: the
            // first tap's dialog IS up answering, but a tap with zero feedback
            // reads dead. This guards the request window only.
            vm.notify("Camera permission request is already open — answer it first.", ToastKind.Info)
        } else {
            // Permission request armed in its OWN gate: the intent gate stays
            // disarmed here — arming it would make a restored permission arm
            // block Take-photo taps with the wrong "already open" copy. Cleared
            // on settle in the callback above; the stale bound covers
            // abandonment. Asked flag arms alongside for rationale routing,
            // with the pre-request value snapshotted.
            cameraPermSince = SystemClock.elapsedRealtime()
            cameraWasAskedAtRequest = cameraAskedBefore
            cameraAskedBefore = true
            // Guarded like the camera intent: the callback clears the gate
            // on settle, but a launch() throw (no handler on an odd OEM)
            // fires no callback — so reset + voice here instead of sticking
            // the 10-min gate with no dialog open.
            runCatching {
                cameraPermission.launch(Manifest.permission.CAMERA)
            }.onFailure {
                cameraPermSince = 0L
                // The asked-before flag resets too (#339): the request never
                // happened, so the NEXT denial is still a first denial and
                // must route to the toast, not to Settings.
                cameraAskedBefore = false
                vm.notify("Couldn't ask for camera permission.")
            }
        }
    }
    // ViewModel notices (attach caps, drops, discards) surface as toasts, not
    // the error banner: the banner's Retry resends a TURN, which is wrong for
    // a staging problem. Head-first through the queue: two rapid notifies both
    // surface instead of the second overwriting the first. Consume-BEFORE-toast:
    // a rotation landing between show and consume would otherwise replay the
    // head in the new composition. Keyed on the entry (text+kind+seq) so
    // identical repeats re-fire. runCatching, not bare: a toast-path throw
    // would eat the notice AND kill this effect.
    val headNotice = state.notices.firstOrNull()
    LaunchedEffect(headNotice) {
        if (headNotice != null) {
            // Toast FIRST, consume after (#339): the other order loses the
            // notice when a rotation lands between the two. A rotation in
            // between may then show it twice — a duplicate toast is a much
            // smaller loss than a silent one.
            runCatching { toasts.toast(headNotice.text, headNotice.kind) }
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
        onPending = vm::onPending,
        slash = state.slash.takeIf { slashQuery(state.pending) != null },
        onPickSkill = vm::pickSkill, onDismissSlash = vm::dismissSlash, onSend = {
            // Staging empties on send: the next live sentence starts a new
            // voicing episode. Queued-while-streaming keeps staging.
            liveStagedVoicedCount = 0
            vm.send()
        }, onStop = vm::stop,
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
        onPickPhotos = {
            // Double-launch guard (#339): the camera path has one, the
            // gallery did not — a second tap opened a second picker and
            // the two results interleaved.
            if (isGalleryPending()) {
                vm.notify("Photo picker is already open.", ToastKind.Info)
                return@ChatScreen
            }
            // Home snapshot LIVE at tap time: the callback runs post-switch,
            // so the tap — not current-at-result — owns the batch.
            // vm.threadIdNow(), not the composition snapshot: state.threadId
            // can lag the VM by a recomposition, homing the batch to the old
            // thread and dropping good picks for a switch that already happened.
            galleryThread = vm.threadIdNow()
            // Guarded like the camera/permission intents: a launch() throw
            // fires no callback — so drop the homed thread + voice here
            // instead of crashing.
            runCatching {
                galleryLauncher.launch("image/*")
            }.onFailure {
                galleryThread = null
                vm.notify("Couldn't open the photo picker.")
            }.onSuccess {
                // Armed only after a successful launch: a throw fires no
                // callback, so the gate must not stick.
                galleryPendingSince = SystemClock.elapsedRealtime()
            }
        },
        onTakePhoto = ::requestCamera,
        onRemovePhoto = { id, stagedIndex ->
            // Evict by the SAME key the strip built (#339): the legacy key
            // carries the staged slot, so rebuilding it with slot 0 evicted
            // the wrong bitmap. The payload comes from the LIVE list — the
            // strip's index is a composition snapshot, and a batch landing
            // between recomposition and tap shifts it. A MISS skips the
            // evict: guessing from the snapshot could free the survivor's
            // bitmap and leave the removed one cached.
            val a = if (id.isBlank()) {
                // Legacy (no id): position only, from the LIVE list.
                vm.pendingAttachmentAt(stagedIndex)
            } else {
                // Stable id: by id, or nothing — a miss must not fall back
                // to a position, which could free the survivor's bitmap.
                vm.pendingAttachmentById(id)
            }
            if (a != null) {
                thumbCache.remove(thumbCacheKey(a.id, stagedIndex, a.base64, state.threadId))
            }
            // Staging may empty here: reset the slate when it does so the
            // next episode voices anew (see onLiveStagedPhotos).
            // Removed by stable pick id, not staged index: the strip's index
            // comes from a composition snapshot, and a batch landing between
            // recomposition and tap shifts indices — removing by position then
            // drops the WRONG photo. Ids are minted per pick and immutable, so
            // the id names the photo regardless of intervening stages. A
            // LEGACY payload has no id (every legacy pick shares the empty
            // one), so that one goes by index (#339).
            if (id.isBlank()) {
                vm.removePendingAttachmentAt(stagedIndex)
            } else {
                vm.removePendingAttachmentById(id)
            }
            if (vm.pendingAttachmentCount() == 0) liveStagedVoicedCount = 0
        },
        // Voiced once per staging episode, not per sentence: rapid
        // multi-sentence speech would otherwise queue identical "photo still
        // staged" notices back to back. Re-voices when the staged COUNT
        // changes; the slate clears when staging empties.
        onLiveStagedPhotos = {
            val n = vm.pendingAttachmentCount()
            if (n > 0 && n != liveStagedVoicedCount) {
                liveStagedVoicedCount = n
                vm.notify(
                    "Heard you — photo still staged, tap send to include it.",
                    ToastKind.Info,
                )
            } else if (n == 0) {
                liveStagedVoicedCount = 0
            }
        },
        onAppendPending = vm::appendPending,
        hasStagedPhotos = { vm.pendingAttachmentCount() > 0 },
        thumbCache = thumbCache,
    )
    // Camera permanently denied: the request UI never shows again, so the
    // dialog — not an auto-open — offers the Settings escape hatch. Explicit
    // tap required; an accidental tap must not surprise-navigate away.
    if (showCameraSettings) {
        AlertDialog(
            onDismissRequest = { showCameraSettings = false },
            title = { Text("Camera access is off") },
            text = {
                Text("Take-photo needs the camera. Open Settings to turn it back on, or use Choose photos instead.")
            },
            confirmButton = {
                TextButton(
                    onClick = {
                        showCameraSettings = false
                        // App context, not the composition Activity: the
                        // dialog may outlive a rotation, and the intent
                        // carries NEW_TASK already — no Activity needed.
                        val appDlgContext = context.applicationContext
                        runCatching {
                            appDlgContext.startActivity(
                                Intent(
                                    Settings.ACTION_APPLICATION_DETAILS_SETTINGS,
                                    Uri.fromParts("package", appDlgContext.packageName, null),
                                ).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK),
                            )
                        }
                    },
                ) { Text("Open Settings") }
            },
            dismissButton = {
                TextButton(onClick = { showCameraSettings = false }) { Text("Cancel") }
            },
        )
    }
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
                    // Failure voices: a device with no share target used to
                    // swallow the tap entirely (runCatching with no else) —
                    // the user retapped a dead button. The text is still
                    // exported, so say what failed and what to do.
                    val shared = runCatching {
                        context.startActivity(android.content.Intent.createChooser(intent, "Share conversation"))
                    }.isSuccess
                    if (!shared) {
                        toasts.toast("No app can share that conversation.", ToastKind.Error)
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
    // Application context for the prefs/catalog reads (#339): the effect's
    // network hop suspends while the sheet may be dismissed, so it must not
    // pin the Activity. UI-only work never needs the Activity here.
    val appContext = context.applicationContext
    var provider by remember { mutableStateOf("") }
    var model by remember { mutableStateOf("") }
    var effort by remember { mutableStateOf("") }
    var catalog by remember { mutableStateOf<List<ProviderOption>>(emptyList()) }
    var loading by remember { mutableStateOf(true) }
    var error by remember { mutableStateOf("") }
    LaunchedEffect(tab) {
        loading = true
        error = ""
        val prefs = appContext.getSharedPreferences(AgentoApp.PREFS_NAME, android.content.Context.MODE_PRIVATE)
        provider = (prefs.getString("provider_$tab", "") ?: "").trim()
        model = (prefs.getString("model_$tab", "") ?: "").trim()
        effort = EffortCatalog.effectiveFor(model, ChatApi(appContext).effortFor(tab, model))
        val api = ChatApi(appContext)
        catalog = runCatching {
            api.fetchCatalog(catalogPath(api)).getOrThrow()
        }.getOrElse { e ->
            error = e.message ?: e.javaClass.simpleName
            emptyList()
        }
        loading = false
    }
    fun save(p: String, m: String, e: String) {
        val api = ChatApi(appContext)
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
                    effort = EffortCatalog.effectiveFor(
                        model,
                        ChatApi(context).effortFor(tab, model),
                    )
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
            val shownEffort = EffortCatalog.effectiveFor(model, effort)
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
    /** Slash skill picker (#372): the live trigger state for the composer
     * text, gated by the text itself (ChatTab passes state.slash only
     * while `slashQuery(state.pending)` is non-null). */
    slash: SlashPickerState? = null,
    onPickSkill: (String) -> Unit = {},
    onDismissSlash: () -> Unit = {},
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
    /** Photo attachments: gallery picker, camera capture, and removing one
     * staged photo. Wired by ChatTab (launchers live there). */
    onPickPhotos: () -> Unit = {},
    onTakePhoto: () -> Unit = {},
    onRemovePhoto: (String, Int) -> Unit = { _, _ -> },
    /** Live transcript heard text while photos are staged: the text parks in
     * the composer for a confirming tap instead of auto-sending with the
     * photos. */
    onLiveStagedPhotos: () -> Unit = {},
    /** Live result accumulation: appends heard text atomically in the
     * ViewModel so rapid results can't clobber on a stale snapshot. */
    onAppendPending: (String) -> Unit = {},
    /** Live read of staged-photo state: the listener closure must not use
     * the composition snapshot (recomposition lag), so ChatTab supplies the
     * ViewModel read directly. */
    hasStagedPhotos: () -> Boolean = { false },
    /** Scroll-back thumbnail memory: hoisted in ChatTab, shared with the
     * composer strip so one bounded cache serves both. */
    thumbCache: ThumbCache? = null,
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
    // rememberSaveable, not remember (#339): a rotation mid-edit used to
    // discard the draft and the drop-turns confirmation silently.
    var editingIndex by rememberSaveable(tab) { mutableStateOf<Int?>(null) }
    var editDraft by rememberSaveable(tab) { mutableStateOf("") }
    var confirmReplace by rememberSaveable(tab) { mutableStateOf<Pair<Int, String>?>(null) }
    // Photo attach menu: gallery vs camera. Plain dialog state — the
    // pickers themselves live in ChatTab (onPickPhotos/onTakePhoto).
    // Saveable for the same reason as the editor above.
    var showAttachMenu by rememberSaveable(tab) { mutableStateOf(false) }
    // Edit state belongs to one thread: a switch or a fresh thread closes it.
    // The attach menu closes too: it is keyed on tab, so without this a
    // thread switch leaves it open over the new thread's composer. The draft
    // text clears with them: a stale draft from the old thread must not
    // linger in state. (The camera Settings dialog closes in ChatTab's effect
    // on the same key — it renders there, so its reset lives there too.)
    LaunchedEffect(state.threadId) {
        editingIndex = null
        editDraft = ""
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
        // Accumulated in the VM like live results: the callback's snapshot can
        // lag a fast typist the same way.
        if (heard.isNotEmpty()) {
            onAppendPending(heard)
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
                    // Append like every other voice path: the stale result
                    // arrives after endLive() but the user may have typed
                    // meanwhile — replacing would overwrite it with stale
                    // speech.
                    onAppendPending(heard)
                    return
                }
                if (heard.trimEnd('.', '!', '?').equals("stop", ignoreCase = true)) {
                    endLive()
                } else {
                    // Blank results (silence frames) park nothing: without
                    // this, an empty heard takes the staged branch and toasts
                    // "photo still staged" for literal silence — but the loop
                    // still re-arms.
                    if (heard.isBlank()) {
                        liveListen()
                        return
                    }
                    silentRounds = 0
                    // Staging reads come from the callback, not the snapshot:
                    // the composition state can lag by a recomposition, and a
                    // stale-empty read here takes the auto-send branch — the
                    // exact wrong-image send the gate exists to prevent.
                    val staged = hasStagedPhotos()
                    // Append when photos are staged: the confirming tap comes
                    // later, so each sentence must accumulate like the one-shot
                    // mic path below — replacing would discard every sentence
                    // but the last.
                    onAppendPending(heard)
                    // Photos need a confirming tap: live transcripts auto-send
                    // by design, but background speech firing a staged-photo
                    // turn with no confirmation is a wrong-image send. Text
                    // parks for the tap instead — but the loop must re-arm.
                    if (!staged) {
                        onSend()
                    } else {
                        onLiveStagedPhotos()
                        liveListen()
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
    // Header overflow menu (#354): Runs/Approvals/Memory live behind "More"
    // so the bar fits a 360dp phone with the title intact.
    var showHeaderMore by remember { mutableStateOf(false) }
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
                        // Ellipsis, never mid-glyph clip (#354): with seven
                        // actions the title collapsed to "C"; the overflow
                        // below frees the row, and the ellipsis is the
                        // backstop at 360dp + 1.3 font scale.
                        Text(title, maxLines = 1, overflow = TextOverflow.Ellipsis)
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
                    // Visible set mirrors chatHeaderVisibleActions (#354):
                    // Model, Conversations, Auto-read, New. Runs, Approvals
                    // and Memory live behind "More" so the title keeps its
                    // row on a 360dp phone. Keep this block in sync with the
                    // two lists — ChatHeaderTest fails otherwise.
                    IconButton(onClick = onModel, enabled = !state.streaming) {
                        Icon(Icons.Filled.Tune, contentDescription = "Model")
                    }
                    IconButton(onClick = onHistory, enabled = !state.streaming) {
                        Icon(Icons.Filled.History, contentDescription = "Conversations")
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
                    // Overflow (#354): Runs (#311), Approvals (#313), Memory
                    // (#315). The badge sums both pending counts so a held
                    // approval or proposal is never invisible while folded.
                    // Box-anchored so the menu drops from the "More" icon,
                    // not the actions row.
                    val overflowBadge =
                        chatHeaderOverflowBadgeCount(
                            state.pendingApprovalCount,
                            state.pendingMemoryCount,
                        )
                    Box {
                        BadgedBox(
                            badge = {
                                if (overflowBadge > 0) {
                                    Badge { Text(pendingCountLabel(overflowBadge)) }
                                }
                            },
                        ) {
                            IconButton(onClick = { showHeaderMore = true }) {
                                Icon(Icons.Filled.MoreVert, contentDescription = "More")
                            }
                        }
                        DropdownMenu(
                            expanded = showHeaderMore,
                            onDismissRequest = { showHeaderMore = false },
                        ) {
                        // Runs (#311): the thread's server-side turns,
                        // in-flight and finished. Gated on a loaded thread: a
                        // blank thread would list every conversation's runs
                        // unscoped.
                        DropdownMenuItem(
                            text = { Text("Runs") },
                            leadingIcon = {
                                Icon(Icons.Filled.List, contentDescription = null)
                            },
                            onClick = {
                                showHeaderMore = false
                                onRuns()
                            },
                            enabled = state.ready && state.threadId.isNotBlank(),
                        )
                        // Approvals (#313): held tool calls need an explicit
                        // decision, and a gate stalls the stream with no
                        // frames. Badged with the pending count; always
                        // enabled once ready — approvals are global, not
                        // per-thread.
                        DropdownMenuItem(
                            text = { Text("Approvals") },
                            leadingIcon = {
                                Icon(Icons.Filled.Approval, contentDescription = null)
                            },
                            trailingIcon = {
                                if (state.pendingApprovalCount > 0) {
                                    Badge {
                                        Text(pendingCountLabel(state.pendingApprovalCount))
                                    }
                                }
                            },
                            onClick = {
                                showHeaderMore = false
                                onApprovals()
                            },
                            enabled = state.ready,
                        )
                        // Memory (#315): learned facts. Badged with pending
                        // proposals awaiting review; always enabled once
                        // ready — memory is per-profile, not per-thread.
                        DropdownMenuItem(
                            text = { Text("Memory") },
                            leadingIcon = {
                                Icon(Icons.Filled.Psychology, contentDescription = null)
                            },
                            trailingIcon = {
                                if (state.pendingMemoryCount > 0) {
                                    Badge {
                                        Text(pendingCountLabel(state.pendingMemoryCount))
                                    }
                                }
                            },
                            onClick = {
                                showHeaderMore = false
                                onMemory()
                            },
                            enabled = state.ready,
                        )
                        }
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
                            // Both stop the readout first, like the IME send:
                            // a spoken reply must not keep playing over the
                            // state these buttons change.
                            if (state.activeRunId.isNotBlank()) {
                                TextButton(onClick = { tts.stop(); onSteer() }) { Text("Steer") }
                            }
                            if (state.queued.isBlank()) {
                                TextButton(onClick = { tts.stop(); onSend() }) { Text("Queue send") }
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
                    // Staged photos: thumbnails with per-photo remove. They
                    // send with the next turn even when the turn has no text;
                    // the count line states the per-turn cap up front. One
                    // wired list drives gate + row + labels.
                    val stagedSlots = state.pendingAttachments.wiredPhotosWithIndex()
                    if (stagedSlots.isNotEmpty()) {
                        Column(modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp)) {
                            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                                // Labeled by WIRED position: the staged index
                                // would read "photo 3 of 2" once blanks stop
                                // aligning the two orders — remove still
                                // targets the staged slot the ViewModel holds.
                                stagedSlots.forEachIndexed { w, (i, a) ->
                                    Box {
                                        AttachmentThumb(
                                            base64 = a.base64,
                                            label = "Staged photo ${w + 1} of " +
                                                "${stagedSlots.size}",
                                            modifier = Modifier.size(64.dp),
                                            legacySlot = i,
                                            stableId = a.id,
                                            threadKey = state.threadId,
                                            cache = thumbCache,
                                        )
                                        // Standard 48dp target: a scanner fails
                                        // anything smaller, and covering the
                                        // thumbnail corner is the lesser evil
                                        // next to an untappable remove.
                                        IconButton(
                                            onClick = { onRemovePhoto(a.id, i) },
                                            modifier = Modifier.align(Alignment.TopEnd).size(48.dp),
                                        ) {
                                            Icon(
                                                Icons.Filled.Close,
                                                contentDescription = "Remove photo ${w + 1}",
                                                modifier = Modifier.size(16.dp),
                                            )
                                        }
                                    }
                                }
                            }
                            Text(
                                "${stagedSlots.size}/$MAX_IMAGES_PER_TURN photos — " +
                                    "sent with your next message.",
                                style = MaterialTheme.typography.labelSmall,
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                                modifier = Modifier.padding(top = 2.dp),
                            )
                        }
                    }
                    // Slash skill picker (#372): anchored above the composer,
                    // never clearing text or staged photos. Rows insert
                    // `/skill:<name> ` and close; typing a complete command
                    // or leaving the `/` prefix closes it via the text gate.
                    val slashNow = slash
                    if (slashNow != null) {
                        SlashPickerCard(
                            state = slashNow,
                            onPick = onPickSkill,
                            onDismiss = onDismissSlash,
                        )
                    }
                    Row(
                        modifier = Modifier.fillMaxWidth().padding(12.dp),
                        verticalAlignment = Alignment.CenterVertically,
                    ) {
                        // Photo attachments: gallery or camera, staged
                        // thumbnails above, sent as image parts with the turn.
                        // Stays live while streaming — staged photos park in
                        // the composer until the next explicit send.
                        Box {
                            // Gated on ready like send: staging work before
                            // the thread loads just parks photos that sit
                            // through the load into a "still loading" notice.
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
                                // Photos send without text: an image-only turn
                                // is a real turn. Counted wired like the cap
                                // math.
                                enabled = (state.pending.isNotBlank() ||
                                    state.pendingAttachments.wiredPhotoCount() > 0) &&
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
                            // Content includes photos: dropping an image-only
                            // turn without confirming is silent data loss.
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
                                                // Carried photos are invisible here:
                                                // the field is text-only, but
                                                // Replace/Fork resends the turn's
                                                // photos with it — say so, or an
                                                // edit to "thanks" resends images.
                                                val wiredCount = msg.attachments.wiredPhotoCount()
                                                if (wiredCount > 0) {
                                                    HintLine(
                                                        attachmentMarker(wiredCount) +
                                                            " — resends with the edited text.",
                                                    )
                                                }
                                                Spacer(modifier = Modifier.height(4.dp))
                                                Row(
                                                    horizontalArrangement = Arrangement.spacedBy(8.dp),
                                                    verticalAlignment = Alignment.CenterVertically,
                                                ) {
                                                    // Empty text allowed with photos:
                                                    // the editor reaches image-only
                                                    // resends, which send() supports.
                                                    val canResend = editDraft.isNotBlank() ||
                                                        msg.attachments.wiredPhotoCount() > 0
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
                                                // Sent photos render above the text;
                                                // an image-only turn shows no "…"
                                                // placeholder — the thumbnails ARE
                                                // the message.
                                                val wiredBubble = msg.attachments.wiredPhotos()
                                                if (wiredBubble.isNotEmpty()) {
                                                    Row(
                                                        horizontalArrangement = Arrangement.spacedBy(8.dp),
                                                    ) {
                                                        // Wired position as slot here,
                                                        // staged index in the strip:
                                                        // history has no staged slots, so
                                                        // the bubble keys legacy payloads
                                                        // on wired position. Divergent only
                                                        // for pre-id payloads.
                                                        wiredBubble.forEachIndexed { ai, a ->
                                                            AttachmentThumb(
                                                                base64 = a.base64,
                                                                label = "Attached photo ${ai + 1} of " +
                                                                    "${wiredBubble.size}",
                                                                modifier = Modifier.size(120.dp),
                                                                legacySlot = ai,
                                                                stableId = a.id,
                                                                threadKey = state.threadId,
                                                                cache = thumbCache,
                                                            )
                                                        }
                                                    }
                                                    if (msg.content.isNotBlank()) {
                                                        Spacer(modifier = Modifier.height(4.dp))
                                                    }
                                                }
                                                if (msg.content.isNotBlank() || msg.attachments.wiredPhotoCount() == 0) {
                                                    // Rendered trimEnd, stored raw:
                                                    // storage is wire-verbatim, so
                                                    // trailing newlines would render
                                                    // as blank lines — trimmed for
                                                    // display, never for the wire.
                                                    // A sent skill command renders as
                                                    // a chip plus the remainder (#372):
                                                    // still projection-only, the
                                                    // stored string is untouched.
                                                    val cmd = parseSkillCommand(msg.content)
                                                    if (cmd != null) {
                                                        SkillChip(cmd.name)
                                                        if (cmd.args.isNotBlank()) {
                                                            Spacer(modifier = Modifier.height(4.dp))
                                                            SelectionContainer {
                                                                Text(
                                                                    cmd.args.trimEnd(),
                                                                    style = MaterialTheme.typography.bodyMedium,
                                                                )
                                                            }
                                                        }
                                                    } else {
                                                        SelectionContainer {
                                                            Text(
                                                                msg.content.trimEnd().ifEmpty { "…" },
                                                                style = MaterialTheme.typography.bodyMedium,
                                                            )
                                                        }
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
            // Content, not text: image-only turns count as dropped turns,
            // like the dropsTurns gate above — otherwise the dialog reports
            // "drops 0 later messages" while discarding photos.
            val dropped = state.messages.drop(idx + 1).count { messageHasContent(it) }
            // Same invisibility as the inline editor above: the edited turn's
            // photos resend with the new text — named here so the confirmation
            // covers the full resend. Counted wired like the HintLine.
            val resendPhotos = state.messages.getOrNull(idx)?.attachments?.wiredPhotoCount() ?: 0
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

/**
 * Slash skill picker card (#372): anchored above the composer while the
 * composer text holds a `/` trigger. Rows show name + description over
 * the Pi-loaded inventory only (taught drafts never reach the list —
 * see [invocableSkills]); tapping inserts `/skill:<name> ` and closes.
 * A failed fetch degrades to the error line — typing the command by hand
 * still works, and the card never blocks the composer.
 */
@Composable
internal fun SlashPickerCard(
    state: SlashPickerState,
    onPick: (String) -> Unit,
    onDismiss: () -> Unit,
) {
    val rows = remember(state.query, state.skills) {
        state.skills.filter { it.matches(state.query) }
    }
    Card(modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp)) {
        Column(modifier = Modifier.padding(vertical = 4.dp)) {
            Row(
                modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp, vertical = 4.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Text(
                    if (state.query.isEmpty()) "Skills" else "Skills matching “${state.query}”",
                    style = MaterialTheme.typography.labelLarge,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.weight(1f),
                )
                TextButton(onClick = onDismiss) { Text("Close") }
            }
            if (state.loading && rows.isEmpty()) {
                LinearProgressIndicator(modifier = Modifier.fillMaxWidth())
            }
            // A failed refresh keeps the stale list AND says so: the error
            // is a banner above the rows, never a replacement that hides
            // the still-usable cached list.
            if (state.error.isNotEmpty()) {
                Text(
                    "Couldn't load skills — showing cached list. Type the command by hand if it's missing.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.error,
                    modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp),
                )
            }
            if (!state.loading && rows.isEmpty() && state.error.isEmpty()) {
                Text(
                    "No matching skills.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp),
                )
            } else if (rows.isNotEmpty()) {
                // Capped: the inventory is small, but the card sits over
                // the transcript — eight rows, then the query narrows it.
                rows.take(8).forEach { s ->
                    Surface(
                        onClick = { onPick(s.name) },
                        modifier = Modifier.fillMaxWidth(),
                        color = MaterialTheme.colorScheme.surface,
                    ) {
                        Column(modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp)) {
                            Text(
                                "/skill:${s.name}",
                                style = MaterialTheme.typography.bodyMedium,
                            )
                            if (s.description.isNotEmpty()) {
                                Text(
                                    s.description,
                                    style = MaterialTheme.typography.bodySmall,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                    maxLines = 2,
                                    overflow = TextOverflow.Ellipsis,
                                )
                            }
                        }
                    }
                }
            }
        }
    }
}

/**
 * Render-time skill chip (#372): a sent `/skill:<name>` turn shows
 * `skill: <name>` plus the remainder as normal text. Syntax-only
 * ([parseSkillCommand]) — the render path never consults the network, and
 * a mistyped name stays visible in the chip instead of silently passing.
 */
@Composable
internal fun SkillChip(name: String) {
    Surface(
        shape = RoundedCornerShape(8.dp),
        color = MaterialTheme.colorScheme.secondaryContainer,
    ) {
        Text(
            "skill: $name",
            style = MaterialTheme.typography.labelLarge,
            color = MaterialTheme.colorScheme.onSecondaryContainer,
            modifier = Modifier.padding(horizontal = 10.dp, vertical = 4.dp),
        )
    }
}

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
