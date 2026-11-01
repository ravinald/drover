package cmd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/prompts"
)

func TestLineDiffKeepsRemovesAndAdds(t *testing.T) {
	got := lineDiff("a\nb\nc", "a\nc\nd")
	var ops strings.Builder
	for _, l := range got {
		ops.WriteString(string(l.op) + l.text + "|")
	}
	if ops.String() != " a|-b| c|+d|" {
		t.Errorf("ops = %q", ops.String())
	}
}

// A rewritten line is paired with what it became, and only the span that
// differs is marked, so the reader sees the edit rather than two whole lines.
func TestLineDiffPairsARewrite(t *testing.T) {
	got := lineDiff("- Smallest diff that does it.", "- Smallest change that does it.")
	if len(got) != 2 || got[0].op != '-' || got[1].op != '+' {
		t.Fatalf("diff = %+v, want one removal and one addition", got)
	}
	if got := spansOf(got[0]); got != "diff" {
		t.Errorf("removed spans = %q, want %q", got, "diff")
	}
	if got := spansOf(got[1]); got != "change" {
		t.Errorf("added spans = %q, want %q", got, "change")
	}
}

// spansOf is a line's changed text, spans joined by "|", leading spaces cut.
func spansOf(l diffLine) string {
	var out []string
	for _, c := range l.changed {
		out = append(out, strings.TrimLeft(l.text[c[0]:c[1]], " "))
	}
	return strings.Join(out, "|")
}

// Two words changed in a long line are two marks, not one span covering
// everything between them.
func TestLineDiffMarksEachChangedRun(t *testing.T) {
	got := lineDiff("  If the work genuinely needs a wider blast radius, say so in your report rather",
		"  If the work needs a wider blast radius, name each extra file in your report rather")
	if s := spansOf(got[0]); s != "genuinely|say so" {
		t.Errorf("removed spans = %q", s)
	}
	if s := spansOf(got[1]); s != "name each extra file" {
		t.Errorf("added spans = %q", s)
	}
}

func TestLineDiffLeavesUnrelatedLinesUnpaired(t *testing.T) {
	got := lineDiff("Run the verify command in the foreground.", "Never push.")
	for _, l := range got {
		if len(l.changed) > 0 {
			t.Errorf("%q was paired with a line it shares nothing with", l.text)
		}
	}
}

func TestRenderDiffBoldsTheEditAndDimsTheRest(t *testing.T) {
	lines := lineDiff("keep\nold word here", "keep\nnew word here")
	plain := renderDiff(lines, false)
	if plain != "  keep\n- old word here\n+ new word here\n" {
		t.Errorf("plain = %q", plain)
	}
	colored := renderDiff(lines, true)
	for _, want := range []string{
		"\x1b[" + pal.Fail + ";1mold\x1b[0m", "\x1b[" + pal.Fail + ";2m word here\x1b[0m",
		"\x1b[" + pal.OK + ";1mnew\x1b[0m", "\x1b[" + pal.OK + ";2m word here\x1b[0m",
	} {
		if !strings.Contains(colored, want) {
			t.Errorf("colored diff lacks %q:\n%q", want, colored)
		}
	}
	if stripSGR(colored) != plain {
		t.Errorf("color changed the text: %q", stripSGR(colored))
	}
}

// A drive, a run or a review started without check still hears that the
// prompt it launches from has fallen behind the built-in.
func TestALaunchWarnsOfAStaleOverride(t *testing.T) {
	s := driveProjectWith(t, "\n[drive]\nmerge_gate = [\"stub-gate\"]\n\n[prompts]\nimplement = \"kickoff.tmpl\"\n")
	write(t, filepath.Join(s.cfg.Root, "kickoff.tmpl"), "Only this: {{ .ID }}.\n")
	write(t, filepath.Join(s.cfg.Root, "work", "01-first.md"), "---\ntitle: First\ndeps:\n---\n\n1. Do it.\n")
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	t.Chdir(s.cfg.Root)
	if out := dryRun(t, "1"); !strings.Contains(out, "kickoff.tmpl replaces the built-in implement template whole") {
		t.Errorf("the drive header does not warn:\n%s", out)
	}
	tmpl, err := prompts.Load(prompts.SlotImplement, filepath.Join(s.cfg.Root, "kickoff.tmpl"))
	if err != nil {
		t.Fatal(err)
	}
	if got := captureErr(t, func() { s.warnPrompt(tmpl) }); !strings.Contains(got, "warning: kickoff.tmpl replaces") {
		t.Errorf("warnPrompt wrote %q", got)
	}
}

func TestCheckDiffSaysWhenThereIsNoBaseToCompare(t *testing.T) {
	s := driveProjectWith(t, "\n[prompts]\nimplement = \"kickoff.tmpl\"\n")
	write(t, filepath.Join(s.cfg.Root, "kickoff.tmpl"),
		`{{define "scope"}}- Own only this item's packages.{{end}}`+"\n"+
			`{{define "intro"}}{{/* drover-base: 000000000000 */}}Do {{ .ID }}.{{end}}`+"\n")
	var b strings.Builder
	s.overrideDiffs(&b, false)
	out := b.String()
	for _, want := range []string{
		"implement/scope in kickoff.tmpl", "records no drover-base",
		"implement/intro in kickoff.tmpl", "no record of default 000000000000",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("check --diff lacks %q:\n%s", want, out)
		}
	}
}

func TestADiffAgainstAnOlderDefaultShowsWhatWent(t *testing.T) {
	scope := prompts.Blocks(prompts.SlotImplement)
	var current prompts.Block
	for _, b := range scope {
		if b.Name == "scope" {
			current = b
		}
	}
	got := renderDiff(lineDiff(current.Default+"\nAn older line.", current.Default), false)
	if !strings.Contains(got, "- An older line.") || strings.Count(got, "\n+ ") != 0 {
		t.Errorf("diff = %q", got)
	}
}
