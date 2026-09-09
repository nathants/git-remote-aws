package main

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/nathants/go-libsodium"
	"github.com/nathants/libaws/lib"
)

// The old binary must be independently built from the pre-keychain implementation
// and its pinned module graph. All remote writes are to guarded scratch resources.
func TestStoredDataCompatibilityAndRotation(t *testing.T) {
	oldBinary := os.Getenv("GIT_REMOTE_AWS_TEST_OLD_BINARY")
	if oldBinary == "" {
		t.Skip("requires pre-keychain binary and guarded scratch AWS resources")
	}
	oldDirectory, err := prepareOldHelper(oldBinary, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	libsodium.Init()
	for _, objectFormat := range []string{"sha1", "sha256"} {
		t.Run(objectFormat, func(t *testing.T) {
			table, bucket, prefix := getTestBucketAndTable(t)
			if err := lib.DynamoDBWaitForReady(context.Background(), table); err != nil {
				t.Fatal(err)
			}
			defer cleanupAws(table, bucket, prefix)
			newPath := os.Getenv("PATH")
			public, secret, err := libsodium.BoxKeypair()
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("GIT_REMOTE_AWS_SECRETKEY", hex.EncodeToString(secret))
			t.Setenv("GIT_REMOTE_AWS_SECRETKEY_FILE", "")
			t.Setenv("GIT_REMOTE_AWS_SECRETKEY_CMD", "")
			dir := t.TempDir()
			runAt(dir, "git", "init", "--object-format="+objectFormat, "--initial-branch=master")
			configureGitIdentity(dir)
			if err := os.WriteFile(filepath.Join(dir, ".publickeys"), []byte(hex.EncodeToString(public)+"\n"), 0644); err != nil {
				t.Fatal(err)
			}
			remote := "aws://" + bucket + "+" + table + "/" + prefix
			runAt(dir, "git", "remote", "add", "origin", remote)
			t.Setenv("PATH", oldDirectory+string(os.PathListSeparator)+newPath)
			for _, text := range []string{"old full bundle", "old incremental bundle"} {
				if err := os.WriteFile(filepath.Join(dir, "file"), []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
				if objectFormat == "sha1" {
					// The old helper allowed an untracked recipient file. Its
					// first tracked policy must be adoptable without rewriting history.
					runAt(dir, "git", "add", "file")
				} else {
					runAt(dir, "git", "add", ".")
				}
				runAt(dir, "git", "commit", "-m", text)
				runAt(dir, "git", "push", "origin", "master")
			}
			expected := runAtOut(dir, "git", "rev-parse", "HEAD")
			// Save provider identities for old encrypted bundles; cloning/rotation must not rewrite them.
			oldKeys := listKeys(bucket, prefix)
			etags := make(map[string]string)
			for _, name := range oldKeys {
				out, err := lib.S3Client().HeadObject(context.Background(), &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(prefix + "/" + name)})
				if err != nil {
					t.Fatal(err)
				}
				etags[name] = aws.ToString(out.ETag)
			}
			t.Setenv("PATH", newPath)
			migrateTestRepository(t, table, bucket, prefix)
			clone := filepath.Join(t.TempDir(), "clone")
			runAt("", "git", "clone", remote, clone)
			if got := runAtOut(clone, "git", "rev-parse", "HEAD"); got != expected {
				t.Fatal("old history changed during new-helper clone")
			}
			configureGitIdentity(clone)
			pub, sec, err := libsodium.RotateKeyChain(libsodium.KeyChains{{public}}, libsodium.KeyChains{{secret}})
			if err != nil {
				t.Fatal(err)
			}
			ptext, err := pub.MarshalText()
			if err != nil {
				t.Fatal(err)
			}
			stext, err := sec.MarshalText()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(clone, ".publickeys"), ptext, 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(clone, "file"), []byte("rotated generation"), 0600); err != nil {
				t.Fatal(err)
			}
			runAt(clone, "git", "add", ".")
			runAt(clone, "git", "commit", "-m", "rotation")
			t.Setenv("GIT_REMOTE_AWS_SECRETKEY", string(stext))
			runAt(clone, "git", "push", "origin", "master")
			latest := runAtOut(clone, "git", "rev-parse", "HEAD")
			secretDir := t.TempDir()
			secretPath, countPath := filepath.Join(secretDir, "secret"), filepath.Join(secretDir, "count")
			commandPath := filepath.Join(secretDir, "source")
			if err := os.WriteFile(secretPath, stext, 0600); err != nil {
				t.Fatal(err)
			}
			script := "#!/bin/sh\n[ \"$1\" = '" + remote + "' ] || exit 2\nprintf 'called\\n' >> '" + countPath + "'\ncat '" + secretPath + "'\n"
			if err := os.WriteFile(commandPath, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GIT_REMOTE_AWS_SECRETKEY", "")
			t.Setenv("GIT_REMOTE_AWS_SECRETKEY_CMD", commandPath)
			fresh := filepath.Join(t.TempDir(), "fresh")
			runAt("", "git", "clone", remote, fresh)
			if got := runAtOut(fresh, "git", "rev-parse", "HEAD"); got != latest {
				t.Fatal("mixed-generation clone has wrong history")
			}
			calls, err := os.ReadFile(countPath)
			if err != nil || string(calls) != "called\n" {
				t.Fatalf("fetch did not reuse its keyring: %v", err)
			}
			t.Setenv("GIT_REMOTE_AWS_SECRETKEY_CMD", "")
			for _, key := range sec[0] {
				t.Setenv("GIT_REMOTE_AWS_SECRETKEY", hex.EncodeToString(key))
				_, stderr, err := runAtResult("", "git", "clone", remote, filepath.Join(t.TempDir(), "incomplete"))
				if err == nil || !strings.Contains(stderr, "no recipient matched") {
					t.Fatalf("incomplete private chain clone: %v", err)
				}
			}
			for name, want := range etags {
				out, err := lib.S3Client().HeadObject(context.Background(), &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(prefix + "/" + name)})
				if err != nil || aws.ToString(out.ETag) != want {
					t.Fatalf("old bundle changed: %s (%v)", name, err)
				}
			}
		})
	}
}

func TestPushNoOpRejectsUncommittedRecipients(t *testing.T) {
	table, bucket, prefix := getTestBucketAndTable(t)
	defer cleanupAws(table, bucket, prefix)
	if err := lib.DynamoDBWaitForReady(context.Background(), table); err != nil {
		t.Fatal(err)
	}
	public := setupEphemeralKeys(t)
	dir := t.TempDir()
	runAt(dir, "git", "init", "-q", "--initial-branch=master")
	configureGitIdentity(dir)
	filename := filepath.Join(dir, ".publickeys")
	if err := os.WriteFile(filename, []byte(public+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runAt(dir, "git", "add", ".publickeys")
	runAt(dir, "git", "commit", "-qm", "policy")
	remote := "aws://" + bucket + "+" + table + "/" + prefix
	runAt(dir, "git", "remote", "add", "origin", remote)
	marker := filepath.Join(t.TempDir(), "called")
	command := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(command, []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_REMOTE_AWS_SECRETKEY", "")
	t.Setenv("GIT_REMOTE_AWS_SECRETKEY_CMD", command)
	runAt(dir, "git", "push", "origin", "master")
	runAt(dir, "git", "ls-remote", "origin")
	runAt(dir, "git", "fetch", "origin")
	if err := os.WriteFile(filename, []byte("\n"+public+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	assertRunAtErrContains(t, dir, ".publickeys has uncommitted changes", "git", "push", "origin", "master")
	// A dirty local policy must not disable read operations.
	runAt(dir, "git", "fetch", "origin")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("push/list/no-op fetch invoked the secret loader: %v", err)
	}
}
