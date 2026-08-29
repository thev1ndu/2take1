"""Minimal MCP-over-HTTP client, speaking the same JSON-RPC 2.0 shape that
keda/mcp-server/server.go implements (initialize / tools/list / tools/call).
No MCP SDK dependency -- this is a small, fixed protocol subset.
"""

from __future__ import annotations

import itertools
from typing import Any

import requests

_ids = itertools.count(1)


class MCPError(RuntimeError):
    pass


class MCPClient:
    def __init__(self, base_url: str, timeout: float = 15.0) -> None:
        self._url = base_url
        self._timeout = timeout
        self._session = requests.Session()

    def _call(self, method: str, params: dict[str, Any] | None = None) -> Any:
        body = {"jsonrpc": "2.0", "id": next(_ids), "method": method}
        if params is not None:
            body["params"] = params
        resp = self._session.post(self._url, json=body, timeout=self._timeout)
        resp.raise_for_status()
        data = resp.json()
        if data.get("error"):
            raise MCPError(data["error"].get("message", "unknown MCP error"))
        return data.get("result")

    def list_tools(self) -> list[dict[str, Any]]:
        """Returns tool definitions already shaped for the Anthropic Messages
        API's `tools` parameter -- MCP's `inputSchema` field maps directly to
        Claude's `input_schema`.
        """
        result = self._call("tools/list")
        return [
            {
                "name": t["name"],
                "description": t["description"],
                "input_schema": t["inputSchema"],
            }
            for t in result["tools"]
        ]

    def call_tool(self, name: str, arguments: dict[str, Any]) -> tuple[str, bool]:
        """Returns (text, is_error)."""
        result = self._call("tools/call", {"name": name, "arguments": arguments})
        text = "".join(block.get("text", "") for block in result.get("content", []))
        return text, bool(result.get("isError"))
