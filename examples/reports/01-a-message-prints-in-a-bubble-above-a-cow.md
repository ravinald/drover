# 1: a message prints in a bubble above a cow

- Item 1 · slug `a-message-prints-in-a-bubble-above-a-cow`
- runner: claude
- deps:
- kind: feature
- severity: M
- Captured 2026-10-20
- Source log `~/.cache/drover/herdsay/01-a-message-prints-in-a-bubble-above-a-cow.20261020-101322.jsonl`
- Outcome success · error false · 14 turns

## Report (self-reported)

Done: `herdsay moo` and `echo moo | herdsay` draw the same bubble above a cow, and an empty message exits 2.

`Say` in `say.go` returns the whole drawing, so the tests compare strings rather than reading a terminal. `main.go` only reads the message and prints what `Say` returns. The animal art lives in a map keyed by name, which is the lookup a second animal will need, but no flag selects one yet: the requirements name only the cow.

`go vet ./... && go test ./...` green. Three tests in `say_test.go` cover the drawing for `moo`, whitespace collapse and the unknown-animal error.

## Validation addendum

_Confirm each requirement against the merged code, not against the summary above._

- [x] R1: `herdsay moo` draws a one-line bubble with the cow's tail joining it. Compared against the exact string in `TestSayDrawsTheCowUnderTheBubble`.
- [x] R2: stdin is read when no arguments are given. Ran `echo moo | herdsay` and diffed it against `herdsay moo`.
- [x] R3: whitespace collapses. Covered by `TestBubbleCollapsesWhitespace`.
- [ ] R4: an empty message exits 2. The code path reads correctly, but nobody ran it from a shell and checked `$?`.
- [x] R5: `Say` refuses an unknown animal, naming it and the known ones. Covered by `TestSayRefusesAnUnknownAnimal`.
- [x] R6: `go vet ./... && go test ./...` passes.

**Divergences:** none.

**Fix:** none outstanding.

**Verify re-run:** `go vet ./... && go test ./...`, ok.

**Validated by / date:** independent · 2026-10-20
