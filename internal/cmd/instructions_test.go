package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The pointer is a link an agent may or may not follow, so the one thing it has
// to carry is what stays true for the agent that skips it: which commands
// refuse. Deriving that from classified is the whole design, and a literal here
// would reintroduce exactly the drift the derivation prevents.
func TestPointerStanzaNamesEveryOperatorCommand(t *testing.T) {
	install()
	got := PointerStanza("DROVER.md")
	// Matched against the stanza with its wrapping collapsed: the text is folded
	// to a column, so a phrase that straddles a line break is present and would
	// fail a literal Contains.
	flat := strings.Join(strings.Fields(got), " ")
	for _, row := range OperatorCommands() {
		if !strings.Contains(flat, row.Class) {
			t.Errorf("stanza omits %s, which refuses inside a launched session", row.Class)
		}
	}
	for _, want := range []string{"DROVER.md", "exit 3", "cannot launch another", "drover agents"} {
		if !strings.Contains(flat, want) {
			t.Errorf("stanza does not mention %q", want)
		}
	}
	// The path is the reader's only route to the instructions, so it appears as
	// link text and as target rather than once.
	if strings.Count(got, "DROVER.md") < 2 {
		t.Errorf("stanza names DROVER.md %d times, want it as both link text and target", strings.Count(got, "DROVER.md"))
	}
}

// A tagged build links its own tag so an agent reads the surface its binary
// has. Anything else resolves to a ref that exists, never to an empty segment
// that would render a 404 into every generated file.
func TestDocsRefPinsOnlyARelease(t *testing.T) {
	original := version
	t.Cleanup(func() { version = original })

	for _, tc := range []struct{ stamped, want string }{
		{"v0.6.1", "v0.6.1"},
		{"v1.0.0", "v1.0.0"},
		{"v0.6.0-35-g8b16d75-dirty", "main"},
		{"v0.6.1-0.20260904014937-8b16d7539239", "main"},
		{"dev", "main"},
		{"", "main"},
	} {
		version = tc.stamped
		if got := docsRef(); got != tc.want {
			t.Errorf("version %q: docsRef = %q, want %q", tc.stamped, got, tc.want)
		}
	}
}

// writeAgents appends rather than overwrites, and the block it appends is the
// only region a later run touches. Anything else in the file is somebody's
// prose, and losing it once would be enough to stop people running the command.
func TestWriteAgentsKeepsWhatItDidNotWrite(t *testing.T) {
	s := fixture(t, "")
	host := filepath.Join(s.cfg.Root, "AGENTS.md")
	const houseRules = "# House rules\n\nNever push to main.\n"
	if err := os.WriteFile(host, []byte(houseRules), 0o644); err != nil {
		t.Fatal(err)
	}

	body, err := s.renderAgents()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.writeAgents("AGENTS.md", body); err != nil {
		t.Fatal(err)
	}
	after := read(t, host)
	if !strings.HasPrefix(after, houseRules) {
		t.Error("the hand-written preamble did not survive the append")
	}

	// A second write regenerates in place. Prose added below the block is on the
	// far side of the end marker and has to survive that too.
	const addendum = "\n## Addendum\n\nAsk before touching the migrations.\n"
	if err := os.WriteFile(host, []byte(after+addendum), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.writeAgents("AGENTS.md", body); err != nil {
		t.Fatal(err)
	}
	final := read(t, host)
	for _, want := range []string{"Never push to main.", "Ask before touching the migrations."} {
		if !strings.Contains(final, want) {
			t.Errorf("regeneration lost %q", want)
		}
	}
}

func TestCheckReportsInstructionDrift(t *testing.T) {
	install()

	t.Run("no block anywhere is silent", func(t *testing.T) {
		s := fixture(t, "")
		if kinds := instructionKinds(t, s); len(kinds) != 0 {
			t.Errorf("a project with no generated block got %v; writing one is a suggestion, not a requirement", kinds)
		}
	})

	t.Run("a current block in AGENTS.md is silent", func(t *testing.T) {
		s := fixture(t, "")
		writeBlock(t, s, "AGENTS.md")
		if kinds := instructionKinds(t, s); len(kinds) != 0 {
			t.Errorf("got %v, want nothing: the block is current and needs no pointer to itself", kinds)
		}
	})

	t.Run("an edited block is stale", func(t *testing.T) {
		s := fixture(t, "")
		writeBlock(t, s, "AGENTS.md")
		// Edited inside the markers. A change to the marker comment or to the
		// preamble above it is not this finding: those are outside the region a
		// regeneration would replace.
		host := filepath.Join(s.cfg.Root, "AGENTS.md")
		text := strings.Replace(read(t, host), agentsEnd, "a hand edit\n"+agentsEnd, 1)
		if err := os.WriteFile(host, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		if kinds := instructionKinds(t, s); !kinds["STALE BLOCK"] {
			t.Errorf("got %v, want STALE BLOCK", kinds)
		}
	})

	t.Run("a block outside AGENTS.md needs a pointer", func(t *testing.T) {
		s := fixture(t, "")
		writeBlock(t, s, "DROVER.md")
		if err := os.WriteFile(filepath.Join(s.cfg.Root, "AGENTS.md"),
			[]byte("# House rules\n\nNever push to main.\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if kinds := instructionKinds(t, s); !kinds["NO POINTER"] {
			t.Errorf("got %v, want NO POINTER", kinds)
		}
	})

	t.Run("the pointer stanza discharges it", func(t *testing.T) {
		s := fixture(t, "")
		writeBlock(t, s, "DROVER.md")
		if err := os.WriteFile(filepath.Join(s.cfg.Root, "AGENTS.md"),
			[]byte(PointerStanza("DROVER.md")), 0o644); err != nil {
			t.Fatal(err)
		}
		if kinds := instructionKinds(t, s); len(kinds) != 0 {
			t.Errorf("got %v, want nothing: the generated stanza is what NO POINTER asks for", kinds)
		}
	})

	t.Run("no AGENTS.md at all is not a finding", func(t *testing.T) {
		s := fixture(t, "")
		writeBlock(t, s, "DROVER.md")
		if kinds := instructionKinds(t, s); kinds["NO POINTER"] {
			t.Error("got NO POINTER with no AGENTS.md; the project told its agents some other way")
		}
	})

	// Found on drover's own repository: CHANGELOG.md quotes the marker inline in
	// the entry announcing the command, and a substring match told the reader to
	// regenerate their changelog.
	t.Run("a quoted marker is not a block", func(t *testing.T) {
		s := fixture(t, "")
		writeBlock(t, s, "AGENTS.md")
		for name, body := range map[string]string{
			"CHANGELOG.md": "- `--write <path>` replaces the text between `" + agentsBegin + "` and `" + agentsEnd + "`.\n",
			"README.md":    "Markers:\n\n```\n" + agentsBegin + "\n...\n" + agentsEnd + "\n```\n",
		} {
			if err := os.WriteFile(filepath.Join(s.cfg.Root, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if kinds := instructionKinds(t, s); len(kinds) != 0 {
			t.Errorf("got %v; prose quoting a marker inline or inside a fence holds no block", kinds)
		}
	})

	t.Run("inverted markers", func(t *testing.T) {
		s := fixture(t, "")
		if err := os.WriteFile(filepath.Join(s.cfg.Root, "AGENTS.md"),
			[]byte(agentsEnd+"\nstranded\n"+agentsBegin+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		kinds := instructionKinds(t, s)
		if !kinds["BAD MARKERS"] {
			t.Errorf("got %v, want BAD MARKERS", kinds)
		}
		if kinds["STALE BLOCK"] {
			t.Error("a file nobody can regenerate is not also stale; that names a fix that does not apply")
		}
	})
}

func instructionKinds(t *testing.T, s *session) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	s.addInstructions(func(f Finding) { out[f.Kind] = true })
	return out
}

func writeBlock(t *testing.T, s *session, name string) {
	t.Helper()
	body, err := s.renderAgents()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.writeAgents(name, body); err != nil {
		t.Fatal(err)
	}
}

// A file whose only markers are quoted holds no block, so --write appends one
// rather than splicing into the sentence doing the quoting.
//
// The splice would be silent and check could not see it afterward: the
// opening marker has prose before it, so findAgentsHosts reads no block there
// and STALE BLOCK could never fire on the result.
func TestWriteAgentsIgnoresAQuotedMarker(t *testing.T) {
	install()
	for _, tc := range []struct{ name, prose string }{
		{"inline", "# Release notes\n\n- `--write <path>` replaces the text between `" +
			agentsBegin + "` and `" + agentsEnd + "` and touches nothing outside them.\n"},
		{"fenced", "# Markers\n\n```\n" + agentsBegin + "\n...\n" + agentsEnd + "\n```\n"},
	} {
		prose := tc.prose
		t.Run(tc.name, func(t *testing.T) {
			s := fixture(t, "")
			host := filepath.Join(s.cfg.Root, "NOTES.md")
			if err := os.WriteFile(host, []byte(prose), 0o644); err != nil {
				t.Fatal(err)
			}
			block, err := s.renderAgents()
			if err != nil {
				t.Fatal(err)
			}
			if err := s.writeAgents("NOTES.md", block); err != nil {
				t.Fatal(err)
			}
			after := read(t, host)
			if !strings.HasPrefix(after, prose) {
				t.Errorf("the quoting prose was rewritten; the block spliced into it rather than appending\n%s",
					after[:min(len(after), 300)])
			}
			if !strings.Contains(after, block) {
				t.Error("the block was not appended whole")
			}
			// The append leaves one real block, so a second run regenerates in
			// place. A splice would leave the file in a shape check reads as
			// holding nothing.
			if err := s.writeAgents("NOTES.md", block); err != nil {
				t.Fatal(err)
			}
			if got := read(t, host); got != after {
				t.Error("the second write was not idempotent")
			}
			if hosts, err := findAgentsHosts(s.cfg.Root); err != nil || len(hosts) != 1 || hosts[0].Name != "NOTES.md" {
				t.Errorf("check sees %v, want the one block --write just left", hosts)
			}
		})
	}
}

// The two surfaces read the markers through one locator, so a file check calls
// a block host is the file --write regenerates rather than splices into. They
// disagreed once, and the file that exposed it was this repository's own
// CHANGELOG.md.
//
// The judge is what writeAgents did to the file, never findBlock: comparing two
// callers of one function proves only that the function is deterministic.
func TestWriteAndCheckAgreeOnWhatHoldsABlock(t *testing.T) {
	install()
	for _, tc := range []struct {
		name, body string
		host       bool // check reads a block here, so --write must regenerate
	}{
		{"a quoted marker", "text `" + agentsBegin + "` more\n", false},
		{"a fenced marker", "```\n" + agentsBegin + "\n" + agentsEnd + "\n```\n", false},
		{"no marker at all", "# Notes\n", false},
		{"real markers", agentsBegin + "\n\nSUPERSEDED\n\n" + agentsEnd + "\n", true},
		{"markers with trailing space", agentsBegin + "  \n\nSUPERSEDED\n\n" + agentsEnd + " \n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fixture(t, "")
			path := filepath.Join(s.cfg.Root, "NOTES.md")
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := hostsInclude(t, s, "NOTES.md"); got != tc.host {
				t.Fatalf("check sees a block here = %v, want %v", got, tc.host)
			}

			block, err := s.renderAgents()
			if err != nil {
				t.Fatal(err)
			}
			if err := s.writeAgents("NOTES.md", block); err != nil {
				t.Fatal(err)
			}

			after := read(t, path)
			switch {
			case tc.host && strings.Contains(after, "SUPERSEDED"):
				t.Error("check called this a block host and --write did not replace what was in it")
			case !tc.host && !strings.HasPrefix(after, tc.body):
				t.Errorf("check saw no block here and --write rewrote the prose anyway:\n%s",
					after[:min(len(after), 200)])
			}
			// Either way the file now holds exactly one block, which is what
			// makes the next run a regeneration rather than a second append.
			if !hostsInclude(t, s, "NOTES.md") {
				t.Error("after the write, check sees no block")
			}
			before := read(t, path)
			if err := s.writeAgents("NOTES.md", block); err != nil {
				t.Fatal(err)
			}
			if read(t, path) != before {
				t.Error("the write is not idempotent")
			}
		})
	}
}

func hostsInclude(t *testing.T, s *session, name string) bool {
	t.Helper()
	hosts, err := findAgentsHosts(s.cfg.Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hosts {
		if h.Name == name {
			return true
		}
	}
	return false
}
