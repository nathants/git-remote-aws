package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/gofrs/uuid"
	"github.com/nathants/libaws/lib"
)

func runAtErr(dir string, args ...string) error {
	fmt.Println("runAt", dir, args)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	cmd.Dir = dir
	return cmd.Run()
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
	if bucket == "" {
		panic("GIT_REMOTE_AWS_TEST_TABLE")
	}
	_, filename, _, _ := runtime.Caller(1)
	root := path.Dir(filename)
	cmd := exec.Command("go", "build", ".")
	cmd.Dir = root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		panic(err)
	}
	var stdout bytes.Buffer
	cmd = exec.Command("which", "git-remote-aws")
	cmd.Stdout = &stdout
	err = cmd.Run()
	if err != nil {
		panic(err)
	}
	actualRoot := path.Dir(stdout.String())
	if root != actualRoot {
		panic(fmt.Sprintf("should have found git-remote-aws on PATH at %s, was found at %s", root, actualRoot))
	}
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
	date := "Aug 1 00:00:00 2022"
	_ = os.Setenv("GIT_COMMITTER_DATE", date)
	_ = os.Setenv("GIT_AUTHOR_DATE", date)
}

func cleanupAws(table, bucket, prefix string) {
	_, err := lib.DynamoDBClient().DeleteItem(&dynamodb.DeleteItemInput{
		TableName: aws.String(table),
		Key: map[string]*dynamodb.AttributeValue{
			"id": {S: aws.String(bucket + "/" + prefix)},
		},
	})
	if err != nil {
		panic(err)
	}
	out, err := lib.S3Client().ListObjects(&s3.ListObjectsInput{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	})
	if err != nil {
		panic(err)
	}
	objects := []*s3.ObjectIdentifier{}
	for _, c := range out.Contents {
		objects = append(objects, &s3.ObjectIdentifier{
			Key: c.Key,
		})
	}
	_, err = lib.S3Client().DeleteObjects(&s3.DeleteObjectsInput{
		Bucket: aws.String(bucket),
		Delete: &s3.Delete{
			Objects: objects,
		},
	})
	if err != nil {
		panic(err)
	}
}

func listKeys(bucket, prefix string) []string {
	var got []string
	out, err := lib.S3Client().ListObjects(&s3.ListObjectsInput{
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
	return got
}

func TestBasic(t *testing.T) {
	dir, cleanup := newTempdir()
	defer cleanup()
	// -
	table, bucket, prefix := getTestBucketAndTable()
	defer cleanupAws(table, bucket, prefix)
	// -
	runAt(dir, "bash", "-c", "cat ~/.git-remote-aws/publickey > .publickeys")
	runAt(dir, "git", "init")
	runAt(dir, "git", "config", "commit.gpgsign", "false")
	runAt(dir, "git", "remote", "add", "origin", "aws://"+bucket+"+"+table+"/"+prefix)
	// -
	runAt(dir, "bash", "-c", "echo foo >> bar")
	runAt(dir, "git", "add", ".")
	runAt(dir, "git", "commit", "-m", "message")
	runAt(dir, "git", "push", "-u", "origin", "master")
	got := listKeys(bucket, prefix)
	expected := []string{
		"0000000000000000000000000000000000000000..2179a6fcb6b47819cd97e8fa0c1723a9e7221988",
	}
	if !reflect.DeepEqual(got, expected) {
		fmt.Println("got:", lib.PformatAlways(got))
		fmt.Println("expected:", lib.PformatAlways(expected))
		t.Fatal()
	}
	// -
	runAt(dir, "bash", "-c", "echo foo >> bar")
	runAt(dir, "git", "add", ".")
	runAt(dir, "git", "commit", "-m", "message")
	runAt(dir, "git", "push", "-u", "origin", "master")
	got = listKeys(bucket, prefix)
	expected = []string{
		"0000000000000000000000000000000000000000..2179a6fcb6b47819cd97e8fa0c1723a9e7221988",
		"2179a6fcb6b47819cd97e8fa0c1723a9e7221988..5147bba478721d4569ae366ae9c70227e7036f9c",
	}
	if !reflect.DeepEqual(got, expected) {
		fmt.Println("got:", lib.PformatAlways(got))
		fmt.Println("expected:", lib.PformatAlways(expected))
		t.Fatal()
	}
	// -
	dir2, cleanup2 := newTempdir()
	defer cleanup2()
	runAt(dir2, "git", "clone", "aws://"+bucket+"+"+table+"/"+prefix)
	got = strings.Split(runAtOut(dir2+"/"+prefix, "git", "log", "--format=%H"), "\n")
	expected = []string{
		"5147bba478721d4569ae366ae9c70227e7036f9c",
		"2179a6fcb6b47819cd97e8fa0c1723a9e7221988",
	}
	if !reflect.DeepEqual(got, expected) {
		fmt.Println("got:", lib.PformatAlways(got))
		fmt.Println("expected:", lib.PformatAlways(expected))
		t.Fatal()
	}
}

func TestBasicSha256(t *testing.T) {
	dir, cleanup := newTempdir()
	defer cleanup()
	// -
	table, bucket, prefix := getTestBucketAndTable()
	defer cleanupAws(table, bucket, prefix)
	// -
	runAt(dir, "bash", "-c", "cat ~/.git-remote-aws/publickey > .publickeys")
	runAt(dir, "git", "init", "--object-format=sha256")
	runAt(dir, "git", "config", "commit.gpgsign", "false")
	runAt(dir, "git", "remote", "add", "origin", "aws://"+bucket+"+"+table+"/"+prefix)
	// -
	runAt(dir, "bash", "-c", "echo foo >> bar")
	runAt(dir, "git", "add", ".")
	runAt(dir, "git", "commit", "-m", "message")
	runAt(dir, "git", "push", "-u", "origin", "master")
	got := listKeys(bucket, prefix)
	expected := []string{
		"0000000000000000000000000000000000000000000000000000000000000000..f12ca64100fe68c55722f2ad619bfbde7e4e493dde3a7cb9eb65dbd43b7adf0a",
	}
	if !reflect.DeepEqual(got, expected) {
		fmt.Println("got:", lib.PformatAlways(got))
		fmt.Println("expected:", lib.PformatAlways(expected))
		t.Fatal()
	}
	// -
	runAt(dir, "bash", "-c", "echo foo >> bar")
	runAt(dir, "git", "add", ".")
	runAt(dir, "git", "commit", "-m", "message")
	runAt(dir, "git", "push", "-u", "origin", "master")
	got = listKeys(bucket, prefix)
	expected = []string{
		"0000000000000000000000000000000000000000000000000000000000000000..f12ca64100fe68c55722f2ad619bfbde7e4e493dde3a7cb9eb65dbd43b7adf0a",
		"f12ca64100fe68c55722f2ad619bfbde7e4e493dde3a7cb9eb65dbd43b7adf0a..4aa82686ab76e2efc9f8df2c7e857b50363981a74222204073f0d9b1a81c29e3",
	}
	if !reflect.DeepEqual(got, expected) {
		fmt.Println("got:", lib.PformatAlways(got))
		fmt.Println("expected:", lib.PformatAlways(expected))
		t.Fatal()
	}
	// -
	dir2, cleanup2 := newTempdir()
	defer cleanup2()
	runAt(dir2, "git", "clone", "aws://"+bucket+"+"+table+"/"+prefix)
	got = strings.Split(runAtOut(dir2+"/"+prefix, "git", "log", "--format=%H"), "\n")
	expected = []string{
		"4aa82686ab76e2efc9f8df2c7e857b50363981a74222204073f0d9b1a81c29e3",
		"f12ca64100fe68c55722f2ad619bfbde7e4e493dde3a7cb9eb65dbd43b7adf0a",
	}
	if !reflect.DeepEqual(got, expected) {
		fmt.Println("got:", lib.PformatAlways(got))
		fmt.Println("expected:", lib.PformatAlways(expected))
		t.Fatal()
	}
}

func TestPushBeforePullShouldFailSha256(t *testing.T) {
	dir, cleanup := newTempdir()
	defer cleanup()
	// -
	table, bucket, prefix := getTestBucketAndTable()
	defer cleanupAws(table, bucket, prefix)
	// -
	runAt(dir, "bash", "-c", "cat ~/.git-remote-aws/publickey > .publickeys")
	runAt(dir, "git", "init", "--object-format=sha256")
	runAt(dir, "git", "config", "commit.gpgsign", "false")
	runAt(dir, "git", "remote", "add", "origin", "aws://"+bucket+"+"+table+"/"+prefix)
	// -
	runAt(dir, "bash", "-c", "echo foo >> bar")
	runAt(dir, "git", "add", ".")
	runAt(dir, "git", "commit", "-m", "message")
	runAt(dir, "git", "push", "-u", "origin", "master")
	got := listKeys(bucket, prefix)
	expected := []string{
		"0000000000000000000000000000000000000000000000000000000000000000..f12ca64100fe68c55722f2ad619bfbde7e4e493dde3a7cb9eb65dbd43b7adf0a",
	}
	if !reflect.DeepEqual(got, expected) {
		fmt.Println("got:", lib.PformatAlways(got))
		fmt.Println("expected:", lib.PformatAlways(expected))
		t.Fatal()
	}
	// -
	runAt(dir, "bash", "-c", "echo foo >> bar")
	runAt(dir, "git", "add", ".")
	runAt(dir, "git", "commit", "-m", "message")
	runAt(dir, "git", "push", "-u", "origin", "master")
	got = listKeys(bucket, prefix)
	expected = []string{
		"0000000000000000000000000000000000000000000000000000000000000000..f12ca64100fe68c55722f2ad619bfbde7e4e493dde3a7cb9eb65dbd43b7adf0a",
		"f12ca64100fe68c55722f2ad619bfbde7e4e493dde3a7cb9eb65dbd43b7adf0a..4aa82686ab76e2efc9f8df2c7e857b50363981a74222204073f0d9b1a81c29e3",
	}
	if !reflect.DeepEqual(got, expected) {
		fmt.Println("got:", lib.PformatAlways(got))
		fmt.Println("expected:", lib.PformatAlways(expected))
		t.Fatal()
	}
	// -
	dir2, cleanup2 := newTempdir()
	defer cleanup2()
	runAt(dir2, "git", "clone", "aws://"+bucket+"+"+table+"/"+prefix)
	dir2 = dir2 + "/" + prefix
	runAt(dir2, "git", "config", "commit.gpgsign", "false")
	got = strings.Split(runAtOut(dir2, "git", "log", "--format=%H"), "\n")
	expected = []string{
		"4aa82686ab76e2efc9f8df2c7e857b50363981a74222204073f0d9b1a81c29e3",
		"f12ca64100fe68c55722f2ad619bfbde7e4e493dde3a7cb9eb65dbd43b7adf0a",
	}
	if !reflect.DeepEqual(got, expected) {
		fmt.Println("got:", lib.PformatAlways(got))
		fmt.Println("expected:", lib.PformatAlways(expected))
		t.Fatal()
	}
	// -
	runAt(dir2, "bash", "-c", "echo foo >> bar")
	runAt(dir2, "git", "add", ".")
	runAt(dir2, "git", "commit", "-m", "message")
	runAt(dir2, "git", "push", "-u", "origin", "master")
	got = listKeys(bucket, prefix)
	expected = []string{
		"0000000000000000000000000000000000000000000000000000000000000000..f12ca64100fe68c55722f2ad619bfbde7e4e493dde3a7cb9eb65dbd43b7adf0a",
		"4aa82686ab76e2efc9f8df2c7e857b50363981a74222204073f0d9b1a81c29e3..2d71f3e7501db5c58d5925ff8ba8013601c46a320a8e917ffa8de7312c3a1c68",
		"f12ca64100fe68c55722f2ad619bfbde7e4e493dde3a7cb9eb65dbd43b7adf0a..4aa82686ab76e2efc9f8df2c7e857b50363981a74222204073f0d9b1a81c29e3",
	}
	if !reflect.DeepEqual(got, expected) {
		fmt.Println("got:", lib.PformatAlways(got))
		fmt.Println("expected:", lib.PformatAlways(expected))
		t.Fatal()
	}
	// -
	runAt(dir, "bash", "-c", "echo should fail >> bar")
	runAt(dir, "git", "add", ".")
	runAt(dir, "git", "commit", "-m", "message")
	if runAtErr(dir, "git", "push", "-u", "origin", "master") == nil {
		t.Fatal("should have failed because need to pull before push")
	}
}

func TestEncryption() {
	dir, cleanup := newTempdir()
	defer cleanup()
	runAt(dir, "bash", "-c", "echo hello | git-remote-aws -e > ciphertext")
	runAt(dir, "bash", "-c", "[ \"$(cat ciphertext)\" != \"hello\" ]")
	runAt(dir, "bash", "-c", "cat ciphertext | git-remote-aws -d > plaintext")
	runAt(dir, "bash", "-c", "[ \"$(cat plaintext)\" = \"hello\" ]")
}
