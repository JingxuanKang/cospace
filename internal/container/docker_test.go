package container

import (
	"reflect"
	"testing"
)

func TestParseDockerPS(t *testing.T) {
	out := "acme\trunning\nthesis\texited\nbroken line\n\nnew\tcreated\n"
	got := parseDockerPS(out)
	want := []Info{{Name: "acme", State: "running"}, {Name: "thesis", State: "stopped"}, {Name: "new", State: "stopped"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

// Both engines satisfy the same Runtime; the daemon never type-switches.
func TestRuntimesImplementInterface(t *testing.T) {
	var _ Runtime = Client{}
	var _ Runtime = Docker{}
}
