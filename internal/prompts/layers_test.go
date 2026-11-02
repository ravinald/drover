package prompts

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func override(t *testing.T, slot, text string) Template {
	t.Helper()
	path := filepath.Join(t.TempDir(), slot+".tmpl")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	tmpl, err := Load(slot, path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return tmpl
}

func blockByName(t *testing.T, slot, name string) Block {
	t.Helper()
	for _, b := range Blocks(slot) {
		if b.Name == name {
			return b
		}
	}
	t.Fatalf("%s has no block %q", slot, name)
	return Block{}
}

func TestEverySlotNamesItsBlocks(t *testing.T) {
	want := map[string][]string{
		SlotImplement: {"context", "done", "intro", "scope"},
		SlotReview:    {"checks", "context", "intro", "verify"},
		SlotDraft:     {"context"},
	}
	for slot, names := range want {
		var got []string
		for _, b := range Blocks(slot) {
			got = append(got, b.Name)
		}
		if !reflect.DeepEqual(got, names) {
			t.Errorf("%s blocks = %v, want %v", slot, got, names)
		}
	}
}

func TestABuiltinRendersAsItsOwnText(t *testing.T) {
	for _, slot := range Slots {
		layered, err := Template{Slot: slot}.Render(Sample())
		if err != nil {
			t.Fatalf("%s: %v", slot, err)
		}
		alone, err := Render(slot, Builtin(slot), Sample())
		if err != nil {
			t.Fatalf("%s alone: %v", slot, err)
		}
		if layered != alone {
			t.Errorf("%s renders differently with no override than on its own", slot)
		}
	}
}

func TestAnOverrideReplacesOnlyItsBlocks(t *testing.T) {
	scope := blockByName(t, SlotImplement, "scope")
	tmpl := override(t, SlotImplement, `{{define "done"}}
- Run make codegen after editing the API spec.
{{- end}}
{{define "scope"}}{{/* drover-base: `+scope.Hash+` */}}- Own only this item's packages.{{end}}
`)
	out, err := tmpl.Render(Sample())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"- Run make codegen after editing the API spec.",
		"- Own only this item's packages.",
		"Operator requirements: not yours to attempt.",
		"**Do not open a pull request and do not push.**",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered prompt lacks %q", want)
		}
	}
	if strings.Contains(out, "Smallest diff that does it.") {
		t.Error("the replaced scope default still renders")
	}
	if got := tmpl.Inspect(); got.Whole || len(got.Behind) != 0 {
		t.Errorf("Inspect = %+v, want nothing to report", got)
	}
}

func TestAReviewOverrideKeepsTheEscalation(t *testing.T) {
	tmpl := override(t, SlotReview, `{{define "checks"}}
- Every queryset reaching IPAM objects is wrapped in .restrict(user, "view").
{{- end}}`)
	out, err := tmpl.Render(Sample())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{".restrict(user, \"view\")", "--operator N[,N...]", "drover request-changes"} {
		if !strings.Contains(out, want) {
			t.Errorf("review prompt lacks %q", want)
		}
	}
}

func TestAnUnknownBlockIsRefused(t *testing.T) {
	tmpl := override(t, SlotImplement, `{{define "definition-of-done"}}- a rule{{end}}`)
	_, err := tmpl.Render(Sample())
	if err == nil || !strings.Contains(err.Error(), `"definition-of-done"`) || !strings.Contains(err.Error(), "schema --prompts") {
		t.Fatalf("err = %v, want a refusal naming the block and where the list is", err)
	}
}

func TestInspectReportsWhatFallsBehind(t *testing.T) {
	intro := blockByName(t, SlotImplement, "intro")
	cases := []struct {
		name   string
		text   string
		whole  bool
		behind []Behind
	}{
		{"a whole file", "You are executing {{ .ID }}.\n", true, nil},
		{"an addition needs no marker", `{{define "context"}}project text{{end}}`, false, nil},
		{"a replaced default with no marker", `{{define "intro"}}Do {{ .ID }}.{{end}}`, false,
			[]Behind{{Block: "intro", Current: intro.Hash}}},
		{"a replaced default whose base moved", `{{define "intro"}}{{/* drover-base: 000000000000 */}}Do {{ .ID }}.{{end}}`, false,
			[]Behind{{Block: "intro", Recorded: "000000000000", Current: intro.Hash}}},
		{"a replaced default at its base", `{{define "intro"}}{{/* drover-base: ` + intro.Hash + ` */}}Do {{ .ID }}.{{end}}`, false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tmpl := override(t, SlotImplement, c.text)
			if _, err := tmpl.Render(Sample()); err != nil {
				t.Fatalf("render: %v", err)
			}
			got := tmpl.Inspect()
			if got.Whole != c.whole || !reflect.DeepEqual(got.Behind, c.behind) {
				t.Errorf("Inspect = %+v, want whole %v behind %+v", got, c.whole, c.behind)
			}
		})
	}
}

func TestAWholeFileStillRendersAsWritten(t *testing.T) {
	tmpl := override(t, SlotImplement, "Only this: {{ .ID }}.\n")
	out, err := tmpl.Render(Sample())
	if err != nil {
		t.Fatal(err)
	}
	if out != "Only this: 1.\n" {
		t.Errorf("out = %q", out)
	}
}
