// Command drover drives AI coding agents from Markdown requirements to reviewed
// code, with an audit trail for every work item.
package main

import (
	"os"

	"github.com/ravinald/drover/internal/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		cmd.PrintError(os.Stderr, err)
		os.Exit(cmd.Code(err))
	}
}
