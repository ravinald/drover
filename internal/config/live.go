package config

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// LiveNone is the frontmatter value that opts an item out of [live] default.
const LiveNone = "none"

// DefaultLiveWait is how long a live check waits for another holding its lock
// before the item parks live_busy.
const DefaultLiveWait = 30 * time.Minute

// LiveCheck is one [live.<name>] table: the argv drover runs against a branch
// head after its review passes and before the merge gate, and the lock it
// shares with every other check naming the same one on this host.
type LiveCheck struct {
	Argv []string
	Lock string
}

// Live is the [live] table. Its own keys are default and wait; every table
// under it is a named check. One table holds both because a check is named the
// way a project names a [verify] command, and a project with one check should
// read it as [live.smoke] rather than under a second level nobody asked for.
type Live struct {
	// Default names the check an item that sets no live key runs.
	Default string
	// Wait bounds how long a check waits on its lock, as a duration.
	Wait   string
	Checks map[string]LiveCheck
}

// UnmarshalTOML decodes [live]: the two scalar keys, and each table as a check.
// Anything else is a refusal naming the key, as an unknown key is anywhere.
func (l *Live) UnmarshalTOML(data any) error {
	m, ok := data.(map[string]any)
	if !ok {
		return fmt.Errorf("live is a table: default, wait, and one [live.<name>] table per check")
	}
	for k, v := range m {
		switch k {
		case "default", "wait":
			s, ok := v.(string)
			if !ok {
				return fmt.Errorf("live.%s is a string", k)
			}
			if k == "default" {
				l.Default = s
			} else {
				l.Wait = s
			}
			continue
		}
		t, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("live.%s is neither default, wait nor a [live.%s] table naming a check", k, k)
		}
		var c LiveCheck
		for ck, cv := range t {
			switch ck {
			case "argv":
				list, ok := cv.([]any)
				if !ok {
					return fmt.Errorf("live.%s.argv is a list of strings", k)
				}
				for _, a := range list {
					s, ok := a.(string)
					if !ok {
						return fmt.Errorf("live.%s.argv is a list of strings", k)
					}
					c.Argv = append(c.Argv, s)
				}
			case "lock":
				s, ok := cv.(string)
				if !ok {
					return fmt.Errorf("live.%s.lock is a string", k)
				}
				c.Lock = s
			default:
				return fmt.Errorf("live.%s.%s is not a key a live check reads: argv and lock are", k, ck)
			}
		}
		if l.Checks == nil {
			l.Checks = map[string]LiveCheck{}
		}
		l.Checks[k] = c
	}
	return nil
}

// MarshalTOML renders [live] as one inline table, every check nested in it,
// so a printed config carries each key the project declared. The empty table
// is what [live] means when nothing declares it.
func (l Live) MarshalTOML() ([]byte, error) {
	var parts []string
	if l.Default != "" {
		parts = append(parts, "default = "+strconv.Quote(l.Default))
	}
	if l.Wait != "" {
		parts = append(parts, "wait = "+strconv.Quote(l.Wait))
	}
	for _, n := range l.Names() {
		c := l.Checks[n]
		argv := make([]string, len(c.Argv))
		for i, a := range c.Argv {
			argv[i] = strconv.Quote(a)
		}
		check := []string{"argv = [" + strings.Join(argv, ", ") + "]"}
		if c.Lock != "" {
			check = append(check, "lock = "+strconv.Quote(c.Lock))
		}
		parts = append(parts, Quote(n)+" = { "+strings.Join(check, ", ")+" }")
	}
	if len(parts) == 0 {
		return []byte("{}"), nil
	}
	return []byte("{ " + strings.Join(parts, ", ") + " }"), nil
}

// Names is every declared check, sorted.
func (l Live) Names() []string {
	out := make([]string, 0, len(l.Checks))
	for n := range l.Checks {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// LiveWait resolves [live] wait.
func (c *Config) LiveWait() time.Duration {
	if d, err := time.ParseDuration(c.Live.Wait); err == nil && d > 0 {
		return d
	}
	return DefaultLiveWait
}

// LiveFor is the check an item runs: the one its live key names, [live]
// default when it names none, and nothing for live: none or a project with no
// default. An unknown name is reported, since an item meaning to be checked
// would otherwise merge unchecked.
func (c *Config) LiveFor(key string) (name string, check LiveCheck, err error) {
	switch key {
	case LiveNone:
		return "", LiveCheck{}, nil
	case "":
		key = c.Live.Default
		if key == "" {
			return "", LiveCheck{}, nil
		}
	}
	check, ok := c.Live.Checks[key]
	if !ok {
		return "", LiveCheck{}, fmt.Errorf("live check %q is not declared: [live] declares %v", key, c.Live.Names())
	}
	return key, check, nil
}

var lockName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func (c *Config) validateLive(path string) error {
	if c.Live.Wait != "" {
		if d, err := time.ParseDuration(c.Live.Wait); err != nil || d <= 0 {
			return fmt.Errorf("%s: live.wait %q is not a positive duration (30m, 2h)", path, c.Live.Wait)
		}
	}
	if d := c.Live.Default; d != "" && d != LiveNone {
		if _, ok := c.Live.Checks[d]; !ok {
			return fmt.Errorf("%s: live.default names %q, and no [live.%s] table declares it", path, d, d)
		}
	}
	for _, n := range c.Live.Names() {
		ch := c.Live.Checks[n]
		if len(ch.Argv) == 0 {
			return fmt.Errorf("%s: [live.%s] has no argv, so there is nothing to run", path, n)
		}
		if ch.Lock != "" && !lockName.MatchString(ch.Lock) {
			return fmt.Errorf("%s: live.%s.lock %q names a file under the lock directory, so it is letters, digits, '.', '_' and '-' only", path, n, ch.Lock)
		}
	}
	return nil
}
