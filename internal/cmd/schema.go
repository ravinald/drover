package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/ravinald/drover/internal/config"
	"github.com/spf13/cobra"
)

var schemaFrontmatter bool

// bodyContract is what one project declares an item's frontmatter may carry:
// which keys, and where a shape is declared, what each may hold. It is the same
// contract check enforces, in the form something writing a body can read.
type bodyContract struct {
	Required []string          `json:"required"`
	Optional []string          `json:"optional"`
	Shape    map[string]string `json:"shape"`
}

var schemaCmd = &cobra.Command{
	Use:   "schema",
	Short: "Print the JSON Schema for .drover.toml, or this project's body contract.",
	Long: `The config reference in prose is for a person; this is the same contract for
everything else — an editor completing keys, a CI job validating a config before
drover ever runs, a generator writing one.

It needs no project, so it answers from anywhere, including a tree that has no
.drover.toml yet.

--frontmatter is the exception. It reads this project's own config and emits
what a body has to satisfy: the required keys, the optional ones, and the shape
declared for each key that has one. An agent drafting a body gets a target it
can read instead of a neighboring file to imitate.`,
	Example: `  drover schema                     # the .drover.toml contract as JSON Schema
  drover schema > .drover.schema.json
  drover schema | jq '.properties.paths.properties | keys'

  drover schema --frontmatter       # what a body in this project must satisfy:
    {"required":["title","deps"],
     "optional":["kind","issue","closes","severity","skills","spec","runner"],
     "shape":{"issue":"int","closes":"intlist","severity":"enum:C,H,M,L"}}

  shape is what check enforces as BAD VALUE. A key with no entry there may hold
  anything, and an empty value passes every shape.`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		if !schemaFrontmatter {
			fmt.Print(config.Schema)
			return nil
		}
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		cfg, err := config.Load(wd)
		if err != nil {
			return err
		}
		// Non-nil throughout: a consumer reading `.optional` wants an empty list
		// from a project that declares none, not a null to special-case.
		out := bodyContract{
			Required: cfg.Frontmatter.Required,
			Optional: cfg.Frontmatter.Optional,
			Shape:    cfg.Frontmatter.Shape,
		}
		if out.Optional == nil {
			out.Optional = []string{}
		}
		if out.Shape == nil {
			out.Shape = map[string]string{}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	},
}

func init() {
	schemaCmd.Flags().BoolVar(&schemaFrontmatter, "frontmatter", false,
		"emit this project's body contract instead: required keys, optional keys and their shapes")
	root.AddCommand(schemaCmd)
}
