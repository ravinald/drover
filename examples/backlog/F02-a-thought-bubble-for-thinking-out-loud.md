---
title: a thought bubble for thinking out loud
deps: B1
kind: feature
issue:
severity: L
skills:
runner: claude
---

Act as the maintainer of `herdsay`. Some messages are thoughts rather than speech, and `cowsay` draws those differently: `cowthink` puts the text in a round thought bubble and replaces the tail with a trail of `o` characters. `herdsay` can only speak.

This item depends on B1 because both change `bubble` in `say.go:31-40`. B1 decides how a long message wraps and which borders each line gets; this item swaps those borders for round ones. Built before B1, it would be built twice.

Requirements:

1. `herdsay --think hmm` draws `hmm` between `(` and `)` on every line, a one-line message included, instead of the speech borders.
2. A wrapped thought wraps exactly as a wrapped speech bubble does: same width, same line breaks. Only the border characters differ.
3. The tail under a thought is `o` where a speech bubble's is `\`, on both tail lines, so the trail reads as rising from the animal.
4. `--think` combines with every other flag `herdsay` accepts, and `herdsay --help` lists it.
5. Tests in `say_test.go` draw a one-line thought and a wrapped thought against their exact expected output.
