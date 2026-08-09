package host

import (
	"encoding/json"
	"testing"
)

func TestSameJSONIgnoresObjectKeyOrder(t *testing.T) {
	t.Parallel()

	first := json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`)
	second := json.RawMessage(`{"required":["name"],"properties":{"name":{"type":"string"}},"type":"object"}`)
	if !sameJSON(first, second) {
		t.Fatal("sameJSON() rejected semantically equal objects")
	}
}
