@file:OptIn(ExperimentalMaterial3Api::class)

package com.vishnu.agento

/**
 * Skills inventory UI (#163, last of four moves).
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
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.ExpandLess
import androidx.compose.material.icons.filled.ExpandMore
import androidx.compose.material.icons.filled.Extension
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
/** Sort order for the Skills section list. */
internal enum class SkillsSort(val title: String) {
    NameAz("Name A–Z"),
    NameZa("Name Z–A"),
}
/** One skill row: name + description + On/Off badge. Tap expands
 * the full description (#123); collapsed text caps at 3 lines. Expansion
 * is keyed by profile+name so switching assistants never leaks open rows. */
@Composable
internal fun SkillCard(profile: String, s: SkillInfo) {
    var open by remember(profile, s.name) { mutableStateOf(false) }
    Card(
        onClick = { open = !open },
        modifier = Modifier.fillMaxWidth(),
    ) {
        Row(
            modifier = Modifier.fillMaxWidth().padding(12.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Column(modifier = Modifier.weight(1f)) {
                Text(s.name, style = MaterialTheme.typography.bodyLarge)
                if (s.description.isNotEmpty()) {
                    Text(
                        s.description,
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        maxLines = if (open) Int.MAX_VALUE else 3,
                        overflow = TextOverflow.Ellipsis,
                    )
                }
            }
            when (s.enabled) {
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
                null -> { }
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
/** Read-only skills inventory for the chat profile — with
 * Tasks-style search filtering and a sort dropdown.
 */
@Composable
internal fun SkillsScreen(wc: WindowClass, onMenu: () -> Unit = {}) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    // Single profile since #289.
    val profile = "chat"
    var skills by remember { mutableStateOf<List<SkillInfo>>(emptyList()) }
    var skillsError by remember { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }
    var loaded by remember { mutableStateOf(false) }
    // Taught skills (#314): user procedures with a draft → test → activate
    // lifecycle, managed here (the inventory above stays read-only).
    var taught by remember { mutableStateOf<List<TaughtSkill>>(emptyList()) }
    var taughtError by remember { mutableStateOf("") }
    var managing by remember { mutableStateOf<TaughtSkill?>(null) }
    var managingError by remember { mutableStateOf("") }
    var composerError by remember { mutableStateOf("") }
    var managingBusy by remember { mutableStateOf(false) }
    var testOutput by remember { mutableStateOf("") }
    var composing by remember { mutableStateOf(false) }
    var newName by remember { mutableStateOf("") }
    var newDescription by remember { mutableStateOf("") }
    var newBody by remember { mutableStateOf("") }
    // Cancelled + replaced on every load() so a stale response can't
    // win; only the latest job may clear busy.
    var loadJob by remember { mutableStateOf<Job?>(null) }

    val teachApi = remember { TeachApi(context) }
    // Hoisted alongside the api: pathFor is pure config, and every
    // construction builds an OkHttpClient for nothing.
    val chatApi = remember { ChatApi(context) }
    val serverApi = remember { ServerApi(context) }
    // Composer writes track their own busy flag: sharing managingBusy with
    // the dialog would disable Edit/Activate/Test on every composer save
    // and vice versa.
    var composerBusy by remember { mutableStateOf(false) }

    /** Taught load with the same single-flight + path guard as load(): a
     * profile switch mid-flight must not land old rows on the new path. */
    var taughtJob by remember { mutableStateOf<Job?>(null) }

    fun loadTaught(path: String) {
        // One call carries every row the review surface needs; failures
        // keep old rows behind the section error, never a wipe.
        taughtError = ""
        taughtJob?.cancel()
        var job: Job? = null
        job = scope.launch {
            try {
                val tr = teachApi.listSkills(path)
                ensureActive()
                if (taughtJob == job && path == chatApi.pathFor(profile)) {
                    taughtError = tr.exceptionOrNull()?.let {
                        "Taught skills unavailable (${it.message?.take(60) ?: "error"})."
                    } ?: ""
                    tr.getOrNull()?.let { taught = it }
                }
            } finally {
                // No busy flag: the section repaints under the main spinner.
            }
        }
        taughtJob = job
    }

    fun load() {
        loadJob?.cancel()
        busy = true
        skillsError = ""
        loaded = false
        val path = chatApi.pathFor(profile)
        loadTaught(path)
        var job: Job? = null
        job = scope.launch {
            try {
                val sr = serverApi.listSkills(path)
                // A cancel landing mid-await must not leave stale state.
                ensureActive()
                if (loadJob == job) {
                    skills = sr.getOrDefault(emptyList())
                    skillsError = sr.exceptionOrNull()?.let {
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

    /** Runs one taught-skill write, then reloads the section when it
     * succeeds. The action returns a Result which is folded here: returning
     * it unfolded would reload silently on failure with no error shown.
     * Errors name the write, never the body; the dialog stays open with its
     * context so a failure does not eat the edit. */
    fun mutate(
        action: suspend (TeachApi, String) -> Result<*>,
        onSuccess: () -> Unit = {},
    ) {
        val path = chatApi.pathFor(profile)
        managingBusy = true
        managingError = ""
        scope.launch {
            try {
                action(teachApi, path).getOrThrow()
                ensureActive()
                onSuccess()
                loadTaught(path)
            } catch (e: Exception) {
                ensureActive()
                managingError = e.message?.take(120) ?: "failed"
            } finally {
                managingBusy = false
            }
        }
    }

    /** Successful saves, consumed by the dialog to collapse its editor. */
    var editSaves by remember { mutableStateOf(0) }

    LaunchedEffect(Unit) { load() }

    // Search + sort (Tasks-style).
    var query by remember { mutableStateOf("") }
    var skillsSort by remember { mutableStateOf(SkillsSort.NameAz) }
    var skillsSortOpen by remember { mutableStateOf(false) }

    fun sortSkills(list: List<SkillInfo>): List<SkillInfo> = when (skillsSort) {
        SkillsSort.NameZa -> list.sortedByDescending { it.name.lowercase(Locale.ROOT) }
        SkillsSort.NameAz -> list.sortedBy { it.name.lowercase(Locale.ROOT) }
    }
    val shownSkills = remember(skills, query, skillsSort) {
        sortSkills(skills.filter { it.matches(query) })
    }

    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text("Skills") },
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
                OutlinedTextField(
                    value = query,
                    onValueChange = { query = it },
                    placeholder = { Text("Search skills…") },
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
                    if (skills.isEmpty() && skillsError.isNotEmpty() && loaded) {
                        item {
                            ErrorCard(raw = skillsError, onRetry = { load() })
                        }
                    }
                    if (loaded && skills.isEmpty() && skillsError.isEmpty()) {
                        item {
                            EmptyState(
                                icon = Icons.Filled.Extension,
                                title = "Nothing listed",
                                subtitle = "This assistant reports no skills.",
                                actionLabel = "Refresh",
                                onAction = { load() },
                            )
                        }
                    }
                    // ── Skills ──
                    item {
                        Text(
                            "Skills",
                            style = MaterialTheme.typography.titleMedium,
                            modifier = Modifier.padding(horizontal = 4.dp, vertical = 4.dp),
                        )
                    }
                    // Totals header (#123): inventory size as a rough
                    // context-load proxy for this assistant.
                    if (loaded && skillsError.isEmpty()) {
                        item {
                            HintLine("${skills.size} skills")
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
                                TextButton(onClick = { skillsSortOpen = true }) {
                                    Text("Sort: ${skillsSort.title}")
                                }
                                DropdownMenu(
                                    expanded = skillsSortOpen,
                                    onDismissRequest = { skillsSortOpen = false },
                                ) {
                                    SkillsSort.entries.forEach { s ->
                                        DropdownMenuItem(
                                            text = { Text(s.title) },
                                            onClick = { skillsSort = s; skillsSortOpen = false },
                                        )
                                    }
                                }
                            }
                        }
                    }
                    if (shownSkills.isNotEmpty()) {
                        items(shownSkills, key = { "s:" + it.name }) { s ->
                            SkillCard(profile, s)
                        }
                    }
                    if (loaded && skills.isNotEmpty()
                        && shownSkills.isEmpty()
                        && skillsError.isEmpty()
                    ) {
                        item {
                            HintLine("No skills match this search.")
                        }
                    }
                    // ── Taught by you (#314) ──
                    item {
                        Row(
                            modifier = Modifier.fillMaxWidth().padding(horizontal = 4.dp, vertical = 4.dp),
                            verticalAlignment = Alignment.CenterVertically,
                        ) {
                            Text(
                                "Taught by you",
                                style = MaterialTheme.typography.titleMedium,
                                modifier = Modifier.weight(1f),
                            )
                            TextButton(onClick = { composing = !composing }) {
                                Text(if (composing) "Cancel" else "New")
                            }
                        }
                    }
                    if (taughtError.isNotBlank()) {
                        item {
                            ErrorCard(raw = taughtError, onRetry = {
                                loadTaught(chatApi.pathFor(profile))
                            })
                        }
                    }
                    if (composing) {
                        item {
                            OutlinedTextField(
                                value = newName,
                                onValueChange = { newName = it },
                                label = { Text("Name (lowercase, hyphens)") },
                                singleLine = true,
                                modifier = Modifier.fillMaxWidth(),
                            )
                        }
                        item {
                            OutlinedTextField(
                                value = newDescription,
                                onValueChange = { newDescription = it },
                                label = { Text("Description") },
                                modifier = Modifier.fillMaxWidth(),
                            )
                        }
                        item {
                            OutlinedTextField(
                                value = newBody,
                                onValueChange = { newBody = it },
                                label = { Text("Procedure") },
                                minLines = 3,
                                modifier = Modifier.fillMaxWidth(),
                            )
                        }
                        item {
                            Row(
                                modifier = Modifier.fillMaxWidth(),
                                horizontalArrangement = Arrangement.End,
                            ) {
                                TextButton(
                                    onClick = {
                                        val n = newName.trim()
                                        val d = newDescription.trim()
                                        val b = newBody.trim()
                                        composerBusy = true
                                        composerError = ""
                                        scope.launch {
                                            try {
                                                val path = chatApi.pathFor(profile)
                                                val (_, clipped) = teachApi.createDraft(path, n, d, b, source = "skills-screen").getOrThrow()
                                                ensureActive()
                                                newName = ""
                                                newDescription = ""
                                                newBody = ""
                                                composing = false
                                                if (clipped) {
                                                    composerError = "Saved, but the text clipped at the length cap."
                                                }
                                                loadTaught(path)
                                            } catch (e: Exception) {
                                                ensureActive()
                                                composerError = e.message?.take(120) ?: "failed"
                                            } finally {
                                                composerBusy = false
                                            }
                                        }
                                    },
                                    // All three: the server requires name,
                                    // description and body, so enabling on a
                                    // subset would only earn a 400.
                                    enabled = !composerBusy && newName.trim().isNotBlank() &&
                                        newDescription.trim().isNotBlank() && newBody.trim().isNotBlank(),
                                ) {
                                    Text(if (composerBusy) "Saving…" else "Save draft")
                                }
                            }
                        }
                        if (composerError.isNotBlank()) {
                            item {
                                Text(
                                    composerError,
                                    style = MaterialTheme.typography.bodySmall,
                                    color = MaterialTheme.colorScheme.error,
                                )
                            }
                        }
                    }
                    if (taught.isEmpty() && taughtError.isBlank()) {
                        item {
                            HintLine("Nothing taught yet — save a thread from chat, or write one here.")
                        }
                    }
                    items(taught, key = { "t:" + it.id }) { t ->
                        Card(
                            onClick = {
                                managing = t
                                managingError = ""
                                testOutput = ""
                            },
                            modifier = Modifier.fillMaxWidth(),
                        ) {
                            Row(
                                modifier = Modifier.fillMaxWidth().padding(12.dp),
                                verticalAlignment = Alignment.CenterVertically,
                            ) {
                                Column(modifier = Modifier.weight(1f)) {
                                    Text(t.name.ifBlank { "?" }, style = MaterialTheme.typography.bodyLarge)
                                    if (t.description.isNotBlank()) {
                                        Text(
                                            t.description,
                                            style = MaterialTheme.typography.bodySmall,
                                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                                            maxLines = 3,
                                            overflow = TextOverflow.Ellipsis,
                                        )
                                    }
                                }
                                Text(
                                    t.status.ifBlank { "?" } + if (t.isTested()) "" else " · untested",
                                    style = MaterialTheme.typography.labelMedium,
                                    color = if (t.isActive()) MaterialTheme.colorScheme.primary
                                    else MaterialTheme.colorScheme.onSurfaceVariant,
                                )
                            }
                        }
                    }
                }
            }
        }
    }
    // The open row re-resolves by id on every recomposition: reloads land
    // new objects, and the dialog must show the fresh status/body, not the
    // tapped snapshot.
    val open = managing?.let { m -> taught.firstOrNull { it.id == m.id } ?: m }
    if (open != null) {
        TeachDialog(
            skill = open,
            editSaves = editSaves,
            testOutput = testOutput,
            acting = managingBusy,
            error = managingError,
            onSaveEdit = { name, description, body ->
                mutate(
                    action = { api, path ->
                        api.updateSkill(path, open.id, name = name, description = description, body = body)
                    },
                    onSuccess = { editSaves++ },
                )
            },
            onTest = { input ->
                testOutput = ""
                mutate(action = { api, path ->
                    // Blank output is a ran-empty signal, not a not-run one:
                    // without the marker the two are indistinguishable.
                    api.testSkill(path, open.id, input).onSuccess {
                        testOutput = it.ifBlank { "(no output)" }
                    }
                })
            },
            onActivate = {
                mutate(action = { api, path -> api.activateSkill(path, open.id) })
            },
            onDisable = {
                mutate(action = { api, path -> api.disableSkill(path, open.id) })
            },
            onPromote = { schedule, name ->
                mutate(action = { api, path -> api.promoteSkill(path, open.id, schedule, name) })
            },
            onDelete = {
                // Close only on success: mutate's onSuccess runs after the
                // write lands, so a failure keeps the dialog with its error.
                val id = open.id
                mutate(
                    action = { api, path -> api.deleteSkill(path, id) },
                    onSuccess = { managing = null },
                )
            },
            onClose = { managing = null },
        )
    }
}
