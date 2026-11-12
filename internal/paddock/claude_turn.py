#!/usr/bin/python3
"""Read only Claude's pinned native transcript and report its latest turn.

The transcript stays inside OpenShell. Return only structural turn metadata to
Paddock; prompt text, tool inputs, and model output never cross this boundary.
"""

import glob
import json
import os
import sys


def is_prompt(event):
    if event.get("type") != "user" or event.get("isSidechain"):
        return False
    message = event.get("message")
    if not isinstance(message, dict) or message.get("role") != "user":
        return False
    if "toolUseResult" in event:
        return False
    content = message.get("content")
    if isinstance(content, str):
        return bool(content.strip())
    if isinstance(content, list):
        return any(isinstance(block, dict) and block.get("type") != "tool_result" for block in content)
    return False


def main(home, session_id, workspace):
    paths = glob.glob(os.path.join(home, "projects", "*", session_id + ".jsonl"))
    if not paths:
        print(json.dumps({"state": "none"}))
        return
    if len(paths) != 1 or os.path.islink(paths[0]) or not os.path.isfile(paths[0]):
        raise ValueError("Claude conversation history is ambiguous")
    expected = os.path.realpath(workspace)
    last_request = -1
    last_completed = -1
    active_prompt = -1
    turn_id = ""
    checked_start = False
    for position, line in enumerate(open(paths[0], "rb")):
        # Claude appends records while running. An incomplete final record is
        # not evidence of a new prompt or a completed turn.
        if not line.endswith(b"\n"):
            break
        event = json.loads(line)
        native_id = event.get("sessionId") or event.get("session_id")
        if native_id and native_id != session_id:
            raise ValueError("Claude transcript does not match the saved conversation")
        cwd = event.get("cwd")
        if cwd and not checked_start:
            if os.path.realpath(cwd) != expected:
                raise ValueError("Claude transcript does not start in the task workspace")
            checked_start = True
        if is_prompt(event):
            last_request = active_prompt = position
            turn_id = event.get("uuid") or ""
        elif event.get("type") == "queue-operation" and event.get("operation") == "enqueue" and event.get("sessionId") == session_id:
            last_request = position
            turn_id = ""
        elif event.get("type") == "system" and event.get("subtype") == "turn_duration" and not event.get("isSidechain") and active_prompt >= 0:
            last_completed = active_prompt
    if last_request < 0:
        state = "none"
    elif not checked_start:
        raise ValueError("Claude transcript has no task workspace anchor")
    elif last_completed >= last_request:
        state = "completed"
    else:
        state = "inProgress"
    print(json.dumps({"id": turn_id, "state": state}))


if __name__ == "__main__":
    main(*sys.argv[1:])
