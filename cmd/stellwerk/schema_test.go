package main

import (
	"os"
	"testing"
)

// The schema in docs/ must match the code. Regenerate with:
//
//	go run -tags goolm ./cmd/stellwerk -schema > docs/stellwerk.schema.json
func TestSchemaFileIsCurrent(t *testing.T) {
	want, err := schema()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("../../docs/stellwerk.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want)+"\n" {
		t.Error("docs/stellwerk.schema.json is outdated, regenerate it (see this test's comment)")
	}
}
