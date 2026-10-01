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
/** Origin filter for the Skills section (default = bundled Hermes). */
internal enum class SkillsOrigin(val title: String) {
    All("All"),
    Default("Default"),
    Custom("Custom"),
}

/** Sort order for the Skills section lists. */
internal enum class SkillsSort(val title: String) {
    NameAz("Name A–Z"),
    NameZa("Name Z–A"),
    Category("Category"),
}
/** One skill row: name + description/category + On/Off badge. Tap expands
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
                val blurb = s.description.ifEmpty { s.category }
                if (blurb.isNotEmpty()) {
                    Text(
                        blurb,
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
/** Read-only skills inventory per assistant — Default vs Custom — with
 * Tasks-style search filtering, origin FilterChips and a sort dropdown.
 * Origin rule: in-the-box Hermes skills carry a category, project skills
 * don't — so non-blank category means default.
 */
@Composable
internal fun SkillsScreen(wc: WindowClass, onMenu: () -> Unit = {}) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    var profile by remember { mutableStateOf("god") }
    var skills by remember { mutableStateOf<List<SkillInfo>>(emptyList()) }
    var skillsError by remember { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }
    var loaded by remember { mutableStateOf(false) }
    // Cancelled + replaced on every load() so rapid profile taps can't
    // let a stale response win; only the latest job may clear busy.
    var loadJob by remember { mutableStateOf<Job?>(null) }

    fun load() {
        loadJob?.cancel()
        busy = true
        skillsError = ""
        loaded = false
        val path = ChatApi(context).pathFor(profile)
        var job: Job? = null
        job = scope.launch {
            try {
                val sr = ServerApi(context).listSkills(path)
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

    LaunchedEffect(profile) { load() }

    val tabs = listOf("god" to "God", "story" to "Story", "resumes" to "Resume and Portfolio")

    // Search + origin filter + sort (Tasks-style).
    var query by remember { mutableStateOf("") }
    var skillsOrigin by remember { mutableStateOf(SkillsOrigin.All) }
    var skillsSort by remember { mutableStateOf(SkillsSort.NameAz) }
    var skillsSortOpen by remember { mutableStateOf(false) }

    fun sortSkills(list: List<SkillInfo>): List<SkillInfo> = when (skillsSort) {
        SkillsSort.NameZa -> list.sortedByDescending { it.name.lowercase(Locale.ROOT) }
        SkillsSort.Category -> list.sortedWith(
            // Empty category sorts last so custom skills trail defaults.
            compareBy({ it.category.ifEmpty { "\uFFFF" }.lowercase(Locale.ROOT) }, { it.name.lowercase(Locale.ROOT) })
        )
        SkillsSort.NameAz -> list.sortedBy { it.name.lowercase(Locale.ROOT) }
    }
    // Origin rule (see ServerApi): bundled Hermes skills carry a category,
    // project skills don't — so non-blank category means default. If the
    // server omits categories entirely, everything counts as default
    // rather than silently emptying the Default section.
    val anyCategorized = remember(skills) { skills.any { it.category.isNotBlank() } }
    val defaultSkills = remember(skills, query, skillsSort, anyCategorized) {
        sortSkills(skills.filter { (it.isDefault() || !anyCategorized) && it.matches(query) })
    }
    val customSkills = remember(skills, query, skillsSort, anyCategorized) {
        // Mirrors the defaultSkills fallback: with no categories anywhere,
        // everything is default, so Custom stays empty (never duplicated).
        if (!anyCategorized) emptyList()
        else sortSkills(skills.filter { !it.isDefault() && it.matches(query) })
    }
    val shownDefaultSkills = if (skillsOrigin == SkillsOrigin.Custom) emptyList() else defaultSkills
    val shownCustomSkills = if (skillsOrigin == SkillsOrigin.Default) emptyList() else customSkills

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
                    // ── Skills (Default vs Custom) ──
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
                            val totalDefault =
                                if (!anyCategorized) skills.size
                                else skills.count { it.isDefault() }
                            HintLine(
                                "${skills.size} skills · " +
                                    "$totalDefault default · " +
                                    "${skills.size - totalDefault} custom"
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
                            SkillsOrigin.entries.forEach { o ->
                                FilterChip(
                                    selected = skillsOrigin == o,
                                    onClick = { skillsOrigin = o },
                                    label = { Text(o.title) },
                                )
                            }
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
                    if (shownDefaultSkills.isNotEmpty()) {
                        item {
                            Text(
                                "Default skills (${shownDefaultSkills.size})",
                                style = MaterialTheme.typography.titleSmall,
                                modifier = Modifier.padding(horizontal = 4.dp),
                            )
                        }
                        items(shownDefaultSkills, key = { "ds:" + it.name }) { s ->
                            SkillCard(profile, s)
                        }
                    }
                    if (shownCustomSkills.isNotEmpty()) {
                        item {
                            Text(
                                "Custom skills (${shownCustomSkills.size})",
                                style = MaterialTheme.typography.titleSmall,
                                modifier = Modifier.padding(horizontal = 4.dp),
                            )
                        }
                        items(shownCustomSkills, key = { "cs:" + it.name }) { s ->
                            SkillCard(profile, s)
                        }
                    }
                    if (loaded && skills.isNotEmpty()
                        && shownDefaultSkills.isEmpty() && shownCustomSkills.isEmpty()
                        && skillsError.isEmpty()
                    ) {
                        item {
                            HintLine("No skills match this search or filter.")
                        }
                    }
                }
            }
        }
    }
}
