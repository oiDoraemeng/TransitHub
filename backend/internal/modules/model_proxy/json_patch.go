package model_proxy

import (
	"encoding/json"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// encoding/json uses the last duplicate field. Keep that behavior for unusual
// inputs before patching: gjson/sjson otherwise use the first occurrence.
// Only duplicate-key inputs need a full RawMessage copy; ordinary requests
// keep their opaque message/tool bytes intact.
func canonicalPatchObject(raw []byte) ([]byte, error) {
	duplicate := false
	seen := make(map[string]bool)
	gjson.GetBytes(raw, "@keys").ForEach(func(_, key gjson.Result) bool {
		name := key.String()
		if seen[name] {
			duplicate = true
			return false
		}
		seen[name] = true
		return true
	})
	if !duplicate {
		return raw, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	return json.Marshal(object)
}

func canonicalPatchMetadata(raw []byte, key string) ([]byte, error) {
	value := gjson.GetBytes(raw, key)
	if !value.IsObject() {
		return raw, nil
	}
	canonical, err := canonicalPatchObject([]byte(value.Raw))
	if err != nil {
		return nil, err
	}
	if string(canonical) == value.Raw {
		return raw, nil
	}
	return sjson.SetRawBytes(raw, key, canonical)
}
