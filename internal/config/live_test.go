package config

import (
	"reflect"
	"testing"

	"github.com/BurntSushi/toml"
)

// A printed config is meant to be pasted, so [live] has to print as TOML that
// decodes back to the checks it came from.
func TestLivePrintsAsTOMLThatLoadsBack(t *testing.T) {
	var in struct {
		Live Live `toml:"live"`
	}
	src := "[live]\ndefault = \"suite\"\nwait = \"2h\"\n\n[live.suite]\nargv = [\"a.sh\", \"--x\"]\nlock = \"guests\"\n\n[live.\"dotted.name\"]\nargv = [\"b.sh\"]\n"
	if _, err := toml.Decode(src, &in); err != nil {
		t.Fatal(err)
	}
	printed, err := in.Live.MarshalTOML()
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Live Live `toml:"live"`
	}
	if _, err := toml.Decode("live = "+string(printed)+"\n", &out); err != nil {
		t.Fatalf("the printed table does not load: %v\n%s", err, printed)
	}
	if !reflect.DeepEqual(in.Live, out.Live) {
		t.Errorf("round trip changed [live]:\n in %+v\nout %+v\nprinted %s", in.Live, out.Live, printed)
	}
	if b, _ := (Live{}).MarshalTOML(); string(b) != "{}" {
		t.Errorf("an empty [live] prints %q, want {}", b)
	}
}
