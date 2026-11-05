package acpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// initialize gains clientCapabilities.session.{notices,compaction} as asked; what the SDK put
// there stays, and nothing else is touched.
func TestWireAdvertisesSessionCapabilities(t *testing.T) {
	var sent bytes.Buffer
	w := NewWire(&sent, strings.NewReader(""), WireOptions{Notices: true, Compaction: true})
	req := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{"fs":{"readTextFile":false,"writeTextFile":false},"terminal":false}}}` + "\n"
	if n, err := w.Write([]byte(req)); err != nil || n != len(req) {
		t.Fatalf("write: %d %v", n, err)
	}
	var got struct {
		Params struct {
			ProtocolVersion    int `json:"protocolVersion"`
			ClientCapabilities struct {
				FS       map[string]bool            `json:"fs"`
				Terminal *bool                      `json:"terminal"`
				Session  map[string]json.RawMessage `json:"session"`
			} `json:"clientCapabilities"`
		} `json:"params"`
	}
	if err := json.Unmarshal(sent.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	c := got.Params.ClientCapabilities
	if got.Params.ProtocolVersion != 1 || c.Terminal == nil || len(c.FS) != 2 || string(c.Session["notices"]) != "{}" || string(c.Session["compaction"]) != "{}" {
		t.Fatalf("initialize: %s", sent.String())
	}
	// Without the options, initialize passes as it is; so does every other message.
	sent.Reset()
	plain := NewWire(&sent, strings.NewReader(""), WireOptions{})
	_, _ = plain.Write([]byte(req))
	if sent.String() != req {
		t.Fatalf("rewritten without options: %s", sent.String())
	}
	sent.Reset()
	other := `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"s","prompt":[]}}` + "\n"
	_, _ = w.Write([]byte(other))
	if sent.String() != other {
		t.Fatalf("prompt changed: %s", sent.String())
	}
}

// The Wire reads steering support off initialize's _meta; the updates the SDK does not know reach
// the client in their place among the SDK's own, through Classify: a notice is not taken for the
// session_info_update the SDK decodes it as, a message around it stays one message.
func TestClassifyKeepsOrderAndKind(t *testing.T) {
	lines := strings.Join([]string{
		`{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":1,"agentCapabilities":{},"_meta":{"steering":{"supported":true}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Hel"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"notice","severity":"warning","title":"Rate limit near","description":"80% used"}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"lo"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"compaction_update","compactionId":"c1","status":"in_progress"}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"compaction_summary_chunk","compactionId":"c1","content":{"type":"text","text":"so far"}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"compaction_update","compactionId":"c1","status":"completed","summary":[{"type":"text","text":"done"}]}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"subagent_update","child":"x"}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"session_info_update","title":"Real title"}}}`,
	}, "\n") + "\n"
	c := &classifying{done: make(chan struct{})}
	c.wire = NewWire(io.Discard, chunked{strings.NewReader(lines), 5}, WireOptions{})
	c.wire.Connect(c)
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		t.Fatalf("updates: %q", c.got)
	}
	want := []string{"sdk agent_message_chunk Hel", "notice warning Rate limit near 80% used", "sdk agent_message_chunk lo",
		"compaction c1 in_progress ", "summary c1 so far", "compaction c1 completed done", "unknown", "sdk session_info_update Real title"}
	if !c.wire.Steering() || !reflect.DeepEqual(c.got, want) {
		t.Fatalf("steering %v, updates:\n%q\nwant\n%q", c.wire.Steering(), c.got, want)
	}
}

// classifying is a client that records what Classify makes of every update.
type classifying struct {
	acp.Client
	wire *Wire
	mu   sync.Mutex
	got  []string
	done chan struct{}
}

func (c *classifying) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	extra, sdk := c.wire.Classify(n.Update)
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case sdk && n.Update.AgentMessageChunk != nil:
		c.got = append(c.got, "sdk agent_message_chunk "+n.Update.AgentMessageChunk.Content.Text.Text)
	case sdk && n.Update.SessionInfoUpdate != nil:
		c.got = append(c.got, "sdk session_info_update "+*n.Update.SessionInfoUpdate.Title)
	case sdk:
		c.got = append(c.got, "sdk "+KindOf(n.Update))
	case extra == nil:
		c.got = append(c.got, "unknown")
	case extra.Notice != nil:
		c.got = append(c.got, fmt.Sprintf("notice %s %s %s", extra.Notice.Severity, extra.Notice.Title, extra.Notice.Description))
	case extra.Compaction != nil:
		c.got = append(c.got, fmt.Sprintf("compaction %s %s %s", extra.Compaction.ID, extra.Compaction.Status, extra.Compaction.Summary))
	case extra.SummaryChunk != nil:
		c.got = append(c.got, fmt.Sprintf("summary %s %s", extra.SummaryChunk.ID, extra.SummaryChunk.Text))
	}
	if len(c.got) == 8 {
		close(c.done)
	}
	return nil
}

// A known update the SDK refused to deliver does not shift the rest: Classify skips past it.
func TestClassifyResynchronises(t *testing.T) {
	w := NewWire(io.Discard, strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"broken"}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"x"}}}}`,
	}, "\n")+"\n"), WireOptions{})
	_, _ = io.ReadAll(w)
	if extra, sdk := w.Classify(acp.UpdateAgentMessageText("x")); !sdk || extra != nil {
		t.Fatalf("resync: %v %v", extra, sdk)
	}
	// Nothing read: the SDK's update stands.
	if _, sdk := w.Classify(acp.UpdateAgentMessageText("y")); !sdk {
		t.Fatal("empty queue")
	}
}

type chunked struct {
	r io.Reader
	n int
}

func (c chunked) Read(p []byte) (int, error) {
	if len(p) > c.n {
		p = p[:c.n]
	}
	return c.r.Read(p)
}

// steeringAgent is an ACP agent on the other end of a real connection: it advertises steering and
// answers _session/steering as claude-agent-acp does.
type steeringAgent struct {
	acp.Agent
	busy bool
	mu   sync.Mutex
	got  []map[string]any
}

func (a *steeringAgent) Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{ProtocolVersion: acp.ProtocolVersionNumber, Meta: map[string]any{"steering": map[string]any{"supported": true}}}, nil
}

func (a *steeringAgent) HandleExtensionMethod(_ context.Context, method string, params json.RawMessage) (any, error) {
	if method != SteerMethod {
		return nil, errors.New("unknown")
	}
	var p map[string]any
	_ = json.Unmarshal(params, &p)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.got = append(a.got, p)
	if !a.busy {
		return map[string]any{"outcome": "promptRequired", "reason": "noRunningTurn"}, nil
	}
	return map[string]any{"outcome": "injected"}, nil
}

// Steer goes to the agent as _session/steering with the text as a prompt and idleBehavior
// promptRequired; the outcome comes back. An agent that did not advertise steering is not asked.
func TestSteer(t *testing.T) {
	toAgent, agentIn := io.Pipe()
	agentOut, fromAgent := io.Pipe()
	agent := &steeringAgent{busy: true}
	acp.NewAgentSideConnection(agent, fromAgent, toAgent)
	conn := Dial(nopClient{}, agentIn, agentOut, WireOptions{})
	ctx := context.Background()
	if _, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
		t.Fatal(err)
	}
	out, err := conn.Steer(ctx, "s1", "also check the tests")
	if err != nil || out != SteerInjected {
		t.Fatalf("busy: %q %v", out, err)
	}
	agent.mu.Lock()
	agent.busy = false
	agent.mu.Unlock()
	if out, err := conn.Steer(ctx, "s1", "next"); err != nil || out != SteerPromptRequired {
		t.Fatalf("idle: %q %v", out, err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	b, _ := json.Marshal(agent.got[0])
	if !strings.Contains(string(b), `"idleBehavior":"promptRequired"`) || !strings.Contains(string(b), `"text":"also check the tests"`) || !strings.Contains(string(b), `"sessionId":"s1"`) {
		t.Fatalf("request: %s", b)
	}

	plain := Dial(nopClient{}, io.Discard, strings.NewReader(""), WireOptions{})
	if out, err := plain.Steer(ctx, "s1", "x"); err != nil || out != SteerPromptRequired {
		t.Fatalf("without steering: %q %v", out, err)
	}
}

type nopClient struct{ acp.Client }

func (nopClient) SessionUpdate(context.Context, acp.SessionNotification) error { return nil }

type recordingConfigurer struct{ calls []string }

func (r *recordingConfigurer) SetSessionMode(_ context.Context, p acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	r.calls = append(r.calls, "mode="+string(p.ModeId))
	return acp.SetSessionModeResponse{}, nil
}

func (r *recordingConfigurer) SetSessionConfigOption(_ context.Context, p acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	r.calls = append(r.calls, string(p.ValueId.ConfigId)+"="+string(p.ValueId.Value))
	return acp.SetSessionConfigOptionResponse{}, nil
}

func option(id string, cat acp.SessionConfigOptionCategory, current string, values ...[2]string) acp.SessionConfigOption {
	opts := acp.SessionConfigSelectOptionsUngrouped{}
	for _, v := range values {
		opts = append(opts, acp.SessionConfigSelectOption{Value: acp.SessionConfigValueId(v[0]), Name: v[1]})
	}
	return acp.SessionConfigOption{Select: &acp.SessionConfigOptionSelect{Id: acp.SessionConfigId(id), Category: &cat,
		CurrentValue: acp.SessionConfigValueId(current), Options: acp.SessionConfigSelectOptions{Ungrouped: &opts}, Type: "select"}}
}

// The reasoning level is selected through the agent's thought_level option, by value or display
// name; an agent without one, or without the level, keeps its own and the caller is told.
func TestConfigureThoughtLevel(t *testing.T) {
	ctx := context.Background()
	st := SessionState{Options: []acp.SessionConfigOption{option("reasoning_effort", acp.SessionConfigOptionCategoryThoughtLevel, "medium",
		[2]string{"low", "Low"}, [2]string{"medium", "Medium"}, [2]string{"xhigh", "Extra high"})}}
	r := &recordingConfigurer{}
	for _, want := range []string{"low", "Extra high", "medium"} {
		if _, err := Configure(ctx, r, "s", st, ThoughtLevelSetting, want); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(r.calls, ",") != "reasoning_effort=low,reasoning_effort=xhigh" {
		t.Fatalf("calls: %v", r.calls)
	}
	if _, err := Configure(ctx, r, "s", st, ThoughtLevelSetting, "ultra"); err == nil || !strings.Contains(err.Error(), `thought_level "ultra" is not one the agent offers [low medium xhigh]`) {
		t.Fatalf("unknown level: %v", err)
	}
	if _, err := Configure(ctx, r, "s", SessionState{}, ThoughtLevelSetting, "low"); err == nil || !strings.Contains(err.Error(), "offers no thought_level option") {
		t.Fatalf("no option: %v", err)
	}
	if _, err := Configure(ctx, r, "s", st, ThoughtLevelSetting, ""); err != nil || len(r.calls) != 2 {
		t.Fatalf("empty: %v %v", err, r.calls)
	}
}

// Selecting a model can change the other options (another model offers other reasoning levels):
// the state Configure returns is the agent's answer, and the next selection goes by it.
func TestConfigureFollowsTheAgentsOptions(t *testing.T) {
	ctx := context.Background()
	st := SessionState{Options: []acp.SessionConfigOption{
		option("model", acp.SessionConfigOptionCategoryModel, "small", [2]string{"small", "Small"}, [2]string{"big", "Big"}),
		option("effort", acp.SessionConfigOptionCategoryThoughtLevel, "low", [2]string{"low", "Low"})}}
	r := &answeringConfigurer{after: []acp.SessionConfigOption{
		option("model", acp.SessionConfigOptionCategoryModel, "big", [2]string{"small", "Small"}, [2]string{"big", "Big"}),
		option("effort", acp.SessionConfigOptionCategoryThoughtLevel, "low", [2]string{"low", "Low"}, [2]string{"xhigh", "Extra high"})}}
	st, err := Configure(ctx, r, "s", st, ModelSetting, "big")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Configure(ctx, r, "s", st, ThoughtLevelSetting, "xhigh"); err != nil || strings.Join(r.calls, ",") != "model=big,effort=xhigh" {
		t.Fatalf("after the model: %v %v", err, r.calls)
	}
	// A mode change is remembered too: setting it again does nothing.
	ms := SessionState{Modes: &acp.SessionModeState{CurrentModeId: "a", AvailableModes: []acp.SessionMode{{Id: "a"}, {Id: "b"}}}}
	rc := &recordingConfigurer{}
	ms, _ = Configure(ctx, rc, "s", ms, ModeSetting, "b")
	_, _ = Configure(ctx, rc, "s", ms, ModeSetting, "b")
	if strings.Join(rc.calls, ",") != "mode=b" || ms.Modes.CurrentModeId != "b" {
		t.Fatalf("mode: %v %v", rc.calls, ms.Modes.CurrentModeId)
	}
}

type answeringConfigurer struct {
	recordingConfigurer
	after []acp.SessionConfigOption
}

func (a *answeringConfigurer) SetSessionConfigOption(ctx context.Context, p acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	_, _ = a.recordingConfigurer.SetSessionConfigOption(ctx, p)
	return acp.SetSessionConfigOptionResponse{ConfigOptions: a.after}, nil
}

// A session request's _meta: the caller's metadata and the system prompt marker, never shared
// with the caller's map.
func TestSessionMeta(t *testing.T) {
	if SessionMeta(nil, "") != nil {
		t.Fatal("nothing: nil")
	}
	caller := map[string]any{"claudeCode": map[string]any{"options": map[string]any{"maxTurns": 50}}}
	m := SessionMeta(caller, "Be brief.")
	if m[SystemPromptMetaKey] != "Be brief." || m["claudeCode"] == nil {
		t.Fatalf("meta: %v", m)
	}
	if _, leaked := caller[SystemPromptMetaKey]; leaked {
		t.Fatal("the caller's map was changed")
	}
}
