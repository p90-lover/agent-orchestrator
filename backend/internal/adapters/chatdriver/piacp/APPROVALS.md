# Pi tool approvals in Chat

pi-acp 0.17 sends no ACP `session/request_permission` and binds no UI context, so a
Pi extension's `ctx.ui.confirm` cannot prompt. Until now AO therefore declared
approvals unsupported for Pi and ran it in bypass only. This note describes how
Pi asks the chat before it runs a tool, so "Ask for approval" and "Approve for me"
work on Pi.

## Path of one request

1. **Pi extension** (`adapters/agent/pi/hooks.go`, AO's managed `ao-activity.ts`).
   A `tool_call` handler runs before every tool. For anything but the read-only
   tools (`read`, `grep`, `find`, `ls`) it starts `ao hooks pi tool-call`
   **asynchronously** (`child_process.spawn`, never `spawnSync`: the extension runs
   inside the pi-acp daemon, and a blocking call would stall it), writes the tool
   call to its stdin and awaits one JSON line: `{"allow": bool, "reason": string}`.
   A deny, a failure or no answer returns `{block: true, reason}` to Pi, so the tool
   does not run. The handler is active only when AO set the approval env below,
   so terminal Pi sessions are unchanged.
2. **`ao hooks pi tool-call`** (`cli/hooks.go`) finds the daemon the way every hook
   does (run file, loopback) and posts to
   `POST /internal/chat/sessions/{sessionId}/tool-approval` with the session's
   token as `Authorization: Bearer`. It waits as long as the request is pending
   and prints the answer.
3. **Daemon route** (`httpd/router.go`). Local-only (`localControlRequest`: no
   Origin, loopback host; `/internal/` is not served on the LAN listener) and
   outside the public `/api/v1` spec. It hands the request to
   `chat.Service.RequestToolApproval`.
4. **Chat service and controller** find the session's live conversation and call
   `ports.ChatToolApprovalRequester.RequestToolApproval`.
5. **ACP conversation** (`chatdriver/acp`) checks the token, then builds an ACP
   `RequestPermissionRequest` (tool call id, kind, title, raw input; options
   allow once / reject once) and passes it to its own `RequestPermission`: the
   exact path a real ACP request takes. So the binding's `PermissionPolicy`
   answers what the mode allows (bypass and auto: everything; accept edits:
   edits), and everything else parks as a pending decision that emits
   `approval.requested`. The app's approval card and its "Approve for me"
   reviewer answer it through the ordinary `ResolveRequest`, unchanged.

## Authentication

The route accepts a request only with the session's own token. piacp creates it
once per session (`<data>/pi-acp/<session>/approval-token`, 0600, 32 random
bytes) and gives the same value to the pi-acp daemon's env
(`AO_PI_APPROVAL_TOKEN`) and to the conversation (acp `Config.ToolApprovalToken`).
The file keeps it stable across AO restarts, because a running pi-acp daemon
keeps the env it started with. The conversation compares in constant time. Other
processes on the machine cannot file requests for a session, and no request can
be answered except through the chat's own resolve route.

## Why not other routes

- A Pi extension cannot reach the ACP client: pi-acp's ext-method dispatcher is
  client-to-agent only.
- A new `/api/v1` route would join the public spec and the generated renderer
  schema; this is AO-internal plumbing between its own CLI and daemon.
- `spawnSync`, as the activity hooks use, would block the pi-acp daemon for as
  long as the person takes to answer.

## What it does not cover

- Tools Pi runs before the extension loads, and extensions that register tools
  with their own execution path outside Pi's tool loop.
- If AO's daemon is unreachable, Pi's mutating tools are blocked (the extension
  fails closed) until it is back.

## Daemons from before this change

A pi-acp daemon keeps the extension and env it started with. One AO started
without the approval env would never ask, so piacp marks each daemon it starts
with that env (`<data>/pi-acp/<session>/daemon-approvals`) and, on launch, stops
a running daemon that has no marker (its control socket's `POST /shutdown`, as
`pi-acp --daemon-stop` does) and starts a fresh one. Pi keeps its sessions on
disk, and AO resumes the chat on the new daemon.
