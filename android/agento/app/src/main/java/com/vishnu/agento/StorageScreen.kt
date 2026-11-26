@file:OptIn(ExperimentalMaterial3Api::class)

package com.vishnu.agento

/**
 * VPS exports browser (#163, last of four moves): a readable tree with
 * per-file download + delete (#583).
 *
 * Folders expand inline (one server list per folder, fetched on first
 * expansion and cached), files show an icon + name + size with download
 * and delete icon buttons. Deletes confirm first, then refresh the
 * parent folder. Uploads land at the root; the tree shows them after the
 * refresh.
 */
import android.net.Uri
import android.provider.OpenableColumns
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.grid.items
import androidx.compose.foundation.lazy.items
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.CloudDownload
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.Description
import androidx.compose.material.icons.filled.Folder
import androidx.compose.material.icons.filled.KeyboardArrowDown
import androidx.compose.material.icons.filled.KeyboardArrowRight
import androidx.compose.material.icons.filled.Menu
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material.icons.filled.Upload
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

/** One folder's cached listing: subfolders first, then files. */
internal data class DirListing(
    val dirs: List<RemoteEntry>,
    val files: List<RemoteEntry>,
)

/** One visible tree row, flattened with its indent depth. */
internal sealed interface TreeRow {
    data class Dir(val path: String, val name: String, val depth: Int) : TreeRow
    data class File(val dir: String, val entry: RemoteEntry, val depth: Int) : TreeRow
}

/** Flattens the cached tree into visible rows: every folder lists its
 * subfolders (then its files), and an open folder's rows follow it one
 * level deeper. Folders with no cached listing contribute nothing — the
 * row is only drawn once its list has landed. */
internal fun flattenTree(
    tree: Map<String, DirListing>,
    open: Set<String>,
): List<TreeRow> {
    val out = mutableListOf<TreeRow>()
    fun walk(path: String, depth: Int) {
        val listing = tree[path] ?: return
        listing.dirs.forEach { d ->
            val p = if (path.isEmpty()) d.name else "$path/${d.name}"
            out.add(TreeRow.Dir(p, d.name, depth))
            if (p in open) walk(p, depth + 1)
        }
        listing.files.forEach { f -> out.add(TreeRow.File(path, f, depth)) }
    }
    walk("", 0)
    return out
}

@Composable
internal fun StorageScreen(onMenu: () -> Unit = {}) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    val toasts = remember(scope) { ToastCenter(scope) }
    // Path -> cached listing, fetched on first expansion. Keys are the
    // slash-joined relative paths the server lists by ("" = root).
    var tree by remember { mutableStateOf(mapOf<String, DirListing>()) }
    var openDirs by remember { mutableStateOf(setOf<String>()) }
    // One folder's fetch in flight: the row shows a spinner of its own, so
    // expanding a folder never blanks the rows already on screen.
    var loadingDirs by remember { mutableStateOf(setOf<String>()) }
    var error by remember { mutableStateOf("") }
    var uploading by remember { mutableStateOf(false) }
    // (dir, file) awaiting delete confirmation.
    var deleting by remember { mutableStateOf<Pair<String, RemoteEntry>?>(null) }

    // Folders whose listing changed while a fetch was in flight (a delete
    // or upload landing mid-load): the in-flight list was snapshotted
    // before the change, so it is refetched once it lands instead of
    // settling stale.
    var pendingReload by remember { mutableStateOf(setOf<String>()) }

    // Drops a folder and everything under it from the cached tree, the
    // open set and the queued refetches: the VPS removed it out from under
    // the app (a 404/400 on refetch), so keeping it would re-request a
    // path that can never list again — every future Refresh would toast
    // the same failure with no row left to collapse.
    fun pruneTree(p: String) {
        val cut = setOf(p) + tree.keys.filter { it.startsWith("$p/") }
        tree = tree - cut
        openDirs = openDirs - cut
        pendingReload = pendingReload - cut
    }

    // True for refusals that describe the path, not the moment: the folder
    // is gone (or its chain broke), so retrying cannot help. Transient
    // failures (auth, 5xx, network) keep their cache entries.
    fun isGone(msg: String) = "HTTP 404" in msg || "invalid path" in msg

    fun loadDir(p: String, force: Boolean = false) {
        // A cached folder opens without refetching; a refresh passes force
        // (delete, upload, manual refresh).
        if (!force && p in tree) {
            openDirs = openDirs + p
            return
        }
        // A forced refresh that arrives mid-load is queued, not dropped:
        // dropping it would land the pre-change snapshot as the settled
        // list (a deleted file reappearing, an upload missing).
        if (p in loadingDirs) {
            if (force) pendingReload = pendingReload + p
            return
        }
        loadingDirs = loadingDirs + p
        if (p.isEmpty()) error = ""
        scope.launch {
            StorageApi(context).list(p).fold(
                onSuccess = { (_, d, f) ->
                    tree = tree + (p to DirListing(d, f))
                    openDirs = openDirs + p
                },
                onFailure = { e ->
                    val msg = e.message ?: e.javaClass.simpleName
                    // A failed folder keeps its collapsed row in place and
                    // says so out loud — unless the path itself is gone, in
                    // which case its rows are pruned so Refresh stops
                    // re-requesting them. A failed root replaces the list
                    // with the error card on first load, but toasts once
                    // rows are already on screen — otherwise a manual
                    // refresh on a flaky network would fail silently under
                    // a stale tree.
                    if (p.isNotEmpty() && isGone(msg)) {
                        pruneTree(p)
                        val fe = friendlyError(msg)
                        toasts.toast("${fe.title} — ${fe.nextStep}", ToastKind.Error)
                    } else if (p.isEmpty() && "" !in tree) error = msg
                    else {
                        val fe = friendlyError(msg)
                        toasts.toast("${fe.title} — ${fe.nextStep}", ToastKind.Error)
                    }
                },
            )
            loadingDirs = loadingDirs - p
            if (p in pendingReload) {
                pendingReload = pendingReload - p
                loadDir(p, force = true)
            }
        }
    }

    fun doDownload(dir: String, f: RemoteEntry) {
        // DownloadManager reports HTTP failures (wrong password, vanished
        // file) asynchronously in its own notification — enqueueing throws
        // only for local failures. So auth and existence are confirmed
        // first, where a refusal still toasts with recovery guidance.
        scope.launch {
            val res = StorageApi(context).list(dir)
            val files = res.getOrElse { e ->
                val fe = friendlyError(e.message ?: "")
                toasts.toast("${fe.title} — ${fe.nextStep}", ToastKind.Error)
                return@launch
            }.third
            if (files.none { it.name == f.name }) {
                toasts.toast(
                    "“${f.name}” is no longer on the server — refreshing.",
                    ToastKind.Error,
                )
                loadDir(dir, force = true)
                return@launch
            }
            runCatching { StorageApi(context).download(dir, f.name) }.onFailure { e ->
                val fe = friendlyError(e.message ?: "")
                toasts.toast("${fe.title} — ${fe.nextStep}", ToastKind.Error)
            }
        }
    }

    fun doDelete(dir: String, f: RemoteEntry) {
        deleting = null
        scope.launch {
            StorageApi(context).delete(dir, f.name).fold(
                onSuccess = {
                    val where = if (dir.isEmpty()) "Exports root" else dir
                    toasts.toast("Deleted “$it” in $where", ToastKind.Success)
                    loadDir(dir, force = true)
                },
                onFailure = { e ->
                    val fe = friendlyError(e.message ?: "")
                    toasts.toast("${fe.title} — ${fe.nextStep}", ToastKind.Error)
                },
            )
        }
    }

    // Uploads go through the system picker (OpenDocument: no storage
    // permission needed, same SAF shape as Settings import) and always land
    // at the root (#583): per-folder targets need a per-row affordance that
    // does not exist yet. The server confines, caps (25 MiB) and never
    // replaces; every refusal is a named error the toast shows
    // verbatim-shaped via friendlyError.
    fun uploadFrom(uri: Uri) {
        uploading = true
        error = ""
        // Grabbed up front on Main (cheap handle, not IO): the blocking
        // query/open below must not re-reach into the Activity per call.
        val cr = context.contentResolver
        scope.launch {
            try {
                // Metadata reads ride IO: SAF queries can block on a slow
                // provider, and this launcher runs on Main. One runCatching
                // over metadata AND the upload, so a server refusal keeps
                // its named error instead of collapsing to generic text.
                val result = withContext(Dispatchers.IO) {
                    runCatching {
                        var displayName = "upload"
                        var size = -1L
                        // Narrow projection: only the two columns read below.
                        cr.query(
                            uri,
                            arrayOf(OpenableColumns.DISPLAY_NAME, OpenableColumns.SIZE),
                            null, null, null,
                        )?.use { c ->
                            val ni = c.getColumnIndex(OpenableColumns.DISPLAY_NAME)
                            val si = c.getColumnIndex(OpenableColumns.SIZE)
                            if (c.moveToFirst()) {
                                if (ni >= 0) {
                                    c.getString(ni)?.trim()?.takeIf { it.isNotEmpty() }?.let {
                                        displayName = it
                                    }
                                }
                                // NULL size means unknown, not zero: getLong
                                // on NULL yields 0, which would declare an
                                // empty body while streaming real bytes.
                                if (si >= 0 && !c.isNull(si)) size = c.getLong(si)
                            }
                        }
                        // Separators cannot survive the server's name check; fold them
                        // here so a trivially fixable name never fails the upload.
                        displayName = displayName.replace('/', '_').replace('\\', '_')
                        if (size > FILES_UPLOAD_LIMIT_BYTES) {
                            throw IllegalStateException(
                                "file_too_large: this file is over the 25 MiB upload limit")
                        }
                        val mime = cr.getType(uri) ?: "application/octet-stream"
                        StorageApi(context).upload("", displayName, mime, size) {
                            cr.openInputStream(uri)
                        }.getOrThrow()
                    }
                }
                result.fold(
                    onSuccess = {
                        toasts.toast("Uploaded ${it.name}", ToastKind.Success)
                        loadDir("", force = true)
                    },
                    onFailure = { e ->
                        // Refusals are the expected path here, not an edge
                        // case: the toast carries the guidance (rename?
                        // shrink?), not just the headline.
                        val fe = friendlyError(e.message ?: "")
                        toasts.toast("${fe.title} — ${fe.nextStep}", ToastKind.Error)
                    },
                )
            } finally {
                // Stuck-disabled Upload (or a wedged progress bar) after a
                // throw outside the Result wrappers must be impossible.
                uploading = false
            }
        }
    }

    val picker = rememberLauncherForActivityResult(
        ActivityResultContracts.OpenDocument(),
    ) { uri: Uri? ->
        if (uri != null) uploadFrom(uri)
    }

    LaunchedEffect(Unit) { loadDir("", force = true) }

    val rows = remember(tree, openDirs) { flattenTree(tree, openDirs) }
    val rootLoaded = "" in tree

    // The toast card floats top-end over the content (Box overlay, not
    // the bottom-anchored Scaffold slot) so it never shifts the layout.
    Box(modifier = Modifier.fillMaxSize()) {
    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text("Files", maxLines = 1) },
                navigationIcon = {
                    IconButton(onClick = onMenu) {
                        Icon(Icons.Filled.Menu, contentDescription = "Menu")
                    }
                },
                actions = {
                    IconButton(
                        onClick = { picker.launch(arrayOf("*/*")) },
                        enabled = !uploading,
                    ) {
                        Icon(Icons.Filled.Upload, contentDescription = "Upload")
                    }
                    IconButton(
                        // Refetches the root and every open folder: cached
                        // subfolders would otherwise go stale behind a
                        // fresh root. Closed folders drop their listings,
                        // so the next expansion refetches instead of
                        // serving a cache the VPS may have changed.
                        onClick = {
                            tree = tree.filterKeys { it.isEmpty() || it in openDirs }
                            loadDir("", force = true)
                            openDirs.forEach { p ->
                                if (p.isNotEmpty()) loadDir(p, force = true)
                            }
                        },
                        enabled = !uploading && loadingDirs.isEmpty(),
                    ) {
                        Icon(Icons.Filled.Refresh, contentDescription = "Refresh")
                    }
                },
            )
        },
    ) { padding ->
        Column(
            modifier = Modifier.fillMaxSize().padding(padding),
            horizontalAlignment = Alignment.CenterHorizontally,
        ) {
            Column(modifier = Modifier.contentWidth(WindowClass.Compact)) {
                if (uploading || loadingDirs.isNotEmpty()) {
                    LinearProgressIndicator(modifier = Modifier.fillMaxWidth())
                }
                HintLine(
                    "Uploads land in the Exports root.",
                    modifier = Modifier.padding(horizontal = 12.dp),
                )
                if (error.isNotEmpty() && !rootLoaded) {
                    ErrorCard(raw = error, onRetry = { loadDir("", force = true) },
                        modifier = Modifier.padding(12.dp))
                }
                if (rootLoaded && rows.isEmpty() && error.isEmpty()) {
                    EmptyState(
                        icon = Icons.Filled.Folder,
                        title = "Exports is empty",
                        subtitle = "Use Upload to park a file here, or drop files into the VPS exports folder to see them.",
                        actionLabel = "Refresh",
                        onAction = { loadDir("", force = true) },
                    )
                }
                LazyColumn(
                    modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp),
                    verticalArrangement = Arrangement.spacedBy(8.dp),
                    contentPadding = PaddingValues(vertical = 8.dp),
                ) {
                    items(rows, key = {
                        when (it) {
                            is TreeRow.Dir -> "d:${it.path}"
                            is TreeRow.File -> "f:${it.dir}/${it.entry.name}"
                        }
                    }) { row ->
                        when (row) {
                            is TreeRow.Dir -> {
                                val open = row.path in openDirs
                                Card(
                                    onClick = {
                                        if (open) openDirs = openDirs - row.path
                                        else loadDir(row.path)
                                    },
                                    modifier = Modifier.fillMaxWidth()
                                        .padding(start = (row.depth * 20).dp),
                                ) {
                                    Row(
                                        modifier = Modifier.padding(12.dp),
                                        verticalAlignment = Alignment.CenterVertically,
                                    ) {
                                        Icon(
                                            if (open) Icons.Filled.KeyboardArrowDown
                                            else Icons.Filled.KeyboardArrowRight,
                                            contentDescription =
                                                if (open) "Collapse ${row.name}"
                                                else "Expand ${row.name}",
                                        )
                                        Icon(Icons.Filled.Folder, contentDescription = null)
                                        Spacer(modifier = Modifier.width(8.dp))
                                        Text(
                                            row.name,
                                            style = MaterialTheme.typography.bodyLarge,
                                            modifier = Modifier.weight(1f),
                                        )
                                        if (row.path in loadingDirs) {
                                            CircularProgressIndicator(
                                                modifier = Modifier.size(20.dp),
                                                strokeWidth = 2.dp,
                                            )
                                        }
                                    }
                                }
                            }
                            is TreeRow.File -> {
                                Card(
                                    modifier = Modifier.fillMaxWidth()
                                        .padding(start = (row.depth * 20).dp),
                                ) {
                                    Row(
                                        modifier = Modifier.padding(12.dp),
                                        verticalAlignment = Alignment.CenterVertically,
                                    ) {
                                        Icon(Icons.Filled.Description, contentDescription = null)
                                        Spacer(modifier = Modifier.width(8.dp))
                                        Column(modifier = Modifier.weight(1f)) {
                                            Text(
                                                row.entry.name,
                                                style = MaterialTheme.typography.bodyLarge,
                                            )
                                            Text(
                                                humanSize(row.entry.size),
                                                style = MaterialTheme.typography.bodySmall,
                                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                                            )
                                        }
                                        IconButton(onClick = {
                                            doDownload(row.dir, row.entry)
                                        }) {
                                            Icon(
                                                Icons.Filled.CloudDownload,
                                                contentDescription = "Download ${row.entry.name}",
                                            )
                                        }
                                        IconButton(onClick = {
                                            deleting = row.dir to row.entry
                                        }) {
                                            Icon(
                                                Icons.Filled.Delete,
                                                contentDescription = "Delete ${row.entry.name}",
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
        ToastHost(center = toasts, modifier = Modifier.fillMaxWidth())
    }
    }

    val target = deleting
    if (target != null) {
        AlertDialog(
            onDismissRequest = { deleting = null },
            title = { Text("Delete file?") },
            text = {
                // Names the parent folder: the tree row had indent context,
                // and IMG_001-style names repeat across folders.
                val where = if (target.first.isEmpty()) "Exports root"
                else target.first
                Text("“${target.second.name}” in $where will be permanently deleted.")
            },
            confirmButton = {
                TextButton(onClick = { doDelete(target.first, target.second) }) {
                    Text("Delete")
                }
            },
            dismissButton = {
                TextButton(onClick = { deleting = null }) { Text("Cancel") }
            },
        )
    }
}
