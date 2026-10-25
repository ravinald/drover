package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The decoder replaces a table held in a map whole, so the project's
// [supervise.ntfy] has to be layered over the per-user one by hand: a project
// that sets only priority keeps the url, token and events the per-user file
// names, and the key it does set wins.
func TestNtfyLayersProjectOverUserKeyByKey(t *testing.T) {
	withGlobal(t, `
[supervise.ntfy]
url        = "https://ntfy.example.com/drover"
token_file = "~/.config/drover/ntfy-token"
events     = ["needs_operator", "needs_supervisor"]
priority   = "default"
`)
	root := project(t, schemaLine+`project = "widget"
[supervise.ntfy]
priority = "urgent"
events   = ["exit"]
`)
	c, err := Load(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	n, on := c.Ntfy()
	if !on {
		t.Fatal("no [supervise.ntfy] after layering")
	}
	home, _ := os.UserHomeDir()
	if n.URL != "https://ntfy.example.com/drover" {
		t.Errorf("url is %q, want the per-user one", n.URL)
	}
	if want := filepath.Join(home, ".config/drover/ntfy-token"); n.TokenFile != want {
		t.Errorf("token_file is %q, want the per-user one with ~ expanded, %q", n.TokenFile, want)
	}
	if n.Priority != "urgent" || !slices.Equal(n.Events, []string{"exit"}) {
		t.Errorf("priority %q events %v, want the project's urgent and [exit]", n.Priority, n.Events)
	}

	// A project silent about the table inherits all of it, defaults included.
	withGlobal(t, "[supervise.ntfy]\nurl = \"https://ntfy.example.com/drover\"\n")
	c, err = Load(project(t, schemaLine+"project = \"widget\"\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if n, _ := c.Ntfy(); n.URL == "" || n.Priority != "high" || !slices.Equal(n.Events, []string{"needs_operator"}) {
		t.Errorf("inherited table with defaults reads %+v", n)
	}
}

// Each table drover would have to guess about is a load error naming the key,
// and the file that wrote it.
func TestNtfyLoadErrors(t *testing.T) {
	for _, c := range []struct {
		name, table, want string
	}{
		{"unknown key", `url = "https://h/t"` + "\ntopic = \"x\"", "supervise.ntfy.topic is not a key drover reads"},
		{"malformed url", `url = "https://h/%zz"`, "supervise.ntfy.url"},
		{"http to a remote host", `url = "http://ntfy.example.com/t"`, "supervise.ntfy.url"},
		{"other scheme", `url = "ftp://h/t"`, "supervise.ntfy.url"},
		{"no topic", `url = "https://ntfy.example.com/"`, "names no topic"},
		{"credentials in the url", `url = "https://u:p@h/t"`, "carries credentials"},
		{"no url", `priority = "high"`, "supervise.ntfy has no url"},
		{"unknown event", `url = "https://h/t"` + "\nevents = [\"merged\"]", "supervise.ntfy.events names merged"},
		{"no events", `url = "https://h/t"` + "\nevents = []", "supervise.ntfy.events"},
		{"unknown priority", `url = "https://h/t"` + "\npriority = \"loud\"", "supervise.ntfy.priority is loud"},
		{"relative token file", `url = "https://h/t"` + "\ntoken_file = \"secrets/token\"", "supervise.ntfy.token_file"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := project(t, schemaLine+"project = \"widget\"\n[supervise.ntfy]\n"+c.table+"\n")
			_, err := Load(root)
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), FileName) {
				t.Errorf("load error %v, want one naming %q in %s", err, c.want, FileName)
			}
		})
	}

	// http is allowed where the token cannot leave the machine.
	for _, url := range []string{"http://127.0.0.1:8080/t", "http://localhost/t", "http://[::1]/t"} {
		if _, err := Load(project(t, schemaLine+"project = \"widget\"\n[supervise.ntfy]\nurl = \""+url+"\"\n")); err != nil {
			t.Errorf("%s refused: %v", url, err)
		}
	}

	// A bad value the per-user file wrote is named there, not in the project.
	withGlobal(t, "[supervise.ntfy]\nurl = \"https://h/t\"\npriority = \"loud\"\n")
	_, err := Load(project(t, schemaLine+"project = \"widget\"\n"))
	if err == nil || !strings.Contains(err.Error(), GlobalFileName) {
		t.Errorf("a per-user value refused as %v, want the error to name %s", err, GlobalFileName)
	}
}
