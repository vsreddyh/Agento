# Changelog

Human-written release notes. The `## [x.y.z]` section for the version in
`android/agento/VERSION` becomes the GitHub Release body (and the in-app
updater text) — every app PR that bumps `VERSION` must add its entry here,
or the release job fails.

## [Unreleased]

## [3.7.1]

- Update notes render as Markdown (no raw `#` markers) and come from a
  human-written changelog — no more auto-generated commit hashes.

## [3.7.0]

- Conversations show which tools and skills were used: a live `Using …`
  indicator while the reply streams, and a per-reply `Tools:` / `Skills:`
  line that persists with history and exports.

## [3.6.1]

- Renamed the Portfolio section to Resume and Portfolio (labels only).

## [3.6.0]

- Skills screen split into Skills (Default vs Custom) and Tools (Default
  vs Custom MCP), each with search, origin filters, and sorts.

## [3.5.3]

- Chat header subtitle ellipsize; message input rides above the keyboard.
