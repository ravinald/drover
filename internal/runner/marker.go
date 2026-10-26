package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// The session marker is the seal's second signal, for a session drover did not
// exec. EnvActor reaches only a child drover starts itself: a thread another
// program opens in the same tree (a sandbox started over MCP, an agent a person
// opens by hand in a drive's worktree) runs under that program's environment and
// carries none of it. So Run also leaves the three values in the directory the
// session works in, and Launched, Supervising and Session read them from there
// when the environment says nothing.
//
// It is the same guardrail as the variable and no more: deleting the file or
// exporting ActorOperator crosses it, and both are deliberate acts.
const (
	MarkerDir  = ".drover"
	MarkerName = "session"
)

// ActorOperator is the value a person exports to steer from inside a sealed
// tree. It overrides a marker and seals nothing; drover never writes it.
const ActorOperator = "operator"

// ErrOccupied is the refusal for a launch into a directory whose marker names a
// session still running. cmd maps it to exit 3.
var ErrOccupied = errors.New("another session holds this tree")

// Marker is the file's whole contents. PID is the drover process that wrote it,
// the one that removes it, so a marker whose PID is gone was left behind.
type Marker struct {
	Actor   string    `json:"actor"`
	Session string    `json:"session"`
	Root    string    `json:"root"`
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
}

// MarkerPath is where the marker for a session working in dir lives.
func MarkerPath(dir string) string { return filepath.Join(dir, MarkerDir, MarkerName) }

// ReadMarker reads one marker file. A file that will not parse still seals,
// as an agent with no session: a guardrail that opens on a malformed file
// opens whenever someone writes one carelessly.
func ReadMarker(path string) (Marker, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- a path drover derived from its own working directory
	if err != nil {
		return Marker{}, err
	}
	var m Marker
	if err := json.Unmarshal(b, &m); err != nil {
		return Marker{Actor: ActorAgent}, fmt.Errorf("%s: %w", path, err)
	}
	if m.Actor == "" {
		m.Actor = ActorAgent
	}
	return m, nil
}

// FindMarker walks up from dir to the nearest marker, stopping at the first
// directory holding a marker or a .drover.toml. The stop at a project root is
// what keeps a marker in a parent repository from sealing a sibling project
// checked out beneath it.
func FindMarker(dir string) (string, Marker, bool) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", Marker{}, false
	}
	for {
		path := MarkerPath(abs)
		if _, err := os.Stat(path); err == nil {
			m, _ := ReadMarker(path)
			return path, m, true
		}
		if _, err := os.Stat(filepath.Join(abs, ".drover.toml")); err == nil {
			return "", Marker{}, false
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", Marker{}, false
		}
		abs = parent
	}
}

// Alive reports whether the process a marker names is still running. A reused
// PID reads as alive, which errs toward refusing a launch rather than toward
// two sessions in one tree.
func Alive(pid int) bool { return pid > 0 && processAlive(pid) }

// ambient is the actor and session this process runs under: the
// environment's when EnvActor is set, the nearest marker's otherwise. The root
// is config.Find's to read, from the directory it was asked about.
func ambient() (actor, session string) {
	if a := os.Getenv(EnvActor); a != "" {
		if a == ActorOperator {
			return a, ""
		}
		return a, os.Getenv(EnvSession)
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", ""
	}
	if _, m, ok := FindMarker(wd); ok {
		if m.Actor == ActorOperator {
			return m.Actor, ""
		}
		return m.Actor, m.Session
	}
	return "", ""
}

// claim writes the marker for a launch into dir and returns what removes it.
// A marker already there whose PID is running refuses; one whose PID is gone
// was left by a drover that died before it could clean up, and is replaced.
//
// The directory gets a .gitignore naming only the marker and itself when it has
// none, so an agent staging everything does not commit the seal with its work.
// A project keeping its backlog under .drover/ loses nothing to it.
func claim(dir string, m Marker) (func(), error) {
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		dir = wd
	}
	path := MarkerPath(dir)
	mdir := filepath.Dir(path)
	_, statErr := os.Stat(mdir)
	madeDir := errors.Is(statErr, fs.ErrNotExist)
	if err := os.MkdirAll(mdir, 0o750); err != nil {
		return nil, fmt.Errorf("create %s: %w", mdir, err)
	}
	ignore := filepath.Join(mdir, ".gitignore")
	madeIgnore := false
	if _, err := os.Stat(ignore); errors.Is(err, fs.ErrNotExist) {
		if err := os.WriteFile(ignore, []byte("/"+MarkerName+"\n/.gitignore\n"), 0o600); err == nil {
			madeIgnore = true
		}
	}
	undo := func() {
		if madeIgnore {
			_ = os.Remove(ignore)
		}
		if madeDir {
			_ = os.Remove(mdir)
		}
	}

	b, err := json.Marshal(m)
	if err != nil {
		undo()
		return nil, err
	}
	b = append(b, '\n')
	for attempt := 0; ; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600) // #nosec G304 -- path is dir/.drover/session
		if err == nil {
			_, werr := f.Write(b)
			cerr := f.Close()
			if werr != nil || cerr != nil {
				_ = os.Remove(path)
				undo()
				return nil, fmt.Errorf("write %s: %w", path, errors.Join(werr, cerr))
			}
			break
		}
		if !errors.Is(err, fs.ErrExist) || attempt > 0 {
			undo()
			return nil, fmt.Errorf("write %s: %w", path, err)
		}
		old, _ := ReadMarker(path)
		if Alive(old.PID) {
			undo()
			return nil, fmt.Errorf("%w: %s names session %s (%s, drover pid %d, started %s).\n"+
				"Wait for it to end, or remove the file if that process is not drover",
				ErrOccupied, path, blank(old.Session), old.Actor, old.PID, old.Started.Format(time.RFC3339))
		}
		fmt.Fprintf(os.Stderr, "warn: replacing stale session marker %s: session %s, pid %d is not running\n",
			path, blank(old.Session), old.PID)
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			undo()
			return nil, fmt.Errorf("remove stale %s: %w", path, err)
		}
	}

	return func() {
		// Only the marker this launch wrote. One that is not ours replaced it,
		// and removing it would unseal a session somebody else started.
		if cur, err := ReadMarker(path); err == nil && cur.Session == m.Session && cur.PID == m.PID {
			_ = os.Remove(path)
		}
		undo()
	}, nil
}

func blank(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
