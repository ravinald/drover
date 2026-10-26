package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/runlog"
	"github.com/ravinald/drover/internal/runner"
	"github.com/ravinald/drover/internal/t3"
	"github.com/spf13/cobra"
)

var t3Cmd = &cobra.Command{
	Use:   "t3",
	Short: "Run stages as T3 Code threads, over the server's MCP endpoint",
	Long: `drover t3 is a runner bridge to a T3 Code server. A [runners] entry whose
headless argv is ` + "`drover t3 run ...`" + ` launches each stage as a T3 Code thread,
follows it to the end, and prints a stream-json log drover reads like any other
runner's: the thread's items under a t3 schema, and the Claude SDK's own
rate_limit_event and result lines, read from T3's provider event log on the host.

[t3] in .drover.toml or the per-user config names the server, the token file,
the project and the provider log directory.`,
	Example: `  drover t3 login                     # sign drover in to [t3] url
  drover t3 discover                  # the server's name, version and tool catalog
  drover t3 run --provider claude -p "Reply with ok"
  drover t3 thread --new --project drover --title "B12 needs you"`,
}

var (
	t3NoBrowser  bool
	t3Check      bool
	t3JSON       bool
	t3Prompt     string
	t3Stdin      bool
	t3Provider   string
	t3Model      string
	t3Permission string
	t3Project    string
	t3Title      string
	t3New        bool
	t3Stop       string
)

var t3LoginCmd = &cobra.Command{
	Use:   "login",
	Short: "Sign drover in to the T3 Code server and write the token file",
	Long: `Runs the OAuth authorization-code flow with PKCE against the metadata the
[t3] url publishes, opens the sign-in page in your browser (--no-browser prints
the URL instead), and writes the grant to [t3] token_file at mode 0600.

T3 Code's sign-in page asks for two things. A one-time pairing code: mint it on
the T3 host with ` + "`t3 auth pairing create`" + `. An access level: choose Full access for
the grant a drive's stages run under, because a Supervised thread waits on
approvals and nobody answers them during a drive. A person exploring with
` + "`drover t3 discover`" + ` can sign in Supervised under a token file of its own.

--check signs nobody in: it exits 0 when the token file holds a grant that is
current or renews, and 1 naming this command when it does not. drive and check
run it as the bridge's sign-in check.`,
	Example: `  drover t3 login                     # opens the sign-in page
  drover t3 login --no-browser        # prints the URL to open elsewhere
  drover t3 login --check             # exit 0 when the grant is usable`,
	Args: cobra.NoArgs,
	RunE: func(c *cobra.Command, _ []string) error {
		cfg, err := t3Config()
		if err != nil {
			return err
		}
		path := cfg.T3TokenFile()
		if t3Check {
			tok := &t3.Tokens{Path: path}
			if _, err := tok.Token(c.Context()); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "signed in: %s holds a current grant for %s\n", path, cfg.T3.URL)
			return nil
		}
		out := c.ErrOrStderr()
		fmt.Fprintln(out, "T3 Code will ask for a pairing code: mint one on the T3 host with `t3 auth pairing create`.")
		fmt.Fprintln(out, "Choose Full access for a grant drive stages run under: a Supervised thread waits on approvals nobody answers during a drive.")
		open := openBrowser
		if t3NoBrowser {
			open = nil
		}
		ctx, cancel := context.WithTimeout(c.Context(), 10*time.Minute)
		defer cancel()
		g, err := t3.Login(ctx, t3.LoginOptions{URL: cfg.T3.URL, Open: open, Out: out})
		if err != nil {
			return err
		}
		if err := t3.WriteGrant(path, g); err != nil {
			return fmt.Errorf("signed in, and the grant would not write to %s: %w", path, err)
		}
		fmt.Fprintf(c.OutOrStdout(), "signed in: wrote %s (mode 0600)\n", path)
		return nil
	},
}

var t3DiscoverCmd = &cobra.Command{
	Use:   "discover",
	Short: "Print the T3 Code server's name, version and tool catalog",
	Long: `Calls MCP initialize and tools/list against [t3] url and prints the server's
name and version and every tool it lists. --json prints the raw tools/list
result instead.`,
	Example: `  drover t3 discover
  drover t3 discover --json | jq -r '.tools[].name'

  # --json is the server's own tools/list result:
  {"tools":[{"name":"t3_thread_launch","description":"Start a thread from outside T3","inputSchema":{"type":"object"}}]}`,
	Args: cobra.NoArgs,
	RunE: func(c *cobra.Command, _ []string) error {
		cfg, err := t3Config()
		if err != nil {
			return err
		}
		cl := t3Client(cfg)
		info, err := cl.Initialize(c.Context())
		if err != nil {
			return err
		}
		tools, raw, err := cl.ListTools(c.Context())
		if err != nil {
			return err
		}
		out := c.OutOrStdout()
		if t3JSON {
			_, err := fmt.Fprintln(out, string(raw))
			return err
		}
		fmt.Fprintf(out, "%s %s at %s · %d tools\n", info.Name, info.Version, cfg.T3.URL, len(tools))
		if v := cfg.T3TestedVersion(); info.Version != v {
			fmt.Fprintf(out, "! the bridge was tested against %s; the provider event log format may have moved\n", v)
		}
		for _, tl := range tools {
			fmt.Fprintf(out, "  %-32s %s\n", tl.Name, firstLine(tl.Description))
		}
		return nil
	},
}

var t3RunCmd = &cobra.Command{
	Use:   "run [prompt]",
	Short: "Launch one T3 Code thread with the prompt and follow it to the end",
	Long: `The bridge a [runners] entry names. Reads the prompt from -p, as the last
positional argument, or from standard input under --stdin; resolves [t3] project
(or --project) to a T3 Code project; checks --provider and --model against the
server's capabilities; launches one thread in the directory it runs in (the
project root, or the worktree drive made); and follows its run to the end,
printing one JSON line per item and a terminal result.

It answers no approval and never changes a thread it did not launch. On an
interrupt, a terminate, or any exit before the run ends, it interrupts the run
it launched. A run that ends anything but completed exits 1.

Turns, tokens, cost and the rate-limit reading come from T3 Code's provider
event log under [t3] provider_logs, not from MCP. Where that log is missing or
holds no Claude SDK result, a usage_unavailable line says so and the result
carries no counts.`,
	Example: `  drover t3 run --provider claude -p "Reply with ok"
  drover t3 run --provider codex --model gpt-6-astra --permission full -p "$(cat prompt.md)"
  drover t3 run --provider claude --stdin < prompt.md`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(c *cobra.Command, args []string) error {
		if !runner.OwnSession() {
			if err := runner.NestedLaunch(); err != nil {
				return Refusal{err}
			}
		}
		out := c.OutOrStdout()
		prompt, err := t3PromptFrom(c, args)
		if err != nil {
			return err
		}
		cfg, err := t3Config()
		if err != nil {
			t3.Fail(out, t3Provider, t3Model, err)
			return err
		}
		stage, cl, err := t3Resolve(c.Context(), cfg, prompt, t3Title)
		if err != nil {
			t3.Fail(out, t3Provider, t3Model, err)
			return err
		}
		stage.Workspace = true
		return t3.Run(c.Context(), t3.Options{
			Client: cl, Launch: stage, Poll: cfg.T3Poll(), ProviderLogs: cfg.T3ProviderLogs(),
			Provider: t3Provider, Session: runner.Session(), Out: out,
		})
	},
}

var t3ThreadCmd = &cobra.Command{
	Use:   "thread",
	Short: "Launch a thread without following it, or interrupt one",
	Long: `--new launches one thread in --project titled --title, with -p as its
first message (an idle thread without one), and prints its id and the link T3
Code opens it at. It does not follow the thread: a person picks it up in T3
Code.

--stop <id> interrupts that thread's newest run. The thread stays, readable in
T3 Code; no tool deletes one. A thread with no run in progress is not an error.`,
	Example: `  drover t3 thread --new --project drover --title "B12 needs you" -p "drover show B12"
  drover t3 thread --stop mcp:68eab9c9-e102-4e85-8e5b-4563f82da101`,
	Args: cobra.NoArgs,
	RunE: func(c *cobra.Command, _ []string) error {
		if t3New == (t3Stop != "") {
			return errors.New("pass exactly one of --new and --stop <thread id>")
		}
		cfg, err := t3Config()
		if err != nil {
			return err
		}
		out := c.OutOrStdout()
		if t3Stop != "" {
			status, err := t3.Interrupt(c.Context(), t3Client(cfg), t3Stop, "", "drover t3 thread --stop")
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "%s: %s\n", t3Stop, status)
			return nil
		}
		if !runner.Supervising() {
			if err := runner.NestedLaunch(); err != nil {
				return Refusal{err}
			}
		}
		if t3Title == "" {
			return errors.New("--new needs --title: it names the thread in T3 Code")
		}
		opened, cl, err := t3Resolve(c.Context(), cfg, t3Prompt, t3Title)
		if err != nil {
			return err
		}
		l, err := t3.Start(c.Context(), cl, opened)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s\n%s\n", l.ThreadID, t3.ThreadURL(l.ThreadID))
		return nil
	},
}

func init() {
	t3LoginCmd.Flags().BoolVar(&t3NoBrowser, "no-browser", false, "print the sign-in URL rather than opening a browser")
	t3LoginCmd.Flags().BoolVar(&t3Check, "check", false, "sign nobody in; exit 0 when the token file holds a usable grant")
	t3DiscoverCmd.Flags().BoolVar(&t3JSON, "json", false, "print the raw tools/list result")
	for _, c := range []*cobra.Command{t3RunCmd, t3ThreadCmd} {
		c.Flags().StringVarP(&t3Prompt, "prompt", "p", "", "the prompt, as the thread's first message")
		c.Flags().StringVar(&t3Provider, "provider", "claude", "claude or codex")
		c.Flags().StringVar(&t3Model, "model", "", "a model id as orchestrator_capabilities lists it (default: the provider's first)")
		c.Flags().StringVar(&t3Permission, "permission", "supervised", "supervised (approval-required) or full (full-access)")
		c.Flags().StringVar(&t3Project, "project", "", "the T3 Code project title (default: [t3] project)")
		c.Flags().StringVar(&t3Title, "title", "", "the thread's title")
	}
	t3RunCmd.Flags().BoolVar(&t3Stdin, "stdin", false, "read the prompt from standard input")
	t3ThreadCmd.Flags().BoolVar(&t3New, "new", false, "launch a thread and print its id and link, without following it")
	t3ThreadCmd.Flags().StringVar(&t3Stop, "stop", "", "interrupt this thread's newest run")
	t3Cmd.AddCommand(t3LoginCmd, t3DiscoverCmd, t3RunCmd, t3ThreadCmd)
	root.AddCommand(t3Cmd)
}

// t3Config is [t3] for the directory drover runs in, refusing one with no url.
func t3Config() (*config.Config, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	cfg, err := config.LoadT3(wd)
	if err != nil {
		return nil, err
	}
	if cfg.T3.URL == "" {
		return nil, fmt.Errorf("[t3] url is not set: name the T3 Code server's MCP endpoint, such as https://host.example.ts.net/mcp, in .drover.toml or %s", config.GlobalPath())
	}
	return cfg, nil
}

func t3Client(cfg *config.Config) *t3.Client {
	tok := &t3.Tokens{Path: cfg.T3TokenFile()}
	return t3.NewClient(cfg.T3.URL, tok.Token)
}

// t3PromptFrom is the prompt from exactly one of -p, the positional argument
// and --stdin, which is where runner.Spec places it.
func t3PromptFrom(c *cobra.Command, args []string) (string, error) {
	sources := 0
	prompt := ""
	if c.Flags().Changed("prompt") {
		sources++
		prompt = t3Prompt
	}
	if len(args) == 1 {
		sources++
		prompt = args[0]
	}
	if t3Stdin {
		sources++
		b, err := io.ReadAll(c.InOrStdin())
		if err != nil {
			return "", fmt.Errorf("read the prompt from stdin: %w", err)
		}
		prompt = string(b)
	}
	if sources != 1 {
		return "", errors.New("give the prompt exactly once: -p <text>, as the last argument, or on stdin under --stdin")
	}
	if strings.TrimSpace(prompt) == "" {
		return "", errors.New("the prompt is empty")
	}
	return prompt, nil
}

// t3Resolve turns the flags into a launch: the prompt checked against the
// server's limit before any call, then the project and the placement.
func t3Resolve(ctx context.Context, cfg *config.Config, prompt, title string) (t3.Launch, *t3.Client, error) {
	if err := t3.CheckPrompt(prompt); err != nil {
		return t3.Launch{}, nil, err
	}
	name := t3Project
	if name == "" {
		name = cfg.T3Project()
	}
	if name == "" {
		return t3.Launch{}, nil, errors.New("no T3 Code project to launch in: set [t3] project or pass --project")
	}
	cl := t3Client(cfg)
	proj, err := t3.FindProject(ctx, cl, name)
	if err != nil {
		return t3.Launch{}, nil, err
	}
	place, err := t3.Place(ctx, cl, t3Provider, t3Model, t3Permission)
	if err != nil {
		return t3.Launch{}, nil, err
	}
	dir, err := os.Getwd()
	if err != nil {
		return t3.Launch{}, nil, err
	}
	if title == "" {
		title = "drover: " + trim(firstLine(strings.TrimSpace(prompt)), 80)
	}
	return t3.Launch{Project: proj, Place: place, Title: title, Prompt: prompt, Dir: dir}, cl, nil
}

// openBrowser hands a URL to the platform's opener, reporting false where it
// has none or it would not start.
func openBrowser(u string) bool {
	var argv []string
	switch runtime.GOOS {
	case "darwin":
		argv = []string{"open", u}
	case "linux":
		argv = []string{"xdg-open", u}
	default:
		return false
	}
	return exec.Command(argv[0], argv[1:]...).Start() == nil // #nosec G204 -- the platform opener, with the URL drover built
}

// t3Blind is why usage_admission cannot see a t3 runner, read off the last
// session the ledger records it running: that session's log printed
// usage_unavailable. Empty when it did not, when the runner is not a bridge,
// or when no such session or log can be found, which is not evidence of
// blindness.
func (s *session) t3Blind(name string) string {
	spec, ok := s.cfg.Runners[name]
	if !ok || !runner.IsT3(spec) || s.ledger == nil {
		return ""
	}
	var sessionID string
	for i := len(s.ledger.Rows) - 1; i >= 0; i-- {
		r := s.ledger.Rows[i]
		if r.Stage != "" && r.Runner == name && r.Session != "" {
			sessionID = r.Session
			break
		}
	}
	if sessionID == "" {
		return ""
	}
	path := t3LogFor(s.cfg.LogDir(), sessionID)
	if path == "" {
		return ""
	}
	return t3UsageUnavailable(path)
}

// t3LogFor is the newest log in dir whose thread.started line names the drover
// session, empty when none does. Stage rows carry the session and not the log,
// and the bridge writes the session on its first line for this lookup.
func t3LogFor(dir, sessionID string) string {
	matches, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return ""
	}
	type dated struct {
		path string
		at   time.Time
	}
	logs := make([]dated, 0, len(matches))
	for _, m := range matches {
		if _, at, ok := runlog.ParseName(filepath.Base(m), time.Local); ok {
			logs = append(logs, dated{m, at})
		}
	}
	sort.Slice(logs, func(i, j int) bool { return logs[i].at.After(logs[j].at) })
	for _, l := range logs {
		if runlog.T3Session(l.path) == sessionID {
			return l.path
		}
	}
	return ""
}

// t3UsageUnavailable is the reason a finished t3 log gave for printing no
// usage, empty when it printed usage or never finished.
func t3UsageUnavailable(path string) string {
	rd, err := runlog.ReadResult(path)
	if err != nil || !rd.Found || rd.Result.Schema != "t3" {
		return ""
	}
	return rd.Result.UsageUnavailable
}

// blindRunners are the runners this drive launches by default whose last
// session reported no usage, each with the reason it gave.
func (s *session) blindRunners(reviewer string) [][2]string {
	impl := driveRunner
	if impl == "" {
		impl = s.cfg.Run.DefaultRunner
	}
	var out [][2]string
	for _, name := range []string{impl, reviewer} {
		if len(out) > 0 && out[0][0] == name {
			continue
		}
		if why := s.t3Blind(name); why != "" {
			out = append(out, [2]string{name, why})
		}
	}
	return out
}

// weeklyLevels reports whether [usage.weekly] sets a level usage_admission
// acts on, which a blind runner would leave it unable to read.
func (s *session) weeklyLevels() bool {
	_, notify := s.cfg.UsageLevel(config.UsageWeekly, "notify")
	_, pause := s.cfg.UsageLevel(config.UsageWeekly, "pause")
	return notify || pause
}

// preflightBlindUsage refuses a drive on a runner usage_admission cannot see
// while [usage.weekly] asks it to hold at a level: such a drive spends the
// week with nothing reading the window. --dry-run says so instead.
func (s *session) preflightBlindUsage(reviewer string) error {
	if driveDry || driveAllowBlind || !s.weeklyLevels() {
		return nil
	}
	blind := s.blindRunners(reviewer)
	if len(blind) == 0 {
		return nil
	}
	return Refusal{fmt.Errorf("usage_admission is blind for runner %s: its last session printed usage_unavailable (%s).\n"+
		"[usage.weekly] sets a level nothing could read during this drive. Fix what the reason names, unset the [usage.weekly] levels, "+
		"or pass --allow-blind-usage to drive anyway. Nothing was launched", blind[0][0], blind[0][1])}
}

// blindPlan is the dry run's line for a blind runner.
func (s *session) blindPlan(w io.Writer, reviewer string) {
	for _, b := range s.blindRunners(reviewer) {
		verdict := "a drive would run: [usage.weekly] sets no level"
		switch {
		case driveAllowBlind:
			verdict = "a drive would run under --allow-blind-usage"
		case s.weeklyLevels():
			verdict = "a drive would refuse to start without --allow-blind-usage, because [usage.weekly] sets a level"
		}
		fmt.Fprintf(w, "==> usage_admission: blind for runner %s, whose last session printed usage_unavailable (%s); %s\n", b[0], b[1], verdict)
	}
}
