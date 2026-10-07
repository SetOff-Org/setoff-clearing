package main

import "testing"

func TestBuildVersion(t *testing.T) {
	old := version
	defer func() { version = old }()
	version = "1.2.3"
	if got := buildVersion(); got != "1.2.3" {
		t.Fatalf("a stamped version must win, got %q", got)
	}
	version = "dev"
	if got := buildVersion(); got == "" {
		t.Fatal("an unstamped build still reports something")
	}
}
