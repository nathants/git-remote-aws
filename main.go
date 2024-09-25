package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbattribute"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/nathants/go-dynamolock"
	"github.com/nathants/go-libsodium"
	"github.com/nathants/libaws/lib"
)

const (
	tempdirPrefix = "git_remote_aws_"
	defaultBranch = "master"
	zeroHash      = "0000000000000000000000000000000000000000"
	zeroHash256   = "0000000000000000000000000000000000000000000000000000000000000000"
)

func reverse[T any](s []T) []T {
	res := []T{}
	for i := len(s) - 1; i >= 0; i-- {
		res = append(res, s[i])
	}
	return res
}

func last[T any](s []T) T {
	return s[len(s)-1]
}

// "aaa..bbb" => "bbb"
func hashEnd(x string) string {
	return strings.SplitN(x, "..", 2)[1]
}

func getBundles(bucket, s3Key string) []string {
	var bundles []string
	fmt.Fprintln(os.Stderr, "get s3://"+bucket+"/"+s3Key)
	out, err := lib.S3Client().GetObject(&s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(s3Key),
	})
	if err == nil {
		defer func() { _ = out.Body.Close() }()
		data, err := io.ReadAll(out.Body)
		if err != nil {
			panic(err)
		}
		for _, bundle := range strings.Split(string(data), "\n") {
			if bundle != "" {
				bundles = append(bundles, bundle)
			}
		}
	}
	return bundles
}

// git helper capabilities
func capabilities() {
	fmt.Println("push")
	fmt.Println("fetch")
	fmt.Println("")
}

type RepoMeta struct {
	BundlesS3Key string `json:"bundles"`
	Branch       string `json:"branch"`
}

// git helper push
func push(table, bucket, prefix, command string) {

	// parse args and assert single branch
	refs := strings.SplitN(command[len("push "):], ":", 2)
	localRef := refs[0]
	remoteRef := refs[1]
	localBranch := last(strings.Split(localRef, "/"))
	remoteBranch := last(strings.Split(remoteRef, "/"))
	if localBranch != remoteBranch {
		panic(fmt.Sprintf("local branch is different from remote branch, %s != %s", localBranch, remoteBranch))
	}
	branch := localBranch

	// fetch and lock remote bundles, defering unlock
	fmt.Fprintln(os.Stderr, "get dynamodb://"+table+"/"+bucket+"/"+prefix)
	unlock, item, err := dynamolock.Lock(context.Background(), table, bucket+"/"+prefix, 10*time.Second, 1*time.Second)
	if err != nil {
		panic(err)
	}
	unlocked := false
	defer func() {
		if !unlocked {
			err := unlock(item)
			if err != nil {
				panic(err)
			}
		}
	}()
	repoMeta := RepoMeta{}
	err = dynamodbattribute.UnmarshalMap(item, &repoMeta)
	if err != nil {
		panic(err)
	}
	bundles := getBundles(bucket, repoMeta.BundlesS3Key)

	if repoMeta.Branch != "" {
		// assert local branch is the same as remote
		if branch != repoMeta.Branch {
			panic(fmt.Sprintf("you cannot have multiple branches in a remote, %s != %s", branch, repoMeta.Branch))
		}
	} else {
		// or set remote branch if it doesn't yet exist
		repoMeta.Branch = branch
	}

	// find latest local hash
	var stdout bytes.Buffer
	cmd := exec.Command("git", "log", "--format=%H", "-1", branch)
	cmd.Stdout = &stdout
	err = cmd.Run()
	if err != nil {
		panic(err)
	}
	hash := strings.Trim(stdout.String(), "\n")

	// if remote has data and latest hash equals local hash, there is nothing to push
	if len(bundles) > 0 && hashEnd(last(bundles)) == hash {
		fmt.Println()
		return
	}

	// if remote has data and latest hash is unknown locally, we need to pull before pushing
	if len(bundles) > 0 {
		hashRemote := hashEnd(last(bundles))
		cmd := exec.Command("git", "branch", branch, "--contains", hashRemote)
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		err := cmd.Run()
		if err != nil {
			panic("failed to run: git branch --contains " + branch + " " + hashRemote)
		}
		if stdout.String() == "" {
			panic("remote has new commits, pull before pushing")
		}
	}

	// create tempdir and defer cleanup
	tempdir, err := os.MkdirTemp("/tmp", tempdirPrefix)
	if err != nil {
		panic(err)
	}
	defer func() { _ = os.RemoveAll(tempdir) }()

	// setup bundle name and bundle target. a new remote bundles all
	// commits. an existing remote bundles all commits since the last
	// bundle in remote.
	bundleTarget := branch
	bundleName := zeroHash + ".." + hash
	if len(hash) == 64 {
		bundleName = zeroHash256 + ".." + hash
	}
	if len(bundles) > 0 {
		bundleTarget = hashEnd(last(bundles)) + ".." + branch
		bundleName = hashEnd(last(bundles)) + ".." + hash
	}

	// create bundle
	bundleFile := tempdir + "/" + bundleName
	fmt.Fprintln(os.Stderr, "git bundle:", path.Base(bundleFile))
	cmd = exec.Command("git", "bundle", "create", bundleFile, bundleTarget)
	var bundleStdout bytes.Buffer
	var bundleStderr bytes.Buffer
	cmd.Stderr = &bundleStderr
	cmd.Stdout = &bundleStdout
	err = cmd.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, bundleStderr.String())
		fmt.Fprintln(os.Stderr, bundleStdout.String())
		panic(err)
	}

	// encrypt
	bundleFileEncrypted := bundleFile + ".encrypted"
	r, err := os.Open(bundleFile)
	if err != nil {
		panic(err)
	}
	w, err := os.Create(bundleFileEncrypted)
	if err != nil {
		panic(err)
	}
	err = libsodium.StreamEncryptRecipients(publicKeys(), r, w)
	if err != nil {
		panic(err)
	}
	err = r.Close()
	if err != nil {
		panic(err)
	}
	err = w.Close()
	if err != nil {
		panic(err)
	}

	// put bundle to s3
	f, err := os.Open(bundleFileEncrypted)
	if err != nil {
		panic(err)
	}
	fmt.Fprintln(os.Stderr, "put s3://"+bucket+"/"+prefix+"/"+bundleName)
	_, err = lib.S3Client().PutObject(&s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(prefix + "/" + bundleName),
		Body:   f,
	})
	if err != nil {
		panic(err)
	}

	// put bundles metadata to s3 and set key in metadata
	bundles = append(bundles, bundleName)
	bundleData := []byte(strings.Join(bundles, "\n"))
	oldBundlesS3Key := repoMeta.BundlesS3Key
	repoMeta.BundlesS3Key = prefix + "/" + "bundles_" + hash
	fmt.Fprintln(os.Stderr, "put s3://"+bucket+"/"+repoMeta.BundlesS3Key)
	_, err = lib.S3Client().PutObject(&s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(repoMeta.BundlesS3Key),
		Body:   bytes.NewReader(bundleData),
	})
	if err != nil {
		panic(err)
	}

	// unlock with new data
	item, err = dynamodbattribute.MarshalMap(repoMeta)
	if err != nil {
		panic(err)
	}
	fmt.Fprintln(os.Stderr, "put dynamodb://"+table+"/"+bucket+"/"+prefix)
	err = unlock(item)
	if err != nil {
		panic(err)
	}
	unlocked = true

	// delete previous bundles metadata when a new one is written
	if oldBundlesS3Key != repoMeta.BundlesS3Key && oldBundlesS3Key != "" {
		_, err = lib.S3Client().DeleteObject(&s3.DeleteObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(oldBundlesS3Key),
		})
		if err != nil {
			panic(err)
		}
	}

	// communicate with git caller
	fmt.Println("ok", localRef)
	fmt.Println("")
}

func secretKey() []byte {
	home := os.Getenv("HOME")
	if home == "" {
		panic("$HOME is empty")
	}
	secretKeyFile := home + "/.git-remote-aws/secretkey"
	env := os.Getenv("GIT_REMOTE_AWS_SECRETKEY")
	if env != "" {
		secretKeyFile = env
	}
	data, err := os.ReadFile(secretKeyFile)
	if err != nil {
		panic(err)
	}
	secretKey, err := hex.DecodeString(string(data))
	if err != nil {
		panic(err)
	}
	return secretKey
}

func publicKey() [][]byte {
	var publicKeys [][]byte
	home := os.Getenv("HOME")
	if home == "" {
		panic("$HOME is empty")
	}
	publicKeyFile := home + "/.git-remote-aws/publickey"
	env := os.Getenv("GIT_REMOTE_AWS_PUBLICKEY")
	if env != "" {
		publicKeyFile = env
	}
	data, err := os.ReadFile(publicKeyFile)
	if err != nil {
		panic(err)
	}
	publicKey, err := hex.DecodeString(string(data))
	if err != nil {
		panic(err)
	}
	publicKeys = append(publicKeys, publicKey)
	return publicKeys
}

// git helper fetch
func fetch(table, bucket, prefix, command string) {

	// parse args to get branch name
	parts := strings.SplitN(command[len("fetch "):], " ", 2) // fetch $shasum refs/heads/$branch
	ref := parts[1]                                          // refs/heads/master
	branch := last(strings.Split(ref, "/"))

	repoMeta := RepoMeta{}

	fmt.Fprintln(os.Stderr, "get dynamodb://"+table+"/"+bucket+"/"+prefix)
	item, err := dynamolock.Read(context.Background(), table, bucket+"/"+prefix)
	if err != nil {
		panic(err)
	}
	err = dynamodbattribute.UnmarshalMap(item, &repoMeta)
	if err != nil {
		panic(err)
	}

	// fetch remote branch and fail if it exists and is not equal to local branch
	if repoMeta.Branch == "" {
		panic("remote not found")
	}
	if branch != repoMeta.Branch {
		panic(fmt.Sprintf("remote branch does not match local branch, %s != %s", branch, repoMeta.Branch))
	}

	// fetch remote bundles metadata
	var bundles []string
	if repoMeta.BundlesS3Key != "" {
		bundles = getBundles(bucket, repoMeta.BundlesS3Key)
	}

	// walk backward from newest to oldest through remote bundles.
	// stop when the bundle end commit exists in the local data. all
	// bundles which do not exist in local need to be fetched.
	var bundlesToFetch []string
	for _, bundle := range reverse(bundles) {
		hash := hashEnd(bundle)
		cmd := exec.Command("git", "branch", branch, "--contains", hash)
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		err := cmd.Run()
		if err == nil && stdout.String() != "" {
			break
		}
		bundlesToFetch = append(bundlesToFetch, bundle)
	}
	bundlesToFetch = reverse(bundlesToFetch)

	// setup tempdir and defer cleanup
	tempdir, err := os.MkdirTemp("/tmp", tempdirPrefix)
	if err != nil {
		panic(err)
	}
	defer func() { _ = os.RemoveAll(tempdir) }()

	// fetch remote bundles and unpack them
	for _, bundle := range bundlesToFetch {

		// fetch object
		fmt.Fprintln(os.Stderr, "get s3://"+bucket+"/"+prefix+"/"+bundle)
		out, err := lib.S3Client().GetObject(&s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(prefix + "/" + bundle),
		})
		if err != nil {
			panic(err)
		}
		bundleFileEncrypted := tempdir + "/" + bundle
		defer func() { _ = out.Body.Close() }()
		f, err := os.Create(bundleFileEncrypted)
		if err != nil {
			panic(err)
		}
		_, err = io.Copy(f, out.Body)
		if err != nil {
			panic(err)
		}
		err = f.Close()
		if err != nil {
			panic(err)
		}

		// decrypt
		bundleFile := bundleFileEncrypted + ".decrypted"
		r, err := os.Open(bundleFileEncrypted)
		if err != nil {
			panic(err)
		}
		w, err := os.Create(bundleFile)
		if err != nil {
			panic(err)
		}
		err = libsodium.StreamDecryptRecipients(secretKey(), r, w)
		if err != nil {
			panic(err)
		}

		// import
		fmt.Fprintln(os.Stderr, "git unbundle:", path.Base(bundleFileEncrypted))
		cmd := exec.Command("git", "bundle", "unbundle", bundleFile)
		var bundleStdout bytes.Buffer
		var bundleStderr bytes.Buffer
		cmd.Stderr = &bundleStderr
		cmd.Stdout = &bundleStdout
		err = cmd.Run()
		if err != nil {
			fmt.Fprintln(os.Stderr, bundleStderr.String())
			fmt.Fprintln(os.Stderr, bundleStdout.String())
			panic(err)
		}

		// remove
		err = os.Remove(bundleFileEncrypted)
		if err != nil {
		    panic(err)
		}
		err = os.Remove(bundleFile)
		if err != nil {
		    panic(err)
		}

	}

	// communicate with git caller
	fmt.Println("")
}

// git helper list
func list(table, bucket, prefix string) {
	repoMeta := RepoMeta{}

	fmt.Fprintln(os.Stderr, "get dynamodb://"+table+"/"+bucket+"/"+prefix)
	item, err := dynamolock.Read(context.Background(), table, bucket+"/"+prefix)
	if err != nil {
		panic(err)
	}
	err = dynamodbattribute.UnmarshalMap(item, &repoMeta)
	if err != nil {
		panic(err)
	}

	// find remote branch, falling back to default branch
	branch := repoMeta.Branch
	if branch == "" {
		branch = defaultBranch
	}

	// fetch remote bundles metadata
	var remoteBundles []string
	if repoMeta.BundlesS3Key != "" {
		remoteBundles = getBundles(bucket, repoMeta.BundlesS3Key)
	}

	// communicate with git caller
	if len(remoteBundles) > 0 {
		// if remote bundles exist, print the latest hash
		hash := hashEnd(last(remoteBundles))
		if len(hash) == 64 {
			fmt.Println(":object-format sha256")
		}
		fmt.Println(hash, "refs/heads/"+branch)
		fmt.Println("@refs/heads/"+branch, "HEAD")
	} else {
		// else print the zero hash
		var stdout bytes.Buffer
		cmd := exec.Command("git", "config", "extensions.objectformat")
		cmd.Stdout = &stdout
		err := cmd.Run()
		objectFormat := ""
		if err == nil {
			objectFormat = strings.Trim(stdout.String(), "\n")
		}
		if objectFormat == "sha256" {
			fmt.Println(zeroHash256, "HEAD")
		} else {
			fmt.Println(zeroHash, "HEAD")
		}
	}
	fmt.Println("")
}

func gitHelper() {

	// parse remote path to get bucket and prefix
	// remoteName := os.Args[1]
	remotePath := os.Args[2]
	if !strings.HasPrefix(remotePath, "aws://") {
		panic("missing prefix aws:// " + remotePath)
	}
	bucketAndTable, prefix, err := lib.SplitOnce(strings.TrimPrefix(remotePath, "aws://"), "/")
	if err != nil {
		panic(err)
	}
	prefix = strings.TrimSuffix(prefix, "/")
	bucket, table, err := lib.SplitOnce(bucketAndTable, "+")
	if err != nil {
		panic(err)
	}

	// cd to git root
	gitDir := os.Getenv("GIT_DIR")
	if gitDir == "" {
		panic("GIT_DIR")
	}
	err = os.Chdir(path.Dir(gitDir))
	if err != nil {
		panic(err)
	}

	ensure := os.Getenv("ensure") == "y"

	// create bucket if needed
	_, err = lib.S3BucketRegion(bucket)
	if err != nil {
		if !ensure {
			fmt.Fprintln(os.Stderr, "fatal: bucket did not exist and ensure=y env var not provided:", bucket)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "creating private s3 bucket:", bucket)
		input, err := lib.S3EnsureInput("", bucket, []string{"acl=private"})
		if err != nil {
			panic(err)
		}
		err = lib.S3Ensure(context.Background(), input, false)
		if err != nil {
			panic(err)
		}
		fmt.Fprintln(os.Stderr, "created private s3 bucket:", bucket)
	}

	// create table if needed
	_, err = lib.DynamoDBClient().DescribeTableWithContext(context.Background(), &dynamodb.DescribeTableInput{
		TableName: aws.String(table),
	})
	if err != nil {
		if !ensure {
			fmt.Fprintln(os.Stderr, "fatal: dynamodb table did not exist and ensure=y env var not provided:", table)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "creating private dynamodb table:", table)
		input, err := lib.DynamoDBEnsureInput("", table, []string{"id:s:hash"}, nil)
		if err != nil {
			panic(err)
		}
		err = lib.DynamoDBEnsure(context.Background(), input, false)
		if err != nil {
			panic(err)
		}
		err = lib.DynamoDBWaitForReady(context.Background(), table)
		if err != nil {
			panic(err)
		}
		fmt.Fprintln(os.Stderr, "created private dynamodb table:", table)
	}

	// read stdin and invoke git remote helpers
	r := bufio.NewReader(os.Stdin)
	for {

		// read line
		command, err := r.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				os.Exit(1)
			}
			panic(err)
		}
		command = strings.TrimRight(command, "\n")

		// invoke git remote helper
		if command == "capabilities" {
			capabilities()
		} else if command == "list for-push" || command == "list" {
			list(table, bucket, prefix)
		} else if strings.HasPrefix(command, "push ") {
			push(table, bucket, prefix, command)
		} else if strings.HasPrefix(command, "fetch ") {
			fetch(table, bucket, prefix, command)
		} else if command == "" {
			os.Exit(0)
		} else {
			panic(fmt.Sprintf("%#v", command))
		}

	}

}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: git-remote-aws --keygen PUBLIC_KEY_FILE SECRET_KEY_FILE")
	fmt.Println()
	fmt.Fprintln(os.Stderr, "example: git-remote-aws --keygen ~/.git-remote-aws/publickey ~/.git-remote-aws/secretkey")
	fmt.Println()
	fmt.Fprintln(os.Stderr, "example: git-remote-aws --encrypt < cat plaintext > ciphertext")
	fmt.Println()
	fmt.Fprintln(os.Stderr, "example: git-remote-aws --decrypt < cat ciphertext > plaintext")
	os.Exit(1)
}

func publicKeys() [][]byte {
	data, err := os.ReadFile(".publickeys")
	if err != nil {
		panic(err)
	}
	var publicKeys [][]byte
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(line) > 0 {
			line, err := hex.DecodeString(string(line))
			if err != nil {
				panic(err)
			}
			pk, _, err := libsodium.BoxKeypair()
			if err != nil {
				panic(err)
			}
			if len(line) != len(pk) {
				panic(fmt.Sprintf("malformed .publickeys file: %d != %d", len(line), len(pk)))
			}
			publicKeys = append(publicKeys, line)
		}
	}
	return publicKeys
}

func encrypt() {
	err := libsodium.StreamEncryptRecipients(publicKey(), os.Stdin, os.Stdout)
	if err != nil {
		panic(err)
	}
}

func decrypt() {
	err := libsodium.StreamDecryptRecipients(secretKey(), os.Stdin, os.Stdout)
	if err != nil {
		panic(err)
	}
}

func main() {
	libsodium.Init()
	switch os.Args[1] {
	case "-h", "--help":
		usage()
	case "-e", "--encrypt":
		encrypt()
	case "-d", "--decrypt":
		decrypt()
	case "-k", "--keygen":
		if len(os.Args) < 4 {
			usage()
		}
		publicKeyFile := os.Args[2]
		if exists(publicKeyFile) {
			fmt.Fprintln(os.Stderr, "fatal: public key file exists, refusing to overwrite:", publicKeyFile)
			os.Exit(1)
		}
		secretKeyFile := os.Args[3]
		if exists(secretKeyFile) {
			fmt.Fprintln(os.Stderr, "fatal: secret key file exists, refusing to overwrite:", secretKeyFile)
			os.Exit(1)
		}
		pk, sk, err := libsodium.BoxKeypair()
		if err != nil {
			panic(err)
		}
		err = os.WriteFile(publicKeyFile, []byte(hex.EncodeToString(pk)), 0600)
		if err != nil {
			panic(err)
		}
		err = os.WriteFile(secretKeyFile, []byte(hex.EncodeToString(sk)), 0600)
		if err != nil {
			panic(err)
		}
	default:
		gitHelper()
	}
}
