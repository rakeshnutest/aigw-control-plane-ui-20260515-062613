# AIGW Control Plane UI (Demo)

Separate control-plane demo repo that pushes MCP policy config to a Docker Envoy AI Gateway instance.

## What it does

- Hosts a small UI on `:18081`
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

Open `http://127.0.0.1:18081` and click **Apply To Envoy**.

## Verify

Use MCP initialize + tools/list curl flow on `http://127.0.0.1:1975/mcp` with header `x-user-id: user-a`.
- unchecked toggle: user-a should only see `kiwi__search-flight`
- checked toggle: user-a should also see `kiwi__feedback-to-devs`
