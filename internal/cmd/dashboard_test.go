package cmd

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/dashboard"
	"github.com/ravinald/drover/internal/lease"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/runlog"
)

const dashboardForge = "https://forge.example/acme/widgets"

var dashboardNow = time.Date(2026, 10, 17, 18, 0, 0, 0, time.UTC)

// dashboardProject is a backlog holding every state a dashboard shows: two
// merged items, one with an audit level and a pull request; a parked item and a chain
// of two blocked behind it; a park waiting on an operator; a scaffold; an item
// that ran; a body that will not parse; and a running drive with one item
// mid-implement and one queued. One title carries markup, one an apostrophe
// and a bare <, and one item names the issue it was published as.
func dashboardProject(t *testing.T) *session {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	dir := shelfProject(t, map[string]string{
		"B01-merged.md":   body("Merged first", ""),
		"B02-parked.md":   body("Parked on no commits", ""),
		"B03-blocked.md":  body("Blocked by the park", "B2"),
		"B04-chained.md":  body("Blocked behind that", "B3"),
		"B05-scaffold.md": "---\ntitle: Scaffold\ndeps:\n---\nSTOP: requirements not yet written\n\n1.\n",
		"B06-audited.md":  body("Merged with a level", ""),
		"B07-markup.md":   body(`<script>alert(1)</script> & co`, ""),
		"B08-running.md":  body("Running now", ""),
		"B09-queued.md":   body("Queued next", ""),
		"B10-ran.md":      body("Ran clean", ""),
		"B11-broken.md":   "no frontmatter here at all\n",
		"B12-trunk.md":    body("Trunk went red", ""),
		"B13-quote.md":    body("The place's sign reads a < b", ""),
		"B14-issue.md":    "---\ntitle: Published as an issue\ndeps: B13\nissue: 42\n---\n\nRequirements:\n\n1. Do it.\n",
	}, nil)
	conf := filepath.Join(dir, config.FileName)
	raw, err := os.ReadFile(conf) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	write(t, conf, strings.Replace(string(raw), `optional = ["kind", "severity"]`, `optional = ["kind", "severity", "issue"]`, 1))
	rows := []string{
		`{"id":"B1","stage":"implement","date":"2026-01-15","ts":"2026-01-15T17:00:00Z"}`,
		`{"id":"B1","merged":true,"date":"2026-01-15","ts":"2026-01-15T18:00:00Z"}`,
		`{"id":"drive","drive":"D1","label":"night run","queued":["B2","B6"],"date":"2026-10-16","ts":"2026-10-16T06:00:00Z"}`,
		`{"id":"B6","stage":"implement","drive":"D1","date":"2026-10-16","ts":"2026-10-16T06:01:00Z"}`,
		`{"id":"B6","pr":236,"branch":"b06-audited","date":"2026-10-16","ts":"2026-10-16T06:40:00Z"}`,
		`{"id":"B6","merged":true,"date":"2026-10-16","ts":"2026-10-16T07:00:00Z"}`,
		`{"id":"B6","reviewer":"claude","level":"reviewed","code_by":"agent","independent":true,"date":"2026-10-16","ts":"2026-10-16T08:00:00Z"}`,
		`{"id":"B2","stage":"implement","drive":"D1","date":"2026-10-16","ts":"2026-10-16T07:01:00Z"}`,
		`{"id":"B2","park":"no_commits","detail":"runner_exit","date":"2026-10-16","ts":"2026-10-16T07:30:00Z"}`,
		`{"id":"B12","park":"trunk_red","detail":"vuln","date":"2026-10-17","ts":"2026-10-17T09:00:00Z"}`,
		`{"id":"drive","drive":"D2","label":"day run","queued":["B8","B9"],"jobs":1,"date":"2026-10-17","ts":"2026-10-17T17:00:00Z"}`,
		`{"id":"B8","stage":"implement","drive":"D2","date":"2026-10-17","ts":"2026-10-17T17:56:00Z"}`,
	}
	ledgerText := strings.Join(rows, "\n") + "\n"
	for _, r := range timedRuns(5) {
		raw, err := jsonLine(r)
		if err != nil {
			t.Fatal(err)
		}
		ledgerText += raw
	}
	write(t, filepath.Join(dir, "ledger.jsonl"), ledgerText)
	isolate(t)
	s := openIn(t, dir)

	for _, it := range s.backlog.Items {
		if it.ID.Bare() != "B10" {
			continue
		}
		path := filepath.Join(s.cfg.LogDir(), runlog.Name(s.cfg.Paths.LogPrefix+runlog.Base(it.ID.Stem(), it.Slug), dashboardNow))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		write(t, path, `{"type":"result","subtype":"success","is_error":false,"num_turns":1,"total_cost_usd":0.01}`+"\n")
	}

	h, err := lease.Acquire(filepath.Join(t.TempDir(), lease.FileName), lease.Lease{Command: "drive", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Update(func(l *lease.Lease) {
		l.Drive, l.Label, l.Started = "D2", "day run", "2026-10-17T17:00:00Z"
		l.Running, l.Queue, l.Jobs = []string{"B8"}, []string{"B9"}, 1
	}); err != nil {
		t.Fatal(err)
	}
	held = h
	t.Cleanup(func() { held = nil; _ = h.Release() })

	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	s.zone = la
	s.offline = true
	forge := dashboardForge
	s.forgeURL = &forge
	was := version
	version = "v9.9.9-test"
	t.Cleanup(func() { version = was })
	return s
}

func jsonLine(r ledger.Row) (string, error) {
	b, err := json.Marshal(r)
	return string(b) + "\n", err
}

func renderBuiltin(t *testing.T, s *session, name string) string {
	t.Helper()
	out, err := s.renderTemplate(dashboard.Prefix+name, s.statusReport(false, false, dashboardNow), dashboardNow)
	if err != nil {
		t.Fatalf("render builtin:%s: %v", name, err)
	}
	return string(out)
}

// Each built-in template renders the fixture of every state as it did when
// the golden was written. DROVER_UPDATE_GOLDEN=1 rewrites them, which is only
// right on a change that means to alter the dashboard.
func TestBuiltinTemplatesMatchTheirGoldens(t *testing.T) {
	stubPATH(t)
	s := dashboardProject(t)
	for _, c := range []struct{ name, file string }{
		{"obsidian", "dashboard-obsidian.md.golden"},
		{"html", "dashboard-html.html.golden"},
	} {
		// The broken body's error names its file under the fixture's temp root.
		got := strings.ReplaceAll(renderBuiltin(t, s, c.name), s.cfg.Root, "<root>")
		path := filepath.Join(goldenDir(t), c.file)
		if os.Getenv("DROVER_UPDATE_GOLDEN") != "" {
			write(t, path, got)
		}
		want, err := os.ReadFile(path) // #nosec G304 -- the test's own fixture
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Errorf("builtin:%s no longer renders its golden:\n--- got\n%s\n--- want\n%s", c.name, got, want)
		}
	}
}

// The golden holds what the layout promises, so a regenerated golden that lost
// a section, a state or a link fails here rather than being accepted unread.
func TestBuiltinTemplatesCarryTheLayout(t *testing.T) {
	stubPATH(t)
	s := dashboardProject(t)
	md := renderBuiltin(t, s, "obsidian")
	page := renderBuiltin(t, s, "html")

	if !strings.HasPrefix(md, "%% drover builtin:obsidian") {
		t.Errorf("the Obsidian render does not open by naming where it came from:\n%.120s", md)
	}
	for name, out := range map[string]string{"obsidian": md, "html": page} {
		var at []int
		for _, h := range []string{"Needs you", "Running drive", "Open", "Done", "Rendered "} {
			i := strings.Index(out, h)
			if i < 0 {
				t.Errorf("builtin:%s has no %q section", name, h)
			}
			at = append(at, i)
		}
		for i := 1; i < len(at); i++ {
			if at[i] < at[i-1] {
				t.Errorf("builtin:%s sections out of order at %d: %v", name, i, at)
			}
		}
		for _, class := range []string{"done", "ready", "running", "blocked", "scaffold", "parked", "broken", "needs-operator", "unaudited"} {
			if !strings.Contains(out, "drover-"+class+`"`) {
				t.Errorf("builtin:%s never carries class drover-%s", name, class)
			}
		}
		for _, want := range []string{"Drive D1", "Drive D2", "day run", "fixture", "v9.9.9-test", "America/Los_Angeles", dashboardForge + "/pull/236"} {
			if !strings.Contains(out, want) {
				t.Errorf("builtin:%s lacks %q", name, want)
			}
		}
		if strings.Index(out, "Merged with a level") > strings.Index(out, "Merged first") {
			t.Errorf("builtin:%s lists done items oldest first", name)
		}
		// B10 ran and B8 is the lease's: each sits under its own state word,
		// and both carry the running class.
		open := out[strings.Index(out, "Open"):strings.Index(out, "Done")]
		for title, word := range map[string]string{"Ran clean": "implemented", "Running now": "running"} {
			at := strings.Index(open, title)
			if at < 0 {
				t.Errorf("builtin:%s lists no %q in Open", name, title)
				continue
			}
			heading := open[strings.LastIndex(open[:at], "drover-"):]
			if want := `drover-running">` + word + "</span>"; !strings.HasPrefix(heading, want) {
				t.Errorf("builtin:%s lists %q under %.40q, want %q", name, title, heading, want)
			}
		}
	}
	for _, want := range []string{"stage implement", "4m elapsed"} {
		if !strings.Contains(md, want) {
			t.Errorf("the Obsidian render lacks %q", want)
		}
	}
	if want := `<td>implement</td><td class="num">4m</td>`; !strings.Contains(page, want) {
		t.Errorf("the HTML Running table lacks the stage and elapsed cells %q", want)
	}
	for _, link := range []string{"[[#^b2|B2]]", "[[#^b3|B3]]", "^b4\n"} {
		if !strings.Contains(md, link) {
			t.Errorf("the Obsidian render lacks the chain link %q", link)
		}
	}
	for _, link := range []string{`href="#item-b2"`, `href="#item-b3"`, `id="item-b4"`, `<meta http-equiv="refresh" content="60" />`} {
		if !strings.Contains(page, link) {
			t.Errorf("the HTML render lacks %q", link)
		}
	}
}

// The page parses with every element closed, and the only addresses in it are
// the forge's: no font, script or stylesheet is fetched from anywhere.
func TestBuiltinHTMLParsesAndRequestsNothing(t *testing.T) {
	stubPATH(t)
	page := renderBuiltin(t, dashboardProject(t), "html")

	dec := xml.NewDecoder(strings.NewReader(page))
	dec.Strict = true
	dec.Entity = xml.HTMLEntity
	var root string
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("the page does not parse: %v", err)
		}
		if el, ok := tok.(xml.StartElement); ok && root == "" {
			root = el.Name.Local
		}
	}
	if root != "html" || !strings.HasPrefix(page, "<!DOCTYPE html>") {
		t.Errorf("the page opens %q under root <%s>, want an HTML5 document", page[:min(len(page), 20)], root)
	}
	links := regexp.MustCompile(`(?i)https?:[^\s"'<>)]*`).FindAllString(page, -1)
	if len(links) == 0 {
		t.Error("the page links no pull request; this arm has nothing to prove")
	}
	for _, u := range links {
		if !strings.HasPrefix(u, dashboardForge+"/") {
			t.Errorf("the page names %s, which is not a forge link", u)
		}
	}
	for _, tag := range []string{"<script", "<link", "<img", "@import", "url("} {
		if strings.Contains(page, tag) {
			t.Errorf("the page carries %s, which fetches or runs something", tag)
		}
	}
}

// A title holding markup reaches the page as text.
func TestBuiltinHTMLEscapesATitle(t *testing.T) {
	stubPATH(t)
	page := renderBuiltin(t, dashboardProject(t), "html")
	if strings.Contains(page, "<script>alert") {
		t.Fatal("a title's <script> reached the page as markup")
	}
	if !strings.Contains(page, "&lt;script&gt;alert(1)&lt;/script&gt; &amp; co") {
		t.Error("the title is not on the page as escaped text")
	}
}

// Every item section is a table, an item naming an issue links its id there,
// and every in-page link lands on a row.
func TestBuiltinHTMLTabulatesTheItems(t *testing.T) {
	stubPATH(t)
	page := renderBuiltin(t, dashboardProject(t), "html")

	at := func(h string) int {
		i := strings.Index(page, h)
		if i < 0 {
			t.Fatalf("the page has no %q", h)
		}
		return i
	}
	bounds := []int{at("<h2>Needs you</h2>"), at("<h2>Running drive</h2>"), at("<h2>Open</h2>"), at("<h2>Done</h2>"), at("<h2>Trends</h2>")}
	for i, name := range []string{"Needs you", "Running drive", "Open", "Done"} {
		section := page[bounds[i]:bounds[i+1]]
		if strings.Contains(section, "<ul") || strings.Contains(section, "<li") {
			t.Errorf("%s still renders a list", name)
		}
		if !strings.Contains(section, "<table") || !strings.Contains(section, "<thead>") {
			t.Errorf("%s renders no table with a header row", name)
		}
	}

	if want := `<a class="id" href="` + dashboardForge + `/issues/42">B14</a>`; !strings.Contains(page, want) {
		t.Errorf("B14's id does not link to its issue: want %s", want)
	}
	if strings.Contains(page, `href="`+dashboardForge+`/issues/0"`) {
		t.Error("an item naming no issue links to issue 0")
	}

	ids := map[string]bool{}
	for _, m := range regexp.MustCompile(`<tr id="([^"]+)"`).FindAllStringSubmatch(page, -1) {
		if ids[m[1]] {
			t.Errorf("row id %s appears twice", m[1])
		}
		ids[m[1]] = true
	}
	for _, m := range regexp.MustCompile(`<h2 id="([^"]+)"`).FindAllStringSubmatch(page, -1) {
		ids[m[1]] = true
	}
	links := regexp.MustCompile(`href="#([^"]+)"`).FindAllStringSubmatch(page, -1)
	if len(links) == 0 {
		t.Fatal("the page carries no in-page link; this arm has nothing to prove")
	}
	for _, m := range links {
		if !ids[m[1]] {
			t.Errorf("the link #%s names no row", m[1])
		}
	}
}

// A leased item the ledger has no stage row for yet shows the same `-` in
// Running as an empty cell anywhere else on the page.
func TestBuiltinHTMLRunningItemWithNoStageShowsADash(t *testing.T) {
	stubPATH(t)
	s := dashboardProject(t)
	if err := held.Update(func(l *lease.Lease) { l.Running, l.Queue = []string{"B8", "B9"}, nil }); err != nil {
		t.Fatal(err)
	}
	page := renderBuiltin(t, s, "html")
	from, to := strings.Index(page, "<h2>Running drive</h2>"), strings.Index(page, "<h2>Open</h2>")
	if from < 0 || to < from {
		t.Fatal("the page has no Running drive section")
	}
	running := page[from:to]
	if !strings.Contains(running, `href="#item-b9">B9</a>`) {
		t.Fatal("B9 is not in Running; this test has nothing to prove")
	}
	if !strings.Contains(running, `<td>-</td><td class="num">-</td>`) {
		t.Errorf("B9's stage and elapsed cells are not both -:\n%s", running)
	}
	if strings.Contains(running, "unknown") {
		t.Errorf("Running says unknown where a cell is empty:\n%s", running)
	}
}

// Markdown renders a quote as typed, so the Obsidian page escapes only what
// would open a tag or an entity.
func TestBuiltinObsidianEscapesOnlyMarkup(t *testing.T) {
	stubPATH(t)
	md := renderBuiltin(t, dashboardProject(t), "obsidian")
	if want := "The place's sign reads a &lt; b"; !strings.Contains(md, want) {
		t.Errorf("the Obsidian render lacks %q", want)
	}
	for _, entity := range []string{"&#39;", "&#34;"} {
		if strings.Contains(md, entity) {
			t.Errorf("the Obsidian render writes %s, which Obsidian shows literally", entity)
		}
	}
}

// An unknown builtin: name is refused naming every one there is, on the
// command line and in [[status.render]], and --html is builtin:html.
func TestUnknownBuiltinIsRefusedWithTheNames(t *testing.T) {
	stubPATH(t)
	dir := t.TempDir()
	t.Chdir(scopeProject(t).cfg.Root)
	resetStatusFlags(t)
	err := execute(t, "status", "--no-tracker", "--template", "builtin:nope")
	if err == nil || !strings.Contains(err.Error(), "builtin:html, builtin:obsidian") {
		t.Errorf("builtin:nope: err = %v, want the built-in names", err)
	}
	resetStatusFlags(t)
	err = execute(t, "status", "--print-template", "builtin:nope")
	if err == nil || !strings.Contains(err.Error(), "builtin:html, builtin:obsidian") {
		t.Errorf("--print-template builtin:nope: err = %v, want the built-in names", err)
	}

	resetStatusFlags(t)
	page := capture(t, func() { err = execute(t, "status", "--no-tracker", "--html", "-o", filepath.Join(dir, "s.html")) })
	if err != nil || page != "" {
		t.Fatalf("--html -o: err %v, stdout %q", err, page)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "s.html")); !strings.HasPrefix(string(b), "<!DOCTYPE html>") { // #nosec G304 -- written above
		t.Errorf("--html did not render builtin:html:\n%.80s", b)
	}
	resetStatusFlags(t)
	if err := execute(t, "status", "--no-tracker", "--html", "--sev", "H"); err == nil {
		t.Error("--sev beside a built-in template was accepted; it renders every item")
	}

	resetStatusFlags(t)
	src, _ := dashboard.Source("obsidian")
	got := capture(t, func() { err = execute(t, "status", "--print-template", "builtin:obsidian") })
	if err != nil || got != src {
		t.Errorf("--print-template printed %d bytes (err %v), want the template's %d", len(got), err, len(src))
	}

	root, _ := os.Getwd()
	raw, err := os.ReadFile(filepath.Join(root, config.FileName)) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, config.FileName), string(raw)+"\n[[status.render]]\ntemplate = \"builtin:nope\"\npath = \"s.md\"\n")
	if _, err := config.Load(filepath.Join(root, config.FileName)); err == nil || !strings.Contains(err.Error(), "builtin:html, builtin:obsidian") {
		t.Errorf("a [[status.render]] naming builtin:nope loaded: %v", err)
	}
}

// The Obsidian snippet styles every class the class function can return.
func TestPrintCSSNamesEveryClass(t *testing.T) {
	stubPATH(t)
	s := viewProject(t, "")
	resetStatusFlags(t)
	var err error
	css := capture(t, func() { err = execute(t, "status", "--print-css", "obsidian") })
	if err != nil {
		t.Fatalf("--print-css obsidian: %v", err)
	}
	classes := map[string]bool{}
	for _, v := range []string{stateReady, stateRan, stateBlocked, stateScaffold, stateDone, stateBroken,
		ledger.LevelClaimed, ledger.LevelReviewed, ledger.LevelVerified, "UNAUDITED", "no_commits", "trunk_red", "needs_operator"} {
		classes[s.class(v)] = true
	}
	if len(classes) != 9 {
		t.Fatalf("class returned %d values, want E50's 9: %v", len(classes), classes)
	}
	for c := range classes {
		if !strings.Contains(css, ".drover-"+c+" ") && !strings.Contains(css, ".drover-"+c+",") {
			t.Errorf("--print-css obsidian has no rule for drover-%s", c)
		}
	}
	resetStatusFlags(t)
	if err := execute(t, "status", "--print-css", "html"); err == nil || !strings.Contains(err.Error(), "obsidian") {
		t.Errorf("--print-css html: err = %v, want a refusal naming obsidian", err)
	}
}

// A [[status.render]] pair naming a built-in renders it rather than resolving
// builtin:obsidian as a file under the project root, and refresh = 0 leaves the
// page with no reload.
func TestStatusRenderTakesABuiltinAndRefresh(t *testing.T) {
	stubPATH(t)
	root := scopeProject(t).cfg.Root
	t.Chdir(root)
	raw, err := os.ReadFile(filepath.Join(root, config.FileName)) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, config.FileName), string(raw)+"\n[status]\nrefresh = 0\n"+
		"\n[[status.render]]\ntemplate = \"builtin:obsidian\"\npath = \"view.md\"\n"+
		"\n[[status.render]]\ntemplate = \"builtin:html\"\npath = \"view.html\"\n")
	resetStatusFlags(t)
	if err := execute(t, "status", "--render", "--no-tracker"); err != nil {
		t.Fatalf("status --render: %v", err)
	}
	md, _ := os.ReadFile(filepath.Join(root, "view.md"))     // #nosec G304 -- written above
	page, _ := os.ReadFile(filepath.Join(root, "view.html")) // #nosec G304 -- written above
	if !strings.HasPrefix(string(md), "%% drover builtin:obsidian") {
		t.Errorf("view.md is not builtin:obsidian:\n%.80s", md)
	}
	if !strings.HasPrefix(string(page), "<!DOCTYPE html>") || strings.Contains(string(page), "http-equiv") {
		t.Errorf("view.html is not builtin:html with refresh off:\n%.400s", page)
	}
}

// [status] index turns builtin:html's header into a breadcrumb back to the
// cross-project index, and without it the header is the project name alone.
func TestStatusIndexLinksTheHeaderBackToTheIndex(t *testing.T) {
	stubPATH(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for extra, want := range map[string]string{
		"\n[status]\nindex = \"index.html\"\n": `<h1><a href="index.html">drover</a> / example</h1>`,
		"":                                     "<h1>example</h1>",
	} {
		t.Chdir(fixture(t, extra).cfg.Root)
		page := runStatus(t, "--html")
		if !strings.Contains(page, want) {
			t.Errorf("with %q the header is not %s:\n%.1200s", extra, want, page)
		}
		if extra == "" && strings.Contains(page, ">drover</a>") {
			t.Errorf("with no index the page links drover somewhere:\n%.1200s", page)
		}
	}
}

// groups prints each open item under its own state, styled by its class: with
// no lease every item keeps its backlog state, and an item the lease names is
// running even when a park from an earlier run still stands on it.
func TestGroupsNamesTheStateAndHonorsTheLease(t *testing.T) {
	s := viewProject(t, "")
	var items []any
	for _, it := range []map[string]any{
		{"id": "A1", "state": stateRan},
		{"id": "A2", "state": stateReady},
		{"id": "A3", "state": stateReady, "park": "no_commits"},
		{"id": "A4", "state": stateDone},
	} {
		items = append(items, it)
	}
	flat := func(holder any) string {
		var b strings.Builder
		for _, g := range s.groups(items, nil, holder) {
			for _, st := range g["states"].([]map[string]any) {
				b.WriteString(st["state"].(string) + "/" + st["class"].(string) + ":")
				for _, it := range st["items"].([]any) {
					b.WriteString(" " + it.(map[string]any)["id"].(string))
				}
				b.WriteString("; ")
			}
		}
		return b.String()
	}
	for _, c := range []struct {
		holder any
		want   string
	}{
		{nil, "parked/parked: A3; implemented/running: A1; ready/ready: A2; "},
		{map[string]any{"running": []any{"A2", "A4"}}, "parked/parked: A3; running/running: A2; implemented/running: A1; "},
		{map[string]any{"running": []any{"A3"}}, "running/running: A3; implemented/running: A1; ready/ready: A2; "},
	} {
		if got := flat(c.holder); got != c.want {
			t.Errorf("groups with lease %v:\n got %s\nwant %s", c.holder, got, c.want)
		}
	}
}

// A paused drive lets an item finish the stage it is inside before it holds
// it. Until the hold is written the item is still working, so it reads
// pausing; once held it reads paused; with no pause it reads running.
func TestARunningItemReadsPausingUntilTheDriveHoldsIt(t *testing.T) {
	lease := func(paused bool, holds ...string) map[string]any {
		var hs []any
		for _, id := range holds {
			hs = append(hs, map[string]any{"id": id, "at": "review"})
		}
		return map[string]any{"paused": paused, "running": []any{"F1", "F2"}, "holds": hs}
	}
	for _, c := range []struct {
		name    string
		lease   map[string]any
		f1, f2  string
		waiting int
	}{
		{"not paused", lease(false), "running", "running", 0},
		{"paused, nothing held yet", lease(true), "pausing", "pausing", 2},
		{"paused, one held", lease(true, "F1"), "paused", "pausing", 1},
		{"paused, all held", lease(true, "F1", "F2"), "paused", "paused", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := runningWord(c.lease, "F1"); got != c.f1 {
				t.Errorf("F1 reads %q, want %q", got, c.f1)
			}
			if got := runningWord(c.lease, "F2"); got != c.f2 {
				t.Errorf("F2 reads %q, want %q", got, c.f2)
			}
			if got := pausingCount(c.lease); got != c.waiting {
				t.Errorf("the drive waits on %d items, want %d", got, c.waiting)
			}
		})
	}
	if got := heldAt(lease(true, "F1"), "F1"); got != "review" {
		t.Errorf("heldAt = %q, want review", got)
	}
}
