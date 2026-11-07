#!/usr/bin/env python3
"""Wire-level MCP client for the mcp-probe spike.

Speaks JSON-RPC over the server's stdio transport and answers the server->client
requests that a real MCP client would answer (elicitation and sampling), so the
M-1 capability questions can be settled without a GUI client in the loop.

Usage: probe_client.py /path/to/mcp-probe
Prints a transcript plus a PASS/FAIL summary; exits non-zero on failure.
"""

import json
import subprocess
import sys
import threading
import time

PROTO = "2025-06-18"


class Client:
    def __init__(self, binary):
        self.proc = subprocess.Popen(
            [binary],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            bufsize=1,
        )
        self.next_id = 100
        self.inbox = []
        self.lock = threading.Lock()
        self.stderr_lines = []
        self.events = []          # (kind, detail) transcript
        self.raw_log = []         # every parsed frame, for forensics
        self.stop = False
        threading.Thread(target=self._read_stdout, daemon=True).start()
        threading.Thread(target=self._read_stderr, daemon=True).start()

    def _read_stdout(self):
        for line in self.proc.stdout:
            line = line.strip()
            if not line:
                continue
            try:
                msg = json.loads(line)
            except json.JSONDecodeError:
                self.events.append(("garbage", line[:120]))
                continue
            with self.lock:
                self.inbox.append(msg)
                self.raw_log.append(msg)

    def _read_stderr(self):
        for line in self.proc.stderr:
            self.stderr_lines.append(line.rstrip())

    def send(self, obj):
        self.proc.stdin.write(json.dumps(obj) + "\n")
        self.proc.stdin.flush()

    def reply(self, req_id, result):
        self.send({"jsonrpc": "2.0", "id": req_id, "result": result})

    def serve_server_requests(self, msg):
        """Answer server->client requests the way a real client would."""
        method = msg.get("method", "")
        rid = msg.get("id")
        if method == "elicitation/create":
            params = msg.get("params", {})
            self.events.append(("elicit", params.get("message", "")[:80]))
            self.reply(rid, {"action": "accept", "content": {"proceed": True}})
            return True
        if method == "sampling/createMessage":
            self.events.append(("sample", f"maxTokens={msg.get('params', {}).get('maxTokens')}"))
            self.reply(rid, {
                "role": "assistant",
                "content": {"type": "text", "text": "STUB: summarized by the client's model."},
                "model": "stub-model-1",
            })
            return True
        return False

    def wait_for(self, want_id, timeout=25.0):
        """Collect messages until the response with want_id arrives.

        Notifications and server->client requests seen on the way are recorded
        and answered; they are the evidence for Q1/Q2/Q3.
        """
        deadline = time.time() + timeout
        progress = []
        while time.time() < deadline:
            with self.lock:
                pending, self.inbox = self.inbox, []
            for msg in pending:
                if msg.get("id") == want_id and "method" not in msg:
                    return msg, progress
                if "id" in msg and "method" in msg:
                    if not self.serve_server_requests(msg):
                        self.reply(msg["id"], {"ok": False})
                elif msg.get("method", "").endswith("notifications/progress"):
                    params = msg.get("params", {})
                    progress.append(params)
                else:
                    self.events.append(("notification", msg.get("method", "")))
            time.sleep(0.02)
        return None, progress

    def call(self, rid, method, params=None):
        self.send({"jsonrpc": "2.0", "id": rid, "method": method, "params": params or {}})
        return self.wait_for(rid)

    def close(self):
        try:
            self.proc.stdin.close()
        except Exception:
            pass
        try:
            self.proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.proc.kill()


def main():
    binary = sys.argv[1] if len(sys.argv) > 1 else "./mcp-probe"
    c = Client(binary)
    results = {}

    # Capabilities must be declared up front: the server gates elicitation on the
    # client advertising it, and answers "client does not support elicitation"
    # otherwise, even when the client is perfectly able to answer the request.
    init, _ = c.call(1, "initialize", {
        "protocolVersion": PROTO,
        "capabilities": {"elicitation": {}, "sampling": {}, "roots": {"listChanged": True}},
        "clientInfo": {"name": "probe-client", "version": "0.0.1"},
    })
    if not init or "result" not in init:
        print("FAIL: no initialize response")
        print("\n".join(c.stderr_lines[-10:]))
        return 1
    caps = init["result"].get("capabilities", {})
    results["initialize"] = f"server={init['result'].get('serverInfo', {}).get('name')} caps={sorted(caps)}"
    c.send({"jsonrpc": "2.0", "method": "notifications/initialized"})

    # Q4: schema cost — read the generated schemas off the wire.
    tl, _ = c.call(2, "tools/list")
    tools = {t["name"]: t for t in tl["result"]["tools"]} if tl and "result" in tl else {}
    results["tools"] = sorted(tools)
    ping = tools.get("ping", {})
    results["Q4_schema"] = {
        "inputSchema": json.dumps(ping.get("inputSchema", {}), sort_keys=True),
        "outputSchema_present": "outputSchema" in ping,
        "annotations": ping.get("annotations"),
    }

    # Q1: elicitation — does the server's Elicit() reach us and come back?
    rw, _ = c.call(3, "tools/call", {
        "name": "confirm_write",
        "arguments": {"reason": "bump example.com/pkg 1.0.0 -> 1.1.0"},
    })
    results["Q1_elicitation"] = (
        rw["result"].get("structuredContent") if rw and "result" in rw else rw
    )

    # Q3: progress — do notifications arrive *during* the call?
    t0 = time.time()
    rs, prog = c.call(4, "tools/call", {
        "name": "slow_task",
        "arguments": {"steps": 4},
        "_meta": {"progressToken": "tok-progress-1"},
    })
    elapsed = time.time() - t0
    results["Q3_progress"] = {
        "notifications_received": len(prog),
        "progress_values": [p.get("progress") for p in prog],
        "call_elapsed_s": round(elapsed, 2),
        "structured": rs["result"].get("structuredContent") if rs else None,
    }

    # Q2: sampling — can the server ask our model for a completion?
    samp, _ = c.call(5, "tools/call", {
        "name": "summarize",
        "arguments": {"text": "Release 2.0 removes the deprecated Run() shim and drops Go 1.22."},
    })
    results["Q2_sampling"] = (
        samp["result"].get("structuredContent") if samp and "result" in samp else samp
    )

    # Resources, since §7 leans on ten of them.
    rl, _ = c.call(6, "resources/list")
    names = [r["uri"] for r in rl["result"]["resources"]] if rl and "result" in rl else []
    rr, _ = c.call(7, "resources/read", {"uri": "probe://info"})
    results["resources"] = {
        "listed": names,
        "read_ok": bool(rr and "result" in rr),
        "bytes": len(rr["result"]["contents"][0]["text"]) if rr and "result" in rr else 0,
    }

    # Q5: cancellation — does the server stop a long tool when we cancel it?
    cid = 20
    c.send({"jsonrpc": "2.0", "id": cid, "method": "tools/call",
            "params": {"name": "slow_task", "arguments": {"steps": 40}}})
    time.sleep(0.35)
    c.send({"jsonrpc": "2.0", "method": "notifications/cancelled",
            "params": {"requestId": cid, "reason": "driver test"}})
    rc, prog5 = c.wait_for(cid, timeout=20)
    results["Q5_raw_frames_for_request"] = [m for m in list(c.raw_log)
                                            if m.get("id") == cid]
    # The SDK reports cancellation as a *tool result* with isError:true and the
    # text "context canceled" -- not as a JSON-RPC error object, and not with
    # code -32800. Callers must therefore test isError, and a server that wants
    # a stable code must map ctx.Err() itself.
    res_obj = (rc or {}).get("result") or {}
    results["Q5_cancel"] = {
        "stopped_early": bool(rc and (rc.get("error") or res_obj.get("isError"))),
        "cancellation_shape": {
            "jsonrpc_error": (rc or {}).get("error"),
            "isError": res_obj.get("isError"),
            "text": ((res_obj.get("content") or [{}])[0] or {}).get("text"),
        },
        "partial_structured": (rc or {}).get("result", {}).get("structuredContent")
        if rc and "result" in rc else None,
        "progress_before_cancel": len(prog5),
    }

    env, _ = c.call(8, "tools/call", {"name": "env", "arguments": {}})
    results["env"] = env["result"].get("structuredContent") if env else None

    c.close()

    print(json.dumps(results, indent=2))
    print("--- server stderr (last 8) ---")
    for line in c.stderr_lines[-8:]:
        print("  " + line)

    # Verdicts
    ok = True
    def check(label, cond, note=""):
        nonlocal ok
        print(("PASS  " if cond else "FAIL  ") + label + (f"  ({note})" if note else ""))
        ok = ok and cond

    check("initialize + tools/list", bool(tools), f"{len(tools)} tools")
    check("Q1 elicitation round-trips",
          results["Q1_elicitation"] and results["Q1_elicitation"].get("action") == "accept",
          str(results["Q1_elicitation"]))
    check("Q3 progress delivered",
          results["Q3_progress"]["notifications_received"] >= 3,
          f"{results['Q3_progress']['notifications_received']} notifications")
    check("Q2 sampling round-trips",
          results["Q2_sampling"] and results["Q2_sampling"].get("supported") is True,
          str(results["Q2_sampling"]))
    check("resources list + read", bool(names) and results["resources"]["read_ok"])
    check("Q5 cancellation stops the tool",
          bool(results["Q5_cancel"]["stopped_early"]),
          f"progress_before_cancel={results['Q5_cancel']['progress_before_cancel']}")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
