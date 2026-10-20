package cmd

import (
	"fmt"
	"strings"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/register"
	"github.com/ravinald/drover/internal/sev"
	"github.com/spf13/cobra"
)

var (
	followUpOwners []string
	followUpSev    string
)

var followUpCmd = &cobra.Command{
	Use:   "follow-up",
	Short: "File or close a follow-up in a file-backed follow-up list.",
	Long: `The write half of ` + "`drover follow-ups`" + `, for a project whose follow-up list is a
file rather than an issue tracker. It appends to {register}; a close is another
append, so the line that opened a follow-up survives it.

It refuses under [register] backend = "github", where the tracker owns the
write, and under "none", where there is nothing to write to.`,
	Args: cobra.ArbitraryArgs,
	RunE: func(_ *cobra.Command, args []string) error {
		// Without this, cobra's legacyArgs accepts anything on a parent and
		// prints help at exit 0, so `drover follow-up clsoe 7` reads as success.
		if len(args) > 0 {
			return fmt.Errorf("unknown subcommand %q: drover follow-up open|close", args[0])
		}
		return fmt.Errorf("drover follow-up needs a subcommand: open or close")
	},
	Example: `  drover follow-up open "contract checks assert shape" --owner 4 --sev M
  drover follow-up open "gh output shape is unpinned"     # unowned: reaches no run
  drover follow-up close 7

  Omitting --owner is legal and files an unowned follow-up, which records the gap
  for a human and reaches no run — the state drover follow-ups flags as such.

  Numbers are allocated one past the highest the file has ever used, closed rows
  included: reusing one would splice two entries into a single history.`,
}

var followUpOpenCmd = &cobra.Command{
	Use:   "open <title>",
	Short: "File a new follow-up against an owner.",
	Long:  "Allocates the next number and appends one open row.",
	Example: `  drover follow-up open "contract checks assert shape" --owner 4 --sev M
  drover follow-up open "two owners share this gap" --owner 4 --owner B2 --sev high`,
	Args: cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		if err := s.registerWritable(); err != nil {
			return err
		}
		// Refused rather than recorded: a rank the vocabulary does not know
		// ranks last everywhere it is read, so a typo files a critical
		// follow-up that sorts below every ranked one and reads as deliberate.
		if followUpSev != "" && sev.Of(followUpSev) == sev.Unranked {
			return Refusal{fmt.Errorf("unknown severity %q (known: %s)",
				followUpSev, strings.Join(severityNames(), ", "))}
		}
		// Refused for the same reason: an owner no body answers to prints as an
		// owned follow-up and reaches no run, which is worse than the unowned
		// one that says so on every line it appears.
		// Exit 1, not a refusal: an id with no body is something drover could
		// not resolve, which is the same answer `follow-up close` gives a number
		// the file does not hold. A rule the project declared is what exits 3.
		for _, owner := range followUpOwners {
			if _, err := s.item(owner); err != nil {
				return fmt.Errorf("--owner %s: %w", owner, err)
			}
		}
		path := s.cfg.Abs(s.cfg.Paths.Register)
		f, err := register.Read(path)
		if err != nil {
			return err
		}
		row := register.Row{
			Number:   f.Max + 1,
			Title:    args[0],
			Severity: followUpSev,
			Owners:   followUpOwners,
			State:    register.StateOpen,
		}
		if err := register.Append(path, row); err != nil {
			return err
		}
		fmt.Printf("filed #%d: %s\n", row.Number, describeRow(row))
		return nil
	},
}

var followUpCloseCmd = &cobra.Command{
	Use:   "close <number>",
	Short: "Close a follow-up by number.",
	Long: "Appends a closing row rather than deleting the one that opened it. The read " +
		"folds newest-wins per number and drops what is closed, so the history stays and " +
		"the index does not.",
	Example: `  drover follow-up close 7`,
	Args: needIssue("follow-up close <number>",
		"that is a row number in {register}, not a drover id: drover follow-ups lists them"),
	RunE: func(_ *cobra.Command, args []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		if err := s.registerWritable(); err != nil {
			return err
		}
		number, err := parsePositive(args[0])
		if err != nil {
			return err
		}
		path := s.cfg.Abs(s.cfg.Paths.Register)
		f, err := register.Read(path)
		if err != nil {
			return err
		}
		var latest *register.Row
		for i := range f.Rows {
			if f.Rows[i].Number == number {
				latest = &f.Rows[i]
			}
		}
		if latest == nil {
			return fmt.Errorf("no #%d in %s (drover follow-ups lists what is open)",
				number, rel(s.cfg.Root, path))
		}
		if latest.State == register.StateClosed {
			fmt.Printf("already closed: #%d\n", number)
			return nil
		}
		// The closing row carries the fields the open row had, so a line read on
		// its own says which entry it closed rather than only its number.
		row := *latest
		row.State = register.StateClosed
		row.Date = ""
		if err := register.Append(path, row); err != nil {
			return err
		}
		fmt.Printf("closed #%d: %s\n", number, row.Title)
		return nil
	},
}

// registerWritable refuses the backends that own their own writes, before
// anything is read. github answers to gh and the tracker's own UI; none has
// nowhere to put a row, and inventing a file for it would give the project a
// follow-up list it declared it did not have.
func (s *session) registerWritable() error {
	backend := s.cfg.RegisterBackend()
	if backend == config.BackendFile {
		return nil
	}
	// The second line names the backend this project actually resolved. It used
	// to name github unconditionally, so a project declaring "none" was told
	// that "the github backend files rows on the tracker" — a sentence about a
	// backend it had not chosen.
	why := "the tracker owns the write"
	if backend == config.BackendNone {
		why = "there is no follow-up list to write to"
	}
	return Refusal{fmt.Errorf(
		"%s resolves [register] backend = %q, so %s and drover follow-up writes nothing here.\n"+
			"Set backend = %q to keep the follow-up list in %s",
		s.cfg.Project, backend, why, config.BackendFile, s.cfg.Paths.Register)}
}

// describeRow is what the operator gets back: the fields that decide where the
// follow-up shows up, so a mistyped flag is visible without a second command.
func describeRow(r register.Row) string {
	rank := r.Severity
	if rank == "" {
		rank = "-"
	}
	owners := "unowned"
	if len(r.Owners) > 0 {
		owners = strings.Join(r.Owners, " ")
	}
	return fmt.Sprintf("[%s] %s (%s)", rank, r.Title, owners)
}

// severityNames is every spelling --sev accepts, letters then words.
func severityNames() []string {
	names := sev.Letters()
	for _, r := range sev.Ranks() {
		names = append(names, r.Word())
	}
	return names
}

func init() {
	followUpOpenCmd.Flags().StringArrayVar(&followUpOwners, "owner", nil,
		"bare id that owns this follow-up, must name an item, repeatable (default: unowned, which reaches no run)")
	followUpOpenCmd.Flags().StringVar(&followUpSev, "sev", "",
		"severity rank, one value: C, H, M or L, or critical, high, medium or low, no none (default: unranked, which is what omitting the flag files and still counts as open)")
	followUpCmd.AddCommand(followUpOpenCmd, followUpCloseCmd)
	root.AddCommand(followUpCmd)
}
