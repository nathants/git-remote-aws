package main

import (
	"debug/buildinfo"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestKeyCompatibilityArtifactRejectsInvalidInputs(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-helper")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{filepath.Join(dir, "missing"), fake, dir, self} {
		if _, err := prepareOldHelper(input, t.TempDir()); err == nil {
			t.Fatalf("invalid old helper accepted: %s", input)
		}
	}
}

// The artifact must be independently built from the pre-keychain helper graph.
func prepareOldHelper(binary, directory string) (string, error) {
	absolute, err := filepath.Abs(binary)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", fmt.Errorf("old helper must be an executable regular file")
	}
	build, err := buildinfo.ReadFile(absolute)
	if err != nil {
		return "", err
	}
	if build.Path != "github.com/nathants/git-remote-aws" {
		return "", fmt.Errorf("old artifact is not git-remote-aws")
	}
	oldDependency := false
	for _, dep := range build.Deps {
		if dep.Path == "github.com/nathants/go-libsodium" {
			oldDependency = dep.Version == "v0.0.0-20260502104057-4e1a79aae4f3" && dep.Replace == nil
		}
	}
	if !oldDependency {
		return "", fmt.Errorf("old helper must use the independently pinned pre-keychain dependency")
	}
	// Git resolves a fixed executable name. Select this exact validated artifact,
	// even if Admin gave it a different filename; never fall through to new PATH.
	if err := os.Symlink(absolute, filepath.Join(directory, "git-remote-aws")); err != nil {
		return "", err
	}
	return directory, nil
}

// The checked-in release runner uses this preflight before creating AWS resources.
func TestKeyCompatibilitySelectedArtifact(t *testing.T) {
	binary := os.Getenv("GIT_REMOTE_AWS_TEST_OLD_BINARY")
	if binary == "" {
		t.Skip("requires the independent pre-keychain artifact")
	}
	if _, err := prepareOldHelper(binary, t.TempDir()); err != nil {
		t.Fatal(err)
	}
}
