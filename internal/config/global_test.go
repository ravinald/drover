package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withGlobal writes a per-user config and points GlobalPath at it for one test.
// t.Setenv restores the package-wide empty XDG root afterwards, so a test that
// writes a global config cannot leak one into the next.
func withGlobal(t *testing.T, body string) {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir := filepath.Join(xdg, "drover")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, GlobalFileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// project writes a minimal .drover.toml and returns its root.
func project(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The three merge behaviors a reader has to be able to predict, asserted
// together because they disagree with each other on purpose: a scalar and a
// table key survive the project's silence, and an array does not.
func TestGlobalLayersUnderTheProject(t *testing.T) {
	withGlobal(t, `
[paths]
backlog = "~/drover/{{ .Project }}/bodies"
[verify]
default = "make check"
lint = "make lint"
[frontmatter]
required = ["title", "deps", "owner"]
[intake]
stop_marker = "HOLD"
`)
	root := project(t, schemaLine+`project = "widget"
[verify]
default = "go test ./..."
[frontmatter]
required = ["title", "deps"]
`)

	c, err := Load(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	home, _ := os.UserHomeDir()
	if want := filepath.Join(home, "drover", "widget", "bodies"); c.Abs(c.Paths.Backlog) != want {
		t.Errorf("paths.backlog = %q, want %q — a global path must resolve for the project reading it",
			c.Abs(c.Paths.Backlog), want)
	}
	if c.Verify["default"] != "go test ./..." {
		t.Errorf("verify.default = %q, want the project's", c.Verify["default"])
	}
	if c.Verify["lint"] != "make lint" {
		t.Errorf("verify.lint = %q, want the global's — a table merges per key", c.Verify["lint"])
	}
	if got := strings.Join(c.Frontmatter.Required, ","); got != "title,deps" {
		t.Errorf("frontmatter.required = %q, want the project's list whole — an array replaces", got)
	}
	if c.Intake.StopMarker != "HOLD" {
		t.Errorf("intake.stop_marker = %q, want the global's", c.Intake.StopMarker)
	}
	if len(c.Sources()) != 2 {
		t.Errorf("Sources() = %v, want the global file and the project's", c.Sources())
	}
}

// A project on a machine with no global config must load exactly as it did
// before there was such a thing.
func TestNoGlobalConfigChangesNothing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := project(t, schemaLine+"project = \"widget\"\n")
	c, err := Load(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Paths.Backlog != ".drover/backlog" {
		t.Errorf("paths.backlog = %q, want the built-in default", c.Paths.Backlog)
	}
	if got := c.Sources(); len(got) != 1 {
		t.Errorf("Sources() = %v, want just the project's config", got)
	}
}

// A relative path in the global file resolves against whichever project read
// it, which would put every project's corpus at the same spot inside itself.
func TestGlobalRejectsRelativePaths(t *testing.T) {
	withGlobal(t, "[paths]\nledger = \"docs/ledger.jsonl\"\n")
	root := project(t, schemaLine+"project = \"widget\"\n")

	_, err := Load(root)
	if err == nil {
		t.Fatal("a relative global path loaded; it would resolve against the project that read it")
	}
	for _, want := range []string{"paths.ledger", "relative"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// A path whose template renders to something relative is the same mistake with
// a template in front of it, so the prefix has to be literal.
func TestGlobalRejectsATemplatedRelativePath(t *testing.T) {
	withGlobal(t, "[paths]\nbacklog = \"{{ .Project }}/bodies\"\n")
	root := project(t, schemaLine+"project = \"widget\"\n")
	if _, err := Load(root); err == nil {
		t.Fatal("a template-leading global path loaded")
	}
}

// root_marker is exempt: it names a file that must exist in whatever tree reads
// it, so relative is the only form it has ever taken.
func TestGlobalAllowsARelativeRootMarker(t *testing.T) {
	withGlobal(t, "[paths]\nroot_marker = \"go.mod\"\n")
	root := project(t, schemaLine+"project = \"widget\"\n")
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Paths.RootMarker != "go.mod" {
		t.Errorf("root_marker = %q, want go.mod", c.Paths.RootMarker)
	}
}

// One project name for every project on the machine collides silently: two
// projects' run logs land in one directory.
func TestGlobalRejectsProject(t *testing.T) {
	withGlobal(t, "project = \"everything\"\n")
	root := project(t, schemaLine+"project = \"widget\"\n")
	_, err := Load(root)
	if err == nil {
		t.Fatal("a global project name loaded")
	}
	if !strings.Contains(err.Error(), "per-project") {
		t.Errorf("error %q does not say why", err)
	}
}

// A typo in the global file takes effect as silence in every project on the
// machine, which is strictly worse than the same typo in one project's config.
func TestGlobalRejectsUnknownKeys(t *testing.T) {
	withGlobal(t, "[paths]\nbodie = \"/tmp/x\"\n")
	root := project(t, schemaLine+"project = \"widget\"\n")
	_, err := Load(root)
	if err == nil {
		t.Fatal("an unknown global key loaded")
	}
	if !strings.Contains(err.Error(), GlobalFileName) {
		t.Errorf("error %q does not name the file the typo is in", err)
	}
}

// Unset means enabled. The zero Config is what every caller that builds one by
// hand gets, and branch automation silently off there is the failure this
// pointer exists to prevent.
func TestGitDefaultsOn(t *testing.T) {
	if !(Git{}).On() {
		t.Error("the zero Git reads as disabled")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := project(t, schemaLine+"project = \"widget\"\n[git]\nenabled = false\n")
	c, err := Load(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Git.On() {
		t.Error("[git] enabled = false read as enabled")
	}
}

// A project inside a repository overrides a global that turned git off.
func TestProjectRestoresGitOverAGlobalThatTurnedItOff(t *testing.T) {
	withGlobal(t, "[git]\nenabled = false\n")
	root := project(t, schemaLine+"project = \"widget\"\n[git]\nenabled = true\n")
	c, err := Load(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !c.Git.On() {
		t.Error("the project's true did not win over the global's false")
	}
}

// The per-user file's own version decides, and it decides before its keys are
// judged. Refusing a stale per-user file as `unknown key(s):
// [github.caveat_label]` would tell the reader a key does not exist when what
// happened is that it was renamed and their file predates the rename. The refusal
// that says so is reached only if layerGlobal checks the version before it runs
// unknownKeys, because Load calls layerGlobal before the project file is decoded.
//
// Four real files on disk, one per case, because the question is which file the
// refusal names and whether it carries the guidance — and a fixture built by
// hand-trimming today's layout cannot answer either.
func TestThePerUserVersionIsJudgedBeforeItsKeys(t *testing.T) {
	const current = "schema_version = 2\n"
	const retired = "[github]\ncaveat_label = \"follow-up\"\n"
	const projectBody = "project = \"b38\"\n[paths]\nledger = \"ledger.jsonl\"\n"

	for _, c := range []struct {
		name, global, conf string
		wantErr            bool
		inGlobal           bool // which file the refusal must name
		wantVersion        bool // whether it is refused for its schema_version
	}{
		// The project is current, so the layered version reads 2 and a check on
		// the layered value alone passes; the per-user file is the stale one.
		{
			name:   "stale per-user under a current project",
			global: "schema_version = 1\n" + retired, conf: current + projectBody,
			wantErr: true, inGlobal: true, wantVersion: true,
		},
		// A stale per-user file is stale whether or not it carries a retired key.
		// Refusing only the ones that do would leave the operator to discover the
		// version on the next rename rather than on this one.
		{
			name:   "stale per-user carrying nothing retired",
			global: "schema_version = 1\n", conf: current + projectBody,
			wantErr: true, inGlobal: true, wantVersion: true,
		},
		{
			name:   "current per-user with an unknown key",
			global: current + retired, conf: current + projectBody,
			wantErr: true, inGlobal: true,
		},
		{
			name:   "stale project under a current per-user",
			global: current, conf: "schema_version = 1\n" + projectBody,
			wantErr: true, wantVersion: true,
		},
		// Absence is not version 1. Every machine whose per-user file predates the
		// key has been fine all along and must stay that way.
		{
			name:   "per-user declaring no version",
			global: "[run]\ndefault_runner = \"claude\"\n", conf: current + projectBody,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			withGlobal(t, c.global)
			_, err := Load(project(t, c.conf))
			if !c.wantErr {
				if err != nil {
					t.Fatalf("Load refused a config that should hold: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Load accepted a config it must refuse")
			}
			want := FileName
			if c.inGlobal {
				want = GlobalFileName
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal names neither %s nor the right file:\n%v", want, err)
			}
			if c.inGlobal && strings.Contains(err.Error(), FileName) {
				t.Errorf("the refusal names the project file for a per-user problem:\n%v", err)
			}
			if got := strings.Contains(err.Error(), "this build understands"); got != c.wantVersion {
				t.Errorf("refused for its schema_version = %v, want %v:\n%v", got, c.wantVersion, err)
			}
			if !c.wantVersion && !strings.Contains(err.Error(), "unknown key(s)") {
				t.Errorf("a current file's bad key should still be refused as unknown:\n%v", err)
			}
		})
	}
}
