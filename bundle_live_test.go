package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/nathants/go-dynamolock"
)

func TestSizedBundlePushAWS(t *testing.T) {
	public := setupEphemeralKeys(t)
	table, bucket, prefix := getTestBucketAndTable(t)
	defer cleanupAws(table, bucket, prefix)
	clients := testAWSClients()
	dir := t.TempDir()
	runAt(dir, "git", "init", "-q", "--object-format=sha256", "-b", "archive/home")
	configureGitIdentity(dir)
	if err := os.WriteFile(filepath.Join(dir, ".publickeys"), []byte(public+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runAt(dir, "git", "config", "remote-aws.bundleSize", "8m")
	for i := range 3 {
		commitBundleData(t, dir, string(rune('a'+i)), 4<<20)
	}
	remote := "aws://" + bucket + "+" + table + "/" + prefix
	runAt(dir, "git", "remote", "add", "origin", remote)
	runAt(dir, "git", "push", "-u", "origin", "archive/home")
	meta, err := dynamolock.Read[RepoMeta](t.Context(), clients.dynamodb, table, bucket+"/"+prefix)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := parseRepository(prefix)
	if err != nil {
		t.Fatal(err)
	}
	m, err := clients.getManifest(t.Context(), bucket, meta.BundlesS3Key, repo, meta.Branch)
	var bundles []bundleRef
	if m != nil {
		bundles = m.Bundles
	}
	if err != nil || len(bundles) < 2 {
		t.Fatalf("initial push did not publish multiple bundles: %v, %v", bundles, err)
	}
	identities := make(map[string]string)
	for _, name := range bundles {
		out, err := clients.s3.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(name.Key)})
		if err != nil {
			t.Fatal(err)
		}
		identities[name.Key] = aws.ToString(out.ETag)
	}
	clone := filepath.Join(t.TempDir(), "clone")
	runAt(filepath.Dir(clone), "git", "clone", remote, clone)
	assertBundleHistory(t, dir, clone, runAtOut(dir, "git", "rev-parse", "HEAD"))
	// An indivisible increment exceeds both the soft bundle target and the
	// multipart threshold, exercising the real AWS checksum/complete protocol.
	tip := commitBundleData(t, dir, "large indivisible increment", 65<<20)
	runAt(dir, "git", "push", "origin", "archive/home")
	runAt(clone, "git", "pull", "--ff-only")
	assertBundleHistory(t, dir, clone, tip)
	meta, err = dynamolock.Read[RepoMeta](t.Context(), clients.dynamodb, table, bucket+"/"+prefix)
	if err != nil {
		t.Fatal(err)
	}
	m, err = clients.getManifest(t.Context(), bucket, meta.BundlesS3Key, repo, meta.Branch)
	if m != nil {
		bundles = m.Bundles
	}
	if err != nil {
		t.Fatal(err)
	}
	out, err := clients.s3.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(last(bundles).Key)})
	if err != nil || aws.ToInt64(out.ContentLength) <= bundleUploadPartSize || !strings.Contains(aws.ToString(out.ETag), "-2") {
		t.Fatalf("large bundle was not uploaded in two parts: %+v, %v", out, err)
	}
	for name, before := range identities {
		out, err := clients.s3.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(name)})
		if err != nil || before != aws.ToString(out.ETag) {
			t.Fatalf("incremental push rewrote prior ciphertext: %s, %v", name, err)
		}
	}
	uploads, err := clients.s3.ListMultipartUploads(t.Context(), &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket), Prefix: aws.String(prefix + "/")})
	if err != nil || len(uploads.Uploads) != 0 {
		t.Fatalf("completed push left multipart uploads: %+v, %v", uploads, err)
	}
}

func TestResumedBundlePushAWS(t *testing.T) {
	public := setupEphemeralKeys(t)
	table, bucket, prefix := getTestBucketAndTable(t)
	defer cleanupAws(table, bucket, prefix)
	clients := testAWSClients()
	dir := t.TempDir()
	runAt(dir, "git", "init", "-q", "-b", "archive/home")
	configureGitIdentity(dir)
	if err := os.WriteFile(filepath.Join(dir, ".publickeys"), []byte(public+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runAt(dir, "git", "config", "remote-aws.bundleSize", "16k")
	var commits []string
	for i := range 5 {
		commits = append(commits, commitBundleData(t, dir, fmt.Sprintf("data %d", i), 32<<10))
	}
	remote := "aws://" + bucket + "+" + table + "/" + prefix
	runAt(dir, "git", "remote", "add", "origin", remote)

	// Stop the real helper after three uploads, before packing the fourth.
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shim := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1 $2" = 'bundle create' ] && [ "${3##*/}" = '%s..%s' ]; then
  echo 'interrupt resumed-push test' >&2
  exit 97
fi
exec '%s' "$@"
`, commits[2], commits[3], git)
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	path := os.Getenv("PATH")
	t.Setenv("PATH", shim+":"+path)
	// Invoke the helper protocol directly: Git prepends its own exec directory
	// to PATH, which would otherwise hide the shim from the helper's Git calls.
	child := exec.CommandContext(t.Context(), "git-remote-aws", "origin", remote)
	child.Dir = dir
	child.Env = append(testEnvironment(), "GIT_DIR=.git")
	child.Stdin = strings.NewReader("push refs/heads/archive/home:refs/heads/archive/home\n\n")
	output, err := child.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "interrupt resumed-push test") {
		t.Fatalf("did not interrupt push: %v\n%s", err, output)
	}
	t.Setenv("PATH", path)
	meta, err := dynamolock.Read[RepoMeta](t.Context(), clients.dynamodb, table, bucket+"/"+prefix)
	if err != nil || meta != nil && meta.BundlesS3Key != "" {
		t.Fatalf("interrupted push published metadata: %+v, %v", meta, err)
	}
	objects, err := clients.s3.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix + "/.remote-aws-v2/bundles/" + last(commits) + "/")})
	if err != nil || len(objects.Contents) != 3 {
		t.Fatalf("expected three completed objects: %+v, %v", objects, err)
	}
	versions := make(map[string]string)
	for _, object := range objects.Contents {
		head, err := clients.s3.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: object.Key})
		if err != nil {
			t.Fatal(err)
		}
		versions[aws.ToString(object.Key)] = aws.ToString(head.VersionId)
	}
	runAt(dir, "git", "config", "remote-aws.bundleSize", "1m")
	runAt(dir, "git", "push", "origin", "archive/home")
	m := liveManifest(t, table, bucket, prefix)
	if len(m.Bundles) != 4 || m.Tip != last(commits) {
		t.Fatalf("did not retain three checkpoints and pack only the suffix: %+v", m)
	}
	for _, object := range objects.Contents {
		key := aws.ToString(object.Key)
		head, err := clients.s3.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: object.Key})
		if err != nil || aws.ToString(head.ETag) != aws.ToString(object.ETag) || aws.ToString(head.VersionId) != versions[key] {
			t.Fatalf("resume changed completed object: %s, %v", key, err)
		}
		found := false
		for _, ref := range m.Bundles[:3] {
			found = found || ref.Key == key && ref.ETag == aws.ToString(object.ETag) && ref.Size == aws.ToInt64(object.Size)
		}
		if !found {
			t.Errorf("completed object omitted from manifest: %s", key)
		}
	}
	clone := filepath.Join(t.TempDir(), "clone")
	runAt("", "git", "clone", remote, clone)
	assertBundleHistory(t, dir, clone, last(commits))
}
