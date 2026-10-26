package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/reports"
	"github.com/ravinald/drover/internal/runlog"
	"github.com/spf13/cobra"
)

var (
	reportSave bool
	reportJSON bool
)

// RunReport is a run's self-report as data. Unfinished distinguishes a log
// with no result event from one that never ran, which is a distinction a
// wrapper deciding whether to review needs and cannot recover from empty text.
type RunReport struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	LogPath    string `json:"log_path"`
	Headline   string `json:"headline,omitempty"`
	Report     string `json:"report"`
	Unfinished bool   `json:"unfinished"`
	// Saved and ReportPath (report_path) describe what --save did, and are absent without
	// it. Without them, a wrapper asking for both cannot tell from the output
	// whether the write happened or was skipped.
	Saved      bool   `json:"saved,omitempty"`
	ReportPath string `json:"report_path,omitempty"`
}

var reportCmd = &cobra.Command{
	Use:   "report <id>",
	Short: "Show what a run claimed, from its most recent log.",
	Long: `A self-report is an unverified claim, not proof. --save writes it beside a
checklist of the item's numbered requirements, and refuses to overwrite a
report a human has already filled in.`,
	Example: `  drover report 1                   # what the last run claimed
  drover report 1 --save            # write it beside a checklist of the requirements
  drover report 1 --json            # one object:
    {"id":"1","headline":"...","report":"...","unfinished":false}

  unfinished distinguishes a log with no result event from one that never ran,
  which empty text cannot tell you.`,
	Args: needID("report <id>"),
	RunE: func(_ *cobra.Command, args []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		it, err := s.item(args[0])
		if err != nil {
			return err
		}
		data, logPath, err := s.reportData(it)
		if err != nil {
			return err
		}

		// The save happens before either output path. Ordered after the JSON
		// return, --save --json would be a silent no-op: the report would come
		// back with no field saying so, and the file a later command reads would
		// never be written.
		var saved bool
		path := s.reportPath(it)
		if reportSave {
			written, err := reports.Write(path, data)
			if err != nil {
				return err
			}
			saved = written
		}

		if reportJSON {
			report := RunReport{
				ID: it.ID.Bare(), Title: it.Title(), LogPath: logPath,
				Headline: data.Headline, Report: data.Report,
				Unfinished: data.Report == "",
			}
			if reportSave {
				report.Saved, report.ReportPath = saved, rel(s.cfg.Root, path)
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(report)
		}

		if !reportSave {
			// Shortened for the terminal only. The --json branch above carries
			// the absolute path, because a wrapper that re-opens it cannot
			// expand a ~.
			fmt.Printf("==> %s\n", displayPath(s.cfg.Root, logPath))
			if data.Headline != "" {
				fmt.Printf("outcome: %s\n", data.Headline)
			}
			fmt.Printf("%s\n%s\n", rule(60), data.Report)
			return nil
		}

		if !saved {
			fmt.Printf("report kept (already saved): %s\n", rel(s.cfg.Root, path))
			return nil
		}
		fmt.Printf("report saved: %s\n", rel(s.cfg.Root, path))
		return nil
	},
}

// logResult is the terminal event of the newest log for one base, and the path
// it came from.
//
// Split out of reportData because the two callers want different halves: a
// report wants the document, with a checklist and an implementer in it, and a
// loop asking whether a review finished or died wants neither. found is false
// when the log exists and carries no terminal event, which is the died-quietly
// case and reads nothing like an absent log.
func (s *session) logResult(base string) (res runlog.Result, logPath string, found bool, err error) {
	logPath, err = runlog.Latest(s.cfg.LogDir(), s.cfg.Paths.LogPrefix+base)
	if err != nil || logPath == "" {
		return res, logPath, false, err
	}
	rd, err := runlog.ReadResult(logPath)
	return rd.Result, logPath, rd.Found, err
}

// reportData gathers a run's self-report for one item.
func (s *session) reportData(it backlog.Item) (reports.Data, string, error) {
	res, logPath, found, err := s.logResult(runlog.Base(it.ID.Stem(), it.Slug))
	if err != nil && logPath == "" {
		return reports.Data{}, "", err
	}
	if logPath == "" {
		return reports.Data{}, "", fmt.Errorf("no run log for %s in %s", it.ID.Bare(), s.cfg.LogDir())
	}
	d := reports.Data{
		ID: it.ID.Bare(), Title: it.Title(), Slug: it.Slug,
		Meta: it.Meta, LogPath: logPath, Body: it.Body,
		Runner:    s.ledger.State(it.ID.Bare()).Implementer,
		RunnerKey: s.cfg.Run.RunnerKey,
	}
	if err != nil {
		return d, logPath, err
	}
	if found {
		d.Report, d.Headline = res.Text, res.Headline()
	}
	return d, logPath, nil
}

// captureReport is the post-run hook. A failure here must not fail the run:
// the work happened, and losing the snapshot is worth a warning, not an error.
func (s *session) captureReport(it backlog.Item) {
	data, _, err := s.reportData(it)
	if err != nil {
		fmt.Fprintf(errOut, "warn: report not captured: %v\n", err)
		return
	}
	path := s.reportPath(it)
	written, err := reports.Write(path, data)
	switch {
	case err != nil:
		fmt.Fprintf(errOut, "warn: report not saved: %v\n", err)
	case written:
		fmt.Fprintf(errOut, "==> report saved: %s\n", rel(s.cfg.Root, path))
	default:
		fmt.Fprintf(errOut, "==> report kept (already saved): %s\n", rel(s.cfg.Root, path))
	}
}

func rule(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '-'
	}
	return string(b)
}

func init() {
	reportCmd.Flags().BoolVar(&reportSave, "save", false, "write the report file instead of printing it")
	reportCmd.Flags().BoolVar(&reportJSON, "json", false, "emit the report as JSON")
	root.AddCommand(reportCmd)
}
