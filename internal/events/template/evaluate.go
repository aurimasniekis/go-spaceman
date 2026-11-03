package template

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Event is one delivered event (the go-pulse envelope fields SpaceMan uses).
type Event struct {
	ID          string
	Subject     string
	Type        string
	Source      string
	ContentType string
	Time        time.Time
	Data        []byte
}

// Result is the outcome of applying a bound definition to an event.
type Result struct {
	// Accepted is false when a filter rejected the event (not an error: the event is acknowledged and recorded).
	Accepted     bool
	SkipReason   string
	Title        string
	Body         string
	Instructions string
	Correlation  string
	Urgency      string
}

// ErrInvalid marks an event that does not satisfy the definition's input requirements; it goes to quarantine.
var ErrInvalid = errors.New("invalid event")

func decode(data []byte) (any, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	if !json.Valid(data) {
		return string(data), nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func eventMap(e Event) map[string]any {
	return map[string]any{"id": e.ID, "subject": e.Subject, "type": e.Type, "source": e.Source, "content_type": e.ContentType,
		"time": e.Time.UTC().Format(time.RFC3339Nano)}
}

func (f Filter) match(root map[string]any) bool {
	v, ok := Lookup(root, f.Path)
	if f.Op == "exists" {
		return ok && v != nil
	}
	s := stringify(v)
	switch f.Op {
	case "eq":
		return ok && s == f.Value
	case "ne":
		return !ok || s != f.Value
	case "in":
		for _, x := range f.Values {
			if ok && s == x {
				return true
			}
		}
		return false
	case "prefix":
		return ok && strings.HasPrefix(s, f.Value)
	}
	return false
}

// Apply validates, filters and renders an event. It returns ErrInvalid (wrapped) for events that fail the input schema;
// a filtered event is a Result with Accepted=false.
func (b *Bound) Apply(e Event) (*Result, error) {
	data, err := decode(e.Data)
	if err != nil {
		return nil, fmt.Errorf("%w: payload is not decodable JSON: %w", ErrInvalid, err)
	}
	if b.schema != nil {
		inst, err := decode(e.Data)
		if err == nil {
			err = b.schema.Validate(normalizeNumbers(inst))
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
	}
	ctx := Context{Event: eventMap(e), Data: data, Vars: b.Vars}
	root := ctx.root()
	for _, f := range b.Filters {
		if !f.match(root) {
			return &Result{Accepted: false, SkipReason: fmt.Sprintf("filter %s %s did not match", f.Path, f.Op)}, nil
		}
	}
	r := &Result{Accepted: true, Urgency: b.Def.Urgency}
	if r.Urgency == "" {
		r.Urgency = "normal"
	}
	for _, x := range []struct {
		dst  *string
		tmpl string
		max  int
		vars bool
	}{{&r.Title, b.Def.Title, MaxTitle, false}, {&r.Body, b.Def.Body, MaxBody, false}, {&r.Instructions, b.Def.Instructions, MaxInstructions, true},
		{&r.Correlation, b.Def.Correlation, 256, false}} {
		c := ctx
		if x.vars {
			// Trusted instructions see only the template's variables, also for versions stored before validation
			// refused event references in them.
			c = Context{Vars: ctx.Vars}
		}
		if *x.dst, err = Render(x.tmpl, c, x.max); err != nil {
			return nil, err
		}
	}
	r.Title = strings.Join(strings.Fields(r.Title), " ")
	if r.Title == "" {
		r.Title = e.Type
	}
	return r, nil
}

// normalizeNumbers converts json.Number to float64 for the schema validator.
func normalizeNumbers(v any) any {
	switch x := v.(type) {
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return f
		}
		return x.String()
	case map[string]any:
		for k, val := range x {
			x[k] = normalizeNumbers(val)
		}
	case []any:
		for i, val := range x {
			x[i] = normalizeNumbers(val)
		}
	}
	return v
}
