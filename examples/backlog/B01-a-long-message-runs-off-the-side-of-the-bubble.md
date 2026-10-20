---
title: a long message runs off the side of the bubble
deps:
kind: bug
issue:
severity: M
skills:
runner: claude
---

Act as the maintainer of `herdsay`. A message longer than a terminal is wide draws as one line: the bubble's top and bottom run past the right edge, the terminal wraps them, and the cow sits under a tangle of underscores and dashes. Every user with something longer than a sentence to say hits it.

`bubble` in `say.go:31-40` draws the whole message on one line between `<` and `>`. It never measures the message against a width, so nothing breaks it into lines.

Classic `cowsay` wraps at 40 columns and draws a multi-line bubble with a different border per line: `/` and `\` on the first line, `|` on the middle lines, `\` and `/` on the last. A one-line message keeps `<` and `>`. Keep that convention, because it is what anyone who has used `cowsay` expects to see.

Requirements:

1. `bubble` wraps the message at word boundaries so no line of text is wider than 40 characters. A single word longer than 40 is broken at 40 rather than left to overflow.
2. A message that fits on one line draws exactly as it does today: the existing test in `say_test.go` passes unchanged.
3. A wrapped message draws a bubble whose every line has the same width, padded with spaces, using `/ \` on the first line, `| |` on the middle lines and `\ /` on the last. A two-line message has no middle lines.
4. Width counts characters, not bytes, so a message in UTF-8 draws a straight right edge.
5. New tests in `say_test.go` cover a two-line message, a message of three or more lines, and a word longer than 40 characters, each against the exact expected drawing. Each fails before this change.
