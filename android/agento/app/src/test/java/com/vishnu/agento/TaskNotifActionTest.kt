package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

/**
 * Notification-action routing (#368): the action-string → mutation map is
 * the compat surface between already-posted notifications and the
 * receiver, so it is pinned — a rename that orphans a live tap must be a
 * deliberate, reviewed change. The receiver body itself needs a Context
 * and stays platform-tested.
 */
class TaskNotifActionTest {

    private val receiver = TaskNotifActionReceiver()

    @Test
    fun `complete and skip actions route`() {
        assertEquals(
            NotifAction.COMPLETE,
            receiver.notifActionFor(TaskNotifActionReceiver.ACTION_COMPLETE),
        )
        assertEquals(
            NotifAction.SKIP,
            receiver.notifActionFor(TaskNotifActionReceiver.ACTION_SKIP),
        )
    }

    @Test
    fun `unknown and null actions route nowhere`() {
        assertNull(receiver.notifActionFor(null))
        assertNull(receiver.notifActionFor(""))
        assertNull(receiver.notifActionFor("com.vishnu.agento.action.CHAT_THREAD"))
        assertNull(receiver.notifActionFor(TaskWidget.ACTION_TASKS))
    }

    @Test
    fun `each action reads its own extra key`() {
        // One key per action lives on the target (#386), so the read in
        // onReceive cannot be wired to the wrong key: the posted intents
        // (TaskReminders) put the task id under these exact extras.
        assertEquals(TaskWidget.EXTRA_COMPLETE_ID, NotifAction.COMPLETE.extraKey)
        assertEquals(TaskNotifActionReceiver.EXTRA_SKIP_ID, NotifAction.SKIP.extraKey)
    }

    @Test
    fun `wire strings are pinned`() {
        // Posted notifications outlive the update that changes this code:
        // renaming an action or extra silently orphans their taps.
        assertEquals(
            "com.vishnu.agento.action.NOTIF_COMPLETE",
            TaskNotifActionReceiver.ACTION_COMPLETE,
        )
        assertEquals(
            "com.vishnu.agento.action.NOTIF_SKIP",
            TaskNotifActionReceiver.ACTION_SKIP,
        )
        assertEquals(
            "com.vishnu.agento.extra.SKIP_ID",
            TaskNotifActionReceiver.EXTRA_SKIP_ID,
        )
        assertEquals(
            "com.vishnu.agento.extra.COMPLETE_ID",
            TaskWidget.EXTRA_COMPLETE_ID,
        )
    }
}
