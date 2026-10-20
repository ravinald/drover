package config

import _ "embed"

// Schema is the JSON Schema for .drover.toml, embedded so a build cannot ship a
// binary that disagrees with the contract it publishes.
//
// It is a second description of a structure the Go types already describe, and
// that duplication is the cost of being readable by something that is not this
// binary: an editor completing keys, a CI job validating a config before drover
// runs, a generator writing one. TestSchemaCoversConfig holds the two together.
//
//go:embed drover.schema.json
var Schema string
