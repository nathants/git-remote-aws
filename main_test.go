package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/gofrs/uuid/v5"
	"github.com/nathants/go-dynamolock"
	"github.com/nathants/go-libsodium"
)

func runAtResult(dir string, args ...string) (string, string, error) {
	fmt.Println("runAt", dir, args)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = testEnvironment()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Dir = dir
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func assertRunAtErrContains(t *testing.T, dir, expected string, args ...string) {
	t.Helper()
	stdout, stderr, err := runAtResult(dir, args...)
	if err == nil {
		t.Fatalf("expected command to fail with %q, but it succeeded: %v\nstdout:\n%s\nstderr:\n%s", expected, args, stdout, stderr)
	}
	output := stdout + stderr
	if !strings.Contains(output, expected) {
		t.Fatalf("expected command failure to contain %q: %v: %v\nstdout:\n%s\nstderr:\n%s", expected, args, err, stdout, stderr)
	}
}

func mustPanicContains(t *testing.T, expected string, f func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic containing %q", expected)
		}
		if !strings.Contains(fmt.Sprint(r), expected) {
			t.Fatalf("expected panic containing %q, got %q", expected, fmt.Sprint(r))
		}
	}()
	f()
}

func setupEphemeralKeys(t *testing.T) string {
	t.Helper()
	libsodium.Init()
	pk, sk, err := libsodium.BoxKeypair()
	if err != nil {
		t.Fatal(err)
	}
	public := hex.EncodeToString(pk)
	t.Setenv("GIT_REMOTE_AWS_PUBLICKEY", public)
	t.Setenv("GIT_REMOTE_AWS_SECRETKEY", hex.EncodeToString(sk))
	t.Setenv("GIT_REMOTE_AWS_SECRETKEY_FILE", "")
	t.Setenv("GIT_REMOTE_AWS_SECRETKEY_CMD", "")
	return public
}

func runAt(dir string, args ...string) {
	fmt.Println("runAt", dir, args)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = testEnvironment()
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	cmd.Dir = dir
	err := cmd.Run()
	if err != nil {
		panic(err)
	}
}

func runAtOut(dir string, args ...string) string {
	fmt.Println("runAt", dir, args)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = testEnvironment()
	cmd.Stderr = os.Stderr
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Dir = dir
	err := cmd.Run()
	if err != nil {
		panic(err)
	}
	return strings.TrimRight(stdout.String(), "\n")
}

func repoRoot() string {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		panic("runtime.Caller")
	}
	return path.Dir(filename)
}

func buildGitRemoteAws(t *testing.T) string {
	root := repoRoot()
	binary := path.Join(t.TempDir(), "git-remote-aws")
	cmd := exec.Command("go", "build", "-o", binary, ".")
	cmd.Dir = root
	cmd.Env = testEnvironment()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		panic(err)
	}
	ensureGitRemoteAwsOnPath(t, binary)
	return binary
}

func ensureGitRemoteAwsOnPath(t *testing.T, binary string) {
	root := path.Dir(binary)
	sep := string(os.PathListSeparator)
	pathEnv := os.Getenv("PATH")
	if pathEnv == "" {
		t.Setenv("PATH", root)
	} else {
		t.Setenv("PATH", root+sep+pathEnv)
	}
	actual, err := exec.LookPath("git-remote-aws")
	if err != nil {
		panic(err)
	}
	if path.Clean(actual) != path.Clean(binary) {
		panic(fmt.Sprintf("git-remote-aws on PATH should resolve to %s, got %s", binary, actual))
	}
}

func configureGitIdentity(dir string) {
	runAt(dir, "git", "config", "commit.gpgsign", "false")
	runAt(dir, "git", "config", "core.hooksPath", "/dev/null")
	runAt(dir, "git", "config", "user.name", "git-remote-aws test")
	runAt(dir, "git", "config", "user.email", "git-remote-aws-test@example.com")
}

func testAWSClients() *awsClients {
	clients, err := newAWSClients(context.Background())
	if err != nil {
		panic(err)
	}
	return clients
}

// getTestBucketAndTable requires GIT_REMOTE_AWS_TEST_ACCOUNT to name the
// configured credentials' account. The helper's ensure=y setup creates a fresh
// bucket and table, which are permanently deleted after the test. The returned
// prefix is a UUID namespace for the test's repository.
func getTestBucketAndTable(t *testing.T) (string, string, string) {
	t.Helper()
	account := os.Getenv("GIT_REMOTE_AWS_TEST_ACCOUNT")
	if account == "" {
		t.Fatal("set GIT_REMOTE_AWS_TEST_ACCOUNT to run live AWS tests")
	}
	clients := testAWSClients()
	// Helpers run with ensure=y, so confirm the scratch account before any of
	// them can create resources.
	identity, err := clients.sts.GetCallerIdentity(t.Context(), &sts.GetCallerIdentityInput{})
	if err != nil {
		t.Fatalf("identify scratch account: %v", err)
	}
	if actual := aws.ToString(identity.Account); actual != account {
		t.Fatalf("wrong aws account %s != %s", actual, account)
	}
	// Cleanups also run when a test panics, unlike code after TestMain's m.Run.
	name := "git-remote-aws-test-" + newUuid()
	t.Cleanup(func() {
		if err := deleteTestResources(clients, account, name); err != nil {
			t.Errorf("delete test bucket and table %s: %v", name, err)
		}
	})
	t.Setenv("ensure", "y")
	buildGitRemoteAws(t)
	setCommitDate()
	prefix := newUuid()
	// Tests access the resources directly, so create them before returning.
	// The helper never retries CreateBucket, so retry setup only while the
	// bucket, which it creates first, is still absent.
	dir := t.TempDir()
	runAt(dir, "git", "init", "-q")
	for attempt := 1; ; attempt++ {
		_, stderr, err := runAtResult(dir, "git", "ls-remote", "aws://"+name+"+"+name+"/"+prefix)
		if err == nil {
			break
		}
		exists, existsErr := clients.bucketExists(t.Context(), name)
		if attempt == 3 || exists || existsErr != nil {
			t.Fatalf("create test bucket and table through ensure=y: %v\n%s", errors.Join(err, existsErr), stderr)
		}
	}
	return name, name, prefix
}

// deleteTestResources permanently deletes a test bucket, including every object
// version, delete marker, and incomplete upload, and the table of the same name.
// Either may be absent when setup failed.
func deleteTestResources(clients *awsClients, account, name string) error {
	bucketCtx, cancelBucket := context.WithTimeout(context.Background(), 5*time.Minute)
	bucketErr := deleteTestBucket(bucketCtx, clients.s3, account, name)
	cancelBucket()
	// A slow or unavailable S3 endpoint must not consume the table's budget.
	tableCtx, cancelTable := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelTable()
	return errors.Join(bucketErr, deleteTestTable(tableCtx, clients.dynamodb, name))
}

func deleteTestTable(ctx context.Context, client *dynamodb.Client, table string) error {
	for {
		_, err := client.DeleteTable(ctx, &dynamodb.DeleteTableInput{TableName: aws.String(table)})
		if _, missing := errors.AsType[*ddbtypes.ResourceNotFoundException](err); err == nil || missing {
			return nil
		}
		if _, inUse := errors.AsType[*ddbtypes.ResourceInUseException](err); !inUse {
			return err
		}
		// Failed setup can leave the table CREATING, when deletion is rejected.
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(err, ctx.Err())
		case <-timer.C:
		}
	}
}

func deleteTestBucket(ctx context.Context, client *s3.Client, account, bucket string) error {
	owner := aws.String(account)
	versions := s3.NewListObjectVersionsPaginator(client, &s3.ListObjectVersionsInput{Bucket: aws.String(bucket), ExpectedBucketOwner: owner})
	for versions.HasMorePages() {
		page, err := versions.NextPage(ctx)
		if apiErr, ok := errors.AsType[smithy.APIError](err); ok && apiErr.ErrorCode() == "NoSuchBucket" {
			return nil
		}
		if err != nil {
			return err
		}
		var objects []s3types.ObjectIdentifier
		for _, version := range page.Versions {
			objects = append(objects, s3types.ObjectIdentifier{Key: version.Key, VersionId: version.VersionId})
		}
		for _, marker := range page.DeleteMarkers {
			objects = append(objects, s3types.ObjectIdentifier{Key: marker.Key, VersionId: marker.VersionId})
		}
		if len(objects) == 0 {
			continue
		}
		// A page holds at most 1000 entries, DeleteObjects' batch limit.
		out, err := client.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String(bucket), ExpectedBucketOwner: owner, Delete: &s3types.Delete{Objects: objects, Quiet: aws.Bool(true)}})
		if err != nil {
			return err
		}
		if len(out.Errors) != 0 {
			return fmt.Errorf("delete object versions: %+v", out.Errors)
		}
	}
	uploads := s3.NewListMultipartUploadsPaginator(client, &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket), ExpectedBucketOwner: owner})
	for uploads.HasMorePages() {
		page, err := uploads.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, upload := range page.Uploads {
			if _, err := client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(bucket), Key: upload.Key, UploadId: upload.UploadId, ExpectedBucketOwner: owner}); err != nil {
				return err
			}
		}
	}
	_, err := client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket), ExpectedBucketOwner: owner})
	return err
}

func newTempdir() (string, func()) {
	tempdir, err := os.MkdirTemp("/tmp", "git_remote_aws_")
	if err != nil {
		panic(err)
	}
	return tempdir, func() { _ = os.RemoveAll(tempdir) }
}

func newUuid() string {
	return uuid.Must(uuid.NewV4()).String()
}

func setCommitDate() {
	date := "Aug 1 00:00:00 2022 +0000"
	_ = os.Setenv("GIT_COMMITTER_DATE", date)
	_ = os.Setenv("GIT_AUTHOR_DATE", date)
}

func listKeys(bucket, prefix string) []string {
	var got []string
	out, err := testAWSClients().s3.ListObjects(context.Background(), &s3.ListObjectsInput{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	})
	if err != nil {
		panic(err)
	}
	for _, c := range out.Contents {
		_, tail, ok := strings.Cut(*c.Key, "/")
		if !ok {
			panic("object key is missing its repository separator")
		}
		if strings.Contains(tail, "..") {
			got = append(got, path.Base(tail))
		}
	}
	sort.Strings(got)
	return got
}

func gitLog(dir string) []string {
	return strings.Split(runAtOut(dir, "git", "log", "--format=%H"), "\n")
}

func assertBundleKeys(t *testing.T, bucket, prefix string, expected []string) {
	t.Helper()
	got := listKeys(bucket, prefix)
	want := append([]string{}, expected...)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func assertLog(t *testing.T, dir string, expected []string) {
	t.Helper()
	got := gitLog(dir)
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("got %v, want %v", got, expected)
	}
}

func getRepoMeta(table, bucket, prefix string) *RepoMeta {
	repoMeta, err := dynamolock.Read[RepoMeta](context.Background(), testAWSClients().dynamodb, table, bucket+"/"+prefix)
	if err != nil {
		panic(err)
	}
	if repoMeta == nil {
		panic("repo metadata not found")
	}
	return repoMeta
}

func deleteObject(bucket, key string) {
	_, err := testAWSClients().s3.DeleteObject(context.Background(), &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		panic(err)
	}
}

func putObject(bucket, key, body string) {
	_, err := testAWSClients().s3.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   strings.NewReader(body),
	})
	if err != nil {
		panic(err)
	}
}

func TestBundleNameParts(t *testing.T) {
	sha1A := strings.Repeat("a", 40)
	sha1B := strings.Repeat("b", 40)
	sha256A := strings.Repeat("1", 64)
	sha256B := strings.Repeat("2", 64)

	got := bundleNameParts(sha1A + ".." + sha1B)
	if !reflect.DeepEqual(got, []string{sha1A, sha1B}) {
		t.Fatalf("got %v", got)
	}
	if got := hashEnd(sha1A + ".." + sha1B); got != sha1B {
		t.Fatalf("hashEnd got %s, expected %s", got, sha1B)
	}

	got = bundleNameParts(sha256A + ".." + sha256B)
	if !reflect.DeepEqual(got, []string{sha256A, sha256B}) {
		t.Fatalf("got %v", got)
	}
	if got := hashEnd(sha256A + ".." + sha256B); got != sha256B {
		t.Fatalf("hashEnd got %s, expected %s", got, sha256B)
	}

	mustPanicContains(t, "invalid bundle name", func() { bundleNameParts("../" + sha1A + ".." + sha1B) })
	mustPanicContains(t, "invalid bundle name", func() { bundleNameParts(sha1A + "/" + sha1B) })
	mustPanicContains(t, "invalid bundle name", func() { bundleNameParts(strings.ToUpper(sha1A) + ".." + sha1B) })
	mustPanicContains(t, "invalid bundle name", func() { bundleNameParts(sha1A + "." + sha1B) })
	mustPanicContains(t, "invalid bundle name", func() { bundleNameParts(sha1A + ".." + strings.Repeat("g", 40)) })
	mustPanicContains(t, "mixed hash lengths", func() { bundleNameParts(sha1A + ".." + sha256B) })
}

func TestBundleNamesFromMetadata(t *testing.T) {
	sha1A := strings.Repeat("a", 40)
	sha1B := strings.Repeat("b", 40)
	sha1C := strings.Repeat("c", 40)

	got := bundleNamesFromMetadata("test metadata", []byte(sha1A+".."+sha1B+"\n"+sha1B+".."+sha1C+"\n"))
	if !reflect.DeepEqual(got, []string{sha1A + ".." + sha1B, sha1B + ".." + sha1C}) {
		t.Fatalf("got %v", got)
	}

	mustPanicContains(t, "bundles metadata is empty", func() { bundleNamesFromMetadata("test metadata", nil) })
	mustPanicContains(t, "bundles metadata is empty", func() { bundleNamesFromMetadata("test metadata", []byte("\n\n")) })
	mustPanicContains(t, "invalid bundle name", func() { bundleNamesFromMetadata("test metadata", []byte("../"+sha1A+".."+sha1B)) })
}

func TestRefBranch(t *testing.T) {
	for _, branch := range []string{"master", "archive/home", "archive/HEAD", "archive/-home", "archive/v1.0"} {
		t.Run(branch, func(t *testing.T) {
			defer func() {
				if value := recover(); value != nil {
					t.Fatalf("valid branch rejected: %v", value)
				}
			}()
			if got := refBranch(t.Context(), "refs/heads/"+branch); got != branch {
				t.Fatalf("branch changed: %q, want %q", got, branch)
			}
		})
	}
	mustPanicContains(t, "ref is not a branch", func() { refBranch(t.Context(), "refs/tags/v1") })
	mustPanicContains(t, "ref is not a branch", func() { refBranch(t.Context(), "HEAD") })
	for _, branch := range []string{"", "main.lock", "archive.lock/home", "main.", "archive/.hidden", "archive//home", "archive/../home", "HEAD", "-main", "@{-1}", "main\x00other"} {
		t.Run("invalid/"+branch, func(t *testing.T) {
			mustPanicContains(t, "invalid branch", func() { refBranch(t.Context(), "refs/heads/"+branch) })
		})
	}
}

func TestBasic(t *testing.T) {
	dir, cleanup := newTempdir()
	defer cleanup()

	table, bucket, prefix := getTestBucketAndTable(t)

	publicKey := setupEphemeralKeys(t)

	runAt(dir, "bash", "-c", "echo "+publicKey+" > .publickeys")
	runAt(dir, "git", "init")
	runAt(dir, "git", "config", "commit.gpgsign", "false")
	configureGitIdentity(dir)
	runAt(dir, "git", "remote", "add", "origin", "aws://"+bucket+"+"+table+"/"+prefix)

	runAt(dir, "bash", "-c", "echo foo >> bar")
	runAt(dir, "git", "add", ".")
	runAt(dir, "git", "commit", "-m", "message")
	first := runAtOut(dir, "git", "rev-parse", "HEAD")
	runAt(dir, "git", "push", "-u", "origin", "master")
	assertBundleKeys(t, bucket, prefix, []string{zeroHash + ".." + first})

	runAt(dir, "bash", "-c", "echo foo >> bar")
	runAt(dir, "git", "add", ".")
	runAt(dir, "git", "commit", "-m", "message")
	second := runAtOut(dir, "git", "rev-parse", "HEAD")
	runAt(dir, "git", "push", "-u", "origin", "master")
	assertBundleKeys(t, bucket, prefix, []string{
		zeroHash + ".." + first,
		first + ".." + second,
	})

	dir2, cleanup2 := newTempdir()
	defer cleanup2()
	runAt(dir2, "git", "clone", "aws://"+bucket+"+"+table+"/"+prefix)
	assertLog(t, dir2+"/"+prefix, []string{second, first})
}

func TestBasicSha256(t *testing.T) {
	dir, cleanup := newTempdir()
	defer cleanup()

	table, bucket, prefix := getTestBucketAndTable(t)

	publicKey := setupEphemeralKeys(t)

	runAt(dir, "bash", "-c", "echo "+publicKey+" > .publickeys")
	runAt(dir, "git", "init", "--object-format=sha256")
	runAt(dir, "git", "config", "commit.gpgsign", "false")
	configureGitIdentity(dir)
	runAt(dir, "git", "remote", "add", "origin", "aws://"+bucket+"+"+table+"/"+prefix)

	runAt(dir, "bash", "-c", "echo foo >> bar")
	runAt(dir, "git", "add", ".")
	runAt(dir, "git", "commit", "-m", "message")
	first := runAtOut(dir, "git", "rev-parse", "HEAD")
	runAt(dir, "git", "push", "-u", "origin", "master")
	assertBundleKeys(t, bucket, prefix, []string{zeroHash256 + ".." + first})

	runAt(dir, "bash", "-c", "echo foo >> bar")
	runAt(dir, "git", "add", ".")
	runAt(dir, "git", "commit", "-m", "message")
	second := runAtOut(dir, "git", "rev-parse", "HEAD")
	runAt(dir, "git", "push", "-u", "origin", "master")
	assertBundleKeys(t, bucket, prefix, []string{
		zeroHash256 + ".." + first,
		first + ".." + second,
	})

	dir2, cleanup2 := newTempdir()
	defer cleanup2()
	runAt(dir2, "git", "clone", "aws://"+bucket+"+"+table+"/"+prefix)
	assertLog(t, dir2+"/"+prefix, []string{second, first})
}

func TestPushBeforePullShouldFailSha256(t *testing.T) {
	dir, cleanup := newTempdir()
	defer cleanup()

	table, bucket, prefix := getTestBucketAndTable(t)

	publicKey := setupEphemeralKeys(t)

	runAt(dir, "bash", "-c", "echo "+publicKey+" > .publickeys")
	runAt(dir, "git", "init", "--object-format=sha256")
	runAt(dir, "git", "config", "commit.gpgsign", "false")
	configureGitIdentity(dir)
	runAt(dir, "git", "remote", "add", "origin", "aws://"+bucket+"+"+table+"/"+prefix)

	runAt(dir, "bash", "-c", "echo foo >> bar")
	runAt(dir, "git", "add", ".")
	runAt(dir, "git", "commit", "-m", "message")
	first := runAtOut(dir, "git", "rev-parse", "HEAD")
	runAt(dir, "git", "push", "-u", "origin", "master")
	assertBundleKeys(t, bucket, prefix, []string{zeroHash256 + ".." + first})

	runAt(dir, "bash", "-c", "echo foo >> bar")
	runAt(dir, "git", "add", ".")
	runAt(dir, "git", "commit", "-m", "message")
	second := runAtOut(dir, "git", "rev-parse", "HEAD")
	runAt(dir, "git", "push", "-u", "origin", "master")
	assertBundleKeys(t, bucket, prefix, []string{
		zeroHash256 + ".." + first,
		first + ".." + second,
	})

	dir2, cleanup2 := newTempdir()
	defer cleanup2()
	runAt(dir2, "git", "clone", "aws://"+bucket+"+"+table+"/"+prefix)
	dir2 = dir2 + "/" + prefix
	runAt(dir2, "git", "config", "commit.gpgsign", "false")
	configureGitIdentity(dir2)
	assertLog(t, dir2, []string{second, first})

	runAt(dir2, "bash", "-c", "echo foo >> bar")
	runAt(dir2, "git", "add", ".")
	runAt(dir2, "git", "commit", "-m", "message")
	third := runAtOut(dir2, "git", "rev-parse", "HEAD")
	runAt(dir2, "git", "push", "-u", "origin", "master")
	assertBundleKeys(t, bucket, prefix, []string{
		zeroHash256 + ".." + first,
		first + ".." + second,
		second + ".." + third,
	})

	runAt(dir, "bash", "-c", "echo should fail >> bar")
	runAt(dir, "git", "add", ".")
	runAt(dir, "git", "commit", "-m", "message")
	assertRunAtErrContains(t, dir, "remote has new commits, pull before pushing", "git", "push", "-u", "origin", "master")
}

func TestEncryption(t *testing.T) {
	setupEphemeralKeys(t)
	binary := buildGitRemoteAws(t)

	dir, cleanup := newTempdir()
	defer cleanup()
	runAt(dir, "bash", "-c", "echo hello | "+binary+" -e > ciphertext")
	runAt(dir, "bash", "-c", "[ \"$(cat ciphertext)\" != \"hello\" ]")
	runAt(dir, "bash", "-c", "cat ciphertext | "+binary+" -d > plaintext")
	runAt(dir, "bash", "-c", "[ \"$(cat plaintext)\" = \"hello\" ]")
}

func TestFirstPushTagIsBanned(t *testing.T) {
	dir, cleanup := newTempdir()
	defer cleanup()
	table, bucket, prefix := getTestBucketAndTable(t)

	publicKey := setupEphemeralKeys(t)

	runAt(dir, "bash", "-c", "echo "+publicKey+" > .publickeys")
	runAt(dir, "git", "init")
	runAt(dir, "git", "config", "commit.gpgsign", "false")
	configureGitIdentity(dir)
	runAt(dir, "git", "remote", "add", "origin", "aws://"+bucket+"+"+table+"/"+prefix)

	runAt(dir, "bash", "-c", "echo foo > bar")
	runAt(dir, "git", "add", ".")
	runAt(dir, "git", "commit", "-m", "initial commit")
	runAt(dir, "git", "tag", "v1")

	assertRunAtErrContains(t, dir, "ref is not a branch", "git", "push", "origin", "v1")
}

func TestFirstPushSlashBranchRoundTrip(t *testing.T) {
	dir, cleanup := newTempdir()
	defer cleanup()
	table, bucket, prefix := getTestBucketAndTable(t)

	publicKey := setupEphemeralKeys(t)

	runAt(dir, "bash", "-c", "echo "+publicKey+" > .publickeys")
	runAt(dir, "git", "init")
	runAt(dir, "git", "config", "commit.gpgsign", "false")
	configureGitIdentity(dir)
	runAt(dir, "git", "remote", "add", "origin", "aws://"+bucket+"+"+table+"/"+prefix)

	runAt(dir, "bash", "-c", "echo foo > bar")
	runAt(dir, "git", "add", ".")
	runAt(dir, "git", "commit", "-m", "initial commit")
	runAt(dir, "git", "checkout", "-b", "feature/slash")

	runAt(dir, "git", "push", "-u", "origin", "refs/heads/feature/slash:refs/heads/feature/slash")
	clone := path.Join(t.TempDir(), "clone")
	runAt(path.Dir(clone), "git", "clone", "aws://"+bucket+"+"+table+"/"+prefix, clone)
	if branch := runAtOut(clone, "git", "symbolic-ref", "HEAD"); branch != "refs/heads/feature/slash" {
		t.Fatalf("clone changed the slash branch: %s", branch)
	}
	if tip, want := runAtOut(clone, "git", "rev-parse", "HEAD"), runAtOut(dir, "git", "rev-parse", "HEAD"); tip != want {
		t.Fatalf("clone changed the pushed tip: got %s, want %s", tip, want)
	}
	if data, err := os.ReadFile(path.Join(clone, "bar")); err != nil || string(data) != "foo\n" {
		t.Fatalf("clone did not check out the pushed content: %q, %v", data, err)
	}
}

func TestBranchesAndTagsAreBanned(t *testing.T) {
	dir, cleanup := newTempdir()
	defer cleanup()
	table, bucket, prefix := getTestBucketAndTable(t)

	publicKey := setupEphemeralKeys(t)

	runAt(dir, "bash", "-c", "echo "+publicKey+" > .publickeys")
	runAt(dir, "git", "init")
	runAt(dir, "git", "config", "commit.gpgsign", "false")
	configureGitIdentity(dir)
	runAt(dir, "git", "remote", "add", "origin", "aws://"+bucket+"+"+table+"/"+prefix)

	runAt(dir, "bash", "-c", "echo foo > bar")
	runAt(dir, "git", "add", ".")
	runAt(dir, "git", "commit", "-m", "initial commit")
	runAt(dir, "git", "push", "-u", "origin", "master")

	runAt(dir, "git", "checkout", "-b", "other-branch")
	runAt(dir, "bash", "-c", "echo extra >> extra.txt")
	runAt(dir, "git", "add", ".")
	runAt(dir, "git", "commit", "-m", "second branch commit")
	assertRunAtErrContains(t, dir, "local branch is different from remote branch", "git", "push", "origin", "other-branch")

	runAt(dir, "git", "tag", "test-tag")
	assertRunAtErrContains(t, dir, "ref is not a branch", "git", "push", "origin", "test-tag")
}

func TestMutatingHistoryIsBanned(t *testing.T) {
	dir, cleanup := newTempdir()
	defer cleanup()
	table, bucket, prefix := getTestBucketAndTable(t)

	publicKey := setupEphemeralKeys(t)

	runAt(dir, "bash", "-c", "echo "+publicKey+" > .publickeys")
	runAt(dir, "git", "init")
	runAt(dir, "git", "config", "commit.gpgsign", "false")
	configureGitIdentity(dir)
	runAt(dir, "git", "remote", "add", "origin", "aws://"+bucket+"+"+table+"/"+prefix)

	runAt(dir, "bash", "-c", "echo foo > bar")
	runAt(dir, "git", "add", ".")
	runAt(dir, "git", "commit", "-m", "A")
	runAt(dir, "git", "push", "-u", "origin", "master")

	runAt(dir, "bash", "-c", "echo bar >> bar")
	runAt(dir, "git", "add", ".")
	runAt(dir, "git", "commit", "-m", "B")
	runAt(dir, "git", "push", "-u", "origin", "master")

	runAt(dir, "git", "reset", "--hard", "HEAD~1")
	runAt(dir, "bash", "-c", "echo baz >> bar")
	runAt(dir, "git", "add", ".")
	runAt(dir, "git", "commit", "-m", "C")

	assertRunAtErrContains(t, dir, "force push is not allowed", "git", "push", "-u", "origin", "master", "--force")
}

func TestPushWithoutPullShouldFail(t *testing.T) {
	dir, cleanup := newTempdir()
	defer cleanup()
	table, bucket, prefix := getTestBucketAndTable(t)

	publicKey := setupEphemeralKeys(t)

	runAt(dir, "bash", "-c", "echo "+publicKey+" > .publickeys")
	runAt(dir, "git", "init")
	runAt(dir, "git", "config", "commit.gpgsign", "false")
	configureGitIdentity(dir)
	runAt(dir, "git", "remote", "add", "origin", "aws://"+bucket+"+"+table+"/"+prefix)

	// first commit and push from dir
	runAt(dir, "bash", "-c", "echo first > file.txt")
	runAt(dir, "git", "add", ".")
	runAt(dir, "git", "commit", "-m", "commit 1")
	runAt(dir, "git", "push", "-u", "origin", "master")

	// clone into dir2
	dir2, cleanup2 := newTempdir()
	defer cleanup2()
	runAt(dir2, "git", "clone", "aws://"+bucket+"+"+table+"/"+prefix)

	// commit and push from dir2
	runAt(dir2+"/"+prefix, "bash", "-c", "echo second > file2.txt")
	runAt(dir2+"/"+prefix, "git", "add", ".")
	configureGitIdentity(dir2 + "/" + prefix)
	runAt(dir2+"/"+prefix, "git", "commit", "-m", "commit 2")
	runAt(dir2+"/"+prefix, "git", "push", "origin", "master")

	// meanwhile, dir never pulled the commit from dir2
	// if we try to push a new commit from dir, it should fail
	runAt(dir, "bash", "-c", "echo third > file3.txt")
	runAt(dir, "git", "add", ".")
	runAt(dir, "git", "commit", "-m", "commit 3")

	assertRunAtErrContains(t, dir, "remote has new commits, pull before pushing", "git", "push", "origin", "master")
}

func TestPushFailsWhenBundlesMetadataObjectIsMissing(t *testing.T) {
	dir, cleanup := newTempdir()
	defer cleanup()
	table, bucket, prefix := getTestBucketAndTable(t)

	publicKey := setupEphemeralKeys(t)

	runAt(dir, "bash", "-c", "echo "+publicKey+" > .publickeys")
	runAt(dir, "git", "init")
	runAt(dir, "git", "config", "commit.gpgsign", "false")
	configureGitIdentity(dir)
	runAt(dir, "git", "remote", "add", "origin", "aws://"+bucket+"+"+table+"/"+prefix)

	runAt(dir, "bash", "-c", "echo first > file.txt")
	runAt(dir, "git", "add", ".")
	runAt(dir, "git", "commit", "-m", "commit 1")
	first := runAtOut(dir, "git", "rev-parse", "HEAD")
	runAt(dir, "git", "push", "-u", "origin", "master")

	repoMeta := getRepoMeta(table, bucket, prefix)
	if repoMeta.BundlesS3Key == "" {
		t.Fatal("expected bundles metadata key after first push")
	}
	deleteObject(bucket, repoMeta.BundlesS3Key)

	runAt(dir, "bash", "-c", "echo second > file2.txt")
	runAt(dir, "git", "add", ".")
	runAt(dir, "git", "commit", "-m", "commit 2")

	assertRunAtErrContains(t, dir, "read manifest", "git", "push", "origin", "master")
	assertBundleKeys(t, bucket, prefix, []string{zeroHash + ".." + first})
}

func TestPushFailsWhenBundlesMetadataObjectIsEmpty(t *testing.T) {
	dir, cleanup := newTempdir()
	defer cleanup()
	table, bucket, prefix := getTestBucketAndTable(t)

	publicKey := setupEphemeralKeys(t)

	runAt(dir, "bash", "-c", "echo "+publicKey+" > .publickeys")
	runAt(dir, "git", "init")
	runAt(dir, "git", "config", "commit.gpgsign", "false")
	configureGitIdentity(dir)
	runAt(dir, "git", "remote", "add", "origin", "aws://"+bucket+"+"+table+"/"+prefix)

	runAt(dir, "bash", "-c", "echo first > file.txt")
	runAt(dir, "git", "add", ".")
	runAt(dir, "git", "commit", "-m", "commit 1")
	first := runAtOut(dir, "git", "rev-parse", "HEAD")
	runAt(dir, "git", "push", "-u", "origin", "master")

	repoMeta := getRepoMeta(table, bucket, prefix)
	if repoMeta.BundlesS3Key == "" {
		t.Fatal("expected bundles metadata key after first push")
	}
	putObject(bucket, repoMeta.BundlesS3Key, "\n")

	runAt(dir, "bash", "-c", "echo second > file2.txt")
	runAt(dir, "git", "add", ".")
	runAt(dir, "git", "commit", "-m", "commit 2")

	assertRunAtErrContains(t, dir, "invalid manifest JSON", "git", "push", "origin", "master")
	assertBundleKeys(t, bucket, prefix, []string{zeroHash + ".." + first})
}

func TestFetchFailsWhenBundleMetadataContainsPathTraversal(t *testing.T) {
	dir, cleanup := newTempdir()
	defer cleanup()
	table, bucket, prefix := getTestBucketAndTable(t)

	publicKey := setupEphemeralKeys(t)

	runAt(dir, "bash", "-c", "echo "+publicKey+" > .publickeys")
	runAt(dir, "git", "init")
	runAt(dir, "git", "config", "commit.gpgsign", "false")
	configureGitIdentity(dir)
	runAt(dir, "git", "remote", "add", "origin", "aws://"+bucket+"+"+table+"/"+prefix)

	runAt(dir, "bash", "-c", "echo first > file.txt")
	runAt(dir, "git", "add", ".")
	runAt(dir, "git", "commit", "-m", "commit 1")
	runAt(dir, "git", "push", "-u", "origin", "master")

	repoMeta := getRepoMeta(table, bucket, prefix)
	startHash := strings.ReplaceAll(newUuid(), "-", "") + "00000000"
	endHash := strings.ReplaceAll(newUuid(), "-", "") + "11111111"
	maliciousBase := startHash + ".." + endHash
	maliciousBundle := "../" + maliciousBase
	escapedPath := path.Join("/tmp", maliciousBase)
	defer func() { _ = os.Remove(escapedPath) }()
	defer func() { _ = os.Remove(escapedPath + ".decrypted") }()
	if _, err := os.Stat(escapedPath); err == nil {
		t.Fatalf("test escape path already exists: %s", escapedPath)
	} else if !os.IsNotExist(err) {
		panic(err)
	}
	repo, err := parseRepository(prefix)
	if err != nil {
		t.Fatal(err)
	}
	m, err := testAWSClients().getManifest(t.Context(), bucket, repoMeta.BundlesS3Key, repo, repoMeta.Branch)
	if err != nil {
		t.Fatal(err)
	}
	m.Bundles[0].Range = maliciousBundle
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	putObject(bucket, repoMeta.BundlesS3Key, string(data))

	dir2, cleanup2 := newTempdir()
	defer cleanup2()
	assertRunAtErrContains(t, dir2, "invalid bundle chain", "git", "clone", "aws://"+bucket+"+"+table+"/"+prefix)
	if _, err := os.Stat(escapedPath); err == nil {
		t.Fatalf("bundle metadata path traversal wrote outside tempdir: %s", escapedPath)
	} else if !os.IsNotExist(err) {
		panic(err)
	}
}
