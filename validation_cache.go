package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// A validation belongs to one adoption/promotion operation, not a long-lived
// client. Even a persistent hit requires a fresh HEAD in each operation.
type bundleValidation struct {
	clients           *awsClients
	bucket, directory string
	inspected         map[string]bundleRef
}

func (clients *awsClients) newBundleValidation(ctx context.Context, bucket string) *bundleValidation {
	validation := &bundleValidation{clients: clients, bucket: bucket, inspected: make(map[string]bundleRef)}
	if directory, err := gitCommand(ctx, "rev-parse", "--path-format=absolute", "--git-path", "git-remote-aws/validation-v2").Output(); err == nil {
		validation.directory = strings.TrimSuffix(string(directory), "\n")
	}
	return validation
}

func (validation *bundleValidation) inspect(ctx context.Context, ref bundleRef) (bundleRef, error) {
	if err := ctx.Err(); err != nil {
		return ref, err
	}
	if previous, ok := validation.inspected[ref.Key]; ok {
		return checkedBundleDescriptor(ref, previous)
	}
	headed, identity, err := validation.clients.headBundle(ctx, validation.bucket, ref)
	if err != nil {
		return ref, err
	}
	cached, ok := validation.load(identity, headed)
	if !ok {
		cached, err = validation.clients.readBundleRecipients(ctx, validation.bucket, headed)
		if err != nil {
			return ref, err
		}
		validation.store(identity, cached)
	}
	checked, err := checkedBundleDescriptor(ref, cached)
	if err != nil {
		return ref, err
	}
	validation.inspected[ref.Key] = cached
	return checked, nil
}

func checkedBundleDescriptor(want, actual bundleRef) (bundleRef, error) {
	if want.Key != actual.Key || want.Range != actual.Range || want.Size != 0 && want.Size != actual.Size || want.ETag != "" && want.ETag != actual.ETag {
		return want, fmt.Errorf("conflicting bundle descriptors: %s", want.Key)
	}
	if len(want.Recipients) != 0 && !slices.Equal(want.Recipients, actual.Recipients) {
		return want, fmt.Errorf("bundle recipient policy disagrees with ciphertext: %s", want.Key)
	}
	// Do not expose the cached slice to callers that might modify it.
	actual.Recipients = slices.Clone(actual.Recipients)
	return actual, nil
}

func validationDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func bundleValidationIdentity(endpoint, bucket string, ref bundleRef) string {
	// Hash endpoint information instead of recording potentially sensitive URLs.
	data, _ := json.Marshal([]any{"validation-v2", manifestCodec, endpoint, bucket, ref.Key, ref.Range, ref.ETag, ref.Size})
	return validationDigest(data)
}

type cachedBundleValidation struct {
	Identity string    `json:"identity"`
	Bundle   bundleRef `json:"bundle"`
	Checksum string    `json:"checksum"`
}

func (entry cachedBundleValidation) checksum() string {
	data, _ := json.Marshal(entry.Bundle)
	return validationDigest(append([]byte(entry.Identity+"\x00"), data...))
}

// Corrupt, missing, outdated or inaccessible cache files are ordinary misses.
// The checksum detects accidental corruption, not hostile local modification.
func (validation *bundleValidation) load(identity string, ref bundleRef) (bundleRef, bool) {
	if validation.directory == "" || identity == "" {
		return ref, false
	}
	file, err := os.OpenFile(filepath.Join(validation.directory, identity+".json"), os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return ref, false
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxManifestBytes {
		return ref, false
	}
	data, err := io.ReadAll(io.LimitReader(file, maxManifestBytes+1))
	if err != nil || len(data) > maxManifestBytes {
		return ref, false
	}
	var entry cachedBundleValidation
	if json.Unmarshal(data, &entry) != nil || entry.Identity != identity || entry.Checksum != entry.checksum() {
		return ref, false
	}
	cached := entry.Bundle
	if cached.Key != ref.Key || cached.Range != ref.Range || cached.ETag != ref.ETag || cached.Size != ref.Size || len(cached.Recipients) == 0 || len(cached.Recipients) > 1<<16 {
		return ref, false
	}
	for i, id := range cached.Recipients {
		if !recipientID.MatchString(id) || i > 0 && cached.Recipients[i-1] >= id {
			return ref, false
		}
	}
	return cached, true
}

func (validation *bundleValidation) store(identity string, ref bundleRef) {
	if validation.directory == "" || identity == "" {
		return
	}
	entry := cachedBundleValidation{Identity: identity, Bundle: ref}
	entry.Checksum = entry.checksum()
	data, err := json.Marshal(entry)
	if err != nil || len(data) > maxManifestBytes {
		return
	}
	if os.MkdirAll(validation.directory, 0700) != nil {
		return
	}
	file, err := os.CreateTemp(validation.directory, ".validation-*")
	if err != nil {
		return
	}
	defer func() { _ = os.Remove(file.Name()) }()
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return
	}
	// Same-directory rename keeps concurrent helpers from reading partial entries.
	_ = os.Rename(file.Name(), filepath.Join(validation.directory, identity+".json"))
}
