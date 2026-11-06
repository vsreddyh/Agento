package com.vishnu.agento

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Inexact-alarm downgrade is visible (#436).
 *
 * The toggle enables away on the inexact path when the exact-alarm grant
 * is missing, so both surfaces must name their true mode: the toggle sub
 * itself and each posted reminder's expanded view. Pure copy — the live
 * grant read ([canScheduleExactAlarms]) needs a device and stays out of
 * this suite by design.
 */
class InexactBadgeTest {

    @Test
    fun `exact toggle sub is the bare inventory line`() {
        assertEquals(REMINDER_TOGGLE_SUB, reminderToggleSub(true))
    }

    @Test
    fun `inexact toggle sub keeps the inventory line and names the mode`() {
        val sub = reminderToggleSub(false)
        assertTrue(
            "inexact sub must keep the base text, got: $sub",
            sub.startsWith(REMINDER_TOGGLE_SUB),
        )
        assertTrue(
            "inexact sub must say inexact, got: $sub",
            sub.contains("Inexact", ignoreCase = true),
        )
        assertTrue(
            "inexact sub must warn about lateness, got: $sub",
            sub.contains("late", ignoreCase = true),
        )
    }

    @Test
    fun `exact reminder posts no badge`() {
        assertNull(inexactAlarmNotice(true))
    }

    @Test
    fun `inexact reminder posts the late warning`() {
        val notice = inexactAlarmNotice(false)
        assertEquals(INEXACT_ALARM_NOTICE, notice)
        assertTrue(notice!!.contains("late", ignoreCase = true))
    }
}
