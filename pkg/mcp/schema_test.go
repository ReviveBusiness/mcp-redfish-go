package mcp

import (
	"reflect"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

// TestOutputSchemaGeneratesObjectType verifies that every output struct that
// previously used interface{} for its Data field now generates a schema with
// type="object" for that field.
//
// Background: the Go MCP SDK calls jsonschema.ForType on the output struct to
// produce outputSchema. When Data was interface{}, jsonschema-go produced an
// empty schema (no Type set) — this is "unrestricted" and some strict MCP
// clients (e.g. dot-ai's Zod validator) reject it because they expect every
// property schema to be a JSON object with a type. Changing to
// map[string]interface{} produces type="object", which is spec-compliant.
func TestOutputSchemaGeneratesObjectType(t *testing.T) {
	testCases := []struct {
		name string
		typ  reflect.Type
	}{
		{"GetResourceOutput", reflect.TypeOf(GetResourceOutput{})},
		{"PowerActionOutput", reflect.TypeOf(PowerActionOutput{})},
		{"SetBootOverrideOutput", reflect.TypeOf(SetBootOverrideOutput{})},
		{"ClearEventLogOutput", reflect.TypeOf(ClearEventLogOutput{})},
		{"SetBiosSettingOutput", reflect.TypeOf(SetBiosSettingOutput{})},
		{"SetAlertConfigOutput", reflect.TypeOf(SetAlertConfigOutput{})},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			schema, err := jsonschema.ForType(tc.typ, &jsonschema.ForOptions{})
			if err != nil {
				t.Fatalf("ForType(%s): %v", tc.name, err)
			}

			// The top-level struct should be type="object"
			if schema.Type != "object" {
				t.Errorf("%s: top-level schema.Type = %q, want \"object\"", tc.name, schema.Type)
			}

			// Check the Data property exists in Properties
			dataProp, ok := schema.Properties["data"]
			if !ok {
				// data is omitempty on some structs — it may be absent from Required
				// but must still appear as a property if the field is exported.
				// The field IS exported and tagged json:"data,...", so it must appear.
				t.Fatalf("%s: schema.Properties has no \"data\" field", tc.name)
			}

			// The fix: map[string]interface{} must produce type="object"
			// Before the fix, interface{} produced Type="" (unrestricted empty schema)
			if dataProp.Type != "object" {
				t.Errorf(
					"%s: properties.data.Type = %q, want \"object\"\n"+
						"  This means the interface{} → map[string]interface{} fix did NOT work.\n"+
						"  An empty type (\"\") means the field is still interface{}, which MCP Zod validators reject.",
					tc.name, dataProp.Type,
				)
			}

			t.Logf("%s: properties.data.Type = %q (PASS)", tc.name, dataProp.Type)
		})
	}
}

// TestNoPropertiesAreEmptySchema verifies that no property in any output struct
// schema has an empty/unconstrained schema (Type == "" and Types == nil).
//
// This is the Go-level equivalent of the MCP Zod check:
//   z.custom((v) => v !== null && typeof v === 'object')
//
// An empty schema (interface{}) would pass Zod's typeof check only if the
// JSON serialization represents it as an object — but jsonschema-go serializes
// an empty schema as {} which is technically valid JSON Schema but some
// validators expect a "type" keyword to be present.
func TestNoPropertiesAreEmptySchema(t *testing.T) {
	testTypes := []struct {
		name string
		typ  reflect.Type
	}{
		{"GetResourceOutput", reflect.TypeOf(GetResourceOutput{})},
		{"PowerActionOutput", reflect.TypeOf(PowerActionOutput{})},
		{"SetBootOverrideOutput", reflect.TypeOf(SetBootOverrideOutput{})},
		{"ClearEventLogOutput", reflect.TypeOf(ClearEventLogOutput{})},
		{"SetBiosSettingOutput", reflect.TypeOf(SetBiosSettingOutput{})},
		{"SetAlertConfigOutput", reflect.TypeOf(SetAlertConfigOutput{})},
	}

	for _, tc := range testTypes {
		t.Run(tc.name, func(t *testing.T) {
			schema, err := jsonschema.ForType(tc.typ, &jsonschema.ForOptions{})
			if err != nil {
				t.Fatalf("ForType(%s): %v", tc.name, err)
			}

			for propName, propSchema := range schema.Properties {
				if propSchema.Type == "" && len(propSchema.Types) == 0 &&
					propSchema.Ref == "" &&
					propSchema.AnyOf == nil && propSchema.OneOf == nil && propSchema.AllOf == nil {
					t.Errorf(
						"%s: property %q has an empty/unconstrained schema (Type=\"\", Types=[])\n"+
							"  This is the signature of interface{} — MCP clients may reject this.\n"+
							"  Fix: change the field type from interface{} to a concrete type.",
						tc.name, propName,
					)
				} else {
					t.Logf("%s.%s: Type=%q Types=%v (has constraints)", tc.name, propName, propSchema.Type, propSchema.Types)
				}
			}
		})
	}
}
