package acpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// A headless client relays what an agent asks a person (a permission, a question) to wherever
// that person is, and their answer back. The agent's request stays open meanwhile: the client
// answers it when the reply comes (Pending), or on its own when nobody does in time.

// Request kinds.
const (
	RequestPermission = "permission" // session/request_permission: pick one of the agent's options
	RequestForm       = "form"       // elicitation/create, form mode: fill a JSON-schema form
	RequestURL        = "url"        // elicitation/create, url mode: open a link (an OAuth sign-in) and come back
)

// Request is what the agent asks, in a form a person's client can show without knowing ACP.
type Request struct {
	ID         string   `json:"request_id"`
	Kind       string   `json:"kind"`
	ToolCallID string   `json:"tool_call_id,omitempty"` // permission: the tool call it is about
	Title      string   `json:"title,omitempty"`
	Message    string   `json:"message,omitempty"`
	Options    []Option `json:"options,omitempty"` // permission: the agent's choices
	// Schema is a form's requested schema (JSON Schema, object of primitive properties), as the
	// agent sent it.
	Schema json.RawMessage `json:"schema,omitempty"`
	URL    string          `json:"url,omitempty"`
}

// Option is one of a permission request's choices; Kind is allow_once, allow_always,
// reject_once or reject_always.
type Option struct {
	ID   string `json:"option_id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// Reply is a person's answer: OptionID for a permission; Action (accept, decline, cancel) and,
// for an accepted form, Content.
type Reply struct {
	RequestID string         `json:"request_id"`
	OptionID  string         `json:"option_id,omitempty"`
	Action    string         `json:"action,omitempty"`
	Content   map[string]any `json:"content,omitempty"`
}

// Reply actions.
const (
	ActionAccept  = "accept"
	ActionDecline = "decline"
	ActionCancel  = "cancel"
)

// PermissionRequest describes a session/request_permission for a person.
func PermissionRequest(p acp.RequestPermissionRequest) Request {
	r := Request{Kind: RequestPermission, ToolCallID: string(p.ToolCall.ToolCallId)}
	if p.ToolCall.Title != nil {
		r.Title = *p.ToolCall.Title
	}
	for _, o := range p.Options {
		r.Options = append(r.Options, Option{ID: string(o.OptionId), Name: o.Name, Kind: string(o.Kind)})
	}
	return r
}

// PermissionResponse is the answer to the agent for a person's reply: the option they picked
// (one the request offered), or a cancelled outcome for a declined or cancelled reply. kind is
// the picked option's kind, "cancelled" otherwise.
func PermissionResponse(r Request, reply Reply) (resp acp.RequestPermissionResponse, kind string, err error) {
	cancelled := acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Cancelled: &acp.RequestPermissionOutcomeCancelled{}}}
	if reply.OptionID == "" {
		switch reply.Action {
		case ActionDecline, ActionCancel:
			return cancelled, "cancelled", nil
		}
		return cancelled, "cancelled", errors.New("a permission is answered with option_id (or action decline / cancel)")
	}
	for _, o := range r.Options {
		if o.ID == reply.OptionID {
			return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Selected: &acp.RequestPermissionOutcomeSelected{
				OptionId: acp.PermissionOptionId(o.ID)}}}, o.Kind, nil
		}
	}
	return cancelled, "cancelled", fmt.Errorf("option %q is not one the request offers", reply.OptionID)
}

// ElicitationRequest describes an elicitation/create for a person.
func ElicitationRequest(p acp.UnstableCreateElicitationRequest) (Request, error) {
	switch {
	case p.Form != nil:
		schema, err := json.Marshal(p.Form.RequestedSchema)
		if err != nil {
			return Request{}, err
		}
		return Request{Kind: RequestForm, Message: p.Form.Message, Schema: schema}, nil
	case p.Url != nil:
		return Request{Kind: RequestURL, Message: p.Url.Message, URL: p.Url.Url}, nil
	}
	return Request{}, errors.New("elicitation: neither form nor url")
}

// ElicitationResponse is the answer to the agent for a person's reply. An accepted form must
// give every required field and only fields the schema has.
func ElicitationResponse(r Request, reply Reply) (acp.UnstableCreateElicitationResponse, error) {
	switch reply.Action {
	case ActionDecline:
		return acp.UnstableCreateElicitationResponse{Decline: &acp.UnstableCreateElicitationDecline{Action: ActionDecline}}, nil
	case ActionCancel:
		return CancelledElicitation(), nil
	case ActionAccept:
	default:
		return CancelledElicitation(), fmt.Errorf("an elicitation is answered with action accept, decline or cancel, not %q", reply.Action)
	}
	if r.Kind == RequestForm {
		if err := checkContent(r.Schema, reply.Content); err != nil {
			return CancelledElicitation(), err
		}
	}
	return acp.UnstableCreateElicitationResponse{Accept: &acp.UnstableCreateElicitationAccept{Action: ActionAccept, Content: reply.Content}}, nil
}

// CancelledElicitation is the answer for a question nobody answered.
func CancelledElicitation() acp.UnstableCreateElicitationResponse {
	return acp.UnstableCreateElicitationResponse{Cancel: &acp.UnstableCreateElicitationCancel{Action: ActionCancel}}
}

// checkContent holds form content to its schema: the required fields, no field the schema does
// not name.
func checkContent(schema json.RawMessage, content map[string]any) error {
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(schema, &s); err != nil {
		return fmt.Errorf("form schema: %w", err)
	}
	for k := range content {
		if _, ok := s.Properties[k]; !ok {
			return fmt.Errorf("the form has no field %q", k)
		}
	}
	for _, k := range s.Required {
		if v, ok := content[k]; !ok || v == nil {
			return fmt.Errorf("field %q is required", k)
		}
	}
	return nil
}

// Pending holds the requests waiting for a person, by id.
type Pending struct {
	mu   sync.Mutex
	reqs map[string]chan Reply
}

// NewPending is an empty set of requests.
func NewPending() *Pending { return &Pending{reqs: map[string]chan Reply{}} }

// Open gives r a fresh id and holds it until Wait returns.
func (p *Pending) Open(r Request) Request {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	r.ID = "req_" + hex.EncodeToString(b)
	p.mu.Lock()
	p.reqs[r.ID] = make(chan Reply, 1)
	p.mu.Unlock()
	return r
}

// ErrNoRequest: the reply names no request waiting (answered already, timed out, never was).
var ErrNoRequest = errors.New("no such request waiting")

// Resolve hands a reply to the request it names.
func (p *Pending) Resolve(reply Reply) error {
	p.mu.Lock()
	ch, ok := p.reqs[reply.RequestID]
	if ok {
		delete(p.reqs, reply.RequestID)
	}
	p.mu.Unlock()
	if !ok {
		return ErrNoRequest
	}
	ch <- reply
	return nil
}

// Wait waits for the reply to an open request; ok is false when the context ended or timeout
// passed first (timeout 0: no limit). The request is closed either way.
func (p *Pending) Wait(ctx context.Context, id string, timeout time.Duration) (reply Reply, ok bool) {
	p.mu.Lock()
	ch := p.reqs[id]
	p.mu.Unlock()
	if ch == nil {
		return Reply{}, false
	}
	var expire <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		expire = t.C
	}
	select {
	case reply = <-ch:
		return reply, true
	case <-ctx.Done():
	case <-expire:
	}
	p.mu.Lock()
	if _, still := p.reqs[id]; still {
		delete(p.reqs, id)
		p.mu.Unlock()
		return Reply{}, false
	}
	p.mu.Unlock()
	// Resolved in the meantime: the reply is in the channel.
	return <-ch, true
}

// Waiting lists the ids of the requests waiting.
func (p *Pending) Waiting() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := make([]string, 0, len(p.reqs))
	for id := range p.reqs {
		ids = append(ids, id)
	}
	return ids
}
