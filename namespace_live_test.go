package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/nathants/go-dynamolock"
)

func liveManifest(t *testing.T, table, bucket, prefix string) *manifest {
	t.Helper()
	repo, err := parseRepository(prefix)
	if err != nil {
		t.Fatal(err)
	}
	meta := getRepoMeta(table, bucket, repo.id())
	m, err := testAWSClients().getManifest(t.Context(), bucket, meta.BundlesS3Key, repo, meta.Branch)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestNamespaceLegacyAdoptionAWS(t *testing.T) {
	table, bucket, prefix := getTestBucketAndTable(t)
	defer cleanupAws(table, bucket, prefix)
	defer cleanupAws(table, bucket, prefix+"/2")
	clients := testAWSClients()
	data, err := os.ReadFile(filepath.Join("testdata", "libaws-sha256.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved libawsCompatibilityFixture
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_REMOTE_AWS_SECRETKEY", saved.SecretKey)
	t.Setenv("GIT_REMOTE_AWS_SECRETKEY_FILE", "")
	t.Setenv("GIT_REMOTE_AWS_SECRETKEY_CMD", "")
	var listKey string
	identities := make(map[string]string)
	for original, body := range saved.Objects {
		key := prefix + "/" + strings.TrimPrefix(original, "/bucket/repo/")
		out, err := clients.s3.PutObject(t.Context(), &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(body), IfNoneMatch: aws.String("*")})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(key, "/bundles_") {
			listKey = key
		} else {
			identities[key] = aws.ToString(out.ETag)
		}
	}
	lease, _, err := dynamolock.Lock[RepoMeta](t.Context(), clients.dynamodb, &dynamolock.LockInput{Table: table, ID: bucket + "/" + prefix, HeartbeatMaxAge: 10 * time.Second, HeartbeatInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Commit(lease.Context(), &RepoMeta{Branch: "archive/home", BundlesS3Key: listKey}); err != nil {
		t.Fatal(err)
	}
	remote := "aws://" + bucket + "+" + table + "/" + prefix
	dir := filepath.Join(t.TempDir(), "source")
	runAt("", "git", "clone", remote+"/1", dir)
	configureGitIdentity(dir)
	runAt(dir, "git", "config", "remote-aws.bundleSize", "1k")
	commitBundleData(t, dir, "first new increment", 4<<10)
	runAt(dir, "git", "push", "origin", "archive/home")
	first := liveManifest(t, table, bucket, prefix)
	if len(first.Bundles) != 2 || first.Bundles[0].Key != prefix+"/"+first.Bundles[0].Range {
		t.Fatalf("legacy promotion moved or rewrote history: %+v", first)
	}
	runAt(dir, "git", "commit", "--amend", "-qm", "shallow rewrite")
	tip := runAtOut(dir, "git", "rev-parse", "HEAD")
	runAt(dir, "git", "remote", "set-url", "origin", remote+"/2")
	runAt(dir, "git", "push", "origin", "archive/home")
	second := liveManifest(t, table, bucket, prefix+"/2")
	if !reflect.DeepEqual(first.Bundles[:1], second.Bundles[:1]) || second.Tip != tip {
		t.Fatalf("did not adopt historical ciphertext: %+v", second)
	}
	for key, want := range identities {
		out, err := clients.s3.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		if err != nil || aws.ToString(out.ETag) != want {
			t.Fatalf("legacy ciphertext changed: %s %v", key, err)
		}
	}
	clone := filepath.Join(t.TempDir(), "clone")
	runAt("", "git", "clone", remote+"/2", clone)
	assertBundleHistory(t, dir, clone, tip)
	meta := getRepoMeta(table, bucket, prefix+"/2")
	_, err = clients.dynamodb.DeleteItem(t.Context(), &dynamodb.DeleteItemInput{TableName: aws.String(table), Key: map[string]ddbtypes.AttributeValue{"id": &ddbtypes.AttributeValueMemberS{Value: bucket + "/" + prefix + "/2"}}})
	if err != nil {
		t.Fatal(err)
	}
	binary, err := exec.LookPath("git-remote-aws")
	if err != nil {
		t.Fatal(err)
	}
	recovery := filepath.Join(t.TempDir(), "recovery")
	runAt("", binary, "--recover", "--remote", remote+"/2", "--manifest", meta.BundlesS3Key, "--directory", recovery)
	assertBundleHistory(t, dir, recovery, tip)
	if meta, err := dynamolock.Read[RepoMeta](t.Context(), clients.dynamodb, table, bucket+"/"+prefix+"/2"); err != nil || meta != nil {
		t.Fatalf("recovery unexpectedly wrote DynamoDB: %+v %v", meta, err)
	}
}

func TestNamespaceConcurrentWritersAWS(t *testing.T) {
	public := setupEphemeralKeys(t)
	table, bucket, prefix := getTestBucketAndTable(t)
	defer cleanupAws(table, bucket, prefix)
	defer cleanupAws(table, bucket, prefix+"/2")
	defer cleanupAws(table, bucket, prefix+"/3")
	dir := t.TempDir()
	runAt(dir, "git", "init", "-q", "-b", "archive/home")
	configureGitIdentity(dir)
	if err := os.WriteFile(filepath.Join(dir, ".publickeys"), []byte(public+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runAt(dir, "git", "config", "remote-aws.bundleSize", "1k")
	for i := range 3 {
		commitBundleData(t, dir, string(rune('a'+i)), 4<<10)
	}
	remote := "aws://" + bucket + "+" + table + "/" + prefix
	other := filepath.Join(t.TempDir(), "other")
	runAt("", "git", "clone", "--no-local", dir, other)
	configureGitIdentity(other)
	runAt(other, "git", "config", "remote-aws.bundleSize", "1k")
	runAt(dir, "git", "remote", "add", "origin", remote+"/2")
	runAt(other, "git", "remote", "set-url", "origin", remote+"/3")
	pushPair := func(wantSuccess int) {
		t.Helper()
		type result struct {
			output []byte
			err    error
		}
		results := make(chan result, 2)
		start := make(chan struct{})
		for _, directory := range []string{dir, other} {
			go func() {
				<-start
				command := exec.CommandContext(t.Context(), "git", "push", "origin", "archive/home")
				command.Dir = directory
				out, err := command.CombinedOutput()
				results <- result{out, err}
			}()
		}
		close(start)
		successes := 0
		for range 2 {
			result := <-results
			if result.err == nil {
				successes++
			} else if !bytes.Contains(result.output, []byte("pull before pushing")) && !bytes.Contains(result.output, []byte("fetch first")) && !bytes.Contains(result.output, []byte("non-fast-forward")) && !bytes.Contains(result.output, []byte("lock unavailable: lock is held")) {
				t.Errorf("unexpected concurrent push failure: %v\n%s", result.err, result.output)
			}
		}
		if successes != wantSuccess {
			t.Fatalf("concurrent pushes: %d successes, want %d", successes, wantSuccess)
		}
	}
	// Different destination leases can share immutable ciphertext, including
	// competing conditional creations for the same tip/range keys.
	pushPair(2)
	left, right := liveManifest(t, table, bucket, prefix+"/2"), liveManifest(t, table, bucket, prefix+"/3")
	if !reflect.DeepEqual(left.Bundles, right.Bundles) {
		t.Fatal("same history did not converge on shared immutable bundles")
	}
	// Two divergent pushes to one existing destination must linearize: exactly
	// one wins, and the loser cannot overwrite either the pointer or ciphertext.
	runAt(other, "git", "remote", "set-url", "origin", remote+"/2")
	runAt(dir, "git", "commit", "--allow-empty", "-qm", "left writer")
	runAt(other, "git", "commit", "--allow-empty", "-qm", "right writer")
	pushPair(1)
	winner := liveManifest(t, table, bucket, prefix+"/2")
	if !reflect.DeepEqual(left.Bundles, winner.Bundles[:len(left.Bundles)]) {
		t.Fatal("concurrent writer replaced shared history")
	}
	clone := filepath.Join(t.TempDir(), "winner")
	runAt("", "git", "clone", remote+"/2", clone)
	runAt(clone, "git", "fsck", "--strict")
}
