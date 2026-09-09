package main

import (
	"bytes"
	"encoding/hex"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nathants/go-libsodium"
)

func TestKeyCLIMultipleRecipientsAndRotation(t *testing.T) {
	setupEphemeralKeys(t)
	binary := buildGitRemoteAws(t)
	dir := t.TempDir()
	public, secret, err := libsodium.RotateKeyChain(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	public, secret, err = libsodium.RotateKeyChain(public, secret)
	if err != nil {
		t.Fatal(err)
	}
	otherPublic, otherSecret, err := libsodium.BoxKeypair()
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("multiple recipients, independent secrets\x00\n")
	run := func(flag string, input []byte) ([]byte, string, error) {
		cmd := exec.Command(binary, flag)
		cmd.Dir = dir
		cmd.Stdin = bytes.NewReader(input)
		var output, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &stderr
		err := cmd.Run()
		return output.Bytes(), stderr.String(), err
	}
	encrypt := func(chains libsodium.KeyChains) []byte {
		text, err := chains.MarshalText()
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("GIT_REMOTE_AWS_PUBLICKEY", string(text))
		cipher, stderr, err := run("--encrypt", plain)
		if err != nil {
			t.Fatalf("encrypt: %v: %s", err, stderr)
		}
		return cipher
	}
	decrypt := func(cipher, key []byte, wantSuccess bool) {
		t.Setenv("GIT_REMOTE_AWS_SECRETKEY", hex.EncodeToString(key))
		got, stderr, err := run("--decrypt", cipher)
		if wantSuccess {
			if err != nil || !bytes.Equal(got, plain) {
				t.Fatalf("decrypt: %v: %s", err, stderr)
			}
		} else if err == nil || !strings.Contains(stderr, "no recipient matched") || len(got) != 0 {
			t.Fatalf("unmatched recipient: error=%v, output bytes=%d, stderr=%s", err, len(got), stderr)
		}
	}
	old := encrypt(libsodium.KeyChains{{public[0][0]}})
	both := encrypt(libsodium.KeyChains{public[0], {otherPublic}})
	removed := encrypt(libsodium.KeyChains{{otherPublic}})
	decrypt(old, secret[0][0], true)
	decrypt(old, secret[0][1], false)
	decrypt(old, otherSecret, false)   // Adding a recipient cannot unlock old ciphertext.
	decrypt(both, secret[0][0], false) // New encryption targets only chain tips.
	decrypt(both, secret[0][1], true)
	decrypt(both, otherSecret, true)
	decrypt(removed, secret[0][1], false)
	decrypt(removed, otherSecret, true)
	decrypt(both, secret[0][1], true) // Removal cannot revoke existing ciphertext.
}

func TestKeyCLISecretCommandLifecycle(t *testing.T) {
	binary := buildGitRemoteAws(t)
	shared := runAtOut(repoRoot(), "go", "list", "-m", "-f", "{{.Dir}}", "github.com/nathants/go-libsodium")
	cmd := exec.Command("python3", "-I", filepath.Join(shared, "keysource", "testdata", "command-lifecycle.py"), binary, "--decrypt")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("CLI secret command lifecycle: %v\n%s", err, output)
	}
}
