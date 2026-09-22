package main

import (
	"flag"
	"testing"
)

func TestDryRunFlagExists(t *testing.T) {
	f := flag.NewFlagSet("retention", flag.ContinueOnError)
	dry := f.Bool("dry-run", false, "report what would be removed without deleting anything")
	if err := f.Parse([]string{"--dry-run"}); err != nil {
		t.Fatal(err)
	}
	if !*dry {
		t.Fatal("expected dry-run true")
	}
}
