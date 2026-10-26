package config

import (
	"fmt"
	"strconv"

	"github.com/BurntSushi/toml"
)

// Off is the value a usage level takes to say "no such step here".
//
// TOML has no null, and a file layered over another can only write a value, so
// a project inheriting a per-user pause_remaining had no way to remove it: 0 is
// a level too, at nothing left. A string the numeric keys never otherwise take
// is the one spelling that cannot be mistaken for a level.
const Off = "off"

// OrOff is a usage level: a number, or Off. The zero value is the number zero,
// so whether a file wrote the key at all is still Declared's question.
type OrOff[T int | float64] struct {
	Value T
	Off   bool
}

// UnmarshalTOML takes the number or the string "off". Each decode replaces
// both fields, because the project's file is decoded over the per-user one into
// the same value and the last file to write the key decides it.
func (o *OrOff[T]) UnmarshalTOML(data any) error {
	*o = OrOff[T]{}
	switch v := data.(type) {
	case string:
		if v == Off {
			o.Off = true
			return nil
		}
	case int64:
		o.Value = T(v)
		return nil
	case float64:
		if _, isFloat := any(o.Value).(float64); isFloat {
			o.Value = T(v)
			return nil
		}
	}
	return fmt.Errorf("give %s or the string %q, not %s", o.form(), Off, describe(data))
}

// MarshalTOML is the literal a file would write: the number, or "off".
func (o OrOff[T]) MarshalTOML() ([]byte, error) {
	if o.Off {
		return []byte(strconv.Quote(Off)), nil
	}
	switch v := any(o.Value).(type) {
	case float64:
		return []byte(strconv.FormatFloat(v, 'g', -1, 64)), nil
	case int:
		return []byte(strconv.Itoa(v)), nil
	}
	return nil, fmt.Errorf("OrOff holds %T", o.Value)
}

func (o OrOff[T]) form() string {
	if _, isFloat := any(o.Value).(float64); isFloat {
		return "a number"
	}
	return "an integer"
}

func describe(data any) string {
	if s, ok := data.(string); ok {
		return "the string " + strconv.Quote(s)
	}
	return fmt.Sprintf("%v", data)
}

var _ toml.Unmarshaler = (*OrOff[int])(nil)

// LevelOff is one usage level whose winning value is "off".
type LevelOff struct {
	// Key is the level dotted, as usage.weekly.pause_remaining.
	Key string
	// Path is the file that wrote "off".
	Path string
	// Inherited is the literal the per-user file sets under a project's "off",
	// empty where it sets none or the "off" is its own.
	Inherited string
}

// offKeys are the six keys that take "off", in reference order.
var offKeys = []struct {
	key string
	get func(Usage) (bool, string)
}{
	{"usage.weekly.notify_remaining", func(u Usage) (bool, string) { return lit(usageLevel(u, UsageWeekly, "notify")) }},
	{"usage.weekly.pause_remaining", func(u Usage) (bool, string) { return lit(usageLevel(u, UsageWeekly, "pause")) }},
	{"usage.five_hour.notify_remaining", func(u Usage) (bool, string) { return lit(usageLevel(u, UsageFiveHour, "notify")) }},
	{"usage.five_hour.pause_remaining", func(u Usage) (bool, string) { return lit(usageLevel(u, UsageFiveHour, "pause")) }},
	{"usage.overage.max_usd", func(u Usage) (bool, string) { return lit(overageUSD(u, "max")) }},
	{"usage.overage.notify_usd", func(u Usage) (bool, string) { return lit(overageUSD(u, "notify")) }},
}

func lit[T int | float64](o OrOff[T]) (bool, string) {
	b, _ := o.MarshalTOML()
	return o.Off, string(b)
}

// SwitchedOff is every usage level whose winning value is "off", with the file
// that wrote it and, where that is the project's, the per-user value it
// replaced. A reader seeing a level missing cannot otherwise tell a project
// that switched it off from a per-user file that never set it.
func (c *Config) SwitchedOff() []LevelOff {
	var out []LevelOff
	for _, k := range offKeys {
		if off, _ := k.get(c.Usage); !off {
			continue
		}
		var winner *layer
		inherited := false
		for i := range c.layers {
			if c.layers[i].declares(k.key) {
				winner = &c.layers[i]
				inherited = inherited || !c.layers[i].own
			}
		}
		if winner == nil {
			continue
		}
		l := LevelOff{Key: k.key, Path: winner.path}
		if winner.own && inherited {
			if off, v := k.get(c.inheritedUsage); !off {
				l.Inherited = v
			}
		}
		out = append(out, l)
	}
	return out
}
