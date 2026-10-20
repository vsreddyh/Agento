# 0006 — Live mode as a call

Status: accepted (#317, call-mode half)

## Context

Voice was push-to-talk dictation plus TTS readout chained into a
talk-listen-talk loop ("live session", #85): the detector cut speech on
talk-over, but the turn underneath kept running, long tool tasks passed in
silence, and hanging up left only whatever the thread already held. #317
offered two resolutions — build a scoped call mode, or position Agento as
explicitly text-only. The text-only ADR was written, rejected, and
reverted: the decision is a proper live mode.

## Decision

**Barge-in that cuts the turn, audible tool milestones, thread as the
record — still half-duplex, still no realtime transport.**

- Barge-in cuts readout AND turn. The talk-over detector (`LiveInterrupt`,
  energy + adaptive floor + sustain gate, unchanged) now runs while the
  reply plays and while the turn streams. A trigger mid-stream claims the
  cancel first, stops the local stream and the server run (`stop()`), and
  the stream-end effect re-listens instead of speaking the stopped
  partial or ending the session. Talking over the readout keeps its old
  meaning (cut speech, re-listen).
- Long tasks are audible. While a live turn streams, newly appearing tools
  are announced once per change ("Using log transaction" — the fresh server
  label when one arrived with the change, else the latest delta's last
  segment de-snaked). A batch that seeds several tools at once yields one
  cue naming the latest, and the whole batch counts as announced.
  Milestones ride `live-cue-*` TTS keys: the detector stays off for cues
  (short self-speech must not self-trigger), their completion never starts
  listening, cues are skipped while TTS isn't up (a parked cue would speak
  late, possibly after the turn), and an engine-dead cue stays silent —
  the reply path reports TTS failure loudly at stream end instead of
  killing a working call over a cue.
- Hang-up adds nothing. Every live turn already lands in the thread,
  including user-stopped partials (marked interrupted). Ending is silent;
  the thread is the summary.
- Phase-keyed detection. The detector restarts on genuine phase
  transitions (quiet room → readout bleed → back, plus streaming↔cue flaps
  at two restarts per milestone) so the floor recalibrates instead of
  carrying a quiet-room threshold into speech, which would cut every
  reply. Each restart's deaf window is the existing ~400 ms calibration,
  and redundant restarts are skipped when already running in the target
  phase. A start failure never downgrades the session: contention with a
  dying recorder reads the same as a dead mic, so the next phase change
  retries instead of sticking the loop in tap-to-talk.

What this still is not: full-duplex realtime. There is no duplex audio
path, no barge-in *during* the 400 ms calibration windows, speech during
a ~1–2 s milestone cue is missed, and a 2-minute task still occupies the
profile's single turn (a second prompt gets `429`). Those are the
documented edges of an energy detector over text turns, not a voice
transport — building one stays out of scope with no realtime model API
on this path.

## Non-goals

- True duplex / realtime voice transport, avatar video, custom voice
  design, glasses/wearable voice.
- A posted hang-up summary (rejected in scoping: the thread already is it).
- Detector retuning (thresholds, sustain) — unchanged from #85.
