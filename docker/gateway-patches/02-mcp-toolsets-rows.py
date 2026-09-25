"""Compat shim: upstream _handle_toolsets() (GET /v1/toolsets) only iterates
configurable toolsets, so connected MCP servers never appear — the app's
Skills screen cannot show them. This script appends one mcp-<server> row per
enabled MCP server (same shape as native rows).

Self-disables when already applied (marker check) and FAILS loudly if the
upstream anchors move, so a silently-unpatched image never builds.

Usage: python3 02-mcp-toolsets-rows.py  (runs against /opt/hermes)
"""
import ast
import os
import py_compile
import sys

HERMES_HOME = "/opt/hermes"
API_SERVER = f"{HERMES_HOME}/gateway/platforms/api_server.py"
TOOLS_CONFIG = f"{HERMES_HOME}/hermes_cli/tools_config.py"
TOOLSETS_MOD = f"{HERMES_HOME}/toolsets.py"

HANDLER_ANCHOR = '    async def _handle_toolsets(self, request: "web.Request") -> "web.Response":'
NEXT_METHOD_ANCHOR = "\n    async def "
# Scoped to the handler body at patch time (count asserted == 1 there).
RETURN_ANCHOR = 'return web.json_response({"object": "list", "platform": "api_server", "data": data})'
MARKER = "Compat shim (repo docker/gateway-patches/02)"

# Locals the patched return block relies on; py_compile cannot catch a
# renamed variable (NameError at runtime, swallowed by except -> silent
# no-op), so their presence in the handler body is asserted at build time.
REQUIRED_LOCALS = (
    "config = load_config()",
    "data: List[Dict[str, Any]] = []",
    "logger.exception",
)

HELPER = '''
    @staticmethod
    def _mcp_toolset_rows(config) -> "list":
        """Compat shim (repo docker/gateway-patches/02): expose connected MCP
        servers as mcp-<server> rows on GET /v1/toolsets. Upstream only iterates
        configurable toolsets, so MCP servers are invisible without this."""
        try:
            from hermes_cli.tools_config import (
                _get_platform_tools, enabled_mcp_server_names)
            from toolsets import resolve_toolset as _resolve
        except Exception:
            logger.warning("MCP rows: toolset imports unavailable")
            return []
        try:
            servers = sorted(enabled_mcp_server_names(config))
        except Exception:
            logger.warning("MCP rows: enabled_mcp_server_names failed")
            return []
        if not servers:
            return []
        try:
            enabled = _get_platform_tools(
                config, "api_server", include_default_mcp_servers=True)
        except Exception:
            logger.warning("MCP rows: platform resolution failed")
            enabled = set()
        rows = []
        for srv in servers:
            name = "mcp-%s" % srv
            try:
                tools = sorted(set(_resolve(name)))
            except Exception:
                logger.warning("MCP rows: resolve failed for %s", name)
                tools = []
            rows.append({
                "name": name,
                "label": "MCP server '%s'" % srv,
                "description": "MCP server '%s' tools" % srv,
                "enabled": bool(name in enabled or srv in enabled),
                "configured": True,
                "tools": tools,
            })
        return rows
'''

# Appends MCP rows, skipping names upstream already lists (dedupe against a
# future native fix). self/logger/config/data are handler locals/globals,
# asserted present below.
RETURN_PATCHED = '''try:
            _seen = {r.get("name") for r in data if isinstance(r, dict)}
            data = list(data) + [
                r for r in self._mcp_toolset_rows(config)
                if r.get("name") not in _seen
            ]
        except Exception:
            logger.exception("GET /v1/toolsets MCP rows failed")
        return web.json_response({"object": "list", "platform": "api_server", "data": data})'''


def _funcdef_params(path, func):
    """Parameter names of a def, via AST (no import). Walks the whole tree
    so methods and nested defs are found even if upstream moves them."""
    with open(path, encoding="utf-8") as f:
        tree = ast.parse(f.read())
    for node in ast.walk(tree):
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) \
                and node.name == func:
            args = node.args
            params = [a.arg for a in list(args.posonlyargs) + list(args.args)] \
                + [a.arg for a in args.kwonlyargs]
            if args.vararg:
                params.append(args.vararg.arg)
            if args.kwarg:
                params.append(args.kwarg.arg)
            return params
    return None


def main() -> int:
    if not os.path.exists(HERMES_HOME):
        print(f"SKIP: no {HERMES_HOME} in this layer.")
        return 0
    try:
        with open(API_SERVER, encoding="utf-8") as f:
            src = f.read()
    except OSError as e:
        print(f"FAIL: cannot read {API_SERVER}: {e}")
        return 1
    if MARKER in src:
        print("OK: MCP toolset rows already present; no patch.")
        return 0

    # Upstream symbols the helper depends on (source-level check, no import).
    checks = (
        (TOOLS_CONFIG, "enabled_mcp_server_names", ("config",)),
        (TOOLS_CONFIG, "_get_platform_tools",
         ("config", "platform", "include_default_mcp_servers")),
        (TOOLSETS_MOD, "resolve_toolset", ("name",)),
    )
    for path, func, wants in checks:
        try:
            params = _funcdef_params(path, func)
        except OSError as e:
            print(f"FAIL: cannot read {path}: {e}")
            return 1
        if params is None:
            print(f"FAIL: {func}() gone from {path}; upstream layout "
                  "changed — update this shim, do not ship broken.")
            return 1
        missing = [w for w in wants if w not in params]
        if missing:
            print(f"FAIL: {func}() lost params {missing}; upstream signature "
                  "changed — update this shim, do not ship broken.")
            return 1

    # Scope everything to the handler body so a same-shaped return in another
    # handler can never be patched by accident.
    hstart = src.find(HANDLER_ANCHOR)
    if hstart < 0:
        print("FAIL: _handle_toolsets anchor not found; upstream layout "
              "changed — update this shim, do not ship broken.")
        return 1
    body_start = hstart + len(HANDLER_ANCHOR)
    next_m = src.find(NEXT_METHOD_ANCHOR, body_start)
    hend = next_m if next_m >= 0 else len(src)
    body = src[body_start:hend]
    if body.count(RETURN_ANCHOR) != 1:
        print(f"FAIL: return anchor count {body.count(RETURN_ANCHOR)} != 1 "
              "in handler body — update this shim, do not ship broken.")
        return 1
    for local in REQUIRED_LOCALS:
        if local not in body:
            print(f"FAIL: handler body lost {local!r}; upstream signature "
                  "changed — update this shim, do not ship broken.")
            return 1

    src = src[:hstart] + HELPER + "\n" + src[hstart:]
    # Re-locate the return (offsets shifted by the helper insert).
    hstart2 = src.find(HANDLER_ANCHOR)
    body_start2 = hstart2 + len(HANDLER_ANCHOR)
    next_m2 = src.find(NEXT_METHOD_ANCHOR, body_start2)
    hend2 = next_m2 if next_m2 >= 0 else len(src)
    body2 = src[body_start2:hend2]
    if body2.count(RETURN_ANCHOR) != 1:
        print(f"FAIL: return anchor count {body2.count(RETURN_ANCHOR)} != 1 "
              "after helper insert — aborting, do not ship broken.")
        return 1
    body2 = body2.replace(RETURN_ANCHOR, RETURN_PATCHED, 1)
    src = src[:body_start2] + body2 + src[hend2:]
    with open(API_SERVER, "w", encoding="utf-8") as f:
        f.write(src)
    try:
        py_compile.compile(API_SERVER, doraise=True)
    except py_compile.PyCompileError as e:
        print(f"FAIL: patched file does not compile: {e}")
        return 1
    print("PATCHED: GET /v1/toolsets now appends mcp-<server> rows.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
