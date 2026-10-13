package com.vishnu.agento

import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The backup allowlist shapes (#169): which dynamic keys may enter a settings
 * export, and — just as important — which nearby keys must not.
 *
 * These are the one part of SettingsBackup a JVM test can pin: export/import
 * need a Context, but the key predicates are pure. A predicate that drifts
 * wide silently backs up (or restores) prefs it was never meant to touch, so
 * the rejections below matter as much as the acceptances.
 */
class SettingsBackupTest {

    // ── isWidgetDisplayKey ───────────────────────────────────────────────

    @Test
    fun `widget display keys match with a widget id suffix`() {
        assertTrue(SettingsBackup.isWidgetDisplayKey("task_widget_view_12"))
        assertTrue(SettingsBackup.isWidgetDisplayKey("task_widget_density_12"))
        assertTrue(SettingsBackup.isWidgetDisplayKey("task_widget_due_12"))
        assertTrue(SettingsBackup.isWidgetDisplayKey("task_widget_scroll_12"))
    }

    @Test
    fun `bare prefixes without an id do not match`() {
        assertFalse(SettingsBackup.isWidgetDisplayKey("task_widget_view_"))
        assertFalse(SettingsBackup.isWidgetDisplayKey("task_widget_due_"))
        assertFalse(SettingsBackup.isWidgetDisplayKey("task_widget_"))
    }

    @Test
    fun `retired and unrelated widget keys do not match`() {
        // The retired diagnostic ladder: export must not resurrect it, and a
        // hand-edited backup must not be able to inject it.
        assertFalse(SettingsBackup.isWidgetDisplayKey("task_widget_style_12"))
        assertFalse(SettingsBackup.isWidgetDisplayKey("task_widget_foo_12"))
        assertFalse(SettingsBackup.isWidgetDisplayKey("task_widget_view"))
    }

    @Test
    fun `non-numeric suffixes do not match`() {
        // Widget ids are ints; a non-numeric tail is either a typo or a
        // hand-edited injection, and neither may enter the backup.
        assertFalse(SettingsBackup.isWidgetDisplayKey("task_widget_view_evil"))
        assertFalse(SettingsBackup.isWidgetDisplayKey("task_widget_due_1x"))
        assertFalse(SettingsBackup.isWidgetDisplayKey("task_widget_scroll_-4"))
    }

    @Test
    fun `string and boolean prefixes are told apart`() {
        // Import accepts each prefix only in its own type, so the split the
        // importer relies on is pinned here: view/density are strings,
        // due/scroll are booleans, and neither side claims the other's keys.
        assertTrue(SettingsBackup.isStringWidgetKey("task_widget_view_12"))
        assertTrue(SettingsBackup.isStringWidgetKey("task_widget_density_12"))
        assertFalse(SettingsBackup.isStringWidgetKey("task_widget_due_12"))
        assertFalse(SettingsBackup.isStringWidgetKey("task_widget_scroll_12"))
        assertTrue(SettingsBackup.isBooleanWidgetKey("task_widget_due_12"))
        assertTrue(SettingsBackup.isBooleanWidgetKey("task_widget_scroll_12"))
        assertFalse(SettingsBackup.isBooleanWidgetKey("task_widget_view_12"))
        assertFalse(SettingsBackup.isBooleanWidgetKey("task_widget_density_12"))
    }

    @Test
    fun `reminder bookkeeping is not a display key`() {
        // task_reminders_enabled travels via BOOLEAN_KEYS; the armed set and
        // the budget snapshot travel at all — restoring either would corrupt
        // the new device's alarm state. None of the three may match here, or
        // the exclusion documented on the predicate is a lie.
        assertFalse(SettingsBackup.isWidgetDisplayKey("task_reminders_enabled"))
        assertFalse(SettingsBackup.isWidgetDisplayKey("task_reminder_armed"))
        assertFalse(SettingsBackup.isWidgetDisplayKey("task_reminder_budget"))
    }

    // ── isEffortMemoryKey (guard: the sibling predicate still holds) ─────

    @Test
    fun `effort memory keys still match per tab and model`() {
        assertTrue(SettingsBackup.isEffortMemoryKey("effort_god_mimo-v2-6-flash"))
        assertTrue(SettingsBackup.isEffortMemoryKey("effort_story_x"))
        assertFalse(SettingsBackup.isEffortMemoryKey("effort_god_"))
        assertFalse(SettingsBackup.isEffortMemoryKey("effort_bogus-model_x"))
        assertFalse(SettingsBackup.isEffortMemoryKey("effort_"))
    }
}
