package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"time"

	"github.com/BurntSushi/toml"
)

// T3 is where the `drover t3` bridge finds a T3 Code server and what it reads on
// the host beside it. Every key but url resolves an undeclared value where it is
// read, through the accessors below, so `drover config` can tell a project that
// chose the default from one that never said.
type T3 struct {
	// URL is the server's MCP endpoint. HTTPS, or plain HTTP to localhost only:
	// the bearer token rides every request, and T3 Code itself refuses outside
	// agents on any other address.
	URL string `toml:"url"`
	// TokenFile holds the OAuth grant `drover t3 login` wrote.
	TokenFile string `toml:"token_file"`
	// Project is the T3 Code project title a thread is launched in, the drover
	// project's own name when unset.
	Project string `toml:"project"`
	// Poll is the bridge's step between reads of a running thread, as a Go
	// duration.
	Poll string `toml:"poll"`
	// ProviderLogs is the directory T3 Code writes each thread's provider event
	// log to. It is the only place a T3 thread's token counts, cost and
	// rate-limit reading can be read from, and it is T3's internal format.
	ProviderLogs string `toml:"provider_logs"`
	// TestedVersion is the T3 Code release the bridge was last verified
	// against. `drover check` names a server reporting any other.
	TestedVersion string `toml:"tested_version"`
}

// The [t3] defaults. T3TestedVersion is the release this build's bridge was
// verified against, on the host the provider log format was read from.
const (
	DefaultT3TokenFile    = "~/.config/drover/t3-token"
	DefaultT3Poll         = 2 * time.Second
	DefaultT3ProviderLogs = "~/.t3/userdata/logs/provider"
	T3TestedVersion       = "0.1.2"
)

// T3TokenFile is [t3] token_file resolved to a path.
func (c *Config) T3TokenFile() string {
	if c.T3.TokenFile == "" {
		return c.Abs(DefaultT3TokenFile)
	}
	return c.Abs(c.T3.TokenFile)
}

// T3Project is the T3 Code project title, the drover project's name when
// [t3] project is unset.
func (c *Config) T3Project() string {
	if c.T3.Project != "" {
		return c.T3.Project
	}
	return c.Project
}

// T3Poll is [t3] poll, or its default. validate has refused anything that does
// not parse to a positive duration.
func (c *Config) T3Poll() time.Duration {
	if d, err := time.ParseDuration(c.T3.Poll); err == nil && d > 0 {
		return d
	}
	return DefaultT3Poll
}

// T3ProviderLogs is [t3] provider_logs resolved to a path.
func (c *Config) T3ProviderLogs() string {
	if c.T3.ProviderLogs == "" {
		return c.Abs(DefaultT3ProviderLogs)
	}
	return c.Abs(c.T3.ProviderLogs)
}

// T3TestedVersion is [t3] tested_version, or the release this build records.
func (c *Config) T3TestedVersion() string {
	if c.T3.TestedVersion != "" {
		return c.T3.TestedVersion
	}
	return T3TestedVersion
}

func (c *Config) validateT3(path string) error {
	if err := CheckT3URL(c.T3.URL); err != nil {
		return fmt.Errorf("%s: t3.url %w", path, err)
	}
	if p := c.T3.Poll; p != "" {
		if d, err := time.ParseDuration(p); err != nil || d <= 0 {
			return fmt.Errorf("%s: t3.poll is %q: give a positive duration such as \"2s\"", path, p)
		}
	}
	return nil
}

// CheckT3URL refuses an MCP endpoint the bridge would send a bearer token to in
// the clear. Empty passes: an unset url is reported by the command that needs
// one, not by every command that loads the config.
func CheckT3URL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("is %q, which is not an absolute URL: give the server's /mcp address, such as https://host.example.ts.net/mcp", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return fmt.Errorf("is %q: the bridge sends its token on every request, so it takes https, or http only to localhost", raw)
}

// LoadT3 is the config a `drover t3` command resolves [t3] from: the project's
// layered config where dir sits inside a project, and the per-user file alone
// where it does not, because signing in to a server is not about any one
// project. The second form carries no project name, so T3Project is empty
// there unless the file sets one.
func LoadT3(dir string) (*Config, error) {
	if _, err := Find(dir); err == nil {
		return Load(dir)
	}
	c := &Config{}
	if g := GlobalPath(); g != "" {
		md, err := toml.DecodeFile(g, c)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return nil, fmt.Errorf("read %s: %w", g, err)
		default:
			c.sources = append(c.sources, g)
			c.absorbDeclared(md, g, false)
		}
	}
	c.applyDefaults()
	if err := c.validateT3(GlobalPath()); err != nil {
		return nil, err
	}
	return c, nil
}
