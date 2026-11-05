package acpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
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
	// A permission's context: the tool's kind (edit, execute, fetch…), its input as the agent
	// gave it (a command, a plan), and the text the tool call carries.
	ToolKind string          `json:"tool_kind,omitempty"`
	RawInput json.RawMessage `json:"raw_input,omitempty"`
	Text     string          `json:"text,omitempty"`
	// Schema is a form's requested schema (JSON Schema, object of primitive properties), as the
	// agent sent it.
	Schema json.RawMessage `json:"schema,omitempty"`
	URL    string          `json:"url,omitempty"`
	// ElicitationID correlates a link question with the agent's elicitation/complete.
	ElicitationID string `json:"elicitation_id,omitempty"`
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

// PermissionRequest describes a session/request_permission for a person: the options and what
// the tool call says it will do (its kind, its input, its text).
func PermissionRequest(p acp.RequestPermissionRequest) Request {
	r := Request{Kind: RequestPermission, ToolCallID: string(p.ToolCall.ToolCallId)}
	if p.ToolCall.Title != nil {
		r.Title = *p.ToolCall.Title
	}
	if p.ToolCall.Kind != nil {
		r.ToolKind = string(*p.ToolCall.Kind)
	}
	if p.ToolCall.RawInput != nil {
		if b, err := json.Marshal(p.ToolCall.RawInput); err == nil && string(b) != "null" {
			r.RawInput = b
		}
	}
	var texts []string
	for _, c := range p.ToolCall.Content {
		if c.Content != nil && c.Content.Content.Text != nil && c.Content.Content.Text.Text != "" {
			texts = append(texts, c.Content.Content.Text.Text)
		}
	}
	r.Text = strings.Join(texts, "\n\n")
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
		return Request{Kind: RequestURL, Message: p.Url.Message, URL: p.Url.Url, ElicitationID: string(p.Url.ElicitationId)}, nil
	}
	return Request{}, errors.New("elicitation: neither form nor url")
}

// ElicitationResponse is the answer to the agent for a person's reply. An accepted form must
// give every required field, only fields the schema has, and values of the field's type (and
// among its choices, when it has some). A link question's accept is the person's consent to open
// the link; the agent says when the interaction completes (elicitation/complete).
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
		if err := CheckContent(r.Schema, reply.Content); err != nil {
			return CancelledElicitation(), err
		}
	}
	return acp.UnstableCreateElicitationResponse{Accept: &acp.UnstableCreateElicitationAccept{Action: ActionAccept, Content: reply.Content}}, nil
}

// CancelledElicitation is the answer for a question nobody answered.
func CancelledElicitation() acp.UnstableCreateElicitationResponse {
	return acp.UnstableCreateElicitationResponse{Cancel: &acp.UnstableCreateElicitationCancel{Action: ActionCancel}}
}

// property is what CheckContent reads of a form field's schema. ACP forms have primitive fields
// (string, number, integer, boolean) and multi-selects (an array of strings).
type property struct {
	Type  string   `json:"type"`
	Enum  []any    `json:"enum"`
	OneOf []choice `json:"oneOf"`
	AnyOf []choice `json:"anyOf"`
	Items *struct {
		Type  string   `json:"type"`
		Enum  []any    `json:"enum"`
		OneOf []choice `json:"oneOf"`
		AnyOf []choice `json:"anyOf"`
	} `json:"items"`
}

type choice struct {
	Const any `json:"const"`
}

// CheckContent holds a form's answers to its schema: the required fields, no field the schema
// does not name, each value of its field's type and among its choices when it has some.
func CheckContent(schema json.RawMessage, content map[string]any) error {
	var s struct {
		Properties map[string]property `json:"properties"`
		Required   []string            `json:"required"`
	}
	if err := json.Unmarshal(schema, &s); err != nil {
		return fmt.Errorf("form schema: %w", err)
	}
	for k, v := range content {
		p, ok := s.Properties[k]
		if !ok {
			return fmt.Errorf("the form has no field %q", k)
		}
		if err := checkValue(p, v); err != nil {
			return fmt.Errorf("field %q: %w", k, err)
		}
	}
	for _, k := range s.Required {
		if v, ok := content[k]; !ok || v == nil {
			return fmt.Errorf("field %q is required", k)
		}
	}
	return nil
}

func checkValue(p property, v any) error {
	if v == nil {
		return nil // absent; required fields are checked apart
	}
	switch p.Type {
	case "string":
		s, ok := v.(string)
		if !ok {
			return errors.New("a string is expected")
		}
		return among(s, p.Enum, append(p.OneOf, p.AnyOf...))
	case "number":
		if _, ok := v.(float64); !ok {
			return errors.New("a number is expected")
		}
	case "integer":
		f, ok := v.(float64)
		if !ok || f != math.Trunc(f) {
			return errors.New("an integer is expected")
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return errors.New("true or false is expected")
		}
	case "array":
		items, ok := v.([]any)
		if !ok {
			return errors.New("a list is expected")
		}
		for _, it := range items {
			s, ok := it.(string)
			if !ok {
				return errors.New("a list of strings is expected")
			}
			if p.Items != nil {
				if err := among(s, p.Items.Enum, append(p.Items.OneOf, p.Items.AnyOf...)); err != nil {
					return err
				}
			}
		}
	default:
		return fmt.Errorf("the field's type %q is not one a form has", p.Type)
	}
	return nil
}

// among: s is one of the choices, when there are any.
func among(s string, enum []any, choices []choice) error {
	if len(enum) == 0 && len(choices) == 0 {
		return nil
	}
	for _, e := range enum {
		if fmt.Sprint(e) == s {
			return nil
		}
	}
	for _, c := range choices {
		if fmt.Sprint(c.Const) == s {
			return nil
		}
	}
	return fmt.Errorf("%q is not one of the choices", s)
}

// Pending holds the requests waiting for a person, by id.
type Pending struct {
	mu     sync.Mutex
	reqs   map[string]*waiting
	closed bool
	done   chan struct{} // closed by Close: every wait ends
}

type waiting struct {
	req      Request
	ch       chan Reply // the reply, once (buffered)
	resolved bool
}

// NewPending is an empty set of requests.
func NewPending() *Pending { return &Pending{reqs: map[string]*waiting{}, done: make(chan struct{})} }

// Open gives r a fresh id and holds it until Wait returns. After Close, the request is opened
// but Wait returns at once: nobody will answer it.
func (p *Pending) Open(r Request) Request {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	r.ID = "req_" + hex.EncodeToString(b)
	p.mu.Lock()
	p.reqs[r.ID] = &waiting{req: r, ch: make(chan Reply, 1)}
	p.mu.Unlock()
	return r
}

// ErrNoRequest: the reply names no request waiting (answered already, timed out, never was).
var ErrNoRequest = errors.New("no such request waiting")

// Resolve hands a reply to the request it names. A request takes one reply: the first; it is
// held for Wait even when Wait has not started yet.
func (p *Pending) Resolve(reply Reply) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	w, ok := p.reqs[reply.RequestID]
	if !ok || w.resolved || p.closed {
		return ErrNoRequest
	}
	w.resolved = true
	w.ch <- reply
	return nil
}

// ResolveElicitation resolves the link question with this elicitation id as accepted (the agent
// reported it complete); id is the request's, ok false when none waits.
func (p *Pending) ResolveElicitation(elicitationID string) (id string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for rid, w := range p.reqs {
		if w.req.Kind == RequestURL && w.req.ElicitationID == elicitationID && elicitationID != "" && !w.resolved && !p.closed {
			w.resolved = true
			w.ch <- Reply{RequestID: rid, Action: ActionAccept}
			return rid, true
		}
	}
	return "", false
}

// Wait waits for the reply to an open request; ok is false when the context ended, timeout
// passed (0: no limit) or Pending was closed first. The request is closed either way. A reply
// that came before Wait started is returned.
func (p *Pending) Wait(ctx context.Context, id string, timeout time.Duration) (reply Reply, ok bool) {
	p.mu.Lock()
	w := p.reqs[id]
	p.mu.Unlock()
	if w == nil {
		return Reply{}, false
	}
	defer func() {
		p.mu.Lock()
		delete(p.reqs, id)
		p.mu.Unlock()
	}()
	var expire <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		expire = t.C
	}
	select {
	case reply = <-w.ch:
		return reply, true
	case <-ctx.Done():
	case <-expire:
	case <-p.done:
	}
	p.mu.Lock()
	resolved := w.resolved
	w.resolved = true // a late reply finds nothing
	p.mu.Unlock()
	if resolved && len(w.ch) > 0 {
		return <-w.ch, true // answered at the last moment: the answer stands
	}
	return Reply{}, false
}

// Close ends every wait without a reply and every one to come: nobody will answer any more
// (the conversation closed).
func (p *Pending) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.closed = true
		close(p.done)
	}
}

// Closed reports whether Close was called.
func (p *Pending) Closed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
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
