package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/gofrs/uuid/v5"
	"github.com/nathants/go-dynamolock"
	"github.com/nathants/go-libsodium"
	"github.com/nathants/libaws/lib"
)

func runAtResult(dir string, args ...string) (string, string, error) {
	fmt.Println("runAt", dir, args)
	cmd := exec.Command(args[0], args[1:]...)
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

func setupEphemeralKeys() (publicKeyHex string, cleanup func()) {
	libsodium.Init()
	pk, sk, err := libsodium.BoxKeypair()
	if err != nil {
		panic(err)
	}
	publicKeyHex = hex.EncodeToString(pk)
	secretKeyHex := hex.EncodeToString(sk)
	oldPub := os.Getenv("GIT_REMOTE_AWS_PUBLICKEY")
	oldSec := os.Getenv("GIT_REMOTE_AWS_SECRETKEY")
	err = os.Setenv("GIT_REMOTE_AWS_PUBLICKEY", publicKeyHex)
	if err != nil {
		panic(err)
	}
	err = os.Setenv("GIT_REMOTE_AWS_SECRETKEY", secretKeyHex)
	if err != nil {
		panic(err)
	}
	cleanup = func() {
		_ = os.Setenv("GIT_REMOTE_AWS_PUBLICKEY", oldPub)
		_ = os.Setenv("GIT_REMOTE_AWS_SECRETKEY", oldSec)
	}
	return publicKeyHex, cleanup
}

func runAt(dir string, args ...string) {
	fmt.Println("runAt", dir, args)
	cmd := exec.Command(args[0], args[1:]...)
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

func buildGitRemoteAws() string {
	root := repoRoot()
	binary := path.Join(root, "git-remote-aws")
	cmd := exec.Command("go", "build", "-o", binary, ".")
	cmd.Dir = root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		panic(err)
	}
	ensureGitRemoteAwsOnPath(binary)
	return binary
}

func ensureGitRemoteAwsOnPath(binary string) {
	root := path.Dir(binary)
	sep := string(os.PathListSeparator)
	pathEnv := os.Getenv("PATH")
	if pathEnv == "" {
		_ = os.Setenv("PATH", root)
	} else {
		_ = os.Setenv("PATH", root+sep+pathEnv)
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
	runAt(dir, "git", "config", "user.name", "git-remote-aws test")
	runAt(dir, "git", "config", "user.email", "git-remote-aws-test@example.com")
}

func getTestBucketAndTable() (string, string, string) {
	prefix := newUuid()
	account := os.Getenv("GIT_REMOTE_AWS_TEST_ACCOUNT")
	if account == "" {
		panic("GIT_REMOTE_AWS_TEST_ACCOUNT")
	}
	acc, err := lib.StsAccount(context.Background())
	if err != nil {
		panic(err)
	}
	if account != acc {
		panic("wrong aws account " + fmt.Sprintf("%s != %s", acc, account))
	}
	bucket := os.Getenv("GIT_REMOTE_AWS_TEST_BUCKET")
	if bucket == "" {
		panic("GIT_REMOTE_AWS_TEST_BUCKET")
	}
	table := os.Getenv("GIT_REMOTE_AWS_TEST_TABLE")
	if table == "" {
		panic("GIT_REMOTE_AWS_TEST_TABLE")
	}
	err = os.Setenv("ensure", "y") // git-remote-aws should create dynamodb tables if needed
	if err != nil {
		panic(err)
	}
	buildGitRemoteAws()
	setCommitDate()
	return table, bucket, prefix
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

func cleanupAws(table, bucket, prefix string) {
	_, err := lib.DynamoDBClient().DeleteItem(context.Background(), &dynamodb.DeleteItemInput{
		TableName: aws.String(table),
		Key: map[string]ddbtypes.AttributeValue{
			"id": &ddbtypes.AttributeValueMemberS{
				Value: bucket + "/" + prefix,
			},
		},
	})
	if err != nil {
		panic(err)
	}
	out, err := lib.S3Client().ListObjects(context.Background(), &s3.ListObjectsInput{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	})
	if err != nil {
		panic(err)
	}
	objects := []s3types.ObjectIdentifier{}
	for _, c := range out.Contents {
		objects = append(objects, s3types.ObjectIdentifier{
			Key: c.Key,
		})
	}
	if len(objects) == 0 {
		return
	}
	_, err = lib.S3Client().DeleteObjects(context.Background(), &s3.DeleteObjectsInput{
		Bucket: aws.String(bucket),
		Delete: &s3types.Delete{
			Objects: objects,
		},
	})
	if err != nil {
		panic(err)
	}
}

func listKeys(bucket, prefix string) []string {
	var got []string
	out, err := lib.S3Client().ListObjects(context.Background(), &s3.ListObjectsInput{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	})
	if err != nil {
		panic(err)
	}
	for _, c := range out.Contents {
		_, tail, err := lib.SplitOnce(*c.Key, "/")
		if err != nil {
			panic(err)
		}
		if strings.Contains(tail, "..") {
			got = append(got, tail)
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
		fmt.Println("got:", lib.PformatAlways(got))
		fmt.Println("expected:", lib.PformatAlways(want))
		t.Fatal()
	}
}

func assertLog(t *testing.T, dir string, expected []string) {
	t.Helper()
	got := gitLog(dir)
	if !reflect.DeepEqual(got, expected) {
		fmt.Println("got:", lib.PformatAlways(got))
		fmt.Println("expected:", lib.PformatAlways(expected))
		t.Fatal()
	}
}

func getRepoMeta(table, bucket, prefix string) *RepoMeta {
	repoMeta, err := dynamolock.Read[RepoMeta](context.Background(), table, bucket+"/"+prefix)
	if err != nil {
		panic(err)
	}
	if repoMeta == nil {
		panic("repo metadata not found")
	}
	return repoMeta
}

func deleteObject(bucket, key string) {
	_, err := lib.S3Client().DeleteObject(context.Background(), &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		panic(err)
	}
}

func putObject(bucket, key, body string) {
	_, err := lib.S3Client().PutObject(context.Background(), &s3.PutObjectInput{
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
	if got := refBranch("refs/heads/master"); got != "master" {
		t.Fatalf("got %s, expected master", got)
	}

	mustPanicContains(t, "ref is not a branch", func() { refBranch("refs/tags/v1") })
	mustPanicContains(t, "ref is not a branch", func() { refBranch("HEAD") })
	mustPanicContains(t, "branch names cannot be empty or contain slashes", func() { refBranch("refs/heads/") })
	mustPanicContains(t, "branch names cannot be empty or contain slashes", func() { refBranch("refs/heads/feature/slash") })
}

func TestBasic(t *testing.T) {
	dir, cleanup := newTempdir()
	defer cleanup()

	table, bucket, prefix := getTestBucketAndTable()
	defer cleanupAws(table, bucket, prefix)

	publicKey, cleanupKeys := setupEphemeralKeys()
	defer cleanupKeys()

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

	table, bucket, prefix := getTestBucketAndTable()
	defer cleanupAws(table, bucket, prefix)

	publicKey, cleanupKeys := setupEphemeralKeys()
	defer cleanupKeys()

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

	table, bucket, prefix := getTestBucketAndTable()
	defer cleanupAws(table, bucket, prefix)

	publicKey, cleanupKeys := setupEphemeralKeys()
	defer cleanupKeys()

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

func TestEncryption(_ *testing.T) {
	_, cleanupKeys := setupEphemeralKeys()
	defer cleanupKeys()
	binary := buildGitRemoteAws()

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
	table, bucket, prefix := getTestBucketAndTable()
	defer cleanupAws(table, bucket, prefix)

	publicKey, cleanupKeys := setupEphemeralKeys()
	defer cleanupKeys()

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

func TestFirstPushBranchWithSlashIsBanned(t *testing.T) {
	dir, cleanup := newTempdir()
	defer cleanup()
	table, bucket, prefix := getTestBucketAndTable()
	defer cleanupAws(table, bucket, prefix)

	publicKey, cleanupKeys := setupEphemeralKeys()
	defer cleanupKeys()

	runAt(dir, "bash", "-c", "echo "+publicKey+" > .publickeys")
	runAt(dir, "git", "init")
	runAt(dir, "git", "config", "commit.gpgsign", "false")
	configureGitIdentity(dir)
	runAt(dir, "git", "remote", "add", "origin", "aws://"+bucket+"+"+table+"/"+prefix)

	runAt(dir, "bash", "-c", "echo foo > bar")
	runAt(dir, "git", "add", ".")
	runAt(dir, "git", "commit", "-m", "initial commit")
	runAt(dir, "git", "checkout", "-b", "feature/slash")

	assertRunAtErrContains(t, dir, "branch names cannot be empty or contain slashes", "git", "push", "-u", "origin", "refs/heads/feature/slash:refs/heads/feature/slash")
}

func TestBranchesAndTagsAreBanned(t *testing.T) {
	dir, cleanup := newTempdir()
	defer cleanup()
	table, bucket, prefix := getTestBucketAndTable()
	defer cleanupAws(table, bucket, prefix)

	publicKey, cleanupKeys := setupEphemeralKeys()
	defer cleanupKeys()

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
	table, bucket, prefix := getTestBucketAndTable()
	defer cleanupAws(table, bucket, prefix)

	publicKey, cleanupKeys := setupEphemeralKeys()
	defer cleanupKeys()

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
	table, bucket, prefix := getTestBucketAndTable()
	defer cleanupAws(table, bucket, prefix)

	publicKey, cleanupKeys := setupEphemeralKeys()
	defer cleanupKeys()

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
	table, bucket, prefix := getTestBucketAndTable()
	defer cleanupAws(table, bucket, prefix)

	publicKey, cleanupKeys := setupEphemeralKeys()
	defer cleanupKeys()

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

	assertRunAtErrContains(t, dir, "failed to get bundles metadata", "git", "push", "origin", "master")
	assertBundleKeys(t, bucket, prefix, []string{zeroHash + ".." + first})
}

func TestPushFailsWhenBundlesMetadataObjectIsEmpty(t *testing.T) {
	dir, cleanup := newTempdir()
	defer cleanup()
	table, bucket, prefix := getTestBucketAndTable()
	defer cleanupAws(table, bucket, prefix)

	publicKey, cleanupKeys := setupEphemeralKeys()
	defer cleanupKeys()

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

	assertRunAtErrContains(t, dir, "bundles metadata is empty", "git", "push", "origin", "master")
	assertBundleKeys(t, bucket, prefix, []string{zeroHash + ".." + first})
}

func TestFetchFailsWhenBundleMetadataContainsPathTraversal(t *testing.T) {
	dir, cleanup := newTempdir()
	defer cleanup()
	table, bucket, prefix := getTestBucketAndTable()
	defer cleanupAws(table, bucket, prefix)

	publicKey, cleanupKeys := setupEphemeralKeys()
	defer cleanupKeys()

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
	putObject(bucket, repoMeta.BundlesS3Key, maliciousBundle)
	putObject(bucket, prefix+"/"+maliciousBundle, "not an encrypted bundle")

	dir2, cleanup2 := newTempdir()
	defer cleanup2()
	assertRunAtErrContains(t, dir2, "invalid bundle name", "git", "clone", "aws://"+bucket+"+"+table+"/"+prefix)
	if _, err := os.Stat(escapedPath); err == nil {
		t.Fatalf("bundle metadata path traversal wrote outside tempdir: %s", escapedPath)
	} else if !os.IsNotExist(err) {
		panic(err)
	}
}
