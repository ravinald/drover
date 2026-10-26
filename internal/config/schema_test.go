package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/ravinald/drover/internal/backlog"
)

// A schema that omits a key is worse than no schema: it validates a correct
// config as wrong, and an editor completing from it hides the key entirely.
// This walks the Go types rather than a hand-kept list, so adding a field to
// Config fails here until the schema learns about it.
func TestSchemaCoversConfig(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal([]byte(Schema), &doc); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	walk(t, reflect.TypeOf(Config{}), doc, ".drover.toml")
}

func walk(t *testing.T, typ reflect.Type, node map[string]any, path string) {
	t.Helper()
	props, _ := node["properties"].(map[string]any)
	if props == nil {
		t.Fatalf("%s: schema node has no properties", path)
	}

	for i := range typ.NumField() {
		f := typ.Field(i)
		tag := f.Tag.Get("toml")
		if tag == "" || tag == "-" {
			continue
		}
		child, ok := props[tag].(map[string]any)
		if !ok {
			t.Errorf("%s: schema is missing %q (field %s)", path, tag, f.Name)
			continue
		}
		if _, ok := child["description"]; !ok {
			t.Errorf("%s.%s: no description — the schema is the machine-readable half of the reference", path, tag)
		}
		if f.Type.Kind() == reflect.Struct && !f.Type.Implements(reflect.TypeFor[toml.Marshaler]()) {
			walk(t, f.Type, child, path+"."+tag)
		}
	}

	// The reverse direction: a schema key no field reads would be documented,
	// completed by an editor, and then rejected by Load as an unknown key.
	for key := range props {
		if key == "default" {
			continue // [verify] default is a map entry, not a field
		}
		if !fieldFor(typ, key) {
			t.Errorf("%s: schema declares %q, which no config field reads", path, key)
		}
	}
}

func fieldFor(typ reflect.Type, tag string) bool {
	for i := range typ.NumField() {
		if typ.Field(i).Tag.Get("toml") == tag {
			return true
		}
	}
	return false
}

// The schema is embedded JSON, so it cannot render SchemaVersion the way the
// init template does. Nothing else compares the two: the test fixtures carry no
// literal version so a bump does not redden the suite, which would let a bump
// ship a schema that rejects every correct config on that build, naming the
// config rather than the schema when it does.
func TestSchemaPinsTheVersionThisBuildUnderstands(t *testing.T) {
	var doc struct {
		ID         string `json:"$id"`
		Properties struct {
			SchemaVersion struct {
				Const int `json:"const"`
			} `json:"schema_version"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(Schema), &doc); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if doc.Properties.SchemaVersion.Const != SchemaVersion {
		t.Errorf("schema pins schema_version const %d, this build understands %d:"+
			" update drover.schema.json in the commit that bumps SchemaVersion",
			doc.Properties.SchemaVersion.Const, SchemaVersion)
	}
	// The $id carries the same version, and can drift from the const on its own.
	if want := fmt.Sprintf("/schema/v%d/", SchemaVersion); !strings.Contains(doc.ID, want) {
		t.Errorf("schema $id is %q, which does not name %q", doc.ID, want)
	}
}

// The shape pattern is the schema's second copy of backlog.ShapeForms, and a
// copy drifts: a form added to ShapeForms and missing from the pattern makes a
// validator generated from `drover schema` reject a .drover.toml that uses it.
// TestSchemaCoversConfig walks keys and never values, so it cannot catch this.
func TestSchemaPatternAcceptsEveryShapeForm(t *testing.T) {
	var doc struct {
		Properties struct {
			Frontmatter struct {
				Properties struct {
					Shape struct {
						AdditionalProperties struct {
							Pattern string `json:"pattern"`
						} `json:"additionalProperties"`
					} `json:"shape"`
				} `json:"properties"`
			} `json:"frontmatter"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(Schema), &doc); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	pattern := doc.Properties.Frontmatter.Properties.Shape.AdditionalProperties.Pattern
	if pattern == "" {
		t.Fatal("schema declares no pattern for a frontmatter shape")
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("shape pattern %q does not compile: %v", pattern, err)
	}
	for _, form := range backlog.ShapeForms() {
		if !re.MatchString(form) {
			t.Errorf("shape pattern %q rejects %q, which ParseShape accepts", pattern, form)
		}
	}
}
