package cmd

import (
	"fmt"
	"regexp"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// The values -ldflags overwrites. Anything still holding one of these was built
// without the Makefile, so Go's own build info answers instead.
const (
	devVersion = "dev"
	devCommit  = "none"
	devDate    = "unknown"
)

// What a module-zip build reports in place of devCommit and devDate. The module
// proxy serves a zip with no .git in it, so `go install pkg@version` produces a
// binary whose build info names the module version and carries no VCS settings
// at all. Nothing in the binary can recover them, which makes this a property of
// the install path rather than a defect a reader should go hunting for.
const unrecorded = "unrecorded (module zip)"

const moduleZipNote = `A module zip carries no commit and no build date, so a go install pkg@version
binary cannot report either. A release binary, or a clone built with make
install, reports both: https://github.com/ravinald/drover/releases`

// Build metadata, set via -ldflags at build time.
var (
	version = devVersion
	commit  = devCommit
	date    = devDate
)

// fromModuleZip records that applyBuildInfo found a module version and no
// revision behind it, which is the one shape whose missing commit has a name.
var fromModuleZip bool

// releaseTag matches a version that names a tag and nothing else. A build from
// a work tree carries git describe's suffix (v0.1.0-35-g8b16d75-dirty) and a
// module-proxy build carries a pseudo-version, and neither one is a ref the
// repository will resolve.
var releaseTag = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// docsRef is the git ref the generated instructions link their documentation
// at. A tagged build pins its own tag, so an agent reads the surface its binary
// actually has rather than whatever main has moved on to. Everything else
// resolves to main, which is the closest true answer for a binary that is not
// standing on a release.
func docsRef() string {
	if releaseTag.MatchString(version) {
		return version
	}
	return "main"
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version, commit, build date.",
	Long: `The version a bug report has to carry. Help text, the generated agent
instructions and the config schema all move with the binary, so a report that
does not name the build describes a surface nobody can reproduce.

A binary installed from the module proxy cannot name its commit and says which
install path can, because "none" reads as a broken binary rather than as the
module zip it came from.`,
	Example: `  drover version
  drover version | head -1          # "drover v0.1.0"`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		fmt.Print(versionReport())
		return nil
	},
}

// versionLine is the first line of `drover version` and the whole of
// `drover --version`; one question gets one answer.
func versionLine() string { return "drover " + version }

// versionReport is everything `drover version` prints. The reason travels
// inline with the field it explains, because a bug report quotes the first
// lines of this and drops the rest.
func versionReport() string {
	report := fmt.Sprintf("%s\n  commit: %s\n  built:  %s\n  go:     %s %s/%s\n",
		versionLine(), commit, date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	if fromModuleZip {
		report += "\n" + moduleZipNote + "\n"
	}
	return report
}

// applyBuildInfo fills in whatever -ldflags did not set. A `go install
// pkg@version` build carries the module version in Main.Version; a build from a
// work tree carries the commit and its timestamp in the vcs settings.
func applyBuildInfo(info *debug.BuildInfo) {
	if info == nil {
		return
	}
	if version == devVersion && moduleVersion(info.Main.Version) {
		version = info.Main.Version
	}
	for _, s := range info.Settings {
		switch {
		case s.Key == "vcs.revision" && commit == devCommit && s.Value != "":
			commit = s.Value
		case s.Key == "vcs.time" && date == devDate && s.Value != "":
			date = s.Value
		}
	}
	// A commit still at its default here means neither -ldflags nor a vcs
	// setting supplied one. Paired with a real module version that is a
	// module-zip install and nothing else: an unstamped work-tree build would
	// have carried vcs.revision, and a build with no VCS at all reports "dev".
	if commit == devCommit && moduleVersion(info.Main.Version) {
		commit = unrecorded
		if date == devDate {
			date = unrecorded
		}
		fromModuleZip = true
	}
}

// moduleVersion reports whether the main module's version says anything. An
// unstamped main module reports "(devel)", which says less than "dev" already
// does.
func moduleVersion(v string) bool { return v != "" && v != "(devel)" }

func init() {
	if info, ok := debug.ReadBuildInfo(); ok {
		applyBuildInfo(info)
	}
	root.Version = version
	root.SetVersionTemplate(versionLine() + "\n")
	root.AddCommand(versionCmd)
}
