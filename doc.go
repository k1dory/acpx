// Package acpx is an Agent Client Protocol (https://agentclientprotocol.com) client layer on top
// of github.com/coder/acp-go-sdk: ready calls for what the protocol and today's agents
// (claude-agent-acp, codex-acp, OpenCode, Hermes) offer beyond what the SDK (v0.13.5) types.
// It was written for a headless client that drives agents in sandboxes (k1dory/cluster-agent-k8s).
//
// What it adds:
//
//   - Wire, a JSON-RPC level layer between the connection and the agent's stdio. It reads the
//     agent's capabilities the SDK drops from the initialize response (the client system prompt,
//     steering), advertises the client's session capabilities the SDK cannot express (live
//     notices, compaction updates), carries a session's system prompt, and hands over the session
//     updates the SDK silently zeroes (notice, compaction_update, compaction_summary_chunk).
//   - Conn: the SDK's client-side connection over a Wire, with Steer (_session/steering: a message
//     joins the turn in progress) and the agent's capabilities.
//   - Config options: select a session's mode, model or reasoning level by category (Configure),
//     matching the agent's own values.
//   - SessionMeta: a session request's _meta from the caller's free-form metadata and the system
//     prompt.
//
// Everything here is a client-side convenience over published ACP surfaces; when the SDK gains a
// field (systemPrompt, notices), the wire-level part of it can go.
package acpx
