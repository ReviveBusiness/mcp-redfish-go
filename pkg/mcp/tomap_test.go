package mcp

import (
	"testing"
)

// TestToMapData verifies the toMapData helper handles all input types correctly.
//
// toMapData exists because Redfish API responses are always JSON objects, but
// the Go HTTP response layer returns interface{}. We need safe type assertion
// with graceful handling of unexpected types (log warning, return nil) rather
// than panicking.
func TestToMapData(t *testing.T) {
	tests := []struct {
		name    string
		input   interface{}
		wantNil bool
		desc    string
	}{
		{
			name:    "nil input",
			input:   nil,
			wantNil: true,
			desc:    "nil should return nil (no warning needed)",
		},
		{
			name:    "valid map",
			input:   map[string]interface{}{"key": "value"},
			wantNil: false,
			desc:    "a proper map[string]interface{} must pass through unchanged",
		},
		{
			name:    "empty map",
			input:   map[string]interface{}{},
			wantNil: false,
			desc:    "an empty map is still a valid map — must not be treated as nil",
		},
		{
			name:    "string input",
			input:   "not a map",
			wantNil: true,
			desc:    "non-map types must return nil (logs a warning)",
		},
		{
			name:    "int input",
			input:   42,
			wantNil: true,
			desc:    "integers must return nil (Redfish would never return a bare int)",
		},
		{
			name:    "bool input",
			input:   true,
			wantNil: true,
			desc:    "booleans must return nil",
		},
		{
			name:    "slice input",
			input:   []interface{}{"a", "b"},
			wantNil: true,
			desc:    "JSON arrays are not objects — must return nil",
		},
		{
			name:    "float input",
			input:   3.14,
			wantNil: true,
			desc:    "floats must return nil",
		},
		{
			name:    "nested map",
			input:   map[string]interface{}{"outer": map[string]interface{}{"inner": 1}},
			wantNil: false,
			desc:    "nested maps must pass through — toMapData only unwraps one level",
		},
		{
			name:    "map with nil value",
			input:   map[string]interface{}{"key": nil},
			wantNil: false,
			desc:    "map with nil values is still a valid map",
		},
		{
			name:    "map with mixed value types",
			input:   map[string]interface{}{"str": "val", "num": 42, "bool": true},
			wantNil: false,
			desc:    "realistic Redfish response with mixed value types",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := toMapData(tt.input)

			if tt.wantNil && result != nil {
				t.Errorf("toMapData(%T) = %v, want nil\n  context: %s", tt.input, result, tt.desc)
				return
			}
			if !tt.wantNil && result == nil {
				t.Errorf("toMapData(%T) = nil, want non-nil map\n  context: %s", tt.input, tt.desc)
				return
			}

			// For non-nil results, verify the map identity (not just non-nil)
			if !tt.wantNil {
				inputMap, _ := tt.input.(map[string]interface{})
				if len(result) != len(inputMap) {
					t.Errorf("toMapData returned map with %d entries, input had %d entries", len(result), len(inputMap))
				}
			}

			t.Logf("toMapData(%T): wantNil=%v result_is_nil=%v (PASS)", tt.input, tt.wantNil, result == nil)
		})
	}
}

// TestToMapDataPreservesValues verifies that toMapData returns the exact same
// map (not a copy), so callers get the original data without any transformation.
func TestToMapDataPreservesValues(t *testing.T) {
	input := map[string]interface{}{
		"@odata.id":   "/redfish/v1/Systems/System.Embedded.1",
		"@odata.type": "#ComputerSystem.v1_5_0.ComputerSystem",
		"PowerState":  "On",
		"Health":      "OK",
		"nested": map[string]interface{}{
			"SubField": "value",
		},
	}

	result := toMapData(input)
	if result == nil {
		t.Fatal("toMapData returned nil for a valid map")
	}

	// Check string/scalar values only — maps are not comparable with !=
	scalarKeys := []string{"@odata.id", "@odata.type", "PowerState", "Health"}
	for _, k := range scalarKeys {
		if result[k] != input[k] {
			t.Errorf("key %q: got %v, want %v", k, result[k], input[k])
		}
	}
	// Verify the nested map is present (existence check, not deep equality)
	if _, ok := result["nested"]; !ok {
		t.Error("key \"nested\" missing from result")
	}

	// Verify it's the same map reference (not a copy)
	input["new_key"] = "added_after"
	if result["new_key"] != "added_after" {
		t.Error("toMapData returned a copy instead of the original map — this is unexpected behavior")
	}

	t.Log("toMapData preserves all values and returns the original map reference (PASS)")
}
