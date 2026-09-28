package main

import (
	"testing"

	"github.com/atipongsena/eacp/internal/version"
)

func TestVersionPrintsTheBuildVersion(t *testing.T) {
	out, err := runWith(t, nil, "version")
	if err != nil {
		t.Fatalf("eacpctl version: %v", err)
	}
	if out != version.Version+"\n" {
		t.Fatalf("eacpctl version printed %q, want %q", out, version.Version+"\n")
	}
}
