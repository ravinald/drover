---
title: a message prints in a bubble above a cow
deps:
kind: feature
issue:
severity: M
skills:
runner: claude
---

Act as the maintainer of `herdsay`, a command that prints a message in a speech bubble above an ASCII animal. Nothing exists yet but the module. This item is the smallest version worth running: one animal, one line of text, read from the arguments or from stdin.

The drawing is the contract every later item builds on, so it has to be testable as a string rather than only by eye. Put the rendering in a function that returns the drawing, and keep `main` to reading input and printing what comes back.

Requirements:

1. `herdsay moo` prints `moo` inside a one-line speech bubble, with a cow drawn beneath it whose tail joins the bubble.
2. With no arguments, `herdsay` reads the message from stdin, so `echo moo | herdsay` prints the same drawing as `herdsay moo`.
3. Runs of whitespace in the message collapse to one space, so a message piped from a file draws the same as one typed on the command line.
4. An empty message exits 2 with an error that says how to pass one, rather than drawing an empty bubble.
5. `Say(msg, animal string) (string, error)` returns the whole drawing, and refuses an animal it does not know with an error naming it and the animals it does know.
6. Tests drive the drawing for `moo`, the whitespace collapse and the unknown-animal error. `go vet ./... && go test ./...` passes.
