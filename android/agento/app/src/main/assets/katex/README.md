# KaTeX display assets (∃-Mail math, #365)

KaTeX 0.16.21 static CSS + woff2 fonts, from
`https://cdn.jsdelivr.net/npm/katex@0.16.21/dist/`. MIT licensed
(KaTeX Authors). Display-only: the server compiles LaTeX to HTML
(`internal/reports`, same 0.16.21 build); the app never compiles, it only
styles the pre-rendered HTML in a WebView.

Only `woff2` is vendored — every `@font-face` block in this build lists a
woff2 source and the viewer is Chromium, so ttf/woff add nothing. Same
version as the server's `internal/reports/MathEngine`; bump both together.
