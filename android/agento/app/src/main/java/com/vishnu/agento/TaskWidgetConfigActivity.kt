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
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.ArrowBack
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
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
import androidx.compose.ui.unit.dp
import androidx.core.content.edit

/**
 * Display settings for one task-widget placement (#536, trimmed in #592
 * to a single comfortable layout): which slice it shows, row order, what
 * each row shows (due date and time separately, repeat condition,
 * parallel marker), section headers, the count switch, and whether — and
 * how long — the list scrolls. No preview, no title editing, no density
 * choice: saved picks are exactly what the placement renders.
 * Launched by the system at add time (android:configure) and from
 * Settings → Widget afterwards. Cancelling at add time aborts
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
                            // Title is read-only since #592 (no UI sets it):
                            // an existing custom title is left untouched.
                            putString("task_widget_view_$appWidgetId", picked.view.name)
                            putString("task_widget_sort_$appWidgetId", picked.sort.name)
                            putBoolean("task_widget_due_date_$appWidgetId", picked.showDueDate)
                            putBoolean("task_widget_due_time_$appWidgetId", picked.showDueTime)
                            // Keep the legacy combined key in step so an old
                            // build degrades to all-or-nothing instead of
                            // contradicting the new split.
                            putBoolean(
                                "task_widget_due_$appWidgetId",
                                picked.showDueDate && picked.showDueTime,
                            )
                            putBoolean("task_widget_repeat_$appWidgetId", picked.showRepeat)
                            putBoolean("task_widget_parallel_$appWidgetId", picked.showParallel)
                            putBoolean("task_widget_sections_$appWidgetId", picked.showSections)
                            putBoolean("task_widget_count_$appWidgetId", picked.showCount)
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

/** Settings for one placement, grouped by what they change (#536): row
 * content, order and the list itself. The switches are independent on
 * purpose: hiding the due date must not take the repeat condition with
 * it. */
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
    var pickedView by rememberSaveable { mutableStateOf(initial.view) }
    var pickedSort by rememberSaveable { mutableStateOf(initial.sort) }
    var pickedDueDate by rememberSaveable { mutableStateOf(initial.showDueDate) }
    var pickedDueTime by rememberSaveable { mutableStateOf(initial.showDueTime) }
    var pickedRepeat by rememberSaveable { mutableStateOf(initial.showRepeat) }
    var pickedParallel by rememberSaveable { mutableStateOf(initial.showParallel) }
    var pickedSections by rememberSaveable { mutableStateOf(initial.showSections) }
    var pickedShowCount by rememberSaveable { mutableStateOf(initial.showCount) }
    var pickedScroll by rememberSaveable { mutableStateOf(initial.scrollable) }
    var pickedStaticRows by rememberSaveable { mutableStateOf(initial.staticRows) }

    // One value for Save: every remaining control writes its picked
    // value. The title carries over untouched (read-only since #592) and
    // density stays Comfortable, so neither is picked here.
    val picked = WidgetSettings(
        view = pickedView,
        sort = pickedSort,
        showDueDate = pickedDueDate,
        showDueTime = pickedDueTime,
        showRepeat = pickedRepeat,
        showParallel = pickedParallel,
        showSections = pickedSections,
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
            // No live preview (#592): options only. What you pick is what
            // the placement shows.
            // Header: the title itself is always on (view name, or a
            // previously set custom title) — the only switch here is the
            // count line.
            SettingsGroup("Header") {
                ConfigSwitchRow(
                    title = "Show the count line",
                    subtitle = "The \"3 open tasks\" line under the title",
                    checked = pickedShowCount,
                    onChange = { pickedShowCount = it },
                )
            }
            // What each row says. Four independent switches (#536, #592):
            // the due date and the due time hide independently, and the
            // repeat condition stays independent of both.
            SettingsGroup("Row content") {
                ConfigSwitchRow(
                    title = "Due date",
                    subtitle = "e.g. Today",
                    checked = pickedDueDate,
                    onChange = { pickedDueDate = it },
                )
                ConfigSwitchRow(
                    title = "Due time",
                    subtitle = "e.g. 09:00 → 10:00",
                    checked = pickedDueTime,
                    onChange = { pickedDueTime = it },
                )
                ConfigSwitchRow(
                    title = "Repeat condition",
                    subtitle = "e.g. Every 3 days — stays with the date or time off",
                    checked = pickedRepeat,
                    onChange = { pickedRepeat = it },
                )
                ConfigSwitchRow(
                    title = "Parallel marker",
                    subtitle = "The word \"parallel\" on a parallel task",
                    checked = pickedParallel,
                    onChange = { pickedParallel = it },
                )
            }
            // Five orders don't fit a segmented row: a radio list instead,
            // same whole-row-selectable shape as the switches above. Each
            // subtitle says what the order does (#592), matching the
            // TaskManager sort it mirrors.
            SettingsGroup("Row order") {
                TaskWidgetSort.entries.forEach { s ->
                    SortRow(
                        title = s.title,
                        subtitle = when (s) {
                            TaskWidgetSort.Server ->
                                "As returned, grouped overdue → today → upcoming"
                            TaskWidgetSort.Start ->
                                "Earliest start first, ties by due time then name"
                            TaskWidgetSort.Name ->
                                "Alphabetical A–Z"
                            TaskWidgetSort.Created ->
                                "Most recently created first"
                            TaskWidgetSort.Estimate ->
                                "Longest estimate first, unestimated last"
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
                ConfigSwitchRow(
                    title = "Section headers",
                    subtitle = "Overdue → Current → … → No due date",
                    checked = pickedSections,
                    onChange = { pickedSections = it },
                )
                ConfigSwitchRow(
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
private fun ConfigSwitchRow(
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
