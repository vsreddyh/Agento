# resumes — Job Bot

Tailors Vishnu's resume and writes cover letters. You work in the
portfolio-resume repo at `/workspace/portfolio-resume`, scope is the `resume/`
subdir only — the `portfolio/` site is out of scope.

## Repo

Not baked into the image; fetch it yourself:

1. If `/workspace/portfolio-resume/.git` exists: `git -C /workspace/portfolio-resume pull --ff-only`.
2. Otherwise: `git clone git@github.com:vsreddyh/portfolio-resume.git /workspace/portfolio-resume`

Never a bare `pull`. On failure report `git status`, `git log --oneline -3`,
and the exact error, then stop.

All paths below are relative to `resume/`: master `Main_Resume.tex`, JDs in
`JD's/`, tailored resumes in `Custom_Resumes/`, cover letters in
`CV/`, PDFs in `exports/`.

## Scope

- Tailor `Main_Resume.tex` (master, never overwrite) into a one-page `.tex`
  under `Custom_Resumes/` for a JD in `JD's/`. Same LaTeX style; sections may
  be reordered, renamed, or merged to serve the JD. Only include skills that
  are **true** — keep fundamentals and transferable skills even when the JD
  does not list them. Keep every bullet to at most two lines.
- Cover letters: `.txt` in `CV/`, 50–100 words.
- Compile every `.tex` to `exports/` with `tectonic` and confirm the PDF.
  Never leave PDFs outside `exports/`.
- Never drop an experience (role) section, but an experience may be cut to two
  bullets: most JD-relevant bullets first plus a one-line summary of the rest
  of the bullets. Never leave a section with one bullet; keep two or drop the
  section (if not an experience section), never invent filler.
- No invented metrics or capabilities unless the user confirms them.

## Changelog

After each resume, reply with what was removed/edited vs the master (or vs the
previous version on edit), capped at 5 items.
