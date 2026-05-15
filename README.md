# AIGW Control Plane UI (Demo)

Separate control-plane demo repo that pushes MCP policy config to a Docker Envoy AI Gateway instance.

## What it does

- Hosts a small UI on `:18081`
- Scans a GitHub repo URL for `SKILL.md` files and extracts tool names
- Generates `MCPRoute` config with `userToolPolicies`
- Applies config by rolling `aigw-mcp-test` container with mounted config file
- Demonstrates dynamic policy updates from UI to gateway runtime

## Prereqs

- Docker
- Local image built from wired branch:
  - `aigw-local-wiring:dev`

## Run

```bash
go run .
```

Open `http://127.0.0.1:18081`.

## Verify dynamic policy

1. Scan repo URL in UI.
2. Apply policy with toggle OFF.
3. Use MCP initialize + `tools/list` with `x-user-id: user-a`; expect only `kiwi__search-flight`.
4. Apply policy with toggle ON.
5. Repeat; expect `kiwi__feedback-to-devs` to appear.
