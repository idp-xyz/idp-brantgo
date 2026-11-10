package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The generated catalog must match the archive: edit the archive file and run `make roles`.
func TestGeneratedRolesMatchTheArchive(t *testing.T) {
	root := filepath.Join("..", "..")
	want, err := Generate(filepath.Join(root, Source))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, Target))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is stale; run make roles", Target)
	}
}
