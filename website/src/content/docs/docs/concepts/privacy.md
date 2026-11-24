---
title: Privacy
description: Are we evil,,,
---

## What we never look at

- prompt text, we only get a hash that proves later what you sent, if you choose to verify a project
- what you type, we only record events _when_ you type
- code, file contents and tool inputs or outputs
- command lines
- local paths

## What's sent to the server

Sablier works with a bunch of events, heartbeats that we use along with some heuristics to calculate the time you spent on something. (The server's algorithm is the same as the one used in `sablier today`)

`sablier sync` sends them in batches of up to 500, with your device credential and a `sablier/<version> (<os>/<arch>)` user agent:

```json
{
  "install_id": "3f2c9a1e-…",
  "events": [ … ],
  "projects": [{ "id": "github.com/espcaa/sablier", "name": "sablier" }]
}
```

`install_id` is a random id made once per machine. `projects` gives each project id a display name.

Every event has these fields:

| Field        | What it is                                                                                                                                       |
| ------------ | ------------------------------------------------------------------------------------------------------------------------------------------------ |
| `seq`        | a counter per machine, 1, 2, 3…                                                                                                                  |
| `ts`         | when it happened, in utc                                                                                                                         |
| `kind`       | one of the kinds below                                                                                                                           |
| `session_id` | `run:` and a random id, new every time you start an agent through sablier                                                                        |
| `project_id` | the first line of a `.sablier-project` or `.wakatime-project` file, else the git remote (like `github.com/espcaa/sablier`), else the folder name |
| `harness`    | `claude-code`, `codex` or `opencode`                                                                                                             |
| `source`     | `launcher` for your terminal input, `adapter` for what the agent reports                                                                         |
| `data`       | depends on the kind                                                                                                                              |
| `prev_hash`  | the `hash` of the event before it                                                                                                                |
| `hash`       | a sha-256 of this event and `prev_hash`, so changing or removing an event after it was sent breaks every hash after it                           |

And the `data` for each kind, exactly:

| Kind               | `data`                                                                                                               |
| ------------------ | -------------------------------------------------------------------------------------------------------------------- |
| `session_start`    | `{"model": "claude-opus-5-5", "harness_session": "…"}`, the agent's own session id and the model when it reports one |
| `session_end`      | `{"reason": "exit"}`                                                                                                 |
| `input_tick`       | `{"window_ms": 5000, "keys": 42, "mouse": 3, "pasted": false}`, how many keys, scrolls and clicks, never which ones  |
| `focus`            | `{"in": true}` when you come back to the terminal, `false` when you leave                                            |
| `prompt_sent`      | `{"pasted": false, "compose_ms": 0, "queued": false, "approval_like": false, "hash": "…"}`                           |
| `approval`         | `{"allow": true, "latency_ms": 2300, "tool": "shell"}`, allowed or not and how long you took                         |
| `interrupt`        | `{}`                                                                                                                 |
| `agent_turn_start` | `{"turn_id": "…"}`                                                                                                   |
| `agent_turn_end`   | `{"turn_id": "…", "reason": "done"}`, or `interrupted`, `error`, `closed`                                            |
| `tool_call`        | `{"tool": "edit", "duration_ms": 840}`, the tool is `shell`, `edit`, `read`, `web`, `agent` or `other`               |

The prompt `hash` is an hmac-sha256 of the prompt (lowercased, spaces squashed), keyed with something made from a secret that never leaves your machine and that prompt's session and time. We can't reverse it, and the same prompt sent twice hashes differently, so we can't even tell when you repeat yourself. It's only there so that if you verify a project later, the server can check the prompts you send match what you actually sent back then.

## What's stored on your device

Everything is in a sqlite database in your data folder (see [configuration](/docs/reference/configuration/)).

That includes cached prompt text. If you udr `sablier verify`, the cli uploads a subset of your prompts, with their individual hashes.
