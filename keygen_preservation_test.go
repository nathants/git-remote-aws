package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nathants/go-libsodium"
	"github.com/nathants/go-libsodium/keysource"
	"golang.org/x/sys/unix"
)

func TestKeygenRetainsDecryptableHistoricalPrefix(t *testing.T) {
	libsodium.Init()
	dir := t.TempDir()
	p, s := filepath.Join(dir, "public"), filepath.Join(dir, "private")
	var beforeP, beforeS []byte
	var fixtures [][]byte
	for generation := 0; generation < 3; generation++ {
		if err := rotateKeyFiles(p, s); err != nil {
			t.Fatal(err)
		}
		nowP, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		nowS, err := os.ReadFile(s)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(nowP, bytes.TrimSuffix(beforeP, []byte("\n"))) || !bytes.HasPrefix(nowS, bytes.TrimSuffix(beforeS, []byte("\n"))) {
			t.Fatal("rotation replaced historical bytes")
		}
		beforeP, beforeS = nowP, nowS
		public, err := keysource.ReadFile(p, false)
		if err != nil {
			t.Fatal(err)
		}
		latest, err := public.Latest()
		if err != nil {
			t.Fatal(err)
		}
		var cipher bytes.Buffer
		if err := libsodium.StreamEncryptRecipients(latest, strings.NewReader("historical file key"), &cipher); err != nil {
			t.Fatal(err)
		}
		fixtures = append(fixtures, bytes.Clone(cipher.Bytes()))
		secret, err := keysource.ReadFile(s, true)
		if err != nil {
			t.Fatal(err)
		}
		ring, err := secret.Keyring()
		if err != nil {
			t.Fatal(err)
		}
		for _, fixture := range fixtures {
			var plain bytes.Buffer
			if err := ring.Decrypt(bytes.NewReader(fixture), &plain); err != nil || plain.String() != "historical file key" {
				t.Fatalf("lost historical decryption: %v", err)
			}
		}
	}
}

func TestKeygenRefusalsPreserveFiles(t *testing.T) {
	libsodium.Init()
	for _, kind := range []string{"locked", "same-inode", "mismatch", "private-publication", "fresh-private-publication", "fresh-public-publication"} {
		t.Run(kind, func(t *testing.T) {
			if strings.Contains(kind, "publication") && os.Geteuid() == 0 {
				t.Skip("requires filesystem permission enforcement")
			}
			dir := t.TempDir()
			pd, sd := filepath.Join(dir, "p"), filepath.Join(dir, "s")
			for _, d := range []string{pd, sd} {
				if err := os.Mkdir(d, 0700); err != nil {
					t.Fatal(err)
				}
			}
			p, s := filepath.Join(pd, "key"), filepath.Join(sd, "key")
			fresh := strings.HasPrefix(kind, "fresh-")
			if !fresh {
				if err := rotateKeyFiles(p, s); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "same-inode":
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(s, p); err != nil {
					t.Fatal(err)
				}
			case "mismatch":
				if err := os.WriteFile(p, []byte(strings.Repeat("1", 64)), 0600); err != nil {
					t.Fatal(err)
				}
			case "locked":
				f, err := os.Open(pd)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = f.Close() }()
				if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
					t.Fatal(err)
				}
			}
			if strings.Contains(kind, "publication") {
				blocked := sd
				if kind == "fresh-public-publication" {
					blocked = pd
				}
				if err := os.Chmod(blocked, 0500); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(blocked, 0700) }()
			}
			beforeP, perr := os.ReadFile(p)
			beforeS, serr := os.ReadFile(s)
			if perr != nil && !os.IsNotExist(perr) {
				t.Fatal(perr)
			}
			if serr != nil && !os.IsNotExist(serr) {
				t.Fatal(serr)
			}
			var output bytes.Buffer
			err := keygen([]string{"--public-key-file", p, "--secret-key-file", s}, &output)
			if err == nil || output.Len() != 0 {
				t.Fatal("refusal did not fail without key output")
			}
			afterP, pe := os.ReadFile(p)
			afterS, se := os.ReadFile(s)
			if !bytes.Equal(beforeP, afterP) || os.IsNotExist(perr) != os.IsNotExist(pe) {
				t.Fatal("refusal changed public file")
			}
			if kind == "fresh-public-publication" {
				secret, err := keysource.ReadFile(s, true)
				if err != nil || len(secret) != 1 {
					t.Fatalf("new private generation not retained: %v", err)
				}
				if _, err := secret.Keyring(); err != nil {
					t.Fatal(err)
				}
			} else if !bytes.Equal(beforeS, afterS) || os.IsNotExist(serr) != os.IsNotExist(se) {
				t.Fatal("refusal changed private file")
			}
		})
	}
}

func TestKeygenFreshPublicationNeverReplacesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing")
	if err := os.WriteFile(path, []byte("retained"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := publishKeyFile(path, []byte("new"), true); err == nil {
		t.Fatal("replaced concurrent file")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "retained" {
		t.Fatal("existing bytes changed")
	}
}
