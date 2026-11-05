package acpx

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

func permission() acp.RequestPermissionRequest {
	title := "Edit main.go"
	return acp.RequestPermissionRequest{SessionId: "s", ToolCall: acp.ToolCallUpdate{ToolCallId: "call_1", Title: &title},
		Options: []acp.PermissionOption{
			{OptionId: "yes", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce},
			{OptionId: "always", Name: "Always allow", Kind: acp.PermissionOptionKindAllowAlways},
			{OptionId: "no", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce}}}
}

// A permission request is described with its options; a reply picks one of them, or declines.
func TestPermissionRoundTrip(t *testing.T) {
	r := PermissionRequest(permission())
	if r.Kind != RequestPermission || r.ToolCallID != "call_1" || r.Title != "Edit main.go" || len(r.Options) != 3 || r.Options[1] != (Option{ID: "always", Name: "Always allow", Kind: "allow_always"}) {
		t.Fatalf("request: %+v", r)
	}
	resp, kind, err := PermissionResponse(r, Reply{OptionID: "no"})
	if err != nil || kind != "reject_once" || resp.Outcome.Selected == nil || resp.Outcome.Selected.OptionId != "no" {
		t.Fatalf("picked: %+v %q %v", resp, kind, err)
	}
	for _, reply := range []Reply{{Action: ActionDecline}, {Action: ActionCancel}} {
		if resp, kind, err := PermissionResponse(r, reply); err != nil || kind != "cancelled" || resp.Outcome.Cancelled == nil {
			t.Fatalf("%+v: %+v %q %v", reply, resp, kind, err)
		}
	}
	for _, bad := range []Reply{{OptionID: "maybe"}, {Action: ActionAccept}, {}} {
		if resp, _, err := PermissionResponse(r, bad); err == nil || resp.Outcome.Cancelled == nil {
			t.Fatalf("%+v accepted: %+v", bad, resp)
		}
	}
}

// A form question carries its schema; an accepted answer gives the required fields and nothing
// the schema does not have. A link question is accepted, declined or cancelled.
func TestElicitationRoundTrip(t *testing.T) {
	desc := "Which database?"
	form := acp.UnstableCreateElicitationRequest{Form: &acp.UnstableCreateElicitationForm{Mode: "form", Message: "Pick one",
		RequestedSchema: acp.UnstableElicitationSchema{Type: "object", Description: &desc, Required: []string{"db"},
			Properties: map[string]any{"db": map[string]any{"type": "string", "oneOf": []any{map[string]any{"const": "pg"}, map[string]any{"const": "mysql"}}}, "note": map[string]any{"type": "string"}}}}}
	r, err := ElicitationRequest(form)
	if err != nil || r.Kind != RequestForm || r.Message != "Pick one" || !strings.Contains(string(r.Schema), `"required":["db"]`) {
		t.Fatalf("form: %+v %v", r, err)
	}
	resp, err := ElicitationResponse(r, Reply{Action: ActionAccept, Content: map[string]any{"db": "pg"}})
	if err != nil || resp.Accept == nil || resp.Accept.Content["db"] != "pg" {
		t.Fatalf("accept: %+v %v", resp, err)
	}
	b, _ := json.Marshal(resp)
	if !strings.Contains(string(b), `"action":"accept"`) {
		t.Fatalf("wire: %s", b)
	}
	for _, bad := range []map[string]any{{}, {"db": "pg", "extra": 1}, {"db": nil}} {
		if resp, err := ElicitationResponse(r, Reply{Action: ActionAccept, Content: bad}); err == nil || resp.Cancel == nil {
			t.Fatalf("%v accepted: %+v", bad, resp)
		}
	}
	if resp, err := ElicitationResponse(r, Reply{Action: ActionDecline}); err != nil || resp.Decline == nil {
		t.Fatalf("decline: %+v %v", resp, err)
	}
	if _, err := ElicitationResponse(r, Reply{OptionID: "x"}); err == nil {
		t.Fatal("an option for a form")
	}
	link := acp.UnstableCreateElicitationRequest{Url: &acp.UnstableCreateElicitationUrl{Mode: "url", Message: "Sign in to GitHub", Url: "https://github.com/login/oauth", ElicitationId: "e1"}}
	r, err = ElicitationRequest(link)
	if err != nil || r.Kind != RequestURL || r.URL != "https://github.com/login/oauth" {
		t.Fatalf("url: %+v %v", r, err)
	}
	if resp, err := ElicitationResponse(r, Reply{Action: ActionAccept}); err != nil || resp.Accept == nil {
		t.Fatalf("url accept: %+v %v", resp, err)
	}
}

// A request waits for its reply; one nobody answers in time, or after the context ended, is
// closed, and a late reply then finds nothing.
func TestPendingWaitsResolvesAndExpires(t *testing.T) {
	p := NewPending()
	r := p.Open(Request{Kind: RequestPermission})
	if !strings.HasPrefix(r.ID, "req_") || len(p.Waiting()) != 1 {
		t.Fatalf("open: %+v %v", r, p.Waiting())
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(20 * time.Millisecond)
		if err := p.Resolve(Reply{RequestID: r.ID, OptionID: "yes"}); err != nil {
			t.Error(err)
		}
	}()
	reply, ok := p.Wait(context.Background(), r.ID, time.Second)
	wg.Wait()
	if !ok || reply.OptionID != "yes" || len(p.Waiting()) != 0 {
		t.Fatalf("reply: %+v %v", reply, ok)
	}
	if err := p.Resolve(Reply{RequestID: r.ID}); !errors.Is(err, ErrNoRequest) {
		t.Fatalf("answered twice: %v", err)
	}

	late := p.Open(Request{Kind: RequestForm})
	if _, ok := p.Wait(context.Background(), late.ID, 10*time.Millisecond); ok {
		t.Fatal("timed out but ok")
	}
	if err := p.Resolve(Reply{RequestID: late.ID}); !errors.Is(err, ErrNoRequest) {
		t.Fatalf("late reply: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	gone := p.Open(Request{Kind: RequestURL})
	cancel()
	if _, ok := p.Wait(ctx, gone.ID, 0); ok || len(p.Waiting()) != 0 {
		t.Fatalf("cancelled: %v %v", ok, p.Waiting())
	}
}

// Attached files follow the text as resource links; a path that is not absolute is left out.
func TestPromptBlocks(t *testing.T) {
	blocks := PromptBlocks("look at these", []string{"/workspace/uploads/a b.png", "relative.txt", "/workspace/repo/../repo/main.go"})
	if len(blocks) != 3 || blocks[0].Text == nil || blocks[0].Text.Text != "look at these" {
		t.Fatalf("blocks: %+v", blocks)
	}
	if l := blocks[1].ResourceLink; l == nil || l.Name != "a b.png" || l.Uri != "file:///workspace/uploads/a%20b.png" {
		t.Fatalf("link: %+v", blocks[1].ResourceLink)
	}
	if l := blocks[2].ResourceLink; l == nil || l.Uri != "file:///workspace/repo/main.go" {
		t.Fatalf("clean: %+v", blocks[2].ResourceLink)
	}
}

// A reply that comes before the client starts waiting is not lost; a second one is refused.
func TestPendingReplyBeforeWait(t *testing.T) {
	p := NewPending()
	r := p.Open(Request{Kind: RequestPermission})
	if err := p.Resolve(Reply{RequestID: r.ID, OptionID: "yes"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Resolve(Reply{RequestID: r.ID, OptionID: "no"}); !errors.Is(err, ErrNoRequest) {
		t.Fatalf("second reply: %v", err)
	}
	if reply, ok := p.Wait(context.Background(), r.ID, time.Second); !ok || reply.OptionID != "yes" {
		t.Fatalf("early reply lost: %+v %v", reply, ok)
	}
}

// Close ends every wait without a reply, and every one opened after it at once.
func TestPendingClose(t *testing.T) {
	p := NewPending()
	r := p.Open(Request{Kind: RequestForm})
	done := make(chan bool)
	go func() { _, ok := p.Wait(context.Background(), r.ID, time.Hour); done <- ok }()
	time.Sleep(10 * time.Millisecond)
	p.Close()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("closed but answered")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not end the wait")
	}
	late := p.Open(Request{Kind: RequestForm})
	start := time.Now()
	if _, ok := p.Wait(context.Background(), late.ID, time.Hour); ok || time.Since(start) > 100*time.Millisecond || !p.Closed() {
		t.Fatal("a request opened after Close waits")
	}
	if err := p.Resolve(Reply{RequestID: late.ID}); !errors.Is(err, ErrNoRequest) {
		t.Fatal("a reply after Close")
	}
}

// The agent's elicitation/complete resolves the link question it belongs to as accepted.
func TestPendingResolveElicitation(t *testing.T) {
	p := NewPending()
	r, _ := ElicitationRequest(acp.UnstableCreateElicitationRequest{Url: &acp.UnstableCreateElicitationUrl{Mode: "url", Url: "https://x", ElicitationId: "e1"}})
	r = p.Open(r)
	if r.ElicitationID != "e1" {
		t.Fatalf("elicitation id: %+v", r)
	}
	if _, ok := p.ResolveElicitation("other"); ok {
		t.Fatal("wrong id resolved")
	}
	if id, ok := p.ResolveElicitation("e1"); !ok || id != r.ID {
		t.Fatalf("resolve: %q %v", id, ok)
	}
	if reply, ok := p.Wait(context.Background(), r.ID, time.Second); !ok || reply.Action != ActionAccept {
		t.Fatalf("reply: %+v %v", reply, ok)
	}
}

// A form's answers are of their field's type and among its choices.
func TestCheckContentTypes(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","required":["db"],"properties":{
		"db":{"type":"string","oneOf":[{"const":"pg"},{"const":"mysql"}]},
		"note":{"type":"string"},
		"n":{"type":"integer"},"ratio":{"type":"number"},"ok":{"type":"boolean"},
		"tags":{"type":"array","items":{"anyOf":[{"const":"a"},{"const":"b"}]}}}}`)
	good := map[string]any{"db": "pg", "note": "x", "n": float64(3), "ratio": 0.5, "ok": true, "tags": []any{"a", "b"}}
	if err := CheckContent(schema, good); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]map[string]any{
		"object for a string": {"db": map[string]any{}},
		"not a choice":        {"db": "oracle"},
		"fraction":            {"db": "pg", "n": 1.5},
		"string for number":   {"db": "pg", "ratio": "1"},
		"string for boolean":  {"db": "pg", "ok": "yes"},
		"tag not a choice":    {"db": "pg", "tags": []any{"c"}},
		"tags not strings":    {"db": "pg", "tags": []any{1}},
		"missing required":    {"note": "x"},
		"unknown field":       {"db": "pg", "x": 1},
	} {
		if err := CheckContent(schema, bad); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// A permission request carries what the tool call will do: its kind, input and text.
func TestPermissionRequestContext(t *testing.T) {
	kind := acp.ToolKindExecute
	p := permission()
	p.ToolCall.Kind = &kind
	p.ToolCall.RawInput = map[string]any{"command": "rm -rf build"}
	p.ToolCall.Content = []acp.ToolCallContent{acp.ToolContent(acp.TextBlock("Plan: clean the build"))}
	r := PermissionRequest(p)
	if r.ToolKind != "execute" || !strings.Contains(string(r.RawInput), "rm -rf build") || r.Text != "Plan: clean the build" {
		t.Fatalf("context: %+v", r)
	}
}
