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

// seal is the actor and session this process runs under, and the marker that
// says so when the environment does not. path is empty for the environment's.
type seal struct {
	actor, session string
	path           string
	marker         Marker
}

// ambient is the environment's seal when EnvActor is set, the nearest marker's
// otherwise. The root is config.Find's to read, from the directory it was asked
// about.
func ambient() seal {
	if a := os.Getenv(EnvActor); a != "" {
		if a == ActorOperator {
			return seal{actor: a}
		}
		return seal{actor: a, session: os.Getenv(EnvSession)}
	}
	wd, err := os.Getwd()
	if err != nil {
		return seal{}
	}
	if path, m, ok := FindMarker(wd); ok {
		if m.Actor == ActorOperator {
			return seal{actor: m.Actor}
		}
		return seal{actor: m.Actor, session: m.Session, path: path, marker: m}
	}
	return seal{}
}

// OwnSession reports whether this process is the session a drover launch
// started rather than something running inside one: its parent is the drover
// that wrote the marker in its tree, and the marker names the session the
// environment carries. A runner that is itself drover (the t3 bridge) needs
// the difference, because Run seals the session it starts and the bridge must
// still run as that session while refusing to be launched from inside one.
func OwnSession() bool {
	wd, err := os.Getwd()
	if err != nil {
		return false
	}
	_, m, ok := FindMarker(wd)
	if !ok || m.PID != os.Getppid() {
		return false
	}
	env := os.Getenv(EnvSession)
	return env != "" && env == m.Session
}

// SealedBy is the marker that seals this process, when a marker rather than the
// environment does. A refusal reads it to say what sealed a person's terminal,
// because "drover launched this session" is not true of one.
func SealedBy() (string, Marker, bool) {
	s := ambient()
	if s.path == "" {
		return "", Marker{}, false
	}
	return s.path, s.marker, true
}

// NestedLaunch is the refusal for a launch from this process, nil when nothing
// seals it against one.
//
// The environment refuses with ErrNested, because only drover sets it. A marker
// refuses only while its pid runs, and with ErrOccupied naming it: the process
// reading it may be a person in a tree a session holds, whom ErrNested would
// misdescribe. A marker whose pid is gone seals no launch, because the session
// it names has ended and claim replaces the file when the launch lands in the
// same tree.
func NestedLaunch() error {
	s := ambient()
	switch {
	case s.actor == "" || s.actor == ActorOperator:
		return nil
	case s.path == "":
		return ErrNested
	case !Alive(s.marker.PID):
		return nil
	}
	return occupied(s.path, s.marker,
		"; a person launching from inside that tree on purpose exports "+EnvActor+"="+ActorOperator+" first")
}

// occupied is ErrOccupied naming the marker and the session it belongs to.
func occupied(path string, m Marker, more string) error {
	return fmt.Errorf("%w: %s names session %s (%s, drover pid %d, started %s).\n"+
		"Wait for it to end, or remove the file if that process is not drover%s",
		ErrOccupied, path, blank(m.Session), m.Actor, m.PID, m.Started.Format(time.RFC3339), more)
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
			return nil, occupied(path, old, "")
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
