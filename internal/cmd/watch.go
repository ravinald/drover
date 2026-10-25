package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
)

var (
	watchUntil []string
	watchSince string
	// watchAfter is --after, negative when not given: 0 is a cursor, the
	// one that counts every event in the file.
	watchAfter = -1
)

var watchCmd = &cobra.Command{
	Use:   "watch [drive id] --until <event,...>",
	Short: "Block until a drive emits one of the named events, and print it.",
	Long: `Follows the drive's event file, as attach does, until the drive emits one of
the events --until names, prints that event as one JSON object and exits 0.
It exits 3 when the drive exits without emitting one, and 1 when this project
has no such drive. With no id it watches the drive running in this checkout,
else the project's latest.

Every event carries seq, its line in the event file from 1. --after <seq>
counts only the events past it, whenever they were written: a loop that
passes the seq of the last event it handled sees each event once and misses
none written between two calls. Without --after only events written after
watch starts count, so an event written between two calls is lost.
--since <ts> counts events stamped at or after ts, for a person reading by
time; ts is one second wide, so a loop on it returns the same event again.
The exit event ends the watch whenever it was written, so watch on a drive
that has already exited returns at once: 0 with that event when --until
names exit, 3 otherwise.

--until takes any event a drive emits: queued, dispatched, preflight, stage,
check, parked, filed, needs_operator, merged, paused, resumed, usage_warning
and exit. usage_warning is a [usage] notify level crossed; a usage hold
arrives as paused and needs_operator with action usage_admission.

This is the loop a supervising session would otherwise write around sleep and
the ledger. watch reads only; an interrupt exits 1 and leaves the drive
running.`,
	Example: `  drover watch --until parked,needs_operator,merged,exit
  drover watch D3 --until exit
  drover watch --until parked,needs_operator,merged,exit --after 7
  drover watch --until usage_warning,needs_operator,exit

  {"event":"parked","ts":"2026-10-18T09:40:51Z","seq":8,"drive":"D3","id":"4",
   "park":"no_record","reason":"the review recorded nothing"}`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(c *cobra.Command, args []string) error {
		if len(watchUntil) == 0 {
			return fmt.Errorf("--until names the events to wait for, one or more of %s", strings.Join(driveEventNames, ", "))
		}
		until := map[string]bool{}
		for _, name := range watchUntil {
			name = strings.TrimSpace(name)
			if !slices.Contains(driveEventNames, name) {
				return fmt.Errorf("--until %q is no drive event; one or more of %s", name, strings.Join(driveEventNames, ", "))
			}
			until[name] = true
		}
		since, err := parseSince(watchSince)
		if err != nil {
			return err
		}
		f, err := resolveFollowed(args)
		if err != nil {
			return err
		}
		// Without a cursor, what the file already holds is history.
		skip := int64(-1)
		if since.IsZero() && watchAfter < 0 {
			skip = 0
			if info, err := os.Stat(f.events); err == nil {
				skip = info.Size()
			}
		}
		var got []byte
		var exited *DriveEvent
		err = f.follow(c.Context(), func(line []byte, n int, end int64, e DriveEvent) bool {
			if e.Event == evExit {
				if until[evExit] {
					got = withSeq(line, n, e)
				} else {
					exited = &e
				}
				return true
			}
			old := end <= skip || n <= watchAfter || (!since.IsZero() && before(e.TS, since))
			if !old && until[e.Event] {
				got = withSeq(line, n, e)
				return true
			}
			return false
		})
		names := strings.Join(slices.Sorted(maps.Keys(until)), ", ")
		switch {
		case got != nil:
			fmt.Fprintf(os.Stdout, "%s\n", got)
			return nil
		case errors.Is(err, errDriveGone):
			return Refusal{fmt.Errorf("%w, and never emitted %s", err, names)}
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("interrupted before drive %s emitted %s; it is still running", f.id, names)
		case err != nil:
			return err
		}
		code := 0
		if exited.Code != nil {
			code = *exited.Code
		}
		return Refusal{fmt.Errorf("drive %s exited (code %d) without emitting %s", f.id, code, names)}
	},
}

// withSeq is the line watch prints for the event at line n: as the drive wrote
// it, or with seq added where the drive's binary stamped none, so the caller
// always has the cursor for its next --after.
func withSeq(line []byte, n int, e DriveEvent) []byte {
	if e.Seq != 0 {
		return slices.Clone(line)
	}
	e.Seq = n
	out, err := json.Marshal(e)
	if err != nil {
		return slices.Clone(line)
	}
	return out
}

func init() {
	watchCmd.Flags().StringSliceVar(&watchUntil, "until", nil,
		"the events to wait for, comma-separated: "+strings.Join(driveEventNames, ", "))
	watchCmd.Flags().IntVar(&watchAfter, "after", -1, "count only events whose seq is past this one, written before watch started or not: the seq of the last event you handled")
	watchCmd.Flags().StringVar(&watchSince, "since", "", "count events stamped at or after this RFC 3339 time, not only those written after watch starts")
	root.AddCommand(watchCmd)
}
