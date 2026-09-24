"""Compat shim: upstream _handle_toolsets() (GET /v1/toolsets) only iterates
configurable toolsets, so connected MCP servers never appear — the app's
Skills screen cannot show them. This script appends one mcp-<server> row per
enabled MCP server (same shape as native rows).

Self-disables when already applied (marker check) and FAILS loudly if the
upstream anchors move, so a silently-unpatched image never builds.

Usage: python3 02-mcp-toolsets-rows.py  (runs against /opt/hermes)
"""
import os
import py_compile
import sys

HERMES_HOME = "/opt/hermes"
API_SERVER = f"{HERMES_HOME}/gateway/platforms/api_server.py"

HANDLER_ANCHOR = '    async def _handle_toolsets(self, request: "web.Request") -> "web.Response":'
RETURN_ANCHOR = '        return web.json_response({"object": "list", "platform": "api_server", "data": data})'
MARKER = "def _mcp_toolset_rows(config)"

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
            return []
        try:
            servers = sorted(enabled_mcp_server_names(config))
        except Exception:
            return []
        if not servers:
            return []
        try:
            enabled = _get_platform_tools(
                config, "api_server", include_default_mcp_servers=True)
        except Exception:
            enabled = set()
        rows = []
        for srv in servers:
            name = "mcp-%s" % srv
            try:
                tools = sorted(set(_resolve(name)))
            except Exception:
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

RETURN_PATCHED = '''        try:
            data = list(data) + self._mcp_toolset_rows(config)
        except Exception:
            logger.exception("GET /v1/toolsets MCP rows failed")
        return web.json_response({"object": "list", "platform": "api_server", "data": data})'''


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
    if HANDLER_ANCHOR not in src:
        print("FAIL: _handle_toolsets anchor not found; upstream layout "
              "changed — update this shim, do not ship broken.")
        return 1
    if RETURN_ANCHOR not in src:
        print("FAIL: toolsets return anchor not found; upstream layout "
              "changed — update this shim, do not ship broken.")
        return 1
    src = src.replace(HANDLER_ANCHOR, HELPER + "\n" + HANDLER_ANCHOR, 1)
    src = src.replace(RETURN_ANCHOR, RETURN_PATCHED, 1)
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
