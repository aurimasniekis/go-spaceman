// Package schemautil compiles and applies the JSON Schemas SpaceMan accepts from administrators and templates: JSON
// Schema 2020-12 only, self-contained (every external reference is refused, so a schema can never read files or reach
// the network), and validation errors reported as short readable text.
package schemautil

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// MaxSchema bounds a schema document.
const MaxSchema = 64 << 10

// noRemote refuses every external reference.
type noRemote struct{}

func (noRemote) Load(url string) (any, error) {
	return nil, fmt.Errorf("external schema references are not allowed (%s)", url)
}

// Compile compiles a self-contained JSON Schema 2020-12 document.
func Compile(raw json.RawMessage) (*jsonschema.Schema, error) {
	if len(raw) > MaxSchema {
		return nil, fmt.Errorf("the schema is larger than %d bytes", MaxSchema)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(noRemote{})
	if err := c.AddResource("urn:spaceman:schema", doc); err != nil {
		return nil, err
	}
	return c.Compile("urn:spaceman:schema")
}

// CompileObject compiles a schema whose root must describe a JSON object (forms and tool inputs).
func CompileObject(raw json.RawMessage) (*jsonschema.Schema, error) {
	var root struct {
		Type any `json:"type"`
	}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	if root.Type != "object" {
		return nil, errors.New(`the root of the schema must be {"type": "object"}`)
	}
	return Compile(raw)
}

// Validate checks a JSON document against a compiled schema, returning a short message on failure.
func Validate(s *jsonschema.Schema, doc json.RawMessage) error {
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		return fmt.Errorf("not valid JSON: %w", err)
	}
	if err := s.Validate(v); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			return errors.New(summarize(ve))
		}
		return err
	}
	return nil
}

// summarize flattens a validation error into "at /path: message" lines (first few causes).
func summarize(ve *jsonschema.ValidationError) string {
	var parts []string
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(parts) >= 5 {
			return
		}
		if len(e.Causes) == 0 {
			loc := "/" + strings.Join(e.InstanceLocation, "/")
			parts = append(parts, fmt.Sprintf("at %s: %s", loc, e.ErrorKind))
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	if len(parts) == 0 {
		return ve.Error()
	}
	return strings.Join(parts, "; ")
}
