package cmd

import (
	"encoding/json"
	"maps"
	"reflect"
	"strings"
	"testing"
)

// encoders pairs each --json command with the Go type it emits. The sample in
// that command's Examples block is what a wrapper author reads and codes
// against, so a renamed tag or a retyped field has to fail here rather than in
// somebody else's parser six months later.
var encoders = map[string]any{
	"status":     StatusReport{},
	"show":       Show{},
	"unaudited":  UnauditedRow{},
	"check":      CheckReport{},
	"follow-ups": RegisterReport{},
	"skills":     SkillUse{},
	"report":     RunReport{},
}

func TestExampleSamplesMatchTheEncoders(t *testing.T) {
	install()
	// Outside a project the expander resolves a placeholder to the config key
	// it comes from, which keeps the samples the same on every host.
	t.Chdir(t.TempDir())
	vars := helpVars()

	for name, shape := range encoders {
		t.Run(name, func(t *testing.T) {
			c, _, err := root.Find([]string{name})
			if err != nil {
				t.Fatalf("no such command: %v", err)
			}
			samples := jsonObjects(substitute(c.Example, vars))
			if len(samples) == 0 {
				t.Fatal("Examples advertises no sample object")
			}
			for _, text := range samples {
				var obj map[string]any
				if err := json.Unmarshal([]byte(text), &obj); err != nil {
					t.Errorf("sample is not valid JSON: %v\n%s", err, text)
					continue
				}
				checkObject(t, reflect.TypeOf(shape), obj, name)
			}
		})
	}
}

// checkObject asserts every key the sample advertises is one the type encodes,
// at the type the sample shows it at.
func checkObject(t *testing.T, typ reflect.Type, obj map[string]any, path string) {
	t.Helper()
	fields := jsonFields(typ)
	for key, val := range obj {
		f, ok := fields[key]
		if !ok {
			t.Errorf("%s.%s: advertised, but %s encodes no such field", path, key, typ)
			continue
		}
		checkValue(t, f.Type, val, path+"."+key)
	}
}

func checkValue(t *testing.T, typ reflect.Type, val any, path string) {
	t.Helper()
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch v := val.(type) {
	case nil:
	case map[string]any:
		switch typ.Kind() {
		case reflect.Struct:
			checkObject(t, typ, v, path)
		case reflect.Map:
			for key, elem := range v {
				checkValue(t, typ.Elem(), elem, path+"."+key)
			}
		default:
			t.Errorf("%s: advertised as an object, encoded as %s", path, typ)
		}
	case []any:
		if typ.Kind() != reflect.Slice && typ.Kind() != reflect.Array {
			t.Errorf("%s: advertised as a list, encoded as %s", path, typ)
			return
		}
		for _, elem := range v {
			checkValue(t, typ.Elem(), elem, path+"[]")
		}
	case string:
		if typ.Kind() != reflect.String {
			t.Errorf("%s: advertised as a string, encoded as %s", path, typ)
		}
	case bool:
		if typ.Kind() != reflect.Bool {
			t.Errorf("%s: advertised as a boolean, encoded as %s", path, typ)
		}
	case float64:
		switch typ.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
			reflect.Float32, reflect.Float64:
		default:
			t.Errorf("%s: advertised as a number, encoded as %s", path, typ)
		}
	default:
		t.Errorf("%s: sample carries %T, which this check does not know", path, val)
	}
}

func jsonFields(typ reflect.Type) map[string]reflect.StructField {
	out := map[string]reflect.StructField{}
	for i := range typ.NumField() {
		f := typ.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		// An untagged embedded struct's fields encode at this level, as
		// encoding/json flattens them.
		if f.Anonymous && name == "" && f.Type.Kind() == reflect.Struct {
			maps.Copy(out, jsonFields(f.Type))
			continue
		}
		switch name {
		case "-":
			continue
		case "":
			name = f.Name
		}
		out[name] = f
	}
	return out
}

// jsonObjects pulls every sample out of an Examples block. A sample opens at {"
// and closes at the brace balancing it, which is what lets one block carry two
// objects on consecutive lines.
func jsonObjects(text string) []string {
	// [...] is the elision a sample uses for a list documented elsewhere.
	// Nothing else in one is abbreviated, so restoring it to an empty list is
	// what makes the surrounding object parse.
	text = strings.ReplaceAll(text, "[...]", "[]")
	var out []string
	for i := 0; i+1 < len(text); i++ {
		if text[i] != '{' || text[i+1] != '"' {
			continue
		}
		if n, ok := balanced(text[i:]); ok {
			out = append(out, text[i:i+n])
			i += n - 1
		}
	}
	return out
}

// balanced returns the length of the object starting at s[0], skipping braces
// that sit inside a string.
func balanced(s string) (int, bool) {
	depth, inString, escaped := 0, false, false
	for i := range len(s) {
		switch c := s[i]; {
		case escaped:
			escaped = false
		case inString && c == '\\':
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
		case c == '{':
			depth++
		case c == '}':
			if depth--; depth == 0 {
				return i + 1, true
			}
		}
	}
	return 0, false
}
