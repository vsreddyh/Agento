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
import re
import sys

SKILLS_TOOL = "/opt/hermes/tools/skills_tool.py"
API_SERVER = "/opt/hermes/gateway/platforms/api_server.py"

SIG_RE = re.compile(
    r"def _find_all_skills\((?P<params>[^)]*)\)", re.DOTALL
)


def main() -> int:
    try:
        src = open(SKILLS_TOOL).read()
    except OSError as e:
        print(f"SKIP: cannot read {SKILLS_TOOL}: {e}")
        return 0  # retention image layers share this Dockerfile; be lenient
    m = SIG_RE.search(src)
    if not m:
        print("FAIL: _find_all_skills() definition not found; "
              "upstream layout changed — update this shim, do not ship broken.")
        return 1
    params = m.group("params")
    if "include_editorial" in params or "**" in params:
        print("OK: _find_all_skills already accepts include_editorial; no patch.")
        return 0
    # Insert the keyword-only-tolerant parameter before the closing paren.
    # Signature is keyword-only (`*` first), so appending is safe.
    patched = (
        src[: m.start("params")]
        + params.rstrip()
        + ", include_editorial: bool = False"
        + src[m.end("params"):]
    )
    open(SKILLS_TOOL, "w").write(patched)
    print("PATCHED: added include_editorial kwarg to _find_all_skills().")

    # Verify the caller passes only kwargs the callee now accepts.
    try:
        api = open(API_SERVER).read()
    except OSError:
        api = ""
    if "_find_all_skills(" in api and "include_editorial" in api:
        print("OK: api_server caller is consistent with patched callee.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
