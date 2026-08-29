"""agent-worker: KEDA-scaled consumer of the `agent:tasks` Redis Stream.

Runs the Claude tool-use loop against the tools exposed by mcp-server, then
publishes the result back over Redis Pub/Sub for websocket-gateway to push to
the browser. There is deliberately no HTTP server here -- this pod exists
only while there is a queued chat message to process, which is what lets
KEDA scale it to zero.
"""

from __future__ import annotations

import logging
import os
import time

import anthropic
import redis

from mcp_client import MCPClient

logging.basicConfig(level=logging.INFO, format="agent-worker: %(message)s")
log = logging.getLogger(__name__)

STREAM_NAME = "agent:tasks"
GROUP_NAME = "agent-workers"
MAX_TOOL_ITERATIONS = 8

SYSTEM_PROMPT = """You are a todo-list prioritization assistant. The user \
describes tasks, deadlines, or a request to reorganize their list; you use \
the available tools to inspect and update it.

Call list_todos first if you need to see the current state before deciding \
what to change. Use set_priority (0-100, higher is more urgent) to reorder \
items rather than relying on the order they were added. When you are done, \
reply with one or two short sentences summarizing what you changed and why \
-- this is shown directly to the user in a chat window, so keep it plain \
and conversational, not a log of tool calls."""


def env(key: str, default: str) -> str:
    return os.environ.get(key, default)


def build_redis() -> redis.Redis:
    return redis.Redis.from_url(
        f"redis://{env('REDIS_ADDR', 'redis:6379')}",
        decode_responses=True,
    )


def ensure_group(r: redis.Redis) -> None:
    try:
        r.xgroup_create(STREAM_NAME, GROUP_NAME, id="0", mkstream=True)
    except redis.ResponseError as e:
        if "BUSYGROUP" not in str(e):
            raise


def wait_for_mcp(mcp: MCPClient) -> list[dict]:
    while True:
        try:
            return mcp.list_tools()
        except Exception as e:  # noqa: BLE001 -- retry loop, any failure is transient here
            log.info("mcp-server not ready yet (%s), retrying", e)
            time.sleep(2)


def run_agent_turn(client: anthropic.Anthropic, mcp: MCPClient, tools: list[dict], user_message: str) -> str:
    model = env("MODEL", "claude-opus-5")
    effort = env("EFFORT", "medium")
    messages: list[dict] = [{"role": "user", "content": user_message}]

    for _ in range(MAX_TOOL_ITERATIONS):
        response = client.messages.create(
            model=model,
            max_tokens=4096,
            system=SYSTEM_PROMPT,
            tools=tools,
            output_config={"effort": effort},
            messages=messages,
        )
        messages.append({"role": "assistant", "content": response.content})

        if response.stop_reason != "tool_use":
            return "".join(b.text for b in response.content if b.type == "text")

        tool_results = []
        for block in response.content:
            if block.type != "tool_use":
                continue
            text, is_error = mcp.call_tool(block.name, block.input)
            tool_results.append(
                {
                    "type": "tool_result",
                    "tool_use_id": block.id,
                    "content": text,
                    "is_error": is_error,
                }
            )
        messages.append({"role": "user", "content": tool_results})

    return "I made some changes but hit my tool-call limit before finishing -- try asking again to continue."


def process_entry(r: redis.Redis, client: anthropic.Anthropic, mcp: MCPClient, tools: list[dict], fields: dict) -> None:
    session_id = fields.get("session_id", "")
    message = fields.get("message", "")
    log.info("processing message for session %s", session_id)

    try:
        reply = run_agent_turn(client, mcp, tools, message)
    except Exception as e:  # noqa: BLE001 -- report the failure to the chat, don't crash the worker
        log.exception("agent turn failed")
        reply = f"Something went wrong handling that request: {e}"

    if session_id:
        r.publish(f"chat:{session_id}", reply)
    r.publish("todos:changed", "1")


def main() -> None:
    r = build_redis()
    ensure_group(r)

    mcp = MCPClient(env("MCP_SERVER_URL", "http://mcp-server:8081/mcp"))
    tools = wait_for_mcp(mcp)
    log.info("loaded %d tools from mcp-server", len(tools))

    client = anthropic.Anthropic()
    consumer = env("HOSTNAME", "agent-worker")

    log.info("listening on stream %s as consumer %s", STREAM_NAME, consumer)
    while True:
        entries = r.xreadgroup(GROUP_NAME, consumer, {STREAM_NAME: ">"}, count=1, block=5000)
        if not entries:
            continue
        for _stream, messages in entries:
            for msg_id, fields in messages:
                process_entry(r, client, mcp, tools, fields)
                r.xack(STREAM_NAME, GROUP_NAME, msg_id)


if __name__ == "__main__":
    main()
