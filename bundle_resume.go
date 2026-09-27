package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Listing is discovery, not validation. Bind any selected object to this listed
// identity with a fresh HEAD, its push-tip metadata, and ciphertext recipients.
func (clients *awsClients) listPushBundles(ctx context.Context, bucket string, repo repository, tip string, recipients []string) (map[string]bundleRef, error) {
	prefix := repo.bundleKey(tip, "")
	refs := make(map[string]bundleRef)
	pages := s3.NewListObjectsV2Paginator(clients.s3, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix)})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("discover completed push bundles: %w", err)
		}
		for _, object := range page.Contents {
			key := aws.ToString(object.Key)
			name, ok := strings.CutPrefix(key, prefix)
			_, end, err := validBundleRange(name)
			if !ok || err != nil || len(end) != len(tip) || aws.ToInt64(object.Size) <= 0 || aws.ToString(object.ETag) == "" {
				return nil, fmt.Errorf("invalid completed push object: %s", key)
			}
			ref := bundleRef{Range: name, Key: key, Size: aws.ToInt64(object.Size), ETag: aws.ToString(object.ETag), Recipients: recipients}
			if previous, exists := refs[name]; exists {
				if _, err := checkedBundleDescriptor(previous, ref); err != nil {
					return nil, err
				}
			}
			refs[name] = ref
		}
	}
	return refs, nil
}

// A push's boundaries lie on its first-parent ancestry path (the published base
// may be a side ancestor). Use Git's native walk once, not size-estimation walks
// or one ancestry process per object. Overlapping attempts form forward edges:
// choose the furthest reachable boundary, then the fewest bundles, with stable
// key order breaking ties. Objects outside that contiguous prefix stay untouched.
func completedPushPrefix(ctx context.Context, base, tip string, refs map[string]bundleRef) ([]bundleRef, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	commits, err := pushBundleCommits(ctx, base, tip)
	if err != nil {
		return nil, err
	}
	start := base
	if start == "" {
		start = strings.Repeat("0", len(tip))
	}
	position := map[string]int{start: 0}
	for i, commit := range commits {
		position[commit] = i + 1
	}
	type edge struct {
		ref      bundleRef
		from, to int
	}
	var edges []edge
	for _, ref := range refs {
		first, last, err := validBundleRange(ref.Range)
		if err != nil {
			return nil, err
		}
		from, hasFrom := position[first]
		to, hasTo := position[last]
		if hasFrom && hasTo && from < to {
			edges = append(edges, edge{ref, from, to})
		}
	}
	slices.SortFunc(edges, func(a, b edge) int {
		if a.to != b.to {
			return a.to - b.to
		}
		return strings.Compare(a.ref.Key, b.ref.Key)
	})
	// Positive counts include the starting boundary. A zero means unreachable.
	count, previous := make([]int, len(commits)+1), make([]int, len(commits)+1)
	count[0] = 1
	end := 0
	for i, edge := range edges {
		if count[edge.from] != 0 && (count[edge.to] == 0 || count[edge.from]+1 < count[edge.to]) {
			count[edge.to], previous[edge.to] = count[edge.from]+1, i
			end = max(end, edge.to)
		}
	}
	var chain []bundleRef
	for end != 0 {
		edge := edges[previous[end]]
		chain = append(chain, edge.ref)
		end = edge.from
	}
	slices.Reverse(chain)
	return chain, nil
}

const pushValidationWorkers = 8

// Keep at most eight metadata requests in flight. Workers write distinct result
// slots; cache files are atomically replaced. Cancel and join every worker before
// returning, and preserve chain order regardless of request completion order.
func (validation *bundleValidation) inspectPushPrefix(ctx context.Context, tip string, refs []bundleRef) ([]bundleRef, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	checked := make([]bundleRef, len(refs))
	jobs := make(chan int)
	var workers sync.WaitGroup
	var progress sync.Mutex
	completed := 0
	for range min(pushValidationWorkers, len(refs)) {
		workers.Go(func() {
			for i := range jobs {
				if ctx.Err() != nil {
					return
				}
				ref, err := validation.inspectPush(ctx, refs[i], tip)
				if err != nil {
					cancel(err)
					return
				}
				checked[i] = ref
				progress.Lock()
				completed++
				fmt.Fprintf(os.Stderr, "reuse completed bundle from the same push (%d/%d): %s\n", completed, len(refs), ref.Key)
				progress.Unlock()
			}
		})
	}
feed:
	for i := range refs {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	workers.Wait()
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	return checked, nil
}
