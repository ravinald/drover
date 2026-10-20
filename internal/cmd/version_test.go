package cmd

import (
	"runtime/debug"
	"strings"
	"testing"
)

// stash swaps the build metadata for one test and restores it, so a test that
// pretends to be an unstamped build cannot leak that into the next one.
func stash(t *testing.T, v, c, d string) {
	t.Helper()
	ov, oc, od, oz := version, commit, date, fromModuleZip
	t.Cleanup(func() { version, commit, date, fromModuleZip = ov, oc, od, oz })
	version, commit, date, fromModuleZip = v, c, d, false
}

func info(mainVersion string, settings ...debug.BuildSetting) *debug.BuildInfo {
	bi := &debug.BuildInfo{Settings: settings}
	bi.Main.Version = mainVersion
	return bi
}

func TestBuildInfoFillsDefaults(t *testing.T) {
	stash(t, devVersion, devCommit, devDate)
	applyBuildInfo(info("v1.2.3",
		debug.BuildSetting{Key: "vcs.revision", Value: "cafef00d"},
		debug.BuildSetting{Key: "vcs.time", Value: "2026-01-02T03:04:05Z"},
	))

	if version == devVersion {
		t.Errorf("version still %q with build info available", devVersion)
	}
	if got, want := versionLine(), "drover v1.2.3"; got != want {
		t.Errorf("versionLine() = %q, want %q", got, want)
	}
	if commit != "cafef00d" {
		t.Errorf("commit = %q, want %q", commit, "cafef00d")
	}
	if date != "2026-01-02T03:04:05Z" {
		t.Errorf("date = %q, want %q", date, "2026-01-02T03:04:05Z")
	}
}

func TestLdflagsWinOverBuildInfo(t *testing.T) {
	stash(t, "v0.4.0-4-g48ad71b", "48ad71b", "2026-08-26T03:06:53Z")
	applyBuildInfo(info("v1.2.3",
		debug.BuildSetting{Key: "vcs.revision", Value: "cafef00d"},
		debug.BuildSetting{Key: "vcs.time", Value: "2026-01-02T03:04:05Z"},
	))

	if version != "v0.4.0-4-g48ad71b" || commit != "48ad71b" || date != "2026-08-26T03:06:53Z" {
		t.Errorf("build info overwrote -ldflags: %s %s %s", version, commit, date)
	}
}

func TestUnstampedBuildInfoKeepsDefaults(t *testing.T) {
	stash(t, devVersion, devCommit, devDate)
	applyBuildInfo(info("(devel)"))
	applyBuildInfo(info(""))
	applyBuildInfo(nil)

	if version != devVersion || commit != devCommit || date != devDate {
		t.Errorf("unstamped build info changed the defaults: %s %s %s", version, commit, date)
	}
}

// The version flag and the version command answer the same question, so they
// have to agree on the answer.
func TestVersionFlagMatchesCommand(t *testing.T) {
	if root.Version != version {
		t.Errorf("root.Version = %q, want %q", root.Version, version)
	}
	if got, want := root.VersionTemplate(), versionLine()+"\n"; got != want {
		t.Errorf("version template = %q, want %q", got, want)
	}
}

// The three shapes a drover binary ships in. Each drives applyBuildInfo with the
// build info that shape actually produces: building the binaries would need a
// module proxy, a tag and a clean tree, and would still assert nothing about the
// shape it failed to reproduce.

// make install, and any -ldflags build: all three values arrive stamped and Go's
// build info is not consulted for any of them.
func TestReportStampedBuild(t *testing.T) {
	stash(t, "v0.4.0-4-g48ad71b", "48ad71b", "2026-08-26T03:06:53Z")
	applyBuildInfo(info("(devel)",
		debug.BuildSetting{Key: "vcs.revision", Value: "cafef00d"},
		debug.BuildSetting{Key: "vcs.time", Value: "2026-01-02T03:04:05Z"},
	))

	got := versionReport()
	for _, want := range []string{"drover v0.4.0-4-g48ad71b\n", "  commit: 48ad71b\n", "  built:  2026-08-26T03:06:53Z\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, moduleZipNote) {
		t.Errorf("stamped build printed the module-zip note:\n%s", got)
	}
}

// go install from a work tree: no -ldflags, but the VCS settings carry the
// commit and its timestamp, so the report is complete without a reason line.
func TestReportWorkTreeBuild(t *testing.T) {
	stash(t, devVersion, devCommit, devDate)
	applyBuildInfo(info("v0.4.1-0.20260826030831-a89726315fa7",
		debug.BuildSetting{Key: "vcs.revision", Value: "a89726315fa748c009ddf478fbdd3075effa5fa9"},
		debug.BuildSetting{Key: "vcs.time", Value: "2026-08-26T03:08:31Z"},
	))

	got := versionReport()
	for _, want := range []string{
		"drover v0.4.1-0.20260826030831-a89726315fa7\n",
		"  commit: a89726315fa748c009ddf478fbdd3075effa5fa9\n",
		"  built:  2026-08-26T03:08:31Z\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q:\n%s", want, got)
		}
	}
	if fromModuleZip {
		t.Error("a VCS-stamped build was read as a module zip")
	}
	if strings.Contains(got, moduleZipNote) {
		t.Errorf("work-tree build printed the module-zip note:\n%s", got)
	}
}

// go install pkg@version: the module zip carries the version and nothing else.
// The commit is unrecoverable, so the report says why and names the install
// paths that do carry one, rather than reporting "none" and "unknown".
func TestReportModuleZipBuild(t *testing.T) {
	stash(t, devVersion, devCommit, devDate)
	applyBuildInfo(info("v0.9.9"))

	got := versionReport()
	for _, want := range []string{
		"drover v0.9.9\n",
		"  commit: " + unrecorded + "\n",
		"  built:  " + unrecorded + "\n",
		moduleZipNote,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q:\n%s", want, got)
		}
	}
	for _, zero := range []string{"commit: " + devCommit, "built:  " + devDate} {
		if strings.Contains(got, zero) {
			t.Errorf("module-zip report still prints %q:\n%s", zero, got)
		}
	}
}

// The unstamped shape has to stay distinguishable from the module zip: nothing
// named a reason, so nothing may claim one.
func TestReportUnstampedBuildClaimsNoReason(t *testing.T) {
	stash(t, devVersion, devCommit, devDate)
	applyBuildInfo(info("(devel)"))

	got := versionReport()
	if !strings.Contains(got, "  commit: "+devCommit+"\n") || !strings.Contains(got, "  built:  "+devDate+"\n") {
		t.Errorf("unstamped report changed:\n%s", got)
	}
	if strings.Contains(got, moduleZipNote) || strings.Contains(got, unrecorded) {
		t.Errorf("unstamped build blamed the module zip:\n%s", got)
	}
}
