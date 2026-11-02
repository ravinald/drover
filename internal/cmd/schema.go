package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/prompts"
	"github.com/spf13/cobra"
)

var (
	schemaFrontmatter bool
	schemaLedger      bool
	schemaPrompts     bool
)

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
can read instead of a neighboring file to imitate.

--ledger emits every key a ledger row may carry, with the Go type it holds,
for a tool appending rows without the binary. It needs no project either.

--prompts emits every block of the built-in prompt templates a [prompts] file
may replace with a define block of the same name: its slot, its name, its default
text and the hash an override of it records as {{/* drover-base: <hash> */}}.
An addition is a block whose default is empty, so it carries no base to record.
It needs no project either.`,
	Example: `  drover schema                     # the .drover.toml contract as JSON Schema
  drover schema > .drover.schema.json
  drover schema | jq '.properties.paths.properties | keys'

  drover schema --frontmatter       # what a body in this project must satisfy:
    {"required":["title","deps"],
     "optional":["kind","issue","closes","severity","skills","spec","runner"],
     "shape":{"issue":"int","closes":"intlist","severity":"enum:C,H,M,L"}}

  drover schema --ledger            # every ledger row key and its type:
    {"changes":"string","changes_source":"string","findings":"string",...}

  drover schema --prompts           # every overridable prompt block:
    [{"slot":"implement","block":"intro","hash":"3f9a1c0d2e4b",
      "default":"You are executing {{.ID}}: ...","addition":false}, ...]
  drover schema --prompts | jq -r '.[] | select(.slot=="review" and .block=="verify") | .default'

  shape is what check enforces as BAD VALUE. A key with no entry there may hold
  anything, and an empty value passes every shape.`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		if schemaPrompts {
			var out []prompts.Block
			for _, slot := range prompts.Slots {
				out = append(out, prompts.Blocks(slot)...)
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(out)
		}
		if schemaLedger {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(ledgerKeys())
		}
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

// ledgerKeys is every key a ledger row may carry and the Go type it holds, read
// off ledger.Row so it lists what drover writes rather than what somebody
// remembered to document.
func ledgerKeys() map[string]string {
	out := map[string]string{}
	t := reflect.TypeOf(ledger.Row{})
	for i := range t.NumField() {
		f := t.Field(i)
		tag, _ := f.Tag.Lookup("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		out[name] = f.Type.String()
	}
	return out
}

func init() {
	schemaCmd.Flags().BoolVar(&schemaLedger, "ledger", false,
		"emit every key a ledger row may carry and its Go type, instead of the config schema")
	schemaCmd.Flags().BoolVar(&schemaFrontmatter, "frontmatter", false,
		"emit this project's body contract instead: required keys, optional keys and their shapes")
	schemaCmd.Flags().BoolVar(&schemaPrompts, "prompts", false,
		"emit every built-in prompt block a [prompts] file may override, with its default and hash")
	schemaCmd.MarkFlagsMutuallyExclusive("frontmatter", "ledger", "prompts")
	root.AddCommand(schemaCmd)
}
