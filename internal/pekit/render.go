package pekit

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

type Event struct {
	Type       string `json:"type"`
	Time       string `json:"time,omitempty"`
	Message    string `json:"message,omitempty"`
	Member     string `json:"member,omitempty"`
	Command    string `json:"command,omitempty"`
	Target     string `json:"target,omitempty"`
	Package    string `json:"package,omitempty"`
	Version    string `json:"version,omitempty"`
	Path       string `json:"path,omitempty"`
	Stream     string `json:"stream,omitempty"`
	Text       string `json:"text,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

type Renderer interface {
	Event(Event)
	Output(member, version, target, stream, text string)
	Error(error)
}

func newRenderer(inv Invocation, out, errw io.Writer, now func() time.Time) Renderer {
	if inv.JSON {
		return &jsonRenderer{out: out, errw: errw, now: now, dryRun: inv.DryRun}
	}
	return &humanRenderer{out: out, errw: errw, now: now, quiet: inv.Quiet}
}

type humanRenderer struct {
	mu    sync.Mutex
	out   io.Writer
	errw  io.Writer
	now   func() time.Time
	quiet bool
}

func (r *humanRenderer) Event(e Event) {
	if r.quiet && !quietEvent(e) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ts := r.now().Format("15:04:05")
	msg := humanEventMessage(e)
	ctx := eventContext(e)
	if ctx != "" {
		_, _ = fmt.Fprintf(r.out, "[%s] [%s] %s\n", ts, ctx, msg)
		return
	}
	_, _ = fmt.Fprintf(r.out, "[%s] %s\n", ts, msg)
}

func quietEvent(e Event) bool {
	switch e.Type {
	case "warning", "artifact", "publish", "workspace_summary":
		return true
	default:
		return false
	}
}

func humanEventMessage(e Event) string {
	msg := e.Message
	if msg == "" {
		msg = e.Type
	}
	switch e.Type {
	case "artifact", "publish", "package_plan", "publish_plan":
		if e.Path != "" {
			msg += ": " + e.Path
		}
	}
	return msg
}

func (r *humanRenderer) Output(member, version, target, stream, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prefixParts := make([]string, 0, 3)
	if member != "" {
		prefixParts = append(prefixParts, member)
	}
	if target != "" {
		prefixParts = append(prefixParts, target)
	}
	if version != "" {
		prefixParts = append(prefixParts, version)
	}
	prefix := ""
	if len(prefixParts) > 0 {
		prefix = "[" + strings.Join(prefixParts, " ") + "] "
	}
	writer := r.out
	if stream == "stderr" {
		writer = r.errw
	}
	lines := strings.SplitAfter(text, "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		_, _ = fmt.Fprint(writer, prefix+line)
		if !strings.HasSuffix(line, "\n") {
			_, _ = fmt.Fprintln(writer)
		}
	}
}

func (r *humanRenderer) Error(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, _ = fmt.Fprintln(r.errw, err.Error())
}

type jsonRenderer struct {
	mu     sync.Mutex
	out    io.Writer
	errw   io.Writer
	now    func() time.Time
	dryRun bool
	events []Event
}

func (r *jsonRenderer) Event(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e.Time == "" {
		e.Time = r.now().UTC().Format(time.RFC3339Nano)
	}
	if r.dryRun {
		r.events = append(r.events, e)
		return
	}
	_ = json.NewEncoder(r.out).Encode(e)
}

func (r *jsonRenderer) Output(member, version, target, stream, text string) {
	r.Event(Event{
		Type:    "target_output",
		Member:  member,
		Version: version,
		Target:  target,
		Stream:  stream,
		Text:    text,
	})
}

func (r *jsonRenderer) Error(err error) {
	r.Event(Event{Type: "error", Message: err.Error()})
}

func (r *jsonRenderer) FlushPlan(inv Invocation) {
	if !r.dryRun {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	doc := struct {
		Type      string  `json:"type"`
		Time      string  `json:"time"`
		Command   string  `json:"command"`
		Workspace bool    `json:"workspace"`
		Events    []Event `json:"events"`
	}{
		Type:      "plan",
		Time:      r.now().UTC().Format(time.RFC3339Nano),
		Command:   string(inv.EffectiveCommand()),
		Workspace: inv.WorkspaceMode,
		Events:    append([]Event(nil), r.events...),
	}
	_ = json.NewEncoder(r.out).Encode(doc)
}

func eventContext(e Event) string {
	parts := make([]string, 0, 5)
	if e.Member != "" {
		parts = append(parts, e.Member)
	}
	if e.Command != "" {
		parts = append(parts, e.Command)
	}
	if e.Target != "" {
		parts = append(parts, e.Target)
	}
	if e.Package != "" {
		parts = append(parts, e.Package)
	}
	if e.Version != "" {
		parts = append(parts, e.Version)
	}
	return strings.Join(parts, " ")
}
