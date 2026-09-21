package main

import (
	"os"
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
