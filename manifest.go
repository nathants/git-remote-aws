package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/gofrs/uuid/v5"
	"github.com/nathants/go-dynamolock"
	"github.com/nathants/go-libsodium"
	"golang.org/x/crypto/blake2b"
)

const manifestVersion = 2
const manifestCodec = "libsodium-secretstream-box-v1"
const maxManifestBytes = 16 << 20

var repositoryComponent = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
var objectID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
var recipientID = regexp.MustCompile(`^[0-9a-f]{128}$`)

type repository struct {
	Namespace string
	Name      string
}

func parseRepository(prefix string) (repository, error) {
	parts := strings.Split(strings.TrimSuffix(prefix, "/"), "/")
	if len(parts) == 1 {
		parts = append(parts, "1")
	}
	if len(parts) != 2 || !repositoryComponent.MatchString(parts[0]) || !repositoryComponent.MatchString(parts[1]) {
		return repository{}, fmt.Errorf("invalid repository %q: expected NAMESPACE[/REPO], with nonempty alphanumeric, dot, underscore or hyphen components", prefix)
	}
	return repository{Namespace: parts[0], Name: parts[1]}, nil
}

func (r repository) canonical() string { return r.Namespace + "/" + r.Name }

// Keep the original lease identity for the default repository. Both URL aliases
// must acquire this same lease; promotion never creates a second coordinator.
func (r repository) id() string {
	if r.Name == "1" {
		return r.Namespace
	}
	return r.canonical()
}

func (r repository) root() string      { return r.Namespace + "/.remote-aws-v2/" }
func (r repository) manifests() string { return r.root() + "repos/" + r.Name + "/manifests/" }
func (r repository) bundleKey(tip, name string) string {
	return r.root() + "bundles/" + tip + "/" + name
}

// A literal legacy NAMESPACE/1 may already be a different repository. Never
// silently alias it to NAMESPACE or acquire the wrong history's lease.
func (clients *awsClients) checkRepositoryAlias(ctx context.Context, table, bucket string, repo repository) error {
	if repo.Name != "1" {
		return nil
	}
	meta, err := dynamolock.Read[RepoMeta](ctx, clients.dynamodb, table, bucket+"/"+repo.canonical())
	if err != nil {
		return err
	}
	if meta != nil && (meta.Branch != "" || meta.BundlesS3Key != "") {
		return fmt.Errorf("legacy repository %s conflicts with the implicit /1 alias; reconcile its metadata explicitly before upgrading", repo.canonical())
	}
	return nil
}

type bundleRef struct {
	Range      string   `json:"range"`
	Key        string   `json:"key"`
	Size       int64    `json:"size"`
	ETag       string   `json:"etag"`
	Recipients []string `json:"recipients"`
}

type manifest struct {
	Version      int         `json:"version"`
	Repository   string      `json:"repository"`
	Branch       string      `json:"branch"`
	Tip          string      `json:"tip"`
	ObjectFormat string      `json:"object_format"`
	Codec        string      `json:"codec"`
	Bundles      []bundleRef `json:"bundles"`
	legacy       bool
}

func newManifest(repo repository, branch, tip string, bundles []bundleRef) *manifest {
	format := "sha1"
	if len(tip) == 64 {
		format = "sha256"
	}
	return &manifest{Version: manifestVersion, Repository: repo.canonical(), Branch: branch, Tip: tip, ObjectFormat: format, Codec: manifestCodec, Bundles: bundles}
}

func validBundleRange(name string) (string, string, error) {
	parts := bundleNamePattern.FindStringSubmatch(name)
	if len(parts) != 3 || len(parts[1]) != len(parts[2]) || parts[1] == parts[2] || parts[2] == strings.Repeat("0", len(parts[2])) {
		return "", "", fmt.Errorf("invalid bundle range: %q", name)
	}
	return parts[1], parts[2], nil
}

func (m *manifest) validate(repo repository) error {
	if m.Version != manifestVersion || m.Repository != repo.canonical() || m.Codec != manifestCodec || !objectID.MatchString(m.Tip) || len(m.Bundles) == 0 {
		return fmt.Errorf("invalid or unsupported manifest for %s", repo.canonical())
	}
	if m.Branch == "" || (m.ObjectFormat != "sha1" || len(m.Tip) != 40) && (m.ObjectFormat != "sha256" || len(m.Tip) != 64) {
		return fmt.Errorf("invalid manifest branch or object format")
	}
	previous := strings.Repeat("0", len(m.Tip))
	seen := make(map[string]bool)
	for _, bundle := range m.Bundles {
		base, tip, err := validBundleRange(bundle.Range)
		if err != nil || base != previous || seen[tip] || len(tip) != len(m.Tip) {
			return fmt.Errorf("discontinuous or invalid bundle chain at %q", bundle.Range)
		}
		seen[tip], previous = true, tip
		if !validBundleKey(repo, bundle) {
			return fmt.Errorf("bundle key escapes its namespace or range: %q", bundle.Key)
		}
		if !m.legacy {
			if bundle.Size <= 0 || bundle.ETag == "" || len(bundle.Recipients) == 0 || !slices.IsSorted(bundle.Recipients) {
				return fmt.Errorf("missing bundle identity or recipient policy: %q", bundle.Key)
			}
			for i, recipient := range bundle.Recipients {
				if !recipientID.MatchString(recipient) || i > 0 && recipient == bundle.Recipients[i-1] {
					return fmt.Errorf("invalid bundle recipient policy: %q", bundle.Key)
				}
			}
		}
	}
	if previous != m.Tip {
		return fmt.Errorf("manifest tip is not covered by its bundle chain")
	}
	return nil
}

func validBundleKey(repo repository, bundle bundleRef) bool {
	if bundle.Key == repo.Namespace+"/"+bundle.Range {
		return true
	}
	if tail, ok := strings.CutPrefix(bundle.Key, repo.Namespace+"/"); ok {
		parts := strings.Split(tail, "/")
		if len(parts) == 2 && repositoryComponent.MatchString(parts[0]) && parts[1] == bundle.Range {
			return true
		}
	}
	tail, ok := strings.CutPrefix(bundle.Key, repo.root()+"bundles/")
	parts := strings.Split(tail, "/")
	return ok && len(parts) == 2 && objectID.MatchString(parts[0]) && len(parts[0]) == len(hashEnd(bundle.Range)) && parts[1] == bundle.Range
}

func (clients *awsClients) getManifest(ctx context.Context, bucket, key string, repo repository, branch string) (*manifest, error) {
	if key == "" {
		return nil, nil
	}
	modern := strings.HasPrefix(key, repo.manifests()) && !strings.Contains(strings.TrimPrefix(key, repo.manifests()), "/")
	legacy := strings.HasPrefix(key, repo.id()+"/bundles_") && !strings.Contains(strings.TrimPrefix(key, repo.id()+"/bundles_"), "/")
	if !modern && !legacy {
		return nil, fmt.Errorf("manifest pointer is outside repository %s: %q", repo.canonical(), key)
	}
	fmt.Fprintln(os.Stderr, "get s3://"+bucket+"/"+key)
	out, err := clients.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", key, err)
	}
	defer func() { _ = out.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(out.Body, maxManifestBytes+1))
	if err != nil || len(data) > maxManifestBytes {
		return nil, fmt.Errorf("read manifest %s: %w", key, errors.Join(err, errors.New("incomplete or oversized manifest")))
	}
	var m *manifest
	if legacy {
		var refs []bundleRef
		for _, name := range bundleNamesFromMetadata(key, data) {
			if _, _, err := validBundleRange(name); err != nil {
				return nil, err
			}
			refs = append(refs, bundleRef{Range: name, Key: repo.id() + "/" + name})
		}
		m = newManifest(repo, branch, hashEnd(last(refs).Range), refs)
		m.legacy = true
	} else {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&m); err != nil || m == nil {
			return nil, fmt.Errorf("invalid manifest JSON: %w", errors.Join(err, errors.New("expected a manifest object")))
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return nil, fmt.Errorf("trailing manifest data")
		}
	}
	if err := m.validate(repo); err != nil {
		return nil, err
	}
	output, err := gitCommand(ctx, "check-ref-format", "--branch", m.Branch).Output()
	if err != nil || string(output) != m.Branch+"\n" {
		return nil, fmt.Errorf("invalid manifest branch %q", m.Branch)
	}
	if branch != "" && m.Branch != branch {
		return nil, fmt.Errorf("manifest branch disagrees with repository metadata")
	}
	return m, nil
}

func (clients *awsClients) putManifest(ctx context.Context, bucket string, repo repository, m *manifest) (string, error) {
	m.legacy = false
	if err := m.validate(repo); err != nil {
		return "", err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	if len(data) > maxManifestBytes {
		return "", fmt.Errorf("manifest exceeds %d bytes", maxManifestBytes)
	}
	id, err := uuid.NewV4()
	if err != nil {
		return "", err
	}
	key := repo.manifests() + m.Tip + "-" + id.String() + ".json"
	_, err = clients.s3.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(data), IfNoneMatch: aws.String("*")})
	return key, err
}

// Header fingerprints describe actual ciphertext recipients, including legacy
// pushes whose uncommitted policy cannot be reconstructed from a boundary commit.
// This reads only the box envelope, not the encrypted Git payload. Authentication
// and complete Git connectivity are checked by the common fetch path.
func (clients *awsClients) inspectBundle(ctx context.Context, bucket string, ref bundleRef) (bundleRef, error) {
	out, err := clients.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(ref.Key)})
	if err != nil {
		return ref, fmt.Errorf("inspect bundle %s: %w", ref.Key, err)
	}
	size, etag := aws.ToInt64(out.ContentLength), aws.ToString(out.ETag)
	if size <= 0 || etag == "" || ref.Size != 0 && ref.Size != size || ref.ETag != "" && ref.ETag != etag {
		return ref, fmt.Errorf("bundle identity changed or is missing: %s", ref.Key)
	}
	ref.Size, ref.ETag = size, etag
	recordedRecipients := ref.Recipients
	ref.Recipients = nil
	countBytes, err := clients.bundleHeaderRange(ctx, bucket, ref, 0, 4)
	if err != nil {
		return ref, err
	}
	count := binary.LittleEndian.Uint32(countBytes)
	// These are the retained libsodium box-envelope wire constants: 64-byte
	// BLAKE2b fingerprint, 48-byte sealed-box overhead, 32-byte stream key.
	const recordSize = 64 + 48 + 32
	if count == 0 || count > 1<<16 || int64(4+count*(4+recordSize)) >= size {
		return ref, fmt.Errorf("invalid encrypted recipient count for %s", ref.Key)
	}
	header, err := clients.bundleHeaderRange(ctx, bucket, ref, 4, int64(count)*(4+recordSize))
	if err != nil {
		return ref, err
	}
	for offset := 0; offset < len(header); offset += 4 + recordSize {
		if binary.LittleEndian.Uint32(header[offset:]) != recordSize {
			return ref, fmt.Errorf("invalid encrypted recipient record for %s", ref.Key)
		}
		ref.Recipients = append(ref.Recipients, hex.EncodeToString(header[offset+4:offset+4+64]))
	}
	slices.Sort(ref.Recipients)
	if len(slices.Compact(slices.Clone(ref.Recipients))) != len(ref.Recipients) {
		return ref, fmt.Errorf("duplicate encrypted recipients for %s", ref.Key)
	}
	if len(recordedRecipients) != 0 && !slices.Equal(recordedRecipients, ref.Recipients) {
		return ref, fmt.Errorf("bundle recipient policy disagrees with ciphertext: %s", ref.Key)
	}
	return ref, nil
}

func (clients *awsClients) bundleHeaderRange(ctx context.Context, bucket string, ref bundleRef, offset, size int64) ([]byte, error) {
	out, err := clients.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(ref.Key), IfMatch: aws.String(ref.ETag), Range: aws.String(fmt.Sprintf("bytes=%d-%d", offset, offset+size-1))})
	if err != nil {
		return nil, fmt.Errorf("read bundle envelope %s: %w", ref.Key, err)
	}
	defer func() { _ = out.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(out.Body, size+1))
	if err != nil || int64(len(data)) != size {
		return nil, fmt.Errorf("incomplete bundle envelope %s: %w", ref.Key, errors.Join(err, io.ErrUnexpectedEOF))
	}
	return data, nil
}

func compatibleRecipients(ref bundleRef, chains libsodium.KeyChains) bool {
	if len(ref.Recipients) != len(chains) {
		return false
	}
	matched := make(map[string]bool)
	for _, chain := range chains {
		found := false
		for _, key := range chain {
			fingerprint := blake2b.Sum512(key)
			id := hex.EncodeToString(fingerprint[:])
			if slices.Contains(ref.Recipients, id) && !matched[id] {
				matched[id], found = true, true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (clients *awsClients) validateBundleChain(ctx context.Context, bucket string, refs []bundleRef) ([]bundleRef, error) {
	refs = slices.Clone(refs)
	for i, ref := range refs {
		base, tip, err := validBundleRange(ref.Range)
		if err != nil {
			return nil, err
		}
		if i == 0 && base != strings.Repeat("0", len(tip)) || i > 0 && base != hashEnd(refs[i-1].Range) {
			return nil, fmt.Errorf("incomplete adopted bundle chain")
		}
		if i > 0 {
			contains, known := gitBranchContains(ctx, tip, base)
			if !contains || !known {
				return nil, fmt.Errorf("bundle range is not an ancestry chain: %s", ref.Range)
			}
		}
		refs[i], err = clients.inspectBundle(ctx, bucket, ref)
		if err != nil {
			return nil, err
		}
	}
	return refs, nil
}

// List immutable snapshots, including complete snapshots from uncertain pushes.
// A disappearing superseded manifest is harmless; missing bundles are not.
func (clients *awsClients) namespaceManifests(ctx context.Context, bucket string, repo repository) ([]string, error) {
	var keys []string
	pages := s3.NewListObjectsV2Paginator(clients.s3, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(repo.Namespace + "/")})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, object := range page.Contents {
			key := aws.ToString(object.Key)
			if _, _, ok := manifestRepository(repo, key); ok {
				keys = append(keys, key)
			}
		}
	}
	slices.Sort(keys)
	return keys, nil
}

func manifestRepository(namespace repository, key string) (repository, bool, bool) {
	if tail, ok := strings.CutPrefix(key, namespace.root()+"repos/"); ok {
		parts := strings.Split(tail, "/")
		if len(parts) == 3 && repositoryComponent.MatchString(parts[0]) && parts[1] == "manifests" && strings.HasSuffix(parts[2], ".json") {
			return repository{Namespace: namespace.Namespace, Name: parts[0]}, false, true
		}
	}
	if tail, ok := strings.CutPrefix(key, namespace.Namespace+"/"); ok {
		parts := strings.Split(tail, "/")
		if len(parts) == 1 && strings.HasPrefix(parts[0], "bundles_") {
			return repository{Namespace: namespace.Namespace, Name: "1"}, true, true
		}
		if len(parts) == 2 && parts[0] != "1" && repositoryComponent.MatchString(parts[0]) && strings.HasPrefix(parts[1], "bundles_") {
			return repository{Namespace: namespace.Namespace, Name: parts[0]}, true, true
		}
	}
	return repository{}, false, false
}

func (clients *awsClients) adoptBundles(ctx context.Context, bucket string, repo repository, branchName, tip string, chains libsodium.KeyChains) ([]bundleRef, error) {
	keys, err := clients.namespaceManifests(ctx, bucket, repo)
	if err != nil {
		return nil, fmt.Errorf("discover namespace bundles: %w", err)
	}
	var best []bundleRef
	bestDistance := int64(-1)
	for _, key := range keys {
		source, legacy, _ := manifestRepository(repo, key)
		branch := ""
		if legacy {
			// Branch names do not affect Git bundle contents. Legacy snapshots
			// lack that field; this placeholder never becomes destination metadata.
			branch = defaultBranch
		}
		m, err := clients.getManifest(ctx, bucket, key, source, branch)
		var missing *s3types.NoSuchKey
		if errors.As(err, &missing) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if source == repo {
			contains, _ := gitBranchContains(ctx, tip, m.Tip)
			if !contains || !legacy && m.Branch != branchName {
				return nil, fmt.Errorf("S3 history exists for %s without a DynamoDB pointer; refusing to rewrite it, recover/reconcile metadata first", repo.canonical())
			}
		}
		if len(m.Tip) != len(tip) {
			continue
		}
		// Containment is monotone along a validated chain. Binary search avoids
		// one Git process per old bundle merely to locate a reuse boundary.
		low, high := 0, len(m.Bundles)
		for low < high {
			mid := low + (high-low)/2
			contains, _ := gitBranchContains(ctx, tip, hashEnd(m.Bundles[mid].Range))
			if contains {
				low = mid + 1
			} else {
				high = mid
			}
		}
		if low == 0 {
			continue
		}
		candidate, err := clients.validateBundleChain(ctx, bucket, m.Bundles[:low])
		if err != nil {
			return nil, err
		}
		for i, ref := range candidate {
			if !compatibleRecipients(ref, chains) {
				candidate = candidate[:i]
				break
			}
		}
		if len(candidate) == 0 {
			continue
		}
		end := hashEnd(last(candidate).Range)
		count, err := gitCommand(ctx, "rev-list", "--count", end+".."+tip).Output()
		if err != nil {
			return nil, err
		}
		distance, err := strconv.ParseInt(strings.TrimSpace(string(count)), 10, 64)
		if err != nil {
			return nil, err
		}
		if bestDistance < 0 || distance < bestDistance {
			best, bestDistance = candidate, distance
		}
	}
	if len(best) != 0 {
		var size int64
		for _, ref := range best {
			size += ref.Size
		}
		fmt.Fprintf(os.Stderr, "reuse %d existing bundles (%d encrypted bytes) through %s\n", len(best), size, hashEnd(last(best).Range))
	}
	return best, nil
}
