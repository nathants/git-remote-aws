package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/nathants/go-dynamolock"
	"github.com/nathants/go-libsodium"
	"github.com/nathants/go-libsodium/keysource"
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

var bundleNamePattern = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})\.\.([0-9a-f]{40}|[0-9a-f]{64})$`)

// "aaa..bbb" => "bbb"
func hashEnd(x string) string {
	return bundleNameParts(x)[1]
}

func bundleNameParts(bundle string) []string {
	parts := bundleNamePattern.FindStringSubmatch(bundle)
	if parts == nil {
		panic("invalid bundle name: " + bundle)
	}
	if len(parts[1]) != len(parts[2]) {
		panic("invalid bundle name with mixed hash lengths: " + bundle)
	}
	return parts[1:]
}

func bundleNamesFromMetadata(location string, data []byte) []string {
	var bundles []string
	for bundle := range strings.SplitSeq(string(data), "\n") {
		if bundle != "" {
			bundleNameParts(bundle)
			bundles = append(bundles, bundle)
		}
	}
	if len(bundles) == 0 {
		panic("bundles metadata is empty: " + location)
	}
	return bundles
}

func getBundles(ctx context.Context, bucket, s3Key string) []string {
	if s3Key == "" {
		return nil
	}
	location := "s3://" + bucket + "/" + s3Key
	fmt.Fprintln(os.Stderr, "get "+location)
	out, err := lib.S3Client().GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(s3Key),
	})
	if err != nil {
		panic(fmt.Errorf("failed to get bundles metadata %s: %w", location, err))
	}
	defer func() { _ = out.Body.Close() }()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		panic(fmt.Errorf("failed to read bundles metadata %s: %w", location, err))
	}
	return bundleNamesFromMetadata(location, data)
}

// git helper capabilities
func capabilities() {
	fmt.Println("push")
	fmt.Println("fetch")
	fmt.Println("")
}

type RepoMeta struct {
	BundlesS3Key string `json:"bundles" dynamodbav:"bundles"`
	Branch       string `json:"branch" dynamodbav:"branch"`
}

func refBranch(ref string) string {
	branch, ok := strings.CutPrefix(ref, "refs/heads/")
	if !ok {
		panic("ref is not a branch: " + ref)
	}
	if branch == "" || strings.Contains(branch, "/") {
		panic("branch names cannot be empty or contain slashes: " + branch)
	}
	return branch
}

func gitBranchContains(branch, hash string) (bool, bool) {
	cmd := exec.Command("git", "merge-base", "--is-ancestor", hash, branch)
	err := cmd.Run()
	if err == nil {
		return true, true
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		switch exitErr.ExitCode() {
		case 1:
			return false, true
		case 128:
			return false, false
		}
	}
	panic("failed to run: git merge-base --is-ancestor " + hash + " " + branch)
}

// git helper push
func push(table, bucket, prefix, command string) {

	// parse args and assert single branch
	refs := strings.SplitN(command[len("push "):], ":", 2)
	localRef := refs[0]
	remoteRef := refs[1]
	if strings.HasPrefix(localRef, "+") || strings.HasPrefix(remoteRef, "+") {
		panic("force push is not allowed")
	}
	localBranch := refBranch(localRef)
	remoteBranch := refBranch(remoteRef)
	if localBranch != remoteBranch {
		panic(fmt.Sprintf("local branch is different from remote branch, %s != %s", localBranch, remoteBranch))
	}
	branch := localBranch

	// Cleanup releases ownership only; failed pushes must never publish metadata.
	fmt.Fprintln(os.Stderr, "get dynamodb://"+table+"/"+bucket+"/"+prefix)
	lockCtx, cancelLock := context.WithCancel(context.Background())
	defer cancelLock()
	lease, repoMeta, err := dynamolock.Lock[RepoMeta](lockCtx, lib.DynamoDBClient(), &dynamolock.LockInput{
		Table:             table,
		ID:                bucket + "/" + prefix,
		HeartbeatMaxAge:   10 * time.Second,
		HeartbeatInterval: 1 * time.Second,
	})
	if err != nil {
		panic(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := lease.Release(cleanup); err != nil {
			panic(fmt.Errorf("release repository lease: %w", err))
		}
	}()
	ctx := lease.Context()
	if repoMeta == nil {
		repoMeta = &RepoMeta{}
	}
	bundles := getBundles(ctx, bucket, repoMeta.BundlesS3Key)

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
	cmd := exec.CommandContext(ctx, "git", "log", "--format=%H", "-1", localRef)
	cmd.Stdout = &stdout
	err = cmd.Run()
	if err != nil {
		panic(err)
	}
	hash := strings.Trim(stdout.String(), "\n")

	// Check ancestry against the selected commit, not a branch that may move.
	if len(bundles) > 0 {
		hashRemote := hashEnd(last(bundles))
		contains, _ := gitBranchContains(hash, hashRemote)
		if !contains {
			panic("remote has new commits, pull before pushing")
		}
	}

	base := ""
	if len(bundles) > 0 {
		base = hashEnd(last(bundles))
	}
	recipients, err := pushRecipients(base, hash)
	if err != nil {
		panic(err)
	}
	// A no-op push must not conceal uncommitted recipient changes either.
	if base == hash {
		fmt.Println()
		return
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
	bundleTarget := localRef
	bundleName := zeroHash + ".." + hash
	if len(hash) == 64 {
		bundleName = zeroHash256 + ".." + hash
	}
	if len(bundles) > 0 {
		bundleTarget = hashEnd(last(bundles)) + ".." + localRef
		bundleName = hashEnd(last(bundles)) + ".." + hash
	} else {
		cmd := exec.CommandContext(ctx, "git", "log", "--format=\"%H%d\"", hash)
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		err := cmd.Run()
		if err != nil {
			panic("failed to run: git log --format=\"%H%d\" " + hash)
		}
		if strings.Contains(stdout.String(), "grafted") {
			panic("grafted repos, those created with `git clone --depth=n`, cannot be used. fetch history then try again: git fetch --unshallow")
		}
	}

	// create bundle
	bundleFile := tempdir + "/" + bundleName
	fmt.Fprintln(os.Stderr, "git bundle:", path.Base(bundleFile))
	if err := createPushBundle(ctx, bundleFile, bundleTarget, hash); err != nil {
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
	err = libsodium.StreamEncryptRecipients(recipients, r, w)
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
	defer func() { _ = f.Close() }()
	fmt.Fprintln(os.Stderr, "put s3://"+bucket+"/"+prefix+"/"+bundleName)
	_, err = lib.S3Client().PutObject(ctx, &s3.PutObjectInput{
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
	_, err = lib.S3Client().PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(repoMeta.BundlesS3Key),
		Body:   bytes.NewReader(bundleData),
	})
	if err != nil {
		panic(err)
	}

	err = lease.Commit(ctx, repoMeta)
	if err != nil {
		panic(err)
	}
	fmt.Fprintln(os.Stderr, "put dynamodb://"+table+"/"+bucket+"/"+prefix, repoMeta)

	// delete previous bundles metadata when a new one is written
	if oldBundlesS3Key != repoMeta.BundlesS3Key && oldBundlesS3Key != "" {
		_, err = lib.S3Client().DeleteObject(context.Background(), &s3.DeleteObjectInput{
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

func secretKey(remotePath string) *libsodium.Keyring {
	ring, err := keysource.Load(context.Background(), remotePath)
	if err != nil {
		panic(err)
	}
	return ring
}

func publicKey() [][]byte {
	chains, err := libsodium.ParseKeyChains(strings.NewReader(os.Getenv("GIT_REMOTE_AWS_PUBLICKEY")))
	if err != nil {
		panic(err)
	}
	keys, err := chains.Latest()
	if err != nil {
		panic(err)
	}
	return keys
}

// git helper fetch
func fetch(table, bucket, prefix, remotePath, command string) {

	// parse args to get branch name
	parts := strings.SplitN(command[len("fetch "):], " ", 2) // fetch $shasum refs/heads/$branch
	ref := parts[1]                                          // refs/heads/master
	branch := refBranch(ref)

	fmt.Fprintln(os.Stderr, "get dynamodb://"+table+"/"+bucket+"/"+prefix)
	repoMeta, err := dynamolock.Read[RepoMeta](context.Background(), lib.DynamoDBClient(), table, bucket+"/"+prefix)
	if err != nil {
		panic(err)
	}
	if repoMeta == nil {
		repoMeta = &RepoMeta{}
	} else {
		fmt.Fprintln(os.Stderr, "got meta:", repoMeta)
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
		bundles = getBundles(context.Background(), bucket, repoMeta.BundlesS3Key)
	}

	// walk backward from newest to oldest through remote bundles.
	// stop when the bundle end commit exists in the local data. all
	// bundles which do not exist in local need to be fetched.
	var bundlesToFetch []string
	for _, bundle := range reverse(bundles) {
		hash := hashEnd(bundle)
		contains, known := gitBranchContains(branch, hash)
		if known && contains {
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
	var ring *libsodium.Keyring
	for _, bundle := range bundlesToFetch {

		// fetch object
		fmt.Fprintln(os.Stderr, "get s3://"+bucket+"/"+prefix+"/"+bundle)
		out, err := lib.S3Client().GetObject(context.Background(), &s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(prefix + "/" + bundle),
		})
		if err != nil {
			panic(err)
		}
		bundleFileEncrypted := path.Join(tempdir, bundle)
		f, err := os.Create(bundleFileEncrypted)
		if err != nil {
			_ = out.Body.Close()
			panic(err)
		}
		_, err = io.Copy(f, out.Body)
		closeBodyErr := out.Body.Close()
		closeFileErr := f.Close()
		if err != nil {
			panic(err)
		}
		if closeBodyErr != nil {
			panic(closeBodyErr)
		}
		if closeFileErr != nil {
			panic(closeFileErr)
		}

		// decrypt
		bundleFile := bundleFileEncrypted + ".decrypted"
		r, err := os.Open(bundleFileEncrypted)
		if err != nil {
			panic(err)
		}
		w, err := os.Create(bundleFile)
		if err != nil {
			_ = r.Close()
			panic(err)
		}
		if ring == nil {
			ring = secretKey(remotePath)
		}
		err = ring.Decrypt(r, w)
		closeReadErr := r.Close()
		closeWriteErr := w.Close()
		if err != nil {
			panic(err)
		}
		if closeReadErr != nil {
			panic(closeReadErr)
		}
		if closeWriteErr != nil {
			panic(closeWriteErr)
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

	fmt.Fprintln(os.Stderr, "get dynamodb://"+table+"/"+bucket+"/"+prefix)
	repoMeta, err := dynamolock.Read[RepoMeta](context.Background(), lib.DynamoDBClient(), table, bucket+"/"+prefix)
	if err != nil {
		panic(err)
	}
	if repoMeta == nil {
		repoMeta = &RepoMeta{}
	} else {
		fmt.Fprintln(os.Stderr, "got meta:", repoMeta)
	}

	// find remote branch, falling back to default branch
	branch := defaultBranch
	if repoMeta != nil && repoMeta.Branch != "" {
		branch = repoMeta.Branch
	}

	// fetch remote bundles metadata
	var remoteBundles []string
	if repoMeta != nil && repoMeta.BundlesS3Key != "" {
		remoteBundles = getBundles(context.Background(), bucket, repoMeta.BundlesS3Key)
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
	_, err = lib.S3BucketRegion(context.Background(), bucket)
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
	_, err = lib.DynamoDBClient().DescribeTable(context.Background(), &dynamodb.DescribeTableInput{
		TableName: aws.String(table),
	})
	if err != nil {
		if !ensure {
			fmt.Fprintln(os.Stderr, "fatal: dynamodb table did not exist and ensure=y env var not provided:", table)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "creating private dynamodb table:", table)
		input, ttl, err := lib.DynamoDBEnsureInput("", table, []string{"id:s:hash"}, nil)
		if err != nil {
			panic(err)
		}
		err = lib.DynamoDBEnsure(context.Background(), input, ttl, false)
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
			// Git may decide the remote is up to date without issuing push.
			// Read-only discovery must remain available with local edits.
			if command == "list for-push" {
				if err := requireCommittedRecipients("HEAD"); err != nil {
					panic(err)
				}
			}
			list(table, bucket, prefix)
		} else if strings.HasPrefix(command, "push ") {
			push(table, bucket, prefix, command)
		} else if strings.HasPrefix(command, "fetch ") {
			fetch(table, bucket, prefix, remotePath, command)
		} else if command == "" {
			os.Exit(0)
		} else {
			panic(fmt.Sprintf("%#v", command))
		}

	}

}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: git-remote-aws --keygen")
	fmt.Println()
	fmt.Fprintln(os.Stderr, "example: eval $(git-remote-aws --keygen)")
	fmt.Println()
	fmt.Fprintln(os.Stderr, "example: echo hello | git-remote-aws --encrypt > ciphertext")
	fmt.Println()
	fmt.Fprintln(os.Stderr, "example: cat ciphertext | git-remote-aws --decrypt")
	os.Exit(1)
}

// Read the exact published recipient policy, never an uncommitted worktree edit.
func recipientChainsAt(commit string) (libsodium.KeyChains, error) {
	tree, err := exec.Command("git", "ls-tree", "--full-tree", commit, "--", ".publickeys").Output()
	if err != nil {
		return nil, fmt.Errorf("inspect committed .publickeys: %w", err)
	}
	if len(tree) == 0 {
		// Old helper versions could encrypt from an untracked file. Absence is
		// not a prior policy to validate; the new tip must still supply one.
		return nil, nil
	}
	fields := strings.Fields(string(tree))
	if len(fields) != 4 || fields[1] != "blob" || fields[3] != ".publickeys" || (fields[0] != "100644" && fields[0] != "100755") {
		return nil, fmt.Errorf("committed .publickeys must be a regular file")
	}
	cmd := exec.Command("git", "cat-file", "blob", fields[2])
	reader, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	chains, parseErr := libsodium.ParseKeyChains(reader)
	if parseErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if parseErr != nil {
		return nil, parseErr
	}
	if waitErr != nil {
		return nil, fmt.Errorf("read committed .publickeys: %w", waitErr)
	}
	if len(chains) == 0 {
		return nil, fmt.Errorf("committed .publickeys is empty")
	}
	return chains, nil
}

func pushRecipients(base, tip string) ([][]byte, error) {
	if err := requireCommittedRecipients(tip); err != nil {
		return nil, err
	}
	next, err := recipientChainsAt(tip)
	if err != nil {
		return nil, err
	}
	if base != "" {
		old, err := recipientChainsAt(base)
		if err != nil {
			return nil, err
		}
		if err := libsodium.ValidateKeyChainTransition(old, next); err != nil {
			return nil, err
		}
	}
	return next.Latest()
}

func encrypt() {
	err := libsodium.StreamEncryptRecipients(publicKey(), os.Stdin, os.Stdout)
	if err != nil {
		panic(err)
	}
}

func decrypt() {
	err := secretKey("").Decrypt(os.Stdin, os.Stdout)
	if err != nil {
		panic(err)
	}
}

func main() {
	// Keep errors concise and never emit stack frames containing key material.
	defer func() {
		if value := recover(); value != nil {
			fmt.Fprintf(os.Stderr, "git-remote-aws: %s\n", value)
			os.Exit(1)
		}
	}()
	libsodium.Init()
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "-h", "--help":
		usage()
	case "-e", "--encrypt":
		encrypt()
	case "-d", "--decrypt":
		decrypt()
	case "-k", "--keygen":
		if err := keygen(os.Args[2:], os.Stdout); err != nil {
			panic(err)
		}
	case "--migrate-dynamolock":
		if err := migrateDynamolock(os.Args[2:], os.Stdout); err != nil {
			panic(err)
		}
	default:
		gitHelper()
	}
}
