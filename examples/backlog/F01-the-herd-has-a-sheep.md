---
title: the herd has a sheep
deps:
kind: feature
issue:
severity: L
skills:
runner: claude
---

Act as the maintainer of `herdsay`. A herd of one is a cow, and the name promises more. `main` in `main.go:28` always asks `Say` for `"cow"`, and `animals` in `say.go:16-20` holds only the cow, so there is no way to choose another.

The lookup already exists: `Say` reads the art from `animals` and refuses a name it does not hold. Adding an animal is a new entry plus a flag to choose it, and nothing about the bubble changes.

Requirements:

1. `herdsay --animal sheep moo` draws a sheep under the bubble. Its art starts with the same two-line tail as the cow's, so it joins the bubble the same way.
2. `--animal` defaults to `cow`, so `herdsay moo` draws exactly what it draws today.
3. `herdsay --animal yak moo` exits 2 with an error naming `yak` and listing `cow, sheep`, and prints no drawing.
4. `herdsay --help` lists `--animal` and the animals it accepts.
5. A test in `say_test.go` draws the sheep against its exact expected art, and the unknown-animal test asserts both known names.
