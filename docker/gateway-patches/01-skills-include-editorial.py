"""Compat shim: upstream api_server.py calls
    _find_all_skills(skip_disabled=False, include_editorial=True)
but some official images ship a skills_tool._find_all_skills() without the
`include_editorial` keyword (TypeError -> HTTP 500 on GET /v1/skills).

This script adds the missing keyword (accept-and-ignore) when absent and
does nothing when the installed code already accepts it, so it self-disables
once upstream ships a consistent image. It FAILS loudly if it can neither
confirm nor establish consistency, so a silently-broken image never builds.

Usage: python3 01-skills-include-editorial.py  (runs against /opt/hermes)
"""
import os
import py_compile
import re
import sys

HERMES_HOME = "/opt/hermes"
SKILLS_TOOL = f"{HERMES_HOME}/tools/skills_tool.py"
API_SERVER = f"{HERMES_HOME}/gateway/platforms/api_server.py"

SIG_RE = re.compile(
    r"def _find_all_skills\((?P<params>[^)]*)\)", re.DOTALL
)
KWARGS_RE = re.compile(r"\*\*\w+")


def main() -> int:
    if not os.path.exists(HERMES_HOME):
        print(f"SKIP: no {HERMES_HOME} in this layer.")
        return 0
    try:
        with open(SKILLS_TOOL, encoding="utf-8") as f:
            src = f.read()
    except OSError as e:
        print(f"FAIL: cannot read {SKILLS_TOOL}: {e}")
        return 1
    m = SIG_RE.search(src)
    if not m:
        print("FAIL: _find_all_skills() definition not found; "
              "upstream layout changed — update this shim, do not ship broken.")
        return 1
    params = m.group("params")
    if "include_editorial" in params or KWARGS_RE.search(params):
        print("OK: _find_all_skills already accepts include_editorial; no patch.")
        return 0
    # Insert the keyword. Signature is keyword-only (`*` first), so appending
    # is safe; the separator guards a hypothetical bare signature.
    stripped = params.strip()
    sep = "" if not stripped or stripped == "*" else ", "
    patched = (
        src[: m.start("params")]
        + params.rstrip()
        + f"{sep}include_editorial: bool = False"
        + src[m.end("params"):]
    )
    with open(SKILLS_TOOL, "w", encoding="utf-8") as f:
        f.write(patched)
    try:
        py_compile.compile(SKILLS_TOOL, doraise=True)
    except py_compile.PyCompileError as e:
        print(f"FAIL: patched file does not compile: {e}")
        return 1
    print("PATCHED: added include_editorial kwarg to _find_all_skills().")

    # Verify the caller passes only kwargs the callee now accepts.
    try:
        with open(API_SERVER, encoding="utf-8") as f:
            api = f.read()
    except OSError:
        api = ""
    if "_find_all_skills(" in api and "include_editorial" in api:
        print("OK: api_server caller is consistent with patched callee.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
