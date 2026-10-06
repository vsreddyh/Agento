@file:OptIn(ExperimentalMaterial3Api::class)

package com.vishnu.agento

/**
 * Tools inventory UI (#163, last of four moves).
 *
 * Moved verbatim out of MainActivity.kt; the only changes are
 * visibility (`private` to `internal`, same package) and the import
 * list, which carries exactly what this file uses.
 */
import android.os.Build
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.grid.items
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Build
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.ExpandLess
import androidx.compose.material.icons.filled.ExpandMore
import androidx.compose.material.icons.filled.List
import androidx.compose.material.icons.filled.Menu
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material.icons.filled.Search
import androidx.compose.material.icons.filled.Sort
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import java.util.Locale
import kotlinx.coroutines.Job
import kotlinx.coroutines.ensureActive
import kotlinx.coroutines.launch
/** Sort order for the Tools section lists. */
internal enum class ToolsSort(val title: String) {
    NameAz("Name A–Z"),
    NameZa("Name Z–A"),
    MostTools("Most tools"),
}
/** One toolset row: label + name + description/tool list + state badges.
 * Tap expands the full description and the complete tool list (#123);
 * collapsed caps at the 10-tool summary. Off toolsets render an Off badge
 * (nothing is filtered); an explicit `configured: false` adds a
 * "Not configured" badge, invisible otherwise. Expansion is keyed by
 * profile+name so switching assistants never leaks open rows. */
@Composable
internal fun ToolsetCard(profile: String, ts: ToolsetInfo) {
    var open by remember(profile, ts.name) { mutableStateOf(false) }
    Card(
        onClick = { open = !open },
        modifier = Modifier.fillMaxWidth(),
    ) {
        Row(
            modifier = Modifier.fillMaxWidth().padding(12.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Column(modifier = Modifier.weight(1f)) {
                Text(ts.label.ifEmpty { ts.name },
                    style = MaterialTheme.typography.bodyLarge)
                if (ts.label.isNotEmpty()) {
                    Text(
                        ts.name,
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
                val blurb = if (open) {
                    listOfNotNull(
                        ts.description.ifEmpty { null },
                        if (ts.tools.isEmpty()) null
                        else "Tools (${ts.tools.size}): ${ts.tools.joinToString(", ")}",
                    ).joinToString("\n")
                } else {
                    ts.description.ifEmpty {
                        if (ts.tools.isEmpty()) "" else
                            "${ts.tools.size} tool(s): " +
                                ts.tools.take(10).joinToString(", ") +
                                if (ts.tools.size > 10) "…" else ""
                    }
                }
                if (blurb.isNotEmpty()) {
                    Text(
                        blurb,
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        maxLines = if (open) Int.MAX_VALUE else 4,
                        overflow = TextOverflow.Ellipsis,
                    )
                }
            }
            Column(horizontalAlignment = Alignment.End) {
                when (ts.enabled) {
                    true -> Text(
                        "On",
                        style = MaterialTheme.typography.labelMedium,
                        color = MaterialTheme.colorScheme.primary,
                    )
                    false -> Text(
                        "Off",
                        style = MaterialTheme.typography.labelMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                    // Unknown state shows no badge rather than Off.
                    null -> { }
                }
                if (ts.configured == false) {
                    Text(
                        "Not configured",
                        style = MaterialTheme.typography.labelMedium,
                        color = MaterialTheme.colorScheme.error,
                    )
                }
                Icon(
                    if (open) Icons.Filled.ExpandLess else Icons.Filled.ExpandMore,
                    contentDescription = if (open) "Collapse" else "Expand",
                    tint = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.size(20.dp),
                )
            }
        }
    }
}

/** One derived MCP server row: name + tool list. Tap expands the full
 * tool list (#123); collapsed caps at the 8-tool summary. Expansion is
 * keyed by profile+name so switching assistants never leaks open rows. */
@Composable
internal fun McpCard(profile: String, server: McpServer) {
    var open by remember(profile, server.name) { mutableStateOf(false) }
    Card(
        onClick = { open = !open },
        modifier = Modifier.fillMaxWidth(),
    ) {
        Column(modifier = Modifier.fillMaxWidth().padding(12.dp)) {
            Row(
                modifier = Modifier.fillMaxWidth(),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Text(
                    server.name,
                    style = MaterialTheme.typography.bodyLarge,
                    modifier = Modifier.weight(1f),
                )
                Icon(
                    if (open) Icons.Filled.ExpandLess else Icons.Filled.ExpandMore,
                    contentDescription = if (open) "Collapse" else "Expand",
                    tint = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.size(20.dp),
                )
            }
            Text(
                if (server.tools.isEmpty()) "No tools listed"
                else if (open) "${server.tools.size} tool(s): ${server.tools.joinToString(", ")}"
                else "${server.tools.size} tool(s): ${server.tools.take(8).joinToString(", ")}" +
                    if (server.tools.size > 8) "…" else "",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                maxLines = if (open) Int.MAX_VALUE else 3,
                overflow = TextOverflow.Ellipsis,
            )
        }
    }
}
/** Read-only tools inventory per assistant — with
 * Tasks-style search filtering and a sort dropdown.
 * Split out of SkillsScreen into its own sidebar section (#106).
 */
@Composable
internal fun ToolsScreen(wc: WindowClass, onMenu: () -> Unit = {}) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    var profile by remember { mutableStateOf("god") }
    var toolsets by remember { mutableStateOf<List<ToolsetInfo>>(emptyList()) }
    var toolsError by remember { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }
    var loaded by remember { mutableStateOf(false) }
    // Cancelled + replaced on every load() so rapid profile taps can't
    // let a stale response win; only the latest job may clear busy.
    var loadJob by remember { mutableStateOf<Job?>(null) }

    fun load() {
        loadJob?.cancel()
        busy = true
        toolsError = ""
        loaded = false
        val path = ChatApi(context).pathFor(profile)
        var job: Job? = null
        job = scope.launch {
            try {
                val tr = ServerApi(context).listToolsets(path)
                // A cancel landing mid-await must not leave stale state.
                ensureActive()
                if (loadJob == job) {
                    toolsets = tr.getOrDefault(emptyList())
                    toolsError = tr.exceptionOrNull()?.let {
                        it.message ?: it.javaClass.simpleName
                    } ?: ""
                    loaded = true
                }
            } finally {
                if (loadJob == job) busy = false
            }
        }
        loadJob = job
    }

    LaunchedEffect(profile) { load() }

    val tabs = listOf("god" to "God", "story" to "Story", "resumes" to "Resume and Portfolio")
    // Everything stays visible (#123, consistent with Skills): explicitly-off
    // toolsets render an Off badge instead of being dropped, and unknown
    // toggle state (null) shows no badge — so flag-less server shapes never
    // blank the section.
    val listedToolsets = toolsets
    // Dedupe: explicit mcp-* toolset rows already render the server, so
    // derived rows parsed from their mcp__<server>__* tools are fallback
    // only (otherwise each server shows twice: 4 servers -> 8 rows).
    val mcp = remember(listedToolsets) {
        dedupMcpServers(listedToolsets, mcpServersFrom(listedToolsets))
    }

    // Search + sort (Tasks-style).
    var query by remember { mutableStateOf("") }
    var toolsSort by remember { mutableStateOf(ToolsSort.NameAz) }
    var toolsSortOpen by remember { mutableStateOf(false) }

    fun sortToolsets(list: List<ToolsetInfo>): List<ToolsetInfo> = when (toolsSort) {
        ToolsSort.NameZa -> list.sortedByDescending { it.label.ifEmpty { it.name }.lowercase(Locale.ROOT) }
        ToolsSort.MostTools -> list.sortedWith(
            compareByDescending<ToolsetInfo> { it.tools.size }
                .thenBy { it.label.ifEmpty { it.name }.lowercase(Locale.ROOT) }
        )
        ToolsSort.NameAz -> list.sortedBy { it.label.ifEmpty { it.name }.lowercase(Locale.ROOT) }
    }
    val shownToolsets = remember(listedToolsets, query, toolsSort) {
        sortToolsets(listedToolsets.filter { it.matches(query) })
    }
    val shownMcpServers = remember(mcp, query, toolsSort) {
        val filtered = mcp.filter { it.matches(query) }
        if (toolsSort == ToolsSort.NameZa) {
            filtered.sortedByDescending { it.name.lowercase(Locale.ROOT) }
        } else {
            filtered.sortedWith(
                compareBy({ if (toolsSort == ToolsSort.MostTools) -it.tools.size else 0 },
                    { it.name.lowercase(Locale.ROOT) })
            )
        }
    }
    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text("Tools") },
                navigationIcon = {
                    IconButton(onClick = onMenu) {
                        Icon(Icons.Filled.Menu, contentDescription = "Menu")
                    }
                },
                actions = {
                    IconButton(onClick = { load() }, enabled = !busy) {
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
            Column(modifier = Modifier.contentWidth(wc)) {
                Row(
                    modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp)
                        .horizontalScroll(rememberScrollState()),
                    horizontalArrangement = Arrangement.spacedBy(8.dp),
                ) {
                    tabs.forEach { (key, label) ->
                        FilterChip(
                            selected = profile == key,
                            onClick = { profile = key },
                            label = { Text(label) },
                        )
                    }
                }
                OutlinedTextField(
                    value = query,
                    onValueChange = { query = it },
                    placeholder = { Text("Search tools…") },
                    leadingIcon = { Icon(Icons.Filled.Search, contentDescription = null) },
                    trailingIcon = if (query.isEmpty()) null else ({
                        IconButton(onClick = { query = "" }) {
                            Icon(Icons.Filled.Close, contentDescription = "Clear search")
                        }
                    }),
                    singleLine = true,
                    modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp, vertical = 4.dp),
                )
                if (busy && !loaded) {
                    Box(modifier = Modifier.fillMaxWidth().padding(32.dp), contentAlignment = Alignment.Center) {
                        CircularProgressIndicator()
                    }
                }
                LazyColumn(
                    modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp),
                    verticalArrangement = Arrangement.spacedBy(8.dp),
                    contentPadding = PaddingValues(vertical = 8.dp),
                ) {
                    if (listedToolsets.isEmpty() && toolsError.isNotEmpty() && loaded) {
                        item {
                            ErrorCard(raw = toolsError, onRetry = { load() })
                        }
                    }
                    if (loaded && listedToolsets.isEmpty() && toolsError.isEmpty()) {
                        item {
                            EmptyState(
                                icon = Icons.Filled.Build,
                                title = "Nothing listed",
                                subtitle = "This assistant reports no toolsets.",
                                actionLabel = "Refresh",
                                onAction = { load() },
                            )
                        }
                    }
                    // ── Tools ──
                    item {
                        Text(
                            "Tools",
                            style = MaterialTheme.typography.titleMedium,
                            modifier = Modifier.padding(horizontal = 4.dp, vertical = 4.dp),
                        )
                    }
                    // Totals header (#123): inventory size as a rough
                    // context-load proxy for this assistant.
                    if (loaded && toolsError.isEmpty()) {
                        item {
                            HintLine(
                                "${listedToolsets.size} toolsets · " +
                                    "${listedToolsets.sumOf { it.tools.size }} tools · " +
                                    "${mcp.size} MCP servers"
                            )
                        }
                    }
                    item {
                        Row(
                            modifier = Modifier.fillMaxWidth()
                                .horizontalScroll(rememberScrollState()),
                            horizontalArrangement = Arrangement.spacedBy(8.dp),
                            verticalAlignment = Alignment.CenterVertically,
                        ) {
                            Box {
                                TextButton(onClick = { toolsSortOpen = true }) {
                                    Text("Sort: ${toolsSort.title}")
                                }
                                DropdownMenu(
                                    expanded = toolsSortOpen,
                                    onDismissRequest = { toolsSortOpen = false },
                                ) {
                                    ToolsSort.entries.forEach { s ->
                                        DropdownMenuItem(
                                            text = { Text(s.title) },
                                            onClick = { toolsSort = s; toolsSortOpen = false },
                                        )
                                    }
                                }
                            }
                        }
                    }
                    if (shownToolsets.isNotEmpty()) {
                        items(shownToolsets, key = { "ts:" + it.name }) { ts ->
                            ToolsetCard(profile, ts)
                        }
                    }
                    if (shownMcpServers.isNotEmpty()) {
                        item {
                            Text(
                                "Read-only — servers are configured on the gateway.",
                                style = MaterialTheme.typography.bodySmall,
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                            )
                        }
                        items(shownMcpServers, key = { "ms:" + it.name }) { server ->
                            McpCard(profile, server)
                        }
                    }
                    if (loaded && listedToolsets.isNotEmpty()
                        && shownToolsets.isEmpty()
                        && shownMcpServers.isEmpty()
                        && toolsError.isEmpty()
                    ) {
                        item {
                            HintLine("No tools match this search.")
                        }
                    }
                }
            }
        }
    }
}

