package agenttools

import "encoding/json"

func marshalOrEmpty(m map[string]any) (json.RawMessage, error) {
	if m == nil {
		return nil, nil
	}
	return json.Marshal(m)
}
