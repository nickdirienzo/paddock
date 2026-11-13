#!/usr/bin/python3
"""Return only public assistant text and completed turns from a pinned transcript."""

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
        print("[]")  # The saved session may be waiting at its first native prompt.
        return
    if len(paths) != 1 or os.path.islink(paths[0]) or not os.path.isfile(paths[0]):
        raise ValueError("Claude conversation history is missing or ambiguous")
    expected = os.path.realpath(workspace)
    anchored = False
    turn_id = ""
    active = False
    events = []
    with open(paths[0], "rb") as transcript:
        for line in transcript:
            if not line.endswith(b"\n"):
                break  # An incomplete append is not a saved event.
            event = json.loads(line)
            native_id = event.get("sessionId") or event.get("session_id")
            if native_id and native_id != session_id:
                raise ValueError("Claude transcript does not match the saved conversation")
            cwd = event.get("cwd")
            if cwd and not anchored:
                if os.path.realpath(cwd) != expected:
                    raise ValueError("Claude transcript does not start in the task workspace")
                anchored = True
            if is_prompt(event):
                turn_id = event.get("uuid") or ""
                if not turn_id:
                    raise ValueError("Claude prompt has no stable native ID")
                active = True
            elif event.get("type") == "assistant" and not event.get("isSidechain") and active:
                message = event.get("message")
                if not isinstance(message, dict) or message.get("role") != "assistant":
                    continue
                content = message.get("content")
                if not isinstance(content, list):
                    continue
                text = "".join(block.get("text", "") for block in content
                               if isinstance(block, dict) and block.get("type") == "text"
                               and isinstance(block.get("text"), str))
                if text.strip():
                    event_id = event.get("uuid") or ""
                    if not event_id:
                        raise ValueError("Claude public message has no stable native ID")
                    phase = "final_answer" if message.get("stop_reason") == "end_turn" else "commentary"
                    events.append({"id": event_id, "type": "message", "turn_id": turn_id,
                                   "phase": phase, "text": text})
            elif (event.get("type") == "system" and event.get("subtype") == "turn_duration"
                  and not event.get("isSidechain") and active):
                event_id = event.get("uuid") or ""
                if not event_id:
                    raise ValueError("Claude turn completion has no stable native ID")
                events.append({"id": event_id, "type": "turn_finished", "turn_id": turn_id,
                               "state": "completed"})
                active = False
    if not anchored:
        raise ValueError("Claude transcript has no task workspace anchor")
    print(json.dumps(events, ensure_ascii=False))


if __name__ == "__main__":
    main(*sys.argv[1:])
