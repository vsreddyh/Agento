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
 * Display settings for one task-widget placement (#536): a custom title,
 * which slice it shows, row density and order, what each row shows (due date
 * and time, repeat condition, parallel marker), section headers, the header
 * switches and whether — and how long — the list scrolls.
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
                    initial = TaskWidget.settingsFor(this, appWidgetId),
                    onCancel = {
                        setResult(RESULT_CANCELED, done)
                        finish()
                    },
                    onSave = { picked ->
                        prefs().edit {
                            // Blank means "follow the view title", so the
                            // key is removed rather than storing "" —
                            // matches titleFor's fallback rule (#532).
                            val trimmed = picked.title.orEmpty()
                                .trim().take(WIDGET_TITLE_MAX)
                            if (trimmed.isEmpty()) {
                                remove("task_widget_title_$appWidgetId")
                            } else {
                                putString("task_widget_title_$appWidgetId", trimmed)
                            }
                            putString("task_widget_view_$appWidgetId", picked.view.name)
                            putString("task_widget_density_$appWidgetId", picked.density.name)
                            putString("task_widget_sort_$appWidgetId", picked.sort.name)
                            putBoolean("task_widget_due_$appWidgetId", picked.showDue)
                            putBoolean("task_widget_repeat_$appWidgetId", picked.showRepeat)
                            putBoolean("task_widget_parallel_$appWidgetId", picked.showParallel)
                            putBoolean("task_widget_sections_$appWidgetId", picked.showSections)
                            putBoolean("task_widget_count_$appWidgetId", picked.showCount)
                            putBoolean("task_widget_show_title_$appWidgetId", picked.showTitle)
                            putString(
                                "task_widget_static_rows_$appWidgetId",
                                picked.staticRows.name,
                            )
                            putBoolean("task_widget_scroll_$appWidgetId", picked.scrollable)
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

/** Settings for one placement, grouped by what they change (#536): title,
 * rows, what each row shows, and the list itself. Everything is live — the
 * preview above the controls redraws as you pick, so a choice never has to
 * be imagined — and the switches are independent on purpose: hiding the due
 * date must not take the repeat condition with it. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun WidgetSettingsScreen(
    initial: WidgetSettings,
    onCancel: () -> Unit,
    onSave: (WidgetSettings) -> Unit,
) {
    // rememberSaveable: rotating before Save must not reset the picks
    // (this screen is a system-launched configure activity, so rotation
    // is routine).
    var pickedTitle by rememberSaveable { mutableStateOf(initial.title.orEmpty()) }
    var pickedView by rememberSaveable { mutableStateOf(initial.view) }
    var pickedDensity by rememberSaveable { mutableStateOf(initial.density) }
    var pickedSort by rememberSaveable { mutableStateOf(initial.sort) }
    var pickedDue by rememberSaveable { mutableStateOf(initial.showDue) }
    var pickedRepeat by rememberSaveable { mutableStateOf(initial.showRepeat) }
    var pickedParallel by rememberSaveable { mutableStateOf(initial.showParallel) }
    var pickedSections by rememberSaveable { mutableStateOf(initial.showSections) }
    var pickedShowTitle by rememberSaveable { mutableStateOf(initial.showTitle) }
    var pickedShowCount by rememberSaveable { mutableStateOf(initial.showCount) }
    var pickedScroll by rememberSaveable { mutableStateOf(initial.scrollable) }
    var pickedStaticRows by rememberSaveable { mutableStateOf(initial.staticRows) }

    // One value for the preview and for Save: a control cannot affect the
    // preview without landing in the saved placement, or the other way
    // round.
    val picked = WidgetSettings(
        view = pickedView,
        density = pickedDensity,
        sort = pickedSort,
        title = pickedTitle,
        showDue = pickedDue,
        showRepeat = pickedRepeat,
        showParallel = pickedParallel,
        showSections = pickedSections,
        showTitle = pickedShowTitle,
        showCount = pickedShowCount,
        scrollable = pickedScroll,
        staticRows = pickedStaticRows,
    )

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
                    onClick = { onSave(picked) },
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
            WidgetPreview(settings = picked, small = previewSmall)
            // Title first: it is the one thing above the list that a
            // placement cannot infer (#532). Blank means "follow the view".
            SettingsGroup("Title") {
                OutlinedTextField(
                    value = pickedTitle,
                    onValueChange = { pickedTitle = it.take(WIDGET_TITLE_MAX) },
                    singleLine = true,
                    label = { Text("Widget title") },
                    placeholder = { Text(picked.view.title) },
                    supportingText = {
                        Text("Blank shows \"${picked.view.title}\" · max $WIDGET_TITLE_MAX characters")
                    },
                    modifier = Modifier.fillMaxWidth(),
                )
                SwitchRow(
                    title = "Show the title",
                    subtitle = "Hides the name; the refresh button stays",
                    checked = pickedShowTitle,
                    onChange = { pickedShowTitle = it },
                )
                SwitchRow(
                    title = "Show the count line",
                    subtitle = "The \"3 open tasks\" line under the title",
                    checked = pickedShowCount,
                    onChange = { pickedShowCount = it },
                )
            }
            // What each row says. Three independent switches (#536): the
            // repeat condition has to survive turning the due date and time
            // off, so it can never ride the due switch.
            SettingsGroup("Row content") {
                SwitchRow(
                    title = "Due date and time",
                    subtitle = "e.g. Today, 09:00 → 10:00",
                    checked = pickedDue,
                    onChange = { pickedDue = it },
                )
                SwitchRow(
                    title = "Repeat condition",
                    subtitle = "e.g. Every 3 days — stays with the due date off",
                    checked = pickedRepeat,
                    onChange = { pickedRepeat = it },
                )
                SwitchRow(
                    title = "Parallel marker",
                    subtitle = "The word \"parallel\" on a parallel task",
                    checked = pickedParallel,
                    onChange = { pickedParallel = it },
                )
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
            // same whole-row-selectable shape as the switches above. The
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
            SettingsGroup("List") {
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
                SwitchRow(
                    title = "Section headers",
                    subtitle = "Overdue → Current → … → No due date",
                    checked = pickedSections,
                    onChange = { pickedSections = it },
                )
                SwitchRow(
                    title = "Scrolling list",
                    subtitle = if (pickedScroll) {
                        "Up to ${TaskWidget.MAX_COLLECTION_ROWS} tasks, scroll through them"
                    } else {
                        "A fixed list, no scrolling"
                    },
                    checked = pickedScroll,
                    onChange = { pickedScroll = it },
                )
                // Only a non-scrolling placement has a row cap to choose:
                // the scrolling list already shows everything it fetched.
                if (!pickedScroll) {
                    Text(
                        "Rows without scrolling",
                        style = MaterialTheme.typography.bodyLarge,
                        modifier = Modifier.padding(horizontal = 16.dp),
                    )
                    SingleChoiceSegmentedButtonRow(Modifier.fillMaxWidth()) {
                        TaskWidgetStaticRows.entries.forEachIndexed { i, cap ->
                            SegmentedButton(
                                selected = pickedStaticRows == cap,
                                onClick = { pickedStaticRows = cap },
                                shape = SegmentedButtonDefaults.itemShape(
                                    index = i, count = TaskWidgetStaticRows.entries.size),
                            ) { Text(cap.rows.toString()) }
                        }
                    }
                    Text(
                        "The rest stay in the Task Manager; the count line says " +
                            "how many are showing.",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        modifier = Modifier.padding(horizontal = 16.dp),
                    )
                }
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

/** One fake agenda row in the preview (#532). [repeat] and [parallel] stay
 * separate fields so the preview can drop any of them on its own, exactly
 * like the placement's three switches (#536). */
private data class PreviewRow(
    val name: String,
    val due: String,
    val repeat: String? = null,
    val parallel: Boolean = false,
)

/** The line one preview row shows: the same order and the same switches as
 * the real widget's [widgetRowLine], so a preview can never promise a line
 * the device would not render. */
private fun previewRowLine(row: PreviewRow, settings: WidgetSettings): String =
    buildList {
        if (settings.showDue && row.due.isNotEmpty()) add(row.due)
        if (settings.showRepeat) row.repeat?.let { add(it) }
        if (settings.showParallel && row.parallel) add("parallel")
    }.joinToString(" · ")

/** Fake Done rows (#536, review): seven, so the non-scrolling row cap is
 * visible here too — 4 hides three, 6 hides one, 8 shows all — instead of a
 * single row that would make the cap and scrolling switches look dead. */
private val PREVIEW_DONE_ROWS: List<PreviewRow> = listOf(
    PreviewRow("Pay rent", "Yesterday, 10:00"),
    PreviewRow("Apply hair oil", "Today, 09:00", "Every 3 days"),
    PreviewRow("Bath", "Today, 20:00", parallel = true),
    PreviewRow("Brush teeth", "Today, 22:00", "Daily"),
    PreviewRow("Buy vegetables", "Tomorrow, 18:00"),
    PreviewRow("Water plants", "Sat, 09:00", "Mon, Wed, Fri"),
    PreviewRow("Plan the week", "", "Every 2 weeks"),
)

/** Fake agenda in the widget's bucket order (#532): Overdue first, no-date
 * last. Shown only for the grouped views — Done renders flat. */
private val PREVIEW_SECTIONS: List<Pair<String, List<PreviewRow>>> = listOf(
    "Overdue" to listOf(
        PreviewRow("Pay rent", "Yesterday, 10:00"),
    ),
    "Current" to listOf(
        PreviewRow("Apply hair oil", "Today, 09:00", "Every 3 days"),
        PreviewRow("Bath", "Today, 20:00", parallel = true),
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
 * same header switches, section order, row control switches, row shape and
 * truncation as the placement — including the row caps (the placement's own
 * non-scrolling cap, [TaskWidget.MAX_COLLECTION_ROWS] scrolling). Row order
 * is the one exception: the fake rows are fixed, so a non-default order
 * gets a caption instead of a re-sorted preview.
 * [small] previews the display-only small layout a placement under either
 * size bound gets on-device (render() returns the small layout before the
 * scrollable check), where density, scrolling and the row cap do not apply. */
@Composable
private fun WidgetPreview(
    settings: WidgetSettings,
    small: Boolean,
) {
    if (small) {
        SmallPreview(settings)
        return
    }
    val compact = settings.density == TaskWidgetDensity.Compact
    // Eight fake rows below for Open/All (1+2+2+1+1+1), seven for Done: the
    // total matches them exactly, so the preview never invents a gap the
    // device would not show — and the Done total exceeds the smallest cap,
    // so the 4/6/8 switch visibly moves there too.
    val total = if (settings.view == TaskWidgetView.Done) {
        PREVIEW_DONE_ROWS.size
    } else {
        8
    }
    // Done is one flat list with no headers, like the real widget.
    val sections: List<Pair<String, List<PreviewRow>>> =
        if (settings.view == TaskWidgetView.Done) {
            listOf("" to PREVIEW_DONE_ROWS)
        } else {
            PREVIEW_SECTIONS
        }
    // Section headers off: one flat list in the same agenda order (#536) —
    // the device strips the headers without reordering ([withoutHeaders]),
    // so the preview flattens the same way.
    val shownSource: List<Pair<String, List<PreviewRow>>> =
        if (settings.showSections) {
            sections
        } else {
            listOf("" to sections.flatMap { it.second })
        }
    // The cap counts rows, and a section header only appears when one of
    // its rows does — the same takeAgendaRows rule as the widget, with the
    // same caps (the placement's cap non-scrolling, MAX_COLLECTION_ROWS
    // scrolling). The hidden line is non-scrolling only: the device never
    // shows "scroll for more" — a scrolling placement with all rows visible
    // shows no qualifier at all.
    val rowCap = if (settings.scrollable) {
        TaskWidget.MAX_COLLECTION_ROWS
    } else {
        settings.staticRows.rows
    }
    val shownSections = ArrayList<Pair<String, List<PreviewRow>>>()
    var shownRows = 0
    for ((name, rows) in shownSource) {
        if (shownRows >= rowCap) break
        val take = rows.take(rowCap - shownRows)
        shownRows += take.size
        shownSections.add(name to take)
    }
    val hidden = if (settings.scrollable) {
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
                    if (settings.showTitle) {
                        Text(
                            widgetTitle(settings.title, settings.view.title),
                            style = MaterialTheme.typography.titleSmall,
                            fontWeight = FontWeight.Bold,
                            maxLines = 1,
                        )
                    }
                    if (settings.showCount) {
                        Text(
                            previewCount(
                                settings.view,
                                if (settings.scrollable) null else settings.staticRows.rows,
                                total,
                            ),
                            style = MaterialTheme.typography.bodySmall,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
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
                            // Same join and same three switches as the
                            // widget row (#536).
                            val line = previewRowLine(row, settings)
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
            // The preview cannot sort: its rows are fixed fake data, while
            // the device orders real rows by the placement's row order
            // first. Say so when a non-default order is picked, or the
            // preview promises an order the device will not render.
            if (settings.sort != TaskWidgetSort.Server) {
                Text(
                    "On-device rows are sorted by ${settings.sort.title}; " +
                        "the preview keeps the agenda order",
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
 * sections, or the one Done row — the same header switches (#536) and the
 * same "showing 1" honesty as the real compact count line. Density,
 * scrolling and the row cap are intentionally absent: the device ignores
 * all three below either size bound, where the layout names exactly one
 * task. The headers switch is a correct no-op here: a flat placement keeps
 * the agenda order ([withoutHeaders]), so the first row is the same either
 * way. */
@Composable
private fun SmallPreview(settings: WidgetSettings) {
    val sections: List<Pair<String, List<PreviewRow>>> =
        if (settings.view == TaskWidgetView.Done) {
            listOf("" to listOf(PreviewRow("Apply hair oil", "Today, 09:00")))
        } else {
            PREVIEW_SECTIONS
        }
    val next = sections.firstOrNull()?.second?.firstOrNull()
    val total = if (settings.view == TaskWidgetView.Done) 1 else 8
    Card(
        modifier = Modifier.fillMaxWidth(),
        shape = RoundedCornerShape(24.dp),
        colors = CardDefaults.cardColors(
            containerColor = MaterialTheme.colorScheme.surfaceVariant),
    ) {
        Column(modifier = Modifier.padding(16.dp)) {
            if (settings.showTitle) {
                Text(
                    widgetTitle(settings.title, settings.view.title),
                    style = MaterialTheme.typography.titleSmall,
                    fontWeight = FontWeight.Bold,
                    maxLines = 1,
                )
            }
            if (settings.showCount) {
                Text(
                    previewCount(settings.view, cap = null, total = total) +
                        shownSuffix(total, 1),
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            if (next != null) {
                Text(
                    next.name,
                    style = MaterialTheme.typography.bodyLarge,
                    maxLines = 1,
                    modifier = Modifier.padding(top = 6.dp),
                )
                val line = previewRowLine(next, settings)
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
                "Small placement: density, scrolling and the row cap don't apply",
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
 * cannot disagree (#532 review). [cap] is the placement's own cap (#536),
 * passed from the same setting the rows are cut with; null means the
 * surface shows everything it has, so no qualifier is possible. */
private fun previewCount(view: TaskWidgetView, cap: Int?, total: Int): String {
    val noun = when (view) {
        TaskWidgetView.Open -> "open task"
        TaskWidgetView.Done -> "done task"
        TaskWidgetView.All -> "task"
    }
    val shown = total.toString() +
        if (total == 1) " $noun" else " ${noun}s"
    if (cap == null) return shown
    // Same rule as the real count line (#532): the qualifier appears only
    // when rows are actually hidden. A one-task Done view capped at 8 hides
    // nothing, so "· showing 1" would promise a disclosure the device never
    // shows.
    val effective = minOf(total, cap)
    return if (total > effective) "$shown · showing $effective" else shown
}
