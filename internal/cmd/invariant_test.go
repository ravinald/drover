package cmd

import (
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/prompts"
)

// The body an item carries once both writers have touched it. Requirements
// first, then what must stay true, then what broke this round.
const guarded = `Requirements:

1. Publish by rename.

` + prompts.GuaranteeHeading + `

- Bytes an already-opened reader sees never change.

` + prompts.ChangesHeading + `

- The mode dropped to 0644.
`

func TestAppendSectionKeepsWhatEarlierPassesRecorded(t *testing.T) {
	out := appendSection(guarded, "- Replace preserves the mode an operator set.")
	for _, want := range []string{
		"- Bytes an already-opened reader sees never change.",
		"- Replace preserves the mode an operator set.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the section lost %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, prompts.GuaranteeHeading); n != 1 {
		t.Errorf("wrote %d copies of the heading, want 1:\n%s", n, out)
	}
}

// The one ordering that matters: a run reads the body top to bottom, so what
// must hold is above what broke. A section appended at the end of the file
// would land under the findings on every item that has any.
func TestAppendSectionLandsAboveTheFindings(t *testing.T) {
	body := `Requirements:

1. Publish by rename.

` + prompts.ChangesHeading + `

- The mode dropped to 0644.
`
	out := appendSection(body, "- Create honors the umask.")
	guarantees := strings.Index(out, prompts.GuaranteeHeading)
	findings := strings.Index(out, prompts.ChangesHeading)
	switch {
	case guarantees < 0:
		t.Fatalf("no guarantee section was written:\n%s", out)
	case guarantees > findings:
		t.Errorf("the guarantees finished below the findings:\n%s", out)
	}
}

// A body with no sections at all takes the heading at the end, which is the
// only place it can go.
func TestAppendSectionCreatesTheSectionWhenTheBodyHasNone(t *testing.T) {
	out := appendSection("Requirements:\n\n1. Publish by rename.\n", "- Create honors the umask.")
	if !strings.HasSuffix(out, prompts.GuaranteeHeading+"\n\n- Create honors the umask.") {
		t.Errorf("the new section is not at the end of the body:\n%s", out)
	}
}

// Four passes each adding a bullet leave one list, not four. Markdown renders
// the blank-line version as separate lists, and an agent reading the second
// list as a continuation of the first is reading a coincidence.
func TestAppendSectionKeepsOneListWhenBothSidesAreBullets(t *testing.T) {
	out := appendSection(guarded, "- Create honors the umask.")
	if !strings.Contains(out, "reader sees never change.\n- Create honors the umask.") {
		t.Errorf("a blank line split one list in two:\n%s", out)
	}
	prose := appendSection(guarded, "Publication is not atomic on this filesystem.")
	if !strings.Contains(prose, "reader sees never change.\n\nPublication is not atomic") {
		t.Errorf("a paragraph ran into the bullet above it:\n%s", prose)
	}
}

// request-changes writes to its own heading and stops at the next one, so the section
// this command builds up survives every round of findings. The two writers
// share one body and neither may reach into the other's section.
func TestRequestChangesLeavesTheGuaranteesAlone(t *testing.T) {
	out := spliceSection(guarded, "- The mode is now preserved; the umask is not honored on create.")
	if !strings.Contains(out, "- Bytes an already-opened reader sees never change.") {
		t.Errorf("a request for changes took the guarantees with it:\n%s", out)
	}
	if strings.Contains(out, "- The mode dropped to 0644.") {
		t.Errorf("the findings were appended rather than replaced:\n%s", out)
	}
}
