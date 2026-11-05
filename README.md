# acpx

An [Agent Client Protocol](https://agentclientprotocol.com) client layer for Go, on top of
[`github.com/coder/acp-go-sdk`](https://github.com/coder/acp-go-sdk) v0.13.5. It adds ready-made
calls for protocol features and agent extensions that the SDK does not type yet. The agents it
was checked against are claude-agent-acp 0.100.0, codex-acp 2.5.0, OpenCode 1.19.5 and Hermes
Agent 0.22.7.

It was written for a headless client that drives agents in sandboxes
([k1dory/cluster-agent-k8s](https://github.com/k1dory/cluster-agent-k8s), `cmd/agent-runner`),
but it does not depend on that project.

```go
conn := acpx.Dial(client, agentStdin, agentStdout, acpx.WireOptions{
	Notices: true, Compaction: true,
	OnUpdate: func(u acpx.ExtraUpdate) { /* notice, compaction_update, compaction_summary_chunk */ },
})
init, _ := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})

var meta map[string]any
if conn.Wire.SystemPrompt() { // sessionCapabilities.systemPrompt
	meta = acpx.SessionMeta(nil, "Follow the repository's conventions.")
}
s, _ := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: dir, McpServers: []acp.McpServer{}, Meta: meta})
st := acpx.SessionState{Modes: s.Modes, Options: s.ConfigOptions}
_ = acpx.Configure(ctx, conn, s.SessionId, st, acpx.ModeSetting, "agent-full-access")
_ = acpx.Configure(ctx, conn, s.SessionId, st, acpx.ThoughtLevelSetting, "high")

// Later, while a prompt turn runs:
out, _ := conn.Steer(ctx, s.SessionId, "also update the tests") // injected | promptRequired
```

## What it adds

| | |
|---|---|
| `Wire` | A JSON-RPC layer between the connection and the agent's stdio. The SDK drops some fields from `initialize`; `Wire` reads two of them, `sessionCapabilities.systemPrompt` and `_meta.steering.supported`. It advertises `clientCapabilities.session.notices` / `.compaction`, which `ClientCapabilities` cannot express. It carries a session's client system prompt (`systemPrompt`, mode `append`). It also decodes `notice`, `compaction_update` and `compaction_summary_chunk`, which the SDK silently zeroes. Lines are handled up to the SDK's own 10 MiB limit, the same way however the agent's output is split into reads. |
| `Conn` | The SDK's `ClientSideConnection` over a `Wire`. Adds `Steer`: the `_session/steering` extension (claude-agent-acp, codex-acp), which puts a message into the turn in progress. It uses `idleBehavior: promptRequired`, so the client still owns every turn. |
| `Configure` | Selects a session's mode, model or reasoning level (`thought_level`) through the agent's configuration options. Mode falls back to `session/set_mode`. It matches the agent's own values: the exact value, a provider-qualified value (`anthropic/claude-…`), or a display name. Anything it cannot apply comes back as an error to report. |
| `SessionMeta` | Builds a session request's `_meta` from your metadata (for example claude-agent-acp's `claudeCode.options`) plus the system prompt. |

When the SDK gains these fields, the wire-level parts can be removed.

## Development

```sh
go vet ./... && go test -race ./...
```

Apache-2.0.
