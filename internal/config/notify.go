package config

import (
	"fmt"
	"maps"
	"net"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"
)

// Notify is the [notify] table as a file writes it. Destinations stays an open
// table of untyped values, because each destination's keys depend on its type
// and an error has to name the destination and the key, which a decoder
// refusing a struct field cannot. Read it through Notifications.
type Notify struct {
	// Coalesce is a Go duration, "0s" or unset for off.
	Coalesce string `toml:"coalesce"`
	// QuietHours is "HH:MM-HH:MM" in local time, unset for none.
	QuietHours   string                    `toml:"quiet_hours"`
	Destinations map[string]map[string]any `toml:"destinations"`
}

// The destination types.
const (
	DestNtfy    = "ntfy"
	DestCommand = "command"
)

// EventExitUndone is the routing key for an exit that leaves an item parked,
// skipped or held. It is not an event the drive emits: it lets a destination
// hear about a drive that left work undone louder than one that did not.
const EventExitUndone = "exit_undone"

// NotifyEvents are the names a destination's events table may route: every
// event a drive emits, and exit_undone.
func NotifyEvents() []string {
	return []string{
		"started", "queued", "dispatched", "preflight", "stage", "check", "parked", "filed",
		"needs_operator", "needs_supervisor", "merged", "paused", "resumed", "config_changed", "usage_warning",
		"exit", EventExitUndone,
	}
}

// NtfyPriorities are ntfy's own priority names, lowest first, which is the
// order quiet hours compare them in.
func NtfyPriorities() []string { return []string{"min", "low", "default", "high", "urgent"} }

// PriorityRank orders a priority name among NtfyPriorities, -1 for none.
func PriorityRank(p string) int { return slices.Index(NtfyPriorities(), p) }

// NotifyDestinationKeys are the keys a [notify.destinations.<name>] table of
// each type takes, in the order an error lists them.
func NotifyDestinationKeys(kind string) []string {
	switch kind {
	case DestNtfy:
		return []string{"type", "events", "url", "token_file", "repeat", "repeat_max"}
	case DestCommand:
		return []string{"type", "events", "argv", "repeat", "repeat_max"}
	}
	return []string{"type", "events"}
}

// MinRepeat is the shortest repeat a destination takes. A needs_operator
// resent more often than this is noise an operator learns to swipe away, which
// is the miss the reminder exists to prevent.
const MinRepeat = 5 * time.Minute

// DefaultRepeatMax is how many reminders a destination sends for one
// needs_operator when repeat is set and repeat_max is not: half an hour of
// reminders at the shortest repeat.
const DefaultRepeatMax = 6

// Destination is one [notify.destinations.<name>] table as a drive sends to it.
type Destination struct {
	Name string
	Type string
	// Events maps each event the destination takes to its ntfy priority.
	Events map[string]string
	// URL and TokenFile are an ntfy destination's. TokenFile is resolved, and
	// the token itself is never held here, so nothing that prints a config can
	// print it.
	URL       string
	TokenFile string
	// Argv is a command destination's.
	Argv []string
	// Repeat is how often an unanswered needs_operator is sent again, 0 for
	// never, and RepeatMax how many times at most.
	Repeat    time.Duration
	RepeatMax int
}

// Priority is the priority an event is sent to d at, false when d does not
// take it. exit_undone defaults to high wherever exit is routed, because an
// exit that left work behind is the one an operator away from the terminal
// most needs to hear.
func (d Destination) Priority(event string) (string, bool) {
	if p, ok := d.Events[event]; ok {
		return p, true
	}
	if event == EventExitUndone {
		if _, ok := d.Events["exit"]; ok {
			return "high", true
		}
	}
	return "", false
}

// QuietHours is a daily window in local time, in minutes past midnight. End
// below Start is a window crossing midnight.
type QuietHours struct{ Start, End int }

// Contains reports whether t falls inside the window.
func (q QuietHours) Contains(t time.Time) bool {
	m := t.Hour()*60 + t.Minute()
	if q.Start <= q.End {
		return m >= q.Start && m < q.End
	}
	return m >= q.Start || m < q.End
}

// Until is the next end of the window after t.
func (q QuietHours) Until(t time.Time) time.Time {
	end := time.Date(t.Year(), t.Month(), t.Day(), q.End/60, q.End%60, 0, 0, t.Location())
	if !end.After(t) {
		end = end.AddDate(0, 0, 1)
	}
	return end
}

// Routing is [notify] resolved: what a drive sends where.
type Routing struct {
	Coalesce time.Duration
	// Quiet is nil where no quiet_hours is set.
	Quiet        *QuietHours
	Destinations []Destination
}

// Notifications is [notify] resolved, destinations ordered by name. It reads
// the raw table, so a value Load would refuse reads as unset rather than
// failing a caller that only wants to print the config.
func (c *Config) Notifications() Routing {
	var r Routing
	if d, err := time.ParseDuration(c.Notify.Coalesce); err == nil && d > 0 {
		r.Coalesce = d
	}
	if q, err := parseQuietHours(c.Notify.QuietHours); err == nil && c.Notify.QuietHours != "" {
		r.Quiet = &q
	}
	for _, name := range slices.Sorted(maps.Keys(c.Notify.Destinations)) {
		raw := c.Notify.Destinations[name]
		d := Destination{Name: name, Events: map[string]string{}}
		d.Type, _ = raw["type"].(string)
		if events, ok := raw["events"].(map[string]any); ok {
			for e, p := range events {
				if s, ok := p.(string); ok {
					d.Events[e] = s
				}
			}
		}
		d.URL, _ = raw["url"].(string)
		if s, _ := raw["token_file"].(string); s != "" {
			d.TokenFile = c.Abs(s)
		}
		if argv, ok := raw["argv"].([]any); ok {
			for _, a := range argv {
				if s, ok := a.(string); ok {
					d.Argv = append(d.Argv, s)
				}
			}
		}
		if s, _ := raw["repeat"].(string); s != "" {
			if rep, err := time.ParseDuration(s); err == nil && rep >= MinRepeat {
				d.Repeat = rep
			}
		}
		d.RepeatMax = DefaultRepeatMax
		if m, ok := raw["repeat_max"].(int64); ok && m > 0 {
			d.RepeatMax = int(m)
		}
		r.Destinations = append(r.Destinations, d)
	}
	return r
}

// parseQuietHours reads "HH:MM-HH:MM". Equal ends are refused: a window of no
// length and one of the whole day read the same, and neither is what an
// operator typing it means.
func parseQuietHours(s string) (QuietHours, error) {
	from, to, ok := strings.Cut(s, "-")
	if !ok {
		return QuietHours{}, fmt.Errorf("write it as HH:MM-HH:MM in local time, such as 22:00-07:00")
	}
	start, err := time.Parse("15:04", strings.TrimSpace(from))
	if err != nil {
		return QuietHours{}, fmt.Errorf("%q is not a 24-hour HH:MM time", from)
	}
	end, err := time.Parse("15:04", strings.TrimSpace(to))
	if err != nil {
		return QuietHours{}, fmt.Errorf("%q is not a 24-hour HH:MM time", to)
	}
	q := QuietHours{Start: start.Hour()*60 + start.Minute(), End: end.Hour()*60 + end.Minute()}
	if q.Start == q.End {
		return QuietHours{}, fmt.Errorf("it starts and ends at the same minute")
	}
	return q, nil
}

// layerNotify fills each project destination from the per-user destination of
// the same name, key by key. The decoder replaces a table held in a map
// wholesale, so without this a project setting only events would lose the url
// and token the per-user file names, and its drive would refuse to load.
// Destinations of different names both stand on their own.
func (c *Config) layerNotify(under map[string]map[string]any) {
	for name, base := range under {
		over, ok := c.Notify.Destinations[name]
		if !ok {
			continue
		}
		for k, v := range base {
			if _, set := over[k]; !set {
				over[k] = v
			}
		}
	}
}

// validateNotify refuses a [notify] table a drive would have to guess about.
// Each error names the file that wrote the key, which is the per-user file as
// often as the project's, and the destination it sits in.
func (c *Config) validateNotify(project string) error {
	if s := c.Notify.Coalesce; s != "" {
		if d, err := time.ParseDuration(s); err != nil || d < 0 {
			return fmt.Errorf("%s: notify.coalesce is %q: give a duration such as \"2m\", or \"0s\" for off",
				c.writer("notify.coalesce", project), s)
		}
	}
	if s := c.Notify.QuietHours; s != "" {
		if _, err := parseQuietHours(s); err != nil {
			return fmt.Errorf("%s: notify.quiet_hours is %q: %v", c.writer("notify.quiet_hours", project), s, err)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.Notify.Destinations)) {
		if err := c.validateDestination(project, name, c.Notify.Destinations[name]); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) validateDestination(project, name string, t map[string]any) error {
	table := "notify.destinations." + Quote(name)
	at := func(key string) string { return c.writer(table+"."+key, project) }
	kind, _ := t["type"].(string)
	switch kind {
	case DestNtfy, DestCommand:
	case "":
		return fmt.Errorf("%s: %s has no type: give type = %q or %q", c.writer(table, project), table, DestNtfy, DestCommand)
	default:
		return fmt.Errorf("%s: %s.type is %v (known: %s, %s)", at("type"), table, t["type"], DestNtfy, DestCommand)
	}
	known := NotifyDestinationKeys(kind)
	for _, key := range slices.Sorted(maps.Keys(t)) {
		val := t[key]
		if !slices.Contains(known, key) {
			return fmt.Errorf("%s: %s.%s is not a key a %s destination takes (known: %s)",
				at(key), table, key, kind, strings.Join(known, ", "))
		}
		switch key {
		case "events":
			if err := checkEvents(table, val); err != nil {
				return fmt.Errorf("%s: %v", at(key), err)
			}
		case "url":
			s, _ := val.(string)
			if err := checkNtfyURL(s); err != nil {
				return fmt.Errorf("%s: %s.url is %q: %v", at(key), table, s, err)
			}
		case "token_file":
			s, _ := val.(string)
			if !strings.HasPrefix(s, "/") && !strings.HasPrefix(s, "~/") {
				return fmt.Errorf("%s: %s.token_file is %v: give a path starting with / or ~/, "+
					"since a token kept inside the project is one commit away from published", at(key), table, val)
			}
		case "repeat":
			s, _ := val.(string)
			if d, err := time.ParseDuration(s); err != nil || d < MinRepeat {
				return fmt.Errorf("%s: %s.repeat is %v: give a duration of at least %s, such as \"10m\", or leave it unset for no reminders",
					at(key), table, val, MinRepeat)
			}
		case "repeat_max":
			if m, ok := val.(int64); !ok || m < 1 {
				return fmt.Errorf("%s: %s.repeat_max is %v: give a positive whole number of reminders, such as 6", at(key), table, val)
			}
		case "argv":
			raw, ok := val.([]any)
			if !ok || len(raw) == 0 {
				return fmt.Errorf("%s: %s.argv is %v: give an argv, such as [\"notify-send\", \"drover\"]", at(key), table, val)
			}
			for _, a := range raw {
				if s, ok := a.(string); !ok || s == "" {
					return fmt.Errorf("%s: %s.argv holds %v, which is not a non-empty string", at(key), table, a)
				}
			}
		}
	}
	required := []string{"events", "url"}
	if kind == DestCommand {
		required = []string{"events", "argv"}
	}
	for _, key := range required {
		if _, ok := t[key]; !ok {
			return fmt.Errorf("%s: %s has no %s: a %s destination needs %s",
				c.writer(table, project), table, key, kind, strings.Join(required, " and "))
		}
	}
	return nil
}

// checkEvents holds an events table to event names a drive emits and to
// ntfy's priorities.
func checkEvents(table string, val any) error {
	events, ok := val.(map[string]any)
	if !ok || len(events) == 0 {
		return fmt.Errorf("%s.events is %v: map at least one event to a priority, such as { needs_operator = \"high\" }", table, val)
	}
	names := make([]string, 0, len(events))
	for e := range events {
		names = append(names, e)
	}
	sort.Strings(names)
	for _, e := range names {
		if !slices.Contains(NotifyEvents(), e) {
			return fmt.Errorf("%s.events.%s is not an event a drive emits (known: %s)", table, e, strings.Join(NotifyEvents(), ", "))
		}
		if p, ok := events[e].(string); !ok || PriorityRank(p) < 0 {
			return fmt.Errorf("%s.events.%s is %v (known: %s)", table, e, events[e], strings.Join(NtfyPriorities(), ", "))
		}
	}
	return nil
}

// checkNtfyURL holds the topic URL to https, which is what keeps the bearer
// token off the wire in the clear. A loopback host is the one exception, since
// the token never leaves the machine.
func checkNtfyURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Host == "" {
		return fmt.Errorf("it names no host: give the topic URL, such as https://ntfy.sh/<topic>")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !loopback(u.Hostname()) {
			return fmt.Errorf("http:// sends the token in the clear, and is allowed only to a loopback host: use https://")
		}
	default:
		return fmt.Errorf("the scheme must be https://")
	}
	if u.User != nil {
		return fmt.Errorf("it carries credentials, which every report naming the url would print: put the token in token_file")
	}
	if strings.Trim(u.Path, "/") == "" {
		return fmt.Errorf("it names no topic: end the path with one, such as https://ntfy.sh/<topic>")
	}
	return nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// writer is the file that last wrote the dotted key or anything beneath it,
// and fallback where no layer did.
func (c *Config) writer(key, fallback string) string {
	for i := len(c.layers) - 1; i >= 0; i-- {
		if c.layers[i].declares(key) {
			return c.layers[i].path
		}
	}
	return fallback
}

// MigrateNotify is the command that rewrites a removed notifier key into a
// [notify] destination, as a refusal names it.
const MigrateNotify = "scripts/migrate-notify.sh"

// removedNotifier refuses [supervise] notify and [supervise.ntfy], which
// [notify] replaced. No layer reads them, so a config carrying one would
// otherwise drop the operator's notifications without a word.
func removedNotifier(path, key string) error {
	return fmt.Errorf("%s: supervise.%s was removed: [notify] destinations replace it. "+
		"Run %s %s (from the drover repository) to rewrite it in place", path, key, MigrateNotify, path)
}
