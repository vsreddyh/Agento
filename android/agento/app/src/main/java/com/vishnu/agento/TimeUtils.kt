package com.vishnu.agento

import java.time.ZoneId

/** Display zone for every user-visible time (#124). Single-user app with
 * no per-user zone: IST is pinned so chat, tasks, reminders and sync
 * stamps read identically on any device zone. Lives here (not in a UI
 * file) so non-UI code like TaskReminders reads it without reaching
 * across screens. */
val IST: ZoneId = ZoneId.of("Asia/Kolkata")
