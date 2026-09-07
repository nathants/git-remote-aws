package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nathants/go-libsodium"
	"github.com/nathants/go-libsodium/keysource"
)

func TestKeygenPersonalFiles(t *testing.T) {
	libsodium.Init()
	dir := t.TempDir()
	public, secret := filepath.Join(dir, "public"), filepath.Join(dir, "private")
	args := []string{"--public-key-file", public, "--secret-key-file", secret}
	for generation := 1; generation <= 3; generation++ {
		var output bytes.Buffer
		if err := keygen(args, &output); err != nil {
			t.Fatal(err)
		}
		if output.Len() != 0 {
			t.Fatal("file keygen printed key material")
		}
		p, err := keysource.ReadFile(public, false)
		if err != nil {
			t.Fatal(err)
		}
		s, err := keysource.ReadFile(secret, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(p) != 1 || len(p[0]) != generation || len(s[0]) != generation {
			t.Fatal("wrong chain lengths")
		}
		for i := range p[0] {
			got, err := libsodium.BoxPublicKey(s[0][i])
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, p[0][i]) {
				t.Fatal("mismatched generation")
			}
		}
	}
	before, err := os.ReadFile(secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(public); err != nil {
		t.Fatal(err)
	}
	if err := keygen(args, io.Discard); err == nil {
		t.Fatal("accepted one missing file")
	}
	after, err := os.ReadFile(secret)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("changed surviving private key")
	}
	if err := os.Symlink(secret, public); err != nil {
		t.Fatal(err)
	}
	if err := keygen(args, io.Discard); err == nil {
		t.Fatal("followed public symlink")
	}
}

func TestKeygenPartialPublication(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires permission enforcement for owner")
	}
	libsodium.Init()
	dir := t.TempDir()
	pubdir := filepath.Join(dir, "pub")
	if err := os.Mkdir(pubdir, 0700); err != nil {
		t.Fatal(err)
	}
	public, secret := filepath.Join(pubdir, "key"), filepath.Join(dir, "private")
	if err := rotateKeyFiles(public, secret); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(public)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pubdir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(pubdir, 0700) })
	if err := rotateKeyFiles(public, secret); err == nil {
		t.Fatal("expected public publication failure")
	}
	s, err := keysource.ReadFile(secret, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(s[0]) != 2 {
		t.Fatal("private extension was not preserved")
	}
	after, err := os.ReadFile(public)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("public file changed on failed publication")
	}
	if err := os.Chmod(pubdir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := rotateKeyFiles(public, secret); err == nil {
		t.Fatal("blind retry accepted mismatched pair")
	}
}

func TestKeychainPushRecipients(t *testing.T) {
	libsodium.Init()
	dir := t.TempDir()
	t.Chdir(dir)
	runAt(dir, "git", "init", "-q")
	configureGitIdentity(dir)
	p, s, err := libsodium.RotateKeyChain(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	commit := func(p libsodium.KeyChains) string {
		t.Helper()
		data, err := p.MarshalText()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(".publickeys", data, 0644); err != nil {
			t.Fatal(err)
		}
		runAt(dir, "git", "add", ".publickeys")
		runAt(dir, "git", "commit", "-qm", "keys")
		return runAtOut(dir, "git", "rev-parse", "HEAD")
	}
	old := commit(p)
	p2, _, err := libsodium.RotateKeyChain(p, s)
	if err != nil {
		t.Fatal(err)
	}
	tip := commit(p2)
	// Uncommitted edits are rejected rather than silently ignored.
	if err := os.WriteFile(".publickeys", []byte("uncommitted"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := pushRecipients(old, tip); err == nil {
		t.Fatal("accepted uncommitted recipient edit")
	}
	runAt(dir, "git", "restore", ".publickeys")
	keys, err := pushRecipients(old, tip)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || !bytes.Equal(keys[0], p2[0][1]) {
		t.Fatal("did not select current generation")
	}
	truncated := commit(p)
	if _, err := pushRecipients(tip, truncated); err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("truncated chain was not rejected: %v", err)
	}
}

func TestKeygenOutputHelpAndArguments(t *testing.T) {
	libsodium.Init()
	var output bytes.Buffer
	if err := keygen([]string{"--help"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "--secret-key-file") || strings.Contains(output.String(), "export GIT_REMOTE_AWS_SECRETKEY=") {
		t.Fatal("keygen help did not describe file mode safely")
	}
	output.Reset()
	if err := keygen(nil, &output); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatal("wrong keygen export count")
	}
	public, err := libsodium.ParseKeyChains(strings.NewReader(strings.TrimPrefix(lines[0], "export GIT_REMOTE_AWS_PUBLICKEY=")))
	if err != nil {
		t.Fatal(err)
	}
	secret, err := libsodium.ParseKeyChains(strings.NewReader(strings.TrimPrefix(lines[1], "export GIT_REMOTE_AWS_SECRETKEY=")))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := libsodium.RotateKeyChain(public, secret); err != nil {
		t.Fatal("keygen emitted mismatched pair")
	}
	for _, args := range [][]string{{"extra"}, {"--public-key-file", "unused"}, {"--secret-key-file", "unused"}} {
		if err := keygen(args, io.Discard); err == nil {
			t.Fatal("accepted incomplete keygen arguments")
		}
	}
	if err := rotateKeyFiles(filepath.Join(t.TempDir(), "same"), filepath.Join(t.TempDir(), "..", "unavailable", "key")); err == nil {
		t.Fatal("accepted missing key directory")
	}
}

func TestKeygenEmptyExplicitPathsNeverPrintSecrets(t *testing.T) {
	libsodium.Init()
	for _, args := range [][]string{{"--public-key-file", "", "--secret-key-file", ""}, {"--public-key-file="}, {"--secret-key-file="}} {
		var output bytes.Buffer
		if err := keygen(args, &output); err == nil || output.Len() != 0 {
			t.Fatal("explicitly empty paths selected stdout keygen")
		}
	}
}
