@file:OptIn(ExperimentalMaterial3Api::class)

package com.vishnu.agento

/**
 * VPS exports browser (#163, last of four moves).
 *
 * Moved verbatim out of MainActivity.kt; the only changes are
 * visibility (`private` to `internal`, same package) and the import
 * list, which carries exactly what this file uses.
 */
import android.net.Uri
import android.provider.OpenableColumns
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.grid.items
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Description
import androidx.compose.material.icons.filled.Folder
import androidx.compose.material.icons.filled.List
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
/** VPS files shelf with breadcrumbs + mobile downloads + uploads (#26, #340). */
@Composable
internal fun StorageScreen(onMenu: () -> Unit = {}) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    val toasts = remember(scope) { ToastCenter(scope) }
    var path by remember { mutableStateOf("") }
    var dirs by remember { mutableStateOf<List<RemoteEntry>>(emptyList()) }
    var files by remember { mutableStateOf<List<RemoteEntry>>(emptyList()) }
    var error by remember { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }
    var uploading by remember { mutableStateOf(false) }

    fun load(p: String) {
        busy = true
        error = ""
        scope.launch {
            StorageApi(context).list(p).fold(
                onSuccess = { (cur, d, f) ->
                    path = cur.ifEmpty { "" }
                    dirs = d
                    files = f
                },
                onFailure = { e -> error = e.message ?: e.javaClass.simpleName },
            )
            busy = false
        }
    }

    // Uploads go through the system picker (OpenDocument: no storage
    // permission needed, same SAF shape as Settings import) into the current
    // folder. The server confines, caps (25 MiB) and never replaces; every
    // refusal is a named error the toast shows verbatim-shaped via
    // friendlyError.
    fun uploadFrom(uri: Uri) {
        uploading = true
        error = ""
        // The target folder is captured now, not read at completion: a
        // navigation mid-upload must not retarget the refresh.
        val target = path
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
                        StorageApi(context).upload(target, displayName, mime, size) {
                            cr.openInputStream(uri)
                        }.getOrThrow()
                    }
                }
                result.fold(
                    onSuccess = {
                        toasts.toast("Uploaded ${it.name}", ToastKind.Success)
                        load(target)
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

    LaunchedEffect(Unit) { load("") }

    val crumbs = remember(path) {
        if (path.isEmpty()) emptyList()
        else path.split("/").scan("") { acc, part ->
            if (acc.isEmpty()) part else "$acc/$part"
        }.drop(1)
    }

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
                    if (path.isNotEmpty()) {
                        TextButton(onClick = {
                            val parent = if ("/" in path) path.substringBeforeLast("/") else ""
                            load(parent)
                        }) { Text("Up") }
                    }
                    IconButton(
                        onClick = { picker.launch(arrayOf("*/*")) },
                        enabled = !uploading,
                    ) {
                        Icon(Icons.Filled.Upload, contentDescription = "Upload")
                    }
                    IconButton(onClick = { load(path) }, enabled = !busy && !uploading) {
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
                if (path.isNotEmpty()) {
                    Row(
                        modifier = Modifier.fillMaxWidth()
                            .horizontalScroll(rememberScrollState())
                            .padding(horizontal = 12.dp, vertical = 4.dp),
                        verticalAlignment = Alignment.CenterVertically,
                    ) {
                        TextButton(onClick = { load("") }) { Text("Files") }
                        crumbs.forEach { c ->
                            Text(" / ", color = MaterialTheme.colorScheme.onSurfaceVariant)
                            TextButton(onClick = { load(c) }) {
                                Text(c.substringAfterLast("/"), maxLines = 1)
                            }
                        }
                    }
                }
                if (busy || uploading) {
                    LinearProgressIndicator(modifier = Modifier.fillMaxWidth())
                }
                if (error.isNotEmpty()) {
                    ErrorCard(raw = error, onRetry = { load(path) },
                        modifier = Modifier.padding(12.dp))
                }
                if (!busy && !uploading && dirs.isEmpty() && files.isEmpty() && error.isEmpty()) {
                    EmptyState(
                        icon = Icons.Filled.Folder,
                        title = "This folder is empty",
                        subtitle = "Use Upload to park a file here, or drop files into the VPS exports folder to see them.",
                        actionLabel = "Refresh",
                        onAction = { load(path) },
                    )
                }
                LazyColumn(
                    modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp),
                    verticalArrangement = Arrangement.spacedBy(8.dp),
                    contentPadding = PaddingValues(vertical = 8.dp),
                ) {
                    items(dirs, key = { "d:" + it.name }) { d ->
                        Card(
                            onClick = { load(if (path.isEmpty()) d.name else "$path/${d.name}") },
                            modifier = Modifier.fillMaxWidth(),
                        ) {
                            Row(
                                modifier = Modifier.padding(12.dp),
                                verticalAlignment = Alignment.CenterVertically,
                            ) {
                                Icon(Icons.Filled.Folder, contentDescription = null)
                                Spacer(modifier = Modifier.width(8.dp))
                                Text(d.name, style = MaterialTheme.typography.bodyLarge)
                            }
                        }
                    }
                    items(files, key = { "f:" + it.name }) { f ->
                        Card(modifier = Modifier.fillMaxWidth()) {
                            Row(
                                modifier = Modifier.padding(12.dp),
                                verticalAlignment = Alignment.CenterVertically,
                            ) {
                                Icon(Icons.Filled.Description, contentDescription = null)
                                Spacer(modifier = Modifier.width(8.dp))
                                Column(modifier = Modifier.weight(1f)) {
                                    Text(f.name, style = MaterialTheme.typography.bodyLarge)
                                    Text(
                                        humanSize(f.size),
                                        style = MaterialTheme.typography.bodySmall,
                                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                                    )
                                }
                                TextButton(onClick = {
                                    runCatching {
                                        StorageApi(context).download(path, f.name)
                                        toasts.toast("Saving ${f.name}…", ToastKind.Success)
                                    }.onFailure { e ->
                                        toasts.toast(
                                            friendlyError(e.message ?: "").title,
                                            ToastKind.Error,
                                        )
                                    }
                                }) { Text("Save") }
                            }
                        }
                    }
                }
            }
        }
        ToastHost(center = toasts, modifier = Modifier.fillMaxWidth())
    }
    }
}
