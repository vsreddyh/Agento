// Package mcpschema publishes the JSON-schema patches the MCP SDK cannot infer
// from a Go type: enums for plain string fields.
//
// The SDK infers every string field as `{"type":"string"}`, so an unknown
// value reaches the handler and fails there. Pinning the enum in the published
// schema makes the same rejection happen one layer earlier — schema first, then
// the store's fail() — while the allowed set stays defined once, in the store
// package that also validates it. Each server's schema_test.go asserts the
// published enum, so the two layers cannot drift apart.
package mcpschema

import (
	"fmt"
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"
)

// EnumSchema infers the schema for in — the same call AddTool makes — and pins
// each named property to its allowed values. A field absent from the inferred
// schema is a programming error rather than a runtime condition (the enum would
// silently never publish), so it panics at startup: the servers build their
// tool list at boot, before any call.
func EnumSchema(in any, enums map[string][]string) *jsonschema.Schema {
	s, err := jsonschema.ForType(reflect.TypeOf(in), nil)
	if err != nil {
		panic(err)
	}
	for field, values := range enums {
		prop, ok := s.Properties[field]
		if !ok {
			panic(fmt.Sprintf("enum field %q missing from schema", field))
		}
		anyVals := make([]any, 0, len(values))
		for _, v := range values {
			anyVals = append(anyVals, v)
		}
		prop.Enum = anyVals
		s.Properties[field] = prop
	}
	return s
}
