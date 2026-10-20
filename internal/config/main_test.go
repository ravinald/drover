package config

import (
	"os"
	"testing"

	"github.com/ravinald/drover/internal/runner"
)

// Load now layers a per-user config under every project, so a test run on a
// machine that has one reads settings no test wrote. Pointing XDG_CONFIG_HOME
// at an empty directory for the whole package makes the absence of a global
// config the default, and a test that wants one writes it there deliberately.
//
// runner.SessionEnv is cleared because Find answers from DROVER_ROOT before
// walking, so a suite run inside a session drive launched would resolve every
// fixture to the project that launched it.
func TestMain(m *testing.M) {
	for _, name := range runner.SessionEnv {
		if err := os.Unsetenv(name); err != nil {
			panic(err)
		}
	}
	dir, err := os.MkdirTemp("", "drover-xdg")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("XDG_CONFIG_HOME", dir); err != nil {
		panic(err)
	}
	code := m.Run()
	if err := os.RemoveAll(dir); err != nil {
		panic(err)
	}
	os.Exit(code)
}
