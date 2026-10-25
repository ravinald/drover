package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The decoder replaces a table held in a map whole, so a project destination
// has to be layered over the per-user one of the same name by hand: a project
// that sets only events keeps the url and token the per-user file names, the
// key it does set wins, and a destination of another name stands beside it.
func TestNotifyLayersProjectOverUserKeyByKey(t *testing.T) {
	withGlobal(t, `
[notify]
coalesce = "2m"
[notify.destinations.phone]
type       = "ntfy"
url        = "https://ntfy.example.com/drover"
token_file = "~/.config/drover/ntfy-token"
events     = { needs_operator = "high", parked = "default" }
[notify.destinations.desk]
type   = "command"
argv   = ["notify-send", "drover"]
events = { needs_operator = "default" }
`)
	root := project(t, schemaLine+`project = "widget"
[notify]
quiet_hours = "22:00-07:00"
[notify.destinations.phone]
events = { exit = "urgent" }
`)
	c, err := Load(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := c.Notifications()
	if r.Coalesce != 2*time.Minute || r.Quiet == nil || r.Quiet.Start != 22*60 || r.Quiet.End != 7*60 {
		t.Errorf("coalesce %v quiet %+v, want the per-user 2m and the project's 22:00-07:00", r.Coalesce, r.Quiet)
	}
	if len(r.Destinations) != 2 || r.Destinations[0].Name != "desk" || r.Destinations[1].Name != "phone" {
		t.Fatalf("destinations %+v, want desk and phone", r.Destinations)
	}
	phone := r.Destinations[1]
	home, _ := os.UserHomeDir()
	if phone.URL != "https://ntfy.example.com/drover" || phone.TokenFile != filepath.Join(home, ".config/drover/ntfy-token") {
		t.Errorf("phone url %q token_file %q, want the per-user ones with ~ expanded", phone.URL, phone.TokenFile)
	}
	if p, ok := phone.Priority("exit"); !ok || p != "urgent" {
		t.Errorf("phone routes exit at %q %v, want the project's urgent", p, ok)
	}
	if _, ok := phone.Priority("parked"); ok {
		t.Error("phone still routes parked: the project's events replace the per-user ones")
	}
	if p, ok := phone.Priority(EventExitUndone); !ok || p != "high" {
		t.Errorf("exit_undone routes at %q %v, want high wherever exit is routed", p, ok)
	}
	if desk := r.Destinations[0]; desk.Type != DestCommand || len(desk.Argv) != 2 {
		t.Errorf("desk reads %+v", desk)
	}
}

// Each table drover would have to guess about is a load error naming the
// destination, the key and the file that wrote it.
func TestNotifyLoadErrors(t *testing.T) {
	ntfy := "[notify.destinations.phone]\ntype = \"ntfy\"\nevents = { needs_operator = \"high\" }\n"
	for _, c := range []struct {
		name, table, want string
	}{
		{"unknown key", ntfy + `url = "https://h/t"` + "\ntopic = \"x\"", "notify.destinations.phone.topic is not a key a ntfy destination takes"},
		{"a command key on ntfy", ntfy + `url = "https://h/t"` + "\nargv = [\"x\"]", "notify.destinations.phone.argv is not a key"},
		{"unknown type", "[notify.destinations.phone]\ntype = \"pager\"\nevents = { exit = \"high\" }", "notify.destinations.phone.type is pager"},
		{"no type", "[notify.destinations.phone]\nevents = { exit = \"high\" }", "notify.destinations.phone has no type"},
		{"malformed url", ntfy + `url = "https://h/%zz"`, "notify.destinations.phone.url"},
		{"http to a remote host", ntfy + `url = "http://ntfy.example.com/t"`, "notify.destinations.phone.url"},
		{"no topic", ntfy + `url = "https://ntfy.example.com/"`, "names no topic"},
		{"credentials in the url", ntfy + `url = "https://u:p@h/t"`, "carries credentials"},
		{"no url", ntfy, "notify.destinations.phone has no url"},
		{"unknown event", "[notify.destinations.phone]\ntype = \"ntfy\"\nurl = \"https://h/t\"\nevents = { landed = \"high\" }", "notify.destinations.phone.events.landed is not an event"},
		{"unknown priority", "[notify.destinations.phone]\ntype = \"ntfy\"\nurl = \"https://h/t\"\nevents = { exit = \"loud\" }", "notify.destinations.phone.events.exit is loud"},
		{"events as a list", "[notify.destinations.phone]\ntype = \"ntfy\"\nurl = \"https://h/t\"\nevents = [\"exit\"]", "notify.destinations.phone.events"},
		{"no events", "[notify.destinations.phone]\ntype = \"ntfy\"\nurl = \"https://h/t\"", "notify.destinations.phone has no events"},
		{"relative token file", ntfy + `url = "https://h/t"` + "\ntoken_file = \"secrets/token\"", "notify.destinations.phone.token_file"},
		{"command with no argv", "[notify.destinations.desk]\ntype = \"command\"\nevents = { exit = \"high\" }", "notify.destinations.desk has no argv"},
		{"command with an empty argv", "[notify.destinations.desk]\ntype = \"command\"\nargv = []\nevents = { exit = \"high\" }", "notify.destinations.desk.argv"},
		{"bad coalesce", "[notify]\ncoalesce = \"soon\"", "notify.coalesce is \"soon\""},
		{"bad quiet hours", "[notify]\nquiet_hours = \"22-7\"", "notify.quiet_hours is \"22-7\""},
		{"empty quiet hours window", "[notify]\nquiet_hours = \"07:00-07:00\"", "same minute"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(project(t, schemaLine+"project = \"widget\"\n"+c.table+"\n"))
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), FileName) {
				t.Errorf("load error %v, want one naming %q in %s", err, c.want, FileName)
			}
		})
	}

	// http is allowed where the token cannot leave the machine.
	for _, url := range []string{"http://127.0.0.1:8080/t", "http://localhost/t", "http://[::1]/t"} {
		if _, err := Load(project(t, schemaLine+"project = \"widget\"\n[notify.destinations.p]\ntype = \"ntfy\"\nurl = \""+url+"\"\nevents = { exit = \"low\" }\n")); err != nil {
			t.Errorf("%s refused: %v", url, err)
		}
	}

	// A bad value the per-user file wrote is named there, not in the project.
	withGlobal(t, "[notify.destinations.phone]\ntype = \"ntfy\"\nurl = \"https://h/t\"\nevents = { exit = \"loud\" }\n")
	_, err := Load(project(t, schemaLine+"project = \"widget\"\n"))
	if err == nil || !strings.Contains(err.Error(), GlobalFileName) {
		t.Errorf("a per-user value refused as %v, want the error to name %s", err, GlobalFileName)
	}
}

// The keys [notify] replaced are refused wherever they are written, naming the
// key and the command that rewrites them, and nothing reads them meanwhile.
func TestRemovedNotifierKeysAreRefused(t *testing.T) {
	for _, c := range []struct{ name, table, key string }{
		{"argv", "[supervise]\nnotify = [\"notify-send\", \"drover\"]\n", "supervise.notify"},
		{"ntfy", "[supervise.ntfy]\nurl = \"https://h/t\"\nevents = [\"exit\"]\n", "supervise.ntfy"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(project(t, schemaLine+"project = \"widget\"\n"+c.table))
			if err == nil || !strings.Contains(err.Error(), c.key+" was removed") || !strings.Contains(err.Error(), MigrateNotify) {
				t.Errorf("load error %v, want the removal of %s naming %s", err, c.key, MigrateNotify)
			}
			withGlobal(t, c.table)
			_, err = Load(project(t, schemaLine+"project = \"widget\"\n"))
			if err == nil || !strings.Contains(err.Error(), GlobalFileName) || !strings.Contains(err.Error(), MigrateNotify) {
				t.Errorf("per-user load error %v, want the removal named in %s", err, GlobalFileName)
			}
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		})
	}
}

// migrate-notify.sh turns both removed keys into destinations that load and
// route as the originals did, in a project config and a per-user config,
// changing no other line. A second run, and a file with neither key, change
// nothing.
func TestMigrateNotifyRewritesBothKeys(t *testing.T) {
	script, err := filepath.Abs("../../scripts/migrate-notify.sh")
	if err != nil {
		t.Fatal(err)
	}
	const projectFile = `project = "widget"   # named for the banner

[paths]
backlog = "items"

# who acts on what
[supervise]
resolve_conflict = "operator"
notify = ["sh", "-c", '''
jq -r '[.action, .run] | join(": ")' | notify-send drover
''']  # the desktop
template = "brief.tmpl"

[review]
max_review_rounds = 3
`
	const userFile = `# per-user
[supervise.ntfy]  # the phone
url        = "https://ntfy.example.com/drover"
token_file = "~/.config/drover/ntfy-token"
events     = ["needs_operator", "parked",
              "exit"]
priority   = "urgent"   # loud

[usage.weekly]
notify_remaining = 20
`
	root := project(t, schemaLine+projectFile)
	withGlobal(t, userFile)
	user := GlobalPath()
	for _, path := range []string{filepath.Join(root, FileName), user} {
		if out, err := exec.Command("bash", script, path).CombinedOutput(); err != nil { // #nosec G204 -- the repository's own script
			t.Fatalf("migrate %s: %v\n%s", path, err, out)
		}
	}
	gotProject, _ := os.ReadFile(filepath.Join(root, FileName)) // #nosec G304 -- written above
	argv := "[\"sh\", \"-c\", '''\njq -r '[.action, .run] | join(\": \")' | notify-send drover\n''']  # the desktop\n"
	wantProject := schemaLine + strings.Replace(projectFile, "notify = "+argv, "", 1) +
		"\n[notify.destinations.command]\ntype = \"command\"\nargv = " + argv +
		"events = { needs_operator = \"default\" }\n"
	if string(gotProject) != wantProject {
		t.Errorf("the project file reads\n%s\nwant\n%s", gotProject, wantProject)
	}
	gotUser, _ := os.ReadFile(user) // #nosec G304 -- written above
	wantUser := `# per-user
[notify.destinations.ntfy]  # the phone
type = "ntfy"
url        = "https://ntfy.example.com/drover"
token_file = "~/.config/drover/ntfy-token"
events = { needs_operator = "urgent", parked = "urgent", exit = "urgent" }

[usage.weekly]
notify_remaining = 20
`
	if string(gotUser) != wantUser {
		t.Errorf("the per-user file reads\n%s\nwant\n%s", gotUser, wantUser)
	}

	c, err := Load(root)
	if err != nil {
		t.Fatalf("the migrated configs do not load: %v", err)
	}
	r := c.Notifications()
	if len(r.Destinations) != 2 {
		t.Fatalf("destinations %+v, want command and ntfy", r.Destinations)
	}
	cmd, phone := r.Destinations[0], r.Destinations[1]
	if cmd.Type != DestCommand || strings.Join(cmd.Argv, "|") != "sh|-c|jq -r '[.action, .run] | join(\": \")' | notify-send drover\n" || len(cmd.Events) != 1 || cmd.Events["needs_operator"] != "default" {
		t.Errorf("the command destination reads %+v", cmd)
	}
	if phone.Type != DestNtfy || phone.URL != "https://ntfy.example.com/drover" || phone.TokenFile == "" || len(phone.Events) != 3 {
		t.Errorf("the ntfy destination reads %+v", phone)
	}
	for _, e := range []string{"needs_operator", "parked", "exit"} {
		if p, ok := phone.Priority(e); !ok || p != "urgent" {
			t.Errorf("ntfy routes %s at %q %v, want urgent as the original priority", e, p, ok)
		}
	}
	if _, ok := phone.Priority("merged"); ok {
		t.Error("ntfy routes merged, which the original did not name")
	}

	for _, path := range []string{filepath.Join(root, FileName), user} {
		before, _ := os.ReadFile(path)                                                                                                  // #nosec G304 -- written above
		if out, err := exec.Command("bash", script, path).CombinedOutput(); err != nil || !strings.Contains(string(out), "unchanged") { // #nosec G204 -- the repository's own script
			t.Errorf("a second run on %s: %v\n%s", path, err, out)
		}
		if after, _ := os.ReadFile(path); string(after) != string(before) { // #nosec G304 -- written above
			t.Errorf("a second run rewrote %s", path)
		}
	}

	// A [supervise.ntfy] naming no events routed needs_operator at its priority,
	// high where it named none, and the migration says so.
	withGlobal(t, "[supervise.ntfy]\nurl = \"https://h/t\"\n")
	if out, err := exec.Command("bash", script, GlobalPath()).CombinedOutput(); err != nil { // #nosec G204 -- the repository's own script
		t.Fatalf("migrate a bare table: %v\n%s", err, out)
	}
	c, err = Load(project(t, schemaLine+"project = \"widget\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if ds := c.Notifications().Destinations; len(ds) != 1 || ds[0].Events["needs_operator"] != "high" || len(ds[0].Events) != 1 {
		t.Errorf("a bare table migrated to %+v, want needs_operator at high", ds)
	}

	// A name quoted in a comment is not one the array routes, on its closing
	// line or between its elements, and the comments stay in the file.
	for _, c := range []struct{ in, want string }{
		{"[supervise.ntfy]\nurl = \"https://h/t\"\nevents = [\"needs_operator\", \"exit\"]  # dropped \"parked\" because it was noisy\n",
			"[notify.destinations.ntfy]\ntype = \"ntfy\"\nurl = \"https://h/t\"\nevents = { needs_operator = \"high\", exit = \"high\" }  # dropped \"parked\" because it was noisy\n"},
		{"[supervise.ntfy]\nurl = \"https://h/t\"\nevents = [\n  \"needs_operator\", # \"soon\"\n  \"exit\",\n] # end\n",
			"[notify.destinations.ntfy]\ntype = \"ntfy\"\nurl = \"https://h/t\"\n# \"soon\"\nevents = { needs_operator = \"high\", exit = \"high\" } # end\n"},
	} {
		withGlobal(t, c.in)
		if out, err := exec.Command("bash", script, GlobalPath()).CombinedOutput(); err != nil { // #nosec G204 -- the repository's own script
			t.Fatalf("migrate a commented events array: %v\n%s", err, out)
		}
		if got, _ := os.ReadFile(GlobalPath()); string(got) != c.want { // #nosec G304 -- written above
			t.Errorf("%q migrated to\n%s\nwant\n%s", c.in, got, c.want)
		}
		cfg, err := Load(project(t, schemaLine+"project = \"widget\"\n"))
		if err != nil {
			t.Fatalf("the migrated file does not load: %v", err)
		}
		if ds := cfg.Notifications().Destinations; len(ds) != 1 || len(ds[0].Events) != 2 || ds[0].Events["needs_operator"] != "high" || ds[0].Events["exit"] != "high" {
			t.Errorf("a commented events array migrated to %+v, want needs_operator and exit alone", ds)
		}
	}
}

// Every key under notify. is documented in the [notify] section of
// docs/configuration.md, so a key added to the table cannot ship unexplained.
func TestEveryNotifyKeyIsDocumented(t *testing.T) {
	raw, err := os.ReadFile("../../docs/configuration.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	start := strings.Index(doc, "\n## `[notify]`")
	if start < 0 {
		t.Fatal("docs/configuration.md has no ## `[notify]` section")
	}
	section := doc[start+1:]
	if end := strings.Index(section[1:], "\n## "); end >= 0 {
		section = section[:end+1]
	}
	for _, key := range NotifyKeys() {
		name := key[strings.LastIndex(key, ".")+1:]
		if !strings.Contains(section, "`"+name+"`") {
			t.Errorf("%s is not documented in the [notify] section of docs/configuration.md", key)
		}
	}
}
