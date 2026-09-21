package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

// Recovery only reads S3. The selected manifest may come from a completed but
// unacknowledged push; choosing it is an explicit administrative decision.
func recoverRepository(args []string) error {
	flags := flag.NewFlagSet("recover", flag.ContinueOnError)
	remote := flags.String("remote", "", "aws://BUCKET+TABLE/NAMESPACE[/REPO]")
	key := flags.String("manifest", "", "exact S3 manifest key to recover (omit to list candidates)")
	directory := flags.String("directory", "", "new, nonexistent checkout directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !strings.HasPrefix(*remote, "aws://") || (*key == "") != (*directory == "") {
		return fmt.Errorf("usage: --recover --remote aws://BUCKET+TABLE/NAMESPACE[/REPO] [--manifest KEY --directory NEW_DIRECTORY]")
	}
	authority, prefix, ok := strings.Cut(strings.TrimPrefix(*remote, "aws://"), "/")
	bucket, table, separated := strings.Cut(authority, "+")
	if !ok || !separated || bucket == "" || table == "" {
		return fmt.Errorf("invalid recovery remote URL")
	}
	repo, err := parseRepository(prefix)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// This standalone command must never initialize or reset a repository
	// selected by the caller's GIT_DIR, index, worktree, or object overrides.
	localVariables, err := gitCommand(ctx, "rev-parse", "--local-env-vars").Output()
	if err != nil {
		return fmt.Errorf("inspect Git recovery environment: %w", err)
	}
	for _, name := range strings.Fields(string(localVariables)) {
		if err := os.Unsetenv(name); err != nil {
			return err
		}
	}
	clients, err := newAWSClients(ctx)
	if err != nil {
		return err
	}
	if *key == "" {
		keys, err := clients.namespaceManifests(ctx, bucket, repo)
		if err != nil {
			return err
		}
		for _, candidate := range keys {
			source, legacy, _ := manifestRepository(repo, candidate)
			if source != repo || legacy {
				continue
			}
			m, err := clients.getManifest(ctx, bucket, candidate, repo, "")
			if err != nil {
				return err
			}
			fmt.Printf("%s\t%s\t%s\n", candidate, m.Branch, m.Tip)
		}
		return nil
	}
	if !strings.HasPrefix(*key, repo.manifests()) {
		return fmt.Errorf("recovery requires a self-contained manifest for %s", repo.canonical())
	}
	m, err := clients.getManifest(ctx, bucket, *key, repo, "")
	if err != nil {
		return err
	}
	absolute, err := filepath.Abs(*directory)
	if err != nil {
		return err
	}
	if err := os.Mkdir(absolute, 0700); err != nil {
		return err
	}
	if err := os.Chdir(absolute); err != nil {
		return err
	}
	if output, err := gitCommand(ctx, "init", "-q", "--object-format="+m.ObjectFormat, "--initial-branch="+m.Branch).CombinedOutput(); err != nil {
		return fmt.Errorf("initialize recovery checkout: %w: %s", err, output)
	}
	clients.fetchManifest(ctx, bucket, *remote, m)
	if output, err := gitCommand(ctx, "reset", "--hard", m.Tip).CombinedOutput(); err != nil {
		return fmt.Errorf("check out recovered history: %w: %s", err, output)
	}
	fmt.Printf("recovered %s at %s into %s (DynamoDB unchanged)\n", repo.canonical(), m.Tip, absolute)
	return nil
}
