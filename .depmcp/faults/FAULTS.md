# depmcp faults found in itself

Generated from `issues.jsonl` and `resolved.jsonl`; do not edit by hand.
Read the open entries, fix them, then record a resolution with `depmcp faults resolve <id>`.
A fault seen again after its resolution reopens automatically.

4 open, 1 resolved.

## DEP-A569750B — no panic recovery at MCP tool boundaries

- kind: `tool-failure` · severity: `bug` · seen 1 time(s)
- first: 2026-11-08T06:51:42.112155649Z · last: 2026-11-08T06:51:42.112155649Z
- command: `faults add`
- **fix:** Wrap tool handlers with a defer/recover that records a KindInternal fault and returns an isError result, so a panic is journaled instead of vanishing into the transport.

```
wrap() records unexpected errors but a panic never reaches it.
```

## DEP-9D45551D — yarn berry is refused rather than supported

- kind: `capability-gap` · severity: `gap` · seen 1 time(s)
- first: 2026-11-08T06:51:42.144482007Z · last: 2026-11-08T06:51:42.144482007Z
- command: `faults add`
- **fix:** Implement a berry lockfile reader (__metadata, checksum, PnP) as a separate ecosystem; the v1 reader deliberately rejects it.

```
v2eco detects a berry yarn.lock and Inventory returns an error.
```

## DEP-9DE3A533 — scheduled runs ignore auto_pr

- kind: `capability-gap` · severity: `gap` · seen 1 time(s)
- first: 2026-11-08T06:51:42.080089032Z · last: 2026-11-08T06:51:42.080089032Z
- command: `faults add`
- **fix:** Honour defaults.auto_pr in RunOnce: when true and verification passed, call OpenPullRequest for each group instead of stopping at the branch.

```
RunOnce always stops before publishing, with the summary text saying so, even when auto_pr is true.
```

## DEP-B8A77617 — advisory target version is recorded without the v prefix

- kind: `capability-gap` · severity: `gap` · seen 1 time(s)
- first: 2026-11-08T06:51:42.062405518Z · last: 2026-11-08T06:51:42.062405518Z
- command: `faults add`
- **fix:** Normalize fixed versions from OSV into the ecosystem's spelling when the plan is built, so a target reads v0.44.0 like every other Go version in the plan.

```
plan for the spike module proposed golang.org/x/sys 0.44.0 while go.mod records v0.41.0; go get accepts either but the plan is internally inconsistent.
```

## Resolved

- DEP-435CC37F — OSV fixed versions without a v prefix were dropped by policy (resolved 2026-11-08T06:51:42.207263797Z: fixed in v0.9.2 by normalizing the v prefix in gomod.Compare and Classify)
