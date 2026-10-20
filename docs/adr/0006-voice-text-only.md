# 0006 — Voice: text-only positioning

Status: accepted (#317)

## Context

Voice today is three separate accessibility features, not a call mode:
push-to-talk dictation into the composer (`RecognizerIntent`), a
hands-free talk-listen-talk loop over text turns ("live session", #85:
system recognizer + TTS readout + talk-over detector), and readout
(`ChatTts` auto-read toggle, per-kind reminder voices from #306). There is
no duplex, no barge-in during streaming except cutting the readout, and no
background work that continues while talking — a 2-minute tool task blocks
the live loop the same way it blocks the chat.

Anyone comparing against Grok/Dots/Muse voice-chat reads "no calls" as
missing. #317 resolves to one of two outcomes: build a scoped voice-call
mode, or position Agento as explicitly text-only with the reason recorded.

## Decision

**Text-only, stated plainly. No call mode.**

A real call mode needs all of: full-duplex audio with barge-in, the agent
continuing tool work audibly while the user talks, and a hang-up that hands
a text summary back into the thread. This stack has none of the substrate:

- Model: one text model (`mimo-v2.6-flash`) over OpenCode Go. There is no
  realtime voice API on this path — no duplex stream to attach a call to,
  no voice-activity endpoint to build barge-in on.
- Transport: `pi-gateway` serves turns over SSE, one executing turn per
  profile; a second prompt while busy gets `429` with the in-flight run id
  (#311). A "call" would be a second concurrent turn on the same profile,
  which the executor refuses by design.
- Latency: the live loop is strictly half-duplex — speak the reply, then
  listen, then send, then stream. The recognizer cannot run while the reply
  streams, and the only mid-turn interruption is cutting TTS. Talking over
  a 2-minute tool task is impossible, not degraded.
- Cost: duplex audio means a sustained realtime stream plus mic plus TTS
  for the whole call on a self-hosted single-model VPS — a new cost and
  reliability surface for a P1.

So the honest scope is the current one: every conversation is a text turn;
voice input and readout are accessibility over those turns. What changes in
this PR is that the positioning is written down where a comparison happens:

- `README.md` (Chat tab) and Settings → About (new Voice card): Agento is
  text-only; no calls in or out. Mic button dictates into the composer,
  the live session is a hands-free talk-listen-talk loop over text turns,
  the speaker icon reads replies aloud.
- The live overlay carries the same one-line caption, at the exact spot
  that otherwise reads as a call UI ("Live session", Speaking/Listening,
  End button).
- `docs/adr/0006-voice-text-only.md` (this file) is the recorded decision
  the issue asks for.

Nothing about the audio path changes: no new permission, no new engine, no
behavior change. Live session, dictation and readout work exactly as before.

## Non-goals

- Full/half-duplex call mode with barge-in, background tool progress
  during a call, and hang-up-to-summary. Revisit only with a realtime
  voice transport the gateway can actually serve.
- Avatar video, custom voice design, glasses/wearable voice (inherited
  from the issue).
