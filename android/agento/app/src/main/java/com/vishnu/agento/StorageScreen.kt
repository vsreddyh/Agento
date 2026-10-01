@file:OptIn(ExperimentalMaterial3Api::class)

package com.vishnu.agento

/**
 * VPS exports browser (#163, last of four moves).
 *
 * Moved verbatim out of MainActivity.kt; the only changes are
 * visibility (`private` to `internal`, same package) and the import
 * list, which carries exactly what this file uses.
 */
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
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.launch
/** VPS exports browser with breadcrumbs + mobile downloads (#26). */
@Composable
internal fun StorageScreen(onMenu: () -> Unit = {}) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    val snackbar = remember { SnackbarHostState() }
    var path by remember { mutableStateOf("") }
    var dirs by remember { mutableStateOf<List<RemoteEntry>>(emptyList()) }
    var files by remember { mutableStateOf<List<RemoteEntry>>(emptyList()) }
    var error by remember { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }

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

    LaunchedEffect(Unit) { load("") }

    val crumbs = remember(path) {
        if (path.isEmpty()) emptyList()
        else path.split("/").scan("") { acc, part ->
            if (acc.isEmpty()) part else "$acc/$part"
        }.drop(1)
    }

    Scaffold(
        snackbarHost = { SnackbarHost(snackbar) },
        topBar = {
            TopAppBar(
                title = { Text("Storage", maxLines = 1) },
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
                    IconButton(onClick = { load(path) }, enabled = !busy) {
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
                if (busy) {
                    LinearProgressIndicator(modifier = Modifier.fillMaxWidth())
                }
                if (error.isNotEmpty()) {
                    ErrorCard(raw = error, onRetry = { load(path) },
                        modifier = Modifier.padding(12.dp))
                }
                if (!busy && dirs.isEmpty() && files.isEmpty() && error.isEmpty()) {
                    EmptyState(
                        icon = Icons.Filled.Folder,
                        title = "This folder is empty",
                        subtitle = "Drop files into the VPS exports folder to see them here.",
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
                                        scope.launch { snackbar.showSnackbar("Saving ${f.name}…") }
                                    }.onFailure { e ->
                                        scope.launch {
                                            snackbar.showSnackbar(
                                                friendlyError(e.message ?: "").title
                                            )
                                        }
                                    }
                                }) { Text("Save") }
                            }
                        }
                    }
                }
            }
        }
    }
}
