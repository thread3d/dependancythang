# M-1 spike findings — official Go MCP SDK

Recorded 2026-11-07. This is the milestone-zero risk spike the specification calls
for: five questions about the server's foundation, answered **by running**, not by
reading release notes. Everything below is reproducible with the two files in this
directory.

- SDK: `github.com/modelcontextprotocol/go-sdk` **v1.8.0**
- Go: `go1.26.10-X:nodwarf5 linux/amd64`
- Protocol version negotiated: **2025-06-18**
- Build: `CGO_ENABLED=0`, statically linked, 11.8 MB binary
- Harness: `probe_client.py` speaks raw JSON-RPC over the server's stdio and
  answers server→client requests (elicitation, sampling) the way a real client would.

```bash
go build -o mcp-probe .
python3 probe_client.py ./mcp-probe
```

## Verdicts

| # | Question | Result |
| --- | --- | --- |
| Q1 | Elicitation round-trips | **Yes**, with a precondition (below) |
| Q2 | Sampling works | **Yes** |
| Q3 | Progress arrives mid-call | **Yes**, 4/4 during an 0.8 s call |
| Q4 | Schema-from-Go-structs cost | **Near zero** — struct tags only |
| Q5 | Cancellation stops a tool cleanly | **Yes**, with a shape surprise |

## Q1 — Elicitation: works only if the client declares the capability

The server's `ServerSession.Elicit` returns a hard error **`client does not support
elicitation`** when the client's `initialize` omitted `capabilities.elicitation`,
even though the client was perfectly capable of answering the request. Declaring the
capability produced a full round-trip:

```
{"action": "accept", "consent": {"proceed": true}, "detail": "elicitation returned"}
```

**Consequence for §7/§5:** the `confirm: true` fallback is not an edge case for
ancient clients — it is the default path for any client that does not advertise
elicitation. Every write tool must branch on the error and fall back, and the
confirmation payload must be computed from plan data so the fallback is as safe as
the interactive path.

## Q2 — Sampling: works, and is *not* capability-gated

`ServerSession.CreateMessage` succeeded and returned both a model name and content.
Notably it worked on the very first run, when the client advertised **no**
capabilities at all — the SDK does not gate sampling the way it gates elicitation.
That asymmetry matters: a client may auto-approve sampling silently, so sampled text
must be treated as untrusted input, exactly as §11 already requires for changelogs.

## Q3 — Progress: delivered mid-call, but only with a token

Four `notifications/progress` frames arrived during a 0.8 s call, carrying
`progress` 1→4 of `total` 4:

```
"notifications_received": 4, "progress_values": [1, 2, 3, 4], "call_elapsed_s": 0.8
```

Delivery requires the caller to put `_meta.progressToken` on the request. A later
call without the token received **zero** notifications — the SDK does not invent one.
So long tools must read `req.Params.GetProgressToken()` and skip emitting when nil.

## Q4 — Schema cost: struct tags, nothing hand-written

Input and output schemas are inferred from Go structs (`json` for names,
`jsonschema` for descriptions), and `outputSchema` is populated too:

```json
{"additionalProperties": false,
 "properties": {"message": {"description": "text to echo back", "type": "string"},
                "repeat": {"description": "how many times to repeat it", "type": "integer"}},
 "required": ["message"], "type": "object"}
```

Annotations serialize as expected: `readOnlyHint`, `idempotentHint`, `openWorldHint`
appeared; `destructiveHint` was omitted when unset. Note `IdempotentHint` is a plain
`bool` while `DestructiveHint`/`OpenWorldHint` are `*bool`, so an unset
`idempotentHint` still emits `false` — harmless, but it means "unset" is not
distinguishable for that one hint.

## Q5 — Cancellation: cooperative, but reported as a tool error

Sending `notifications/cancelled` for an in-flight call stopped the work — the
server logged `slow_task cancelled at step 3/40` and returned immediately. But the
response was **not** a JSON-RPC error and did **not** use code `-32800`:

```json
{"jsonrpc": "2.0", "id": 20,
 "result": {"content": [{"type": "text", "text": "context canceled"}], "isError": true}}
```

Three consequences, all of which the specification must absorb:

1. Client-side cancellation detection is `isError`, not a JSON-RPC `error`.
2. There is **no structured output** on a cancelled call — so anything that must be
   recorded (a run marked `aborted`) must be persisted by the handler *before* it
   returns, never inferred from the response.
3. The text is the bare `context canceled`, so a stable error code from §7's closed
   set has to be mapped by us from `ctx.Err()`, not sourced from the SDK.

## Not covered by this spike

- **Prompts.** `Server.AddPrompt` exists in the API, but prompts were not exercised.
  Low risk; verify during M6 when the four prompts are actually written.
- **A GUI client.** This harness is a faithful wire-level client, but real-client
  behaviour around elicitation UX (whether the prompt is actually shown to a human,
  and what `Action` comes back on dismissal) still needs one run against Claude Code
  and one against DSH.
- **Streamable HTTP.** Irrelevant to Rev 2, which has no daemon.
