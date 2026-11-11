package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestTPHelpAndInvalidArgumentsNeedNoRegistry(t *testing.T) {
	t.Setenv("PADDOCK_HOME", "/this/path/must/not/be/created")
	var out bytes.Buffer
	if err := run([]string{"tp", "--help"}, &out); err != nil || !strings.Contains(out.String(), "Team Principal") {
		t.Fatal(out.String(), err)
	}
	for _, args := range [][]string{{"tp", "unexpected"}, {"tp", "--runtime", "host"}, {"tp", "--agent"}} {
		if err := run(args, &out); err == nil {
			t.Fatal("accepted invalid TP arguments", args)
		}
	}
}
