# 0007 — Chat photo attachments

Status: accepted (#318, images half)

## Context

Every chat turn was text: the composer had no attach affordance and
`ChatMessage` was `{role, content: String}`, so reporting a photo, receipt
or screenshot meant retyping it by hand. The gateway already accepted the
conventional multipart shape (`image_url`/`input_image` base64 data URIs
→ Pi `ImageContent`) and Pi's `prompt` command takes `message` + `images`
— only the app was missing. Documents (PDF/DOCX/plain) have no such
server path: Pi's `prompt` takes no file/document input, so there is
nothing for a document part type to convert INTO.

## Decision

**Ship photos now (client-only, server already done); defer documents
explicitly; keep the fail-closed part-type contract.**

- Composer: attach (+) menu → gallery multi-pick (no permission needed —
  the system picker grants the chosen images only) or camera capture
  (CAMERA permission + FileProvider staged file, deleted after decode).
  Staged thumbnails strip with per-photo remove and a `n/2` count line.
- Each pick is downscaled (long side 1568px) and JPEG re-encoded (quality
  80, retry 60) before staging, so a 12 MP capture never sits in memory
  at full size and never rides the wire at full size.
- Limits (all client-enforced with named, actionable errors; the
  gateway's 4 MiB body cap stays as backstop):

  | Limit | Value | When exceeded |
  |---|---|---|
  | Photos per turn | 2 | picker takes free slots only; excess dropped with a toast |
  | Long side | 1568 px | downscaled at pick time, never refused |
  | Per-photo base64 | ~1.5 M chars (~1.1 MB raw) | named toast (actual size + cap + "pick a different photo") |
  | Original file read | ~32 MB (`MAX_SOURCE_BYTES`) | named error ("over about 32.0 MB") — bounds the pre-shrink read against mislabeled giants |
  | Turn wire size | ~3.5 M bytes preflight | named send error, phrased for the turn ("remove a photo…" with photos, "shorten the thread…" without) |

  Two capped photos plus framing stay under the preflight with headroom
  on a short thread (invariant test); the preflight sits below the gateway
  cap so a refusal is always the app's message, never an opaque 413. History
  text is the variable: a 200-message thread carries unbounded kilobytes on
  top of the photos, so a maxed-photo turn on a long thread trips the same
  preflight by design (fail-closed refuse, conditionally phrased).
- Wire: the conventional multipart array (`text` + `image_url` parts with
  base64 data URIs — the shape the gateway already parses), but ONLY on
  the last user turn in the request. The server prompts Pi with the
  trailing turn alone and Pi holds history in its own session file, so
  earlier photo turns ride as text and the body stays bounded however
  long the thread gets. Image-only turns send images with no text part;
  remote URLs are never produced (the gateway refuses to fetch them).
- Persistence: full bounded payloads live in the thread JSON
  (`StoredMessage.attachments`, default-empty so pre-#318 history reads
  as text-only), which is what lets retry and app-killed-turn resume
  resend the exact composed turn. Cost: roughly 150–600 KB per photo in
  the thread file; the 200-message cap bounds the file; the Settings
  backup mirrors thread files wholesale so photo threads export larger.
  Exports refuse past ~12 MB on disk (~25 MB heap peak, the materialized
  string would OOM first) with a named error — remedy is per-thread delete
  in the thread switcher; text-only backups never approach it. Exports
  (sharesheet Markdown, skill drafts) carry a `[n photo(s) attached]`
  marker instead of payloads — the marker is honest about turns whose
  content is an image.
- Staged (unsent) photos are in-memory only: rotation survives them
  (ViewModel scope), process death drops them with the draft and the
  queued text, with no recovery — each payload is ~1.5 MB base64, far past
  what `SavedStateHandle` should carry, and `persist()` writes sent
  threads only. Re-pick after a kill. A future "persist staging" change
  must store refs/Uris (plus the shrink step), never payloads.
- App-killed-turn resume covers image tails: resending the identical
  history reproduces the identical reattach key (same text + same image
  signature), so it reattaches rather than duplicating, exactly like
  text.

## Non-goals

- Documents (PDF/DOCX/plain): DEFERRED, reason recorded in #318. Pi's
  `prompt` RPC has no document/file input, so a new part type would have
  nowhere to go; inlining extracted text client-side would misrepresent
  the file (a PDF becomes pasted text with no provenance, large docs blow
  the body cap, binary formats need parsers). Revisit when Pi accepts
  file parts — the fail-closed 400 for unknown part types stays until
  then, by design.
- Audio/video attachments, remote-URL images, OCR/vision tuning.
- Changing the fail-closed part-type contract (unknown types still 400
  naming the type, never silently dropped).
