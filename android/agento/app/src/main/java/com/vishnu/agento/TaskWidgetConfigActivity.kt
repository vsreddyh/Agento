package com.vishnu.agento

import android.appwidget.AppWidgetManager
import android.content.Context
import android.content.Intent
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.ArrowBack
import androidx.compose.material.icons.filled.KeyboardDoubleArrowRight
import androidx.compose.material.icons.filled.RadioButtonUnchecked
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.RadioButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SegmentedButton
import androidx.compose.material3.SegmentedButtonDefaults
import androidx.compose.material3.SingleChoiceSegmentedButtonRow
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.clearAndSetSemantics
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.core.content.edit

/**
 * Display settings for one task-widget placement: a custom title, which
 * slice it shows, row density, row order, the due line, and whether the
 * list scrolls.
 * Launched by the system at add time (android:configure) and from
 * Settings → Home-screen widget afterwards. Cancelling at add time aborts
 * the placement; saving re-renders that widget via [TaskWidget].
 */
class TaskWidgetConfigActivity : ComponentActivity() {

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val appWidgetId = intent?.extras?.getInt(
            AppWidgetManager.EXTRA_APPWIDGET_ID,
            AppWidgetManager.INVALID_APPWIDGET_ID,
        ) ?: AppWidgetManager.INVALID_APPWIDGET_ID
        // No id (shouldn't happen): nothing to configure.
        if (appWidgetId == AppWidgetManager.INVALID_APPWIDGET_ID) {
            setResult(RESULT_CANCELED)
            finish()
            return
        }
        // Return the id on save so the host binds this placement.
        val done = Intent().putExtra(AppWidgetManager.EXTRA_APPWIDGET_ID, appWidgetId)
        setResult(RESULT_CANCELED, done)
        setContent {
            val dark = isSystemInDarkTheme()
            AgentoTheme(dark = dark) {
                WidgetSettingsScreen(
                    title = TaskWidget.titleFor(this, appWidgetId, ""),
                    view = TaskWidget.viewFor(this, appWidgetId),
                    density = TaskWidget.densityFor(this, appWidgetId),
                    sort = TaskWidget.sortFor(this, appWidgetId),
                    showDue = TaskWidget.showDueFor(this, appWidgetId),
                    scrollable = TaskWidget.scrollableFor(this, appWidgetId),
                    onCancel = {
                        setResult(RESULT_CANCELED, done)
                        finish()
                    },
                    onSave = { t, v, d, s, due, scroll ->
                        prefs().edit {
                            // Blank means "follow the view title", so the
                            // key is removed rather than storing "" —
                            // matches titleFor's fallback rule (#532).
                            val trimmed = t.trim().take(WIDGET_TITLE_MAX)
                            if (trimmed.isEmpty()) {
                                remove("task_widget_title_$appWidgetId")
                            } else {
                                putString("task_widget_title_$appWidgetId", trimmed)
                            }
                            putString("task_widget_view_$appWidgetId", v.name)
                            putString("task_widget_density_$appWidgetId", d.name)
                            putString("task_widget_sort_$appWidgetId", s.name)
                            putBoolean("task_widget_due_$appWidgetId", due)
                            putBoolean("task_widget_scroll_$appWidgetId", scroll)
                            // Retired diagnostic ladder: drop the key here
                            // too, not just on widget delete.
                            remove("task_widget_style_$appWidgetId")
                        }
                        TaskWidget.refresh(this@TaskWidgetConfigActivity)
                        setResult(RESULT_OK, done)
                        finish()
                    },
                )
            }
        }
    }

    private fun prefs() = applicationContext.getSharedPreferences(
        AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
}

/** Settings for one placement. Everything is live: the preview above the
 * controls redraws as you pick, so a choice never has to be imagined. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun WidgetSettingsScreen(
    title: String,
    view: TaskWidgetView,
    density: TaskWidgetDensity,
    sort: TaskWidgetSort,
    showDue: Boolean,
    scrollable: Boolean,
    onCancel: () -> Unit,
    onSave: (String, TaskWidgetView, TaskWidgetDensity, TaskWidgetSort, Boolean, Boolean) -> Unit,
) {
    // rememberSaveable: rotating before Save must not reset the picks
    // (this screen is a system-launched configure activity, so rotation
    // is routine).
    var pickedTitle by rememberSaveable { mutableStateOf(title) }
    var pickedView by rememberSaveable { mutableStateOf(view) }
    var pickedDensity by rememberSaveable { mutableStateOf(density) }
    var pickedSort by rememberSaveable { mutableStateOf(sort) }
    var pickedDue by rememberSaveable { mutableStateOf(showDue) }
    var pickedScroll by rememberSaveable { mutableStateOf(scrollable) }

    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text("Widget settings") },
                navigationIcon = {
                    IconButton(onClick = onCancel) {
                        Icon(Icons.Filled.ArrowBack, contentDescription = "Back")
                    }
                },
            )
        },
        bottomBar = {
            // Sticky actions: Save must never scroll out of reach.
            Row(
                modifier = Modifier
                    .fillMaxWidth()
                    // Scaffold does not inset a custom bottom bar for the
                    // system nav bar; without this the actions sit under it
                    // on gesture navigation.
                    .navigationBarsPadding()
                    .padding(horizontal = 16.dp, vertical = 12.dp),
                horizontalArrangement = Arrangement.spacedBy(12.dp),
            ) {
                TextButton(onClick = onCancel, modifier = Modifier.weight(1f)) {
                    Text("Cancel")
                }
                Button(
                    onClick = {
                        onSave(
                            pickedTitle, pickedView, pickedDensity,
                            pickedSort, pickedDue, pickedScroll,
                        )
                    },
                    modifier = Modifier.weight(2f),
                ) {
                    Text("Save")
                }
            }
        },
    ) { padding ->
        Column(
            modifier = Modifier
                .fillMaxSize()
                .padding(padding)
                .verticalScroll(rememberScrollState())
                .padding(horizontal = 16.dp),
            verticalArrangement = Arrangement.spacedBy(16.dp),
        ) {
            // Preview-only: the device picks the layout from the
            // placement's size, which the config screen cannot know (#532).
            // Without this an operator sizing to 2 cells sees a list preview
            // but gets the display-only small layout on-device.
            var previewSmall by rememberSaveable { mutableStateOf(false) }
            SettingsGroup("Preview size") {
                SingleChoiceSegmentedButtonRow(Modifier.fillMaxWidth()) {
                    SegmentedButton(
                        selected = !previewSmall,
                        onClick = { previewSmall = false },
                        shape = SegmentedButtonDefaults.itemShape(index = 0, count = 2),
                    ) { Text("Full") }
                    SegmentedButton(
                        selected = previewSmall,
                        onClick = { previewSmall = true },
                        shape = SegmentedButtonDefaults.itemShape(index = 1, count = 2),
                    ) { Text("Small") }
                }
            }
            WidgetPreview(
                title = pickedTitle,
                view = pickedView,
                density = pickedDensity,
                showDue = pickedDue,
                scrollable = pickedScroll,
                small = previewSmall,
            )
            // Title first: it is the one thing above the list that a
            // placement cannot infer (#532). Blank means "follow the view".
            SettingsGroup("Title") {
                OutlinedTextField(
                    value = pickedTitle,
                    onValueChange = { pickedTitle = it.take(WIDGET_TITLE_MAX) },
                    singleLine = true,
                    label = { Text("Widget title") },
                    placeholder = { Text(pickedView.title) },
                    supportingText = {
                        Text("Blank shows \"${pickedView.title}\" · max $WIDGET_TITLE_MAX characters")
                    },
                    modifier = Modifier.fillMaxWidth(),
                )
            }
            SettingsGroup("Tasks shown") {
                SingleChoiceSegmentedButtonRow(Modifier.fillMaxWidth()) {
                    TaskWidgetView.entries.forEachIndexed { i, v ->
                        SegmentedButton(
                            selected = pickedView == v,
                            onClick = { pickedView = v },
                            shape = SegmentedButtonDefaults.itemShape(
                                index = i, count = TaskWidgetView.entries.size),
                        ) { Text(v.title) }
                    }
                }
            }
            SettingsGroup("Row style") {
                SingleChoiceSegmentedButtonRow(Modifier.fillMaxWidth()) {
                    TaskWidgetDensity.entries.forEachIndexed { i, d ->
                        SegmentedButton(
                            selected = pickedDensity == d,
                            onClick = { pickedDensity = d },
                            shape = SegmentedButtonDefaults.itemShape(
                                index = i, count = TaskWidgetDensity.entries.size),
                        ) { Text(d.title) }
                    }
                }
            }
            // Five orders don't fit a segmented row: a radio list instead,
            // same whole-row-selectable shape as the switches below. The
            // non-Server orders mirror the Task Manager's sort menu, so a
            // placement can match the in-app list.
            SettingsGroup("Row order") {
                TaskWidgetSort.entries.forEach { s ->
                    SortRow(
                        title = s.title,
                        subtitle = when (s) {
                            TaskWidgetSort.Server ->
                                "As the server returns them — today's behaviour"
                            else -> "Same order as “${s.title}” in the Task Manager"
                        },
                        selected = pickedSort == s,
                        onClick = { pickedSort = s },
                    )
                }
            }
            SettingsGroup("Options") {
                SwitchRow(
                    title = "Show due dates",
                    subtitle = "A friendly line under each task name",
                    checked = pickedDue,
                    onChange = { pickedDue = it },
                )
                SwitchRow(
                    title = "Scrolling list",
                    subtitle = if (pickedScroll) {
                        "Up to ${TaskWidget.MAX_COLLECTION_ROWS} tasks, scroll through them"
                    } else {
                        "First ${TaskWidget.STATIC_ROW_LIMIT} tasks, no scrolling"
                    },
                    checked = pickedScroll,
                    onChange = { pickedScroll = it },
                )
            }
            Spacer(modifier = Modifier.height(4.dp))
        }
    }
}

/** A card per group of controls, with a small heading. */
@Composable
private fun SettingsGroup(title: String, content: @Composable () -> Unit) {
    Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
        Text(
            title,
            style = MaterialTheme.typography.labelLarge,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        Card(modifier = Modifier.fillMaxWidth()) {
            Column(
                // Horizontal padding too: the segmented rows were touching
                // the card edges.
                modifier = Modifier.padding(horizontal = 12.dp, vertical = 12.dp),
                verticalArrangement = Arrangement.spacedBy(4.dp),
            ) { content() }
        }
    }
}

/** One order option; the whole row selects, same shape as [SwitchRow]. */
@Composable
private fun SortRow(
    title: String,
    subtitle: String,
    selected: Boolean,
    onClick: () -> Unit,
) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .selectable(
                selected = selected,
                onClick = onClick,
                role = Role.RadioButton,
            )
            .padding(horizontal = 16.dp, vertical = 10.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Column(modifier = Modifier.weight(1f)) {
            Text(title, style = MaterialTheme.typography.bodyLarge)
            Text(
                subtitle,
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
        Spacer(modifier = Modifier.width(12.dp))
        // The row is the single radio control; the button is decoration,
        // so hide it from TalkBack to avoid two radio nodes.
        RadioButton(
            selected = selected,
            onClick = null,
            modifier = Modifier.clearAndSetSemantics { },
        )
    }
}

/** One switch with an explanatory subtitle; the whole row toggles. */
@Composable
private fun SwitchRow(
    title: String,
    subtitle: String,
    checked: Boolean,
    onChange: (Boolean) -> Unit,
) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .selectable(
                selected = checked,
                onClick = { onChange(!checked) },
                role = Role.Switch,
            )
            .padding(horizontal = 16.dp, vertical = 10.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Column(modifier = Modifier.weight(1f)) {
            Text(title, style = MaterialTheme.typography.bodyLarge)
            Text(
                subtitle,
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
        Spacer(modifier = Modifier.width(12.dp))
        // The row is the single switch control; the visual switch is
        // decoration, so hide it from TalkBack to avoid two switch nodes.
        Switch(
            checked = checked,
            onCheckedChange = null,
            modifier = Modifier.clearAndSetSemantics { },
        )
    }
}

/** One fake agenda row in the preview. [repeat] keeps its own line so the
 * preview shows that a repeat condition survives "show due dates" off,
 * exactly like [widgetSecondaryLine]. */
private data class PreviewRow(
    val name: String,
    val due: String,
    val repeat: String? = null,
)

/** Fake agenda in the widget's bucket order (#532): Overdue first, no-date
 * last. Shown only for the grouped views — Done renders flat. */
private val PREVIEW_SECTIONS: List<Pair<String, List<PreviewRow>>> = listOf(
    "Overdue" to listOf(
        PreviewRow("Pay rent", "Yesterday, 10:00"),
    ),
    "Current" to listOf(
        PreviewRow("Apply hair oil", "Today, 09:00", "Every 3 days"),
        PreviewRow("Bath", "Today, 20:00"),
    ),
    "Later today" to listOf(
        PreviewRow("Brush teeth", "Today, 22:00", "Daily"),
        PreviewRow("Change bedsheets", "Today, 23:00"),
    ),
    "Tomorrow" to listOf(
        PreviewRow("Buy vegetables", "Tomorrow, 18:00"),
    ),
    "This week" to listOf(
        PreviewRow("Water plants", "Sat, 09:00", "Mon, Wed, Fri"),
    ),
    "No due date" to listOf(
        PreviewRow("Plan the week", "", "Every 2 weeks"),
    ),
)

/** Miniature of the real widget, so each choice is visible immediately:
 * same title, section order, row shape, due line and truncation as the
 * placement — including the row caps (8 non-scrolling, 100 scrolling).
 * [small] previews the display-only small layout a placement under either
 * size bound gets on-device (render() returns the small layout before the
 * scrollable check), where density and scrolling do not apply. */
@Composable
private fun WidgetPreview(
    title: String,
    view: TaskWidgetView,
    density: TaskWidgetDensity,
    showDue: Boolean,
    scrollable: Boolean,
    small: Boolean,
) {
    if (small) {
        SmallPreview(title = title, view = view, showDue = showDue)
        return
    }
    val compact = density == TaskWidgetDensity.Compact
    // Eight fake rows below (1+2+2+1+1+1): the total matches them exactly,
    // so the preview never invents a gap the device would not show.
    val total = if (view == TaskWidgetView.Done) 1 else 8
    // Done is one flat list with no headers, like the real widget.
    val sections: List<Pair<String, List<PreviewRow>>> =
        if (view == TaskWidgetView.Done) {
            listOf("" to listOf(PreviewRow("Apply hair oil", "Today, 09:00")))
        } else {
            PREVIEW_SECTIONS
        }
    // The cap counts rows, and a section header only appears when one of
    // its rows does — the same takeAgendaRows rule as the widget, with the
    // same caps (STATIC_ROW_LIMIT non-scrolling, MAX_COLLECTION_ROWS
    // scrolling). The hidden line is non-scrolling only: the device never
    // shows "scroll for more" — a scrolling placement with all rows visible
    // shows no qualifier at all.
    val rowCap = if (scrollable) {
        TaskWidget.MAX_COLLECTION_ROWS
    } else {
        TaskWidget.STATIC_ROW_LIMIT
    }
    val shownSections = ArrayList<Pair<String, List<PreviewRow>>>()
    var shownRows = 0
    for ((name, rows) in sections) {
        if (shownRows >= rowCap) break
        val take = rows.take(rowCap - shownRows)
        shownRows += take.size
        shownSections.add(name to take)
    }
    val hidden = if (scrollable) {
        0
    } else {
        (total - shownRows).coerceAtLeast(0)
    }
    Card(
        modifier = Modifier.fillMaxWidth(),
        shape = RoundedCornerShape(24.dp),
        colors = CardDefaults.cardColors(
            // surfaceVariant, not surfaceContainerHighest: the rest of the
            // app only uses the former, so this can't drift on an M3 bump.
            containerColor = MaterialTheme.colorScheme.surfaceVariant),
    ) {
        Column(modifier = Modifier.padding(16.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Column(modifier = Modifier.weight(1f)) {
                    Text(
                        widgetTitle(title, view.title),
                        style = MaterialTheme.typography.titleSmall,
                        fontWeight = FontWeight.Bold,
                        maxLines = 1,
                    )
                    Text(
                        previewCount(view, scrollable, total),
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
                Spacer(modifier = Modifier.width(8.dp))
                Icon(
                    Icons.Filled.Refresh,
                    contentDescription = null,
                    tint = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.size(20.dp),
                )
            }
            HorizontalDivider(
                modifier = Modifier.padding(vertical = 10.dp),
                color = MaterialTheme.colorScheme.outlineVariant,
            )
            shownSections.forEach { (section, rows) ->
                if (section.isNotEmpty()) {
                    val count = sections.first { it.first == section }.second.size
                    Text(
                        "$section · $count",
                        style = MaterialTheme.typography.labelMedium,
                        fontWeight = FontWeight.Bold,
                        color = if (section == "Overdue") {
                            MaterialTheme.colorScheme.error
                        } else {
                            MaterialTheme.colorScheme.onSurface
                        },
                        modifier = Modifier.padding(top = 6.dp),
                    )
                }
                rows.forEach { row ->
                    Row(
                        modifier = Modifier
                            .fillMaxWidth()
                            .padding(vertical = if (compact) 2.dp else 4.dp),
                        verticalAlignment = Alignment.CenterVertically,
                    ) {
                        Icon(
                            Icons.Filled.RadioButtonUnchecked,
                            contentDescription = null,
                            tint = MaterialTheme.colorScheme.onSurfaceVariant,
                            modifier = Modifier.size(if (compact) 18.dp else 22.dp),
                        )
                        Spacer(modifier = Modifier.width(10.dp))
                        Column(modifier = Modifier.weight(1f)) {
                            Text(
                                row.name,
                                style = if (compact) {
                                    MaterialTheme.typography.bodyMedium
                                } else {
                                    MaterialTheme.typography.bodyLarge
                                },
                                maxLines = 1,
                            )
                            // Same join as the widget row: due, then repeat.
                            val line = buildList {
                                if (showDue && row.due.isNotEmpty()) add(row.due)
                                if (row.repeat != null) add(row.repeat)
                            }.joinToString(" · ")
                            if (line.isNotEmpty()) {
                                Text(
                                    line,
                                    style = MaterialTheme.typography.bodySmall,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                    maxLines = 1,
                                )
                            }
                        }
                        Spacer(modifier = Modifier.width(8.dp))
                        Icon(
                            Icons.Filled.KeyboardDoubleArrowRight,
                            contentDescription = "Skip",
                            tint = MaterialTheme.colorScheme.onSurfaceVariant,
                            modifier = Modifier.size(if (compact) 16.dp else 20.dp),
                        )
                    }
                }
            }
            if (hidden > 0) {
                Text(
                    "+ $hidden more in the app",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(top = 4.dp),
                )
            }
        }
    }
}

/** Miniature of the small layout: title, count and the single next task
 * (#532). Same agenda order as the device — the first row of the grouped
 * sections, or the one Done row — and the same "showing 1" honesty as the
 * real compact count line. Density and scrolling are intentionally absent:
 * the device ignores both below either size bound. */
@Composable
private fun SmallPreview(
    title: String,
    view: TaskWidgetView,
    showDue: Boolean,
) {
    val sections: List<Pair<String, List<PreviewRow>>> =
        if (view == TaskWidgetView.Done) {
            listOf("" to listOf(PreviewRow("Apply hair oil", "Today, 09:00")))
        } else {
            PREVIEW_SECTIONS
        }
    val total = if (view == TaskWidgetView.Done) 1 else 8
    val next = sections.firstOrNull()?.second?.firstOrNull()
    Card(
        modifier = Modifier.fillMaxWidth(),
        shape = RoundedCornerShape(24.dp),
        colors = CardDefaults.cardColors(
            containerColor = MaterialTheme.colorScheme.surfaceVariant),
    ) {
        Column(modifier = Modifier.padding(16.dp)) {
            Text(
                widgetTitle(title, view.title),
                style = MaterialTheme.typography.titleSmall,
                fontWeight = FontWeight.Bold,
                maxLines = 1,
            )
            Text(
                previewCount(view, scrollable = true, total = total) + shownSuffix(total, 1),
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            if (next != null) {
                Text(
                    next.name,
                    style = MaterialTheme.typography.bodyLarge,
                    maxLines = 1,
                    modifier = Modifier.padding(top = 6.dp),
                )
                val line = buildList {
                    if (showDue && next.due.isNotEmpty()) add(next.due)
                    if (next.repeat != null) add(next.repeat)
                }.joinToString(" · ")
                if (line.isNotEmpty()) {
                    Text(
                        line,
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        maxLines = 1,
                    )
                }
            }
            Text(
                "Small placement: density and scrolling don't apply",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.padding(top = 4.dp),
            )
        }
    }
}

/** Honest count line: the non-scrolling mode caps rows, so say so — and
 * never claim a cap above the total it is capping. [total] is the fake
 * dataset's own row count, passed in so the line and the rendered rows
 * cannot disagree (#532 review). */
private fun previewCount(view: TaskWidgetView, scrollable: Boolean, total: Int): String {
    val noun = when (view) {
        TaskWidgetView.Open -> "open task"
        TaskWidgetView.Done -> "done task"
        TaskWidgetView.All -> "task"
    }
    val shown = total.toString() +
        if (total == 1) " $noun" else " ${noun}s"
    if (scrollable) return shown
    // Same rule as the real count line (#532): the qualifier appears only
    // when rows are actually hidden. A one-task Done view capped at 8 hides
    // nothing, so "· showing 1" would promise a disclosure the device never
    // shows.
    val cap = minOf(total, TaskWidget.STATIC_ROW_LIMIT)
    return if (total > cap) "$shown · showing $cap" else shown
}
