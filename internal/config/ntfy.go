package config

import (
	"fmt"
	"net"
	"net/url"
	"slices"
	"sort"
	"strings"
)

// SuperviseNtfyKey is the [supervise.ntfy] table, the third [supervise] key
// that is a setting rather than an action.
const SuperviseNtfyKey = "ntfy"

// Ntfy is [supervise.ntfy] as a drive sends it, defaults filled in.
type Ntfy struct {
	// URL is the topic URL each event is POSTed to.
	URL string
	// TokenFile is where the bearer token is read from at send time, resolved,
	// and empty for a topic that takes no token. The token itself is never
	// held here, so nothing that prints a config can print it.
	TokenFile string
	// Events are the drive event names that send.
	Events []string
	// Priority is the ntfy priority every send carries.
	Priority string
}

// NtfyEvents are the drive events [supervise.ntfy] events may name.
func NtfyEvents() []string {
	return []string{"needs_operator", "needs_supervisor", "parked", "exit"}
}

// NtfyPriorities are ntfy's own priority names, lowest first.
func NtfyPriorities() []string { return []string{"min", "low", "default", "high", "urgent"} }

// The [supervise.ntfy] keys, in the order an error lists them.
var ntfyKeys = []string{"url", "token_file", "events", "priority"}

// Ntfy is [supervise.ntfy] with its defaults, and false when no layer wrote
// the table. It reads the raw table, so it answers for an Inspection as readily
// as for a Config Load returned, and a value Load would refuse reads as unset.
func (c *Config) Ntfy() (Ntfy, bool) {
	raw, ok := c.Supervise[SuperviseNtfyKey].(map[string]any)
	if !ok {
		return Ntfy{}, false
	}
	n := Ntfy{Events: []string{"needs_operator"}, Priority: "high"}
	n.URL, _ = raw["url"].(string)
	if s, _ := raw["token_file"].(string); s != "" {
		n.TokenFile = c.Abs(s)
	}
	if events, ok := raw["events"].([]any); ok {
		n.Events = nil
		for _, e := range events {
			if s, ok := e.(string); ok {
				n.Events = append(n.Events, s)
			}
		}
	}
	if s, ok := raw["priority"].(string); ok {
		n.Priority = s
	}
	return n, true
}

// layerNtfy fills the project's [supervise.ntfy] from the per-user table
// under it, key by key. The decoder replaces a table held in a map wholesale,
// so without this a project setting only events would lose the url and token
// the per-user file names, and its drive would refuse to load.
func (c *Config) layerNtfy(under any) {
	base, ok := under.(map[string]any)
	if !ok {
		return
	}
	over, ok := c.Supervise[SuperviseNtfyKey].(map[string]any)
	if !ok {
		return
	}
	for k, v := range base {
		if _, set := over[k]; !set {
			over[k] = v
		}
	}
}

// validateNtfy refuses a [supervise.ntfy] table a drive would have to guess
// about. Each error names the file that wrote the key, which is the per-user
// file as often as the project's.
func (c *Config) validateNtfy(project string, v any) error {
	at := func(key string) string { return c.writer("supervise.ntfy."+key, project) }
	t, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: supervise.ntfy is %v: write it as a table, [supervise.ntfy], with a url", c.writer("supervise.ntfy", project), v)
	}
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		val := t[key]
		switch key {
		case "url":
			s, _ := val.(string)
			if err := checkNtfyURL(s); err != nil {
				return fmt.Errorf("%s: supervise.ntfy.url is %q: %v", at(key), s, err)
			}
		case "token_file":
			s, _ := val.(string)
			if !strings.HasPrefix(s, "/") && !strings.HasPrefix(s, "~/") {
				return fmt.Errorf("%s: supervise.ntfy.token_file is %v: give a path starting with / or ~/, "+
					"since a token kept inside the project is one commit away from published", at(key), val)
			}
		case "events":
			raw, ok := val.([]any)
			if !ok || len(raw) == 0 {
				return fmt.Errorf("%s: supervise.ntfy.events is %v: name at least one of %s",
					at(key), val, strings.Join(NtfyEvents(), ", "))
			}
			for _, e := range raw {
				if s, ok := e.(string); !ok || !slices.Contains(NtfyEvents(), s) {
					return fmt.Errorf("%s: supervise.ntfy.events names %v, which is not an event it sends (known: %s)",
						at(key), e, strings.Join(NtfyEvents(), ", "))
				}
			}
		case "priority":
			if s, ok := val.(string); !ok || !slices.Contains(NtfyPriorities(), s) {
				return fmt.Errorf("%s: supervise.ntfy.priority is %v (known: %s)",
					at(key), val, strings.Join(NtfyPriorities(), ", "))
			}
		default:
			return fmt.Errorf("%s: supervise.ntfy.%s is not a key drover reads (known: %s)",
				at(key), key, strings.Join(ntfyKeys, ", "))
		}
	}
	if _, ok := t["url"]; !ok {
		return fmt.Errorf("%s: supervise.ntfy has no url: give the topic URL, such as https://ntfy.sh/<topic>",
			c.writer("supervise.ntfy", project))
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
