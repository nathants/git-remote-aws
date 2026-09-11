package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func bundleUploadClients(endpoint string) *awsClients {
	return &awsClients{s3: s3.New(s3.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(endpoint), UsePathStyle: true,
		Retryer: aws.NopRetryer{},
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
	})}
}

func sparseBundleFile(t *testing.T, size int64) *os.File {
	t.Helper()
	file, err := os.Create(filepath.Join(t.TempDir(), "ciphertext"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if err := file.Truncate(size); err != nil {
		t.Fatal(err)
	}
	for _, offset := range []int64{0, size / 2, size - 1} {
		if _, err := file.WriteAt([]byte{byte(offset%251 + 1)}, offset); err != nil {
			t.Fatal(err)
		}
	}
	return file
}

func TestBundleMultipartUpload(t *testing.T) {
	const size = 64<<20 + 19
	file := sparseBundleFile(t, size)
	tip := strings.Repeat("a", 40)
	var mu sync.Mutex
	var calls []string
	var offset int64
	checksums := make(map[int]string)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		query := r.URL.Query()
		switch {
		case r.Method == http.MethodPost && query.Has("uploads"):
			calls = append(calls, "create")
			if r.Header.Get("X-Amz-Meta-Git-Remote-Aws-Push-Tip") != tip || r.Header.Get("X-Amz-Checksum-Algorithm") != "CRC32" {
				t.Errorf("multipart initiation omitted identity or checksums: %v", r.Header)
			}
			_, _ = io.WriteString(w, `<InitiateMultipartUploadResult><UploadId>upload</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == http.MethodPut && query.Get("uploadId") == "upload":
			number, err := strconv.Atoi(query.Get("partNumber"))
			if err != nil {
				t.Error(err)
			}
			calls = append(calls, fmt.Sprintf("part %d", number))
			checksum, digest := crc32.NewIEEE(), sha256.New()
			n, err := io.Copy(io.MultiWriter(checksum, digest), r.Body)
			if err != nil {
				t.Error(err)
			}
			expected := sha256.New()
			if _, err := io.Copy(expected, io.NewSectionReader(file, offset, n)); err != nil {
				t.Error(err)
			}
			if n > 64<<20 || n <= 0 || string(digest.Sum(nil)) != string(expected.Sum(nil)) {
				t.Errorf("part %d corrupted or unbounded: offset=%d size=%d", number, offset, n)
			}
			offset += n
			checksums[number] = base64.StdEncoding.EncodeToString(binary.BigEndian.AppendUint32(nil, checksum.Sum32()))
			w.Header().Set("ETag", fmt.Sprintf(`"part-%d"`, number))
			w.Header().Set("X-Amz-Checksum-Crc32", checksums[number])
		case r.Method == http.MethodPost && query.Get("uploadId") == "upload":
			calls = append(calls, "complete")
			var complete struct {
				Parts []struct {
					PartNumber    int
					ETag          string
					ChecksumCRC32 string
				} `xml:"Part"`
			}
			if err := xml.NewDecoder(r.Body).Decode(&complete); err != nil {
				t.Error(err)
			}
			if len(complete.Parts) != 2 || r.Header.Get("If-None-Match") != "*" {
				t.Errorf("incomplete or unconditional completion: %+v", complete)
			}
			for i, part := range complete.Parts {
				if part.PartNumber != i+1 || part.ETag != fmt.Sprintf(`"part-%d"`, i+1) || part.ChecksumCRC32 != checksums[i+1] {
					t.Errorf("completion lost part identity/checksum: %+v", part)
				}
			}
			_, _ = io.WriteString(w, `<CompleteMultipartUploadResult><ETag>"completed"</ETag></CompleteMultipartUploadResult>`)
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `<Error><Code>EntityTooLarge</Code><Message>multipart required</Message></Error>`)
		}
	}))
	defer server.Close()
	if err := bundleUploadClients(server.URL).uploadBundle(t.Context(), "bucket", "repo/bundle", tip, file); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(calls, ","); got != "create,part 1,part 2,complete" || offset != size {
		t.Fatalf("multipart upload did not transfer the complete file: %s, %d bytes", got, offset)
	}
}

func TestBundleUploadNeverOverwritesExistingCiphertext(t *testing.T) {
	for _, prior := range []string{"same-tip", "other-tip", "historical-without-metadata"} {
		t.Run(prior, func(t *testing.T) {
			file := sparseBundleFile(t, 64)
			tip := strings.Repeat("a", 40)
			var mu sync.Mutex
			var overwrites, heads int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch r.Method {
				case http.MethodPut:
					if r.Header.Get("If-None-Match") != "*" {
						overwrites++
						return
					}
					w.WriteHeader(http.StatusPreconditionFailed)
					_, _ = io.WriteString(w, `<Error><Code>PreconditionFailed</Code></Error>`)
				case http.MethodHead:
					heads++
					if prior == "same-tip" {
						w.Header().Set("X-Amz-Meta-Git-Remote-Aws-Push-Tip", tip)
					} else if prior == "other-tip" {
						w.Header().Set("X-Amz-Meta-Git-Remote-Aws-Push-Tip", strings.Repeat("b", 40))
					}
					w.Header().Set("Content-Length", "64")
				default:
					t.Errorf("unexpected collision operation: %s", r.Method)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			defer server.Close()
			err := bundleUploadClients(server.URL).uploadBundle(t.Context(), "bucket", "repo/bundle", tip, file)
			if (err == nil) != (prior == "same-tip") {
				t.Errorf("wrong collision outcome: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if overwrites != 0 || heads != 1 {
				t.Errorf("existing ciphertext was not protected: overwrites=%d heads=%d", overwrites, heads)
			}
		})
	}
}

func TestBundleMultipartFailureCleanup(t *testing.T) {
	for _, scenario := range []string{"start", "part", "missing-etag", "missing-checksum", "complete", "ambiguous-complete", "embedded-error", "cancel", "abort-failure", "collision-reuse", "collision-abort-failure"} {
		t.Run(scenario, func(t *testing.T) {
			file := sparseBundleFile(t, 64<<20+19)
			tip := strings.Repeat("a", 40)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var mu sync.Mutex
			var parts, completes, aborts, deletes int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					t.Error(err)
				}
				query := r.URL.Query()
				fail := func(status int, code, message string) {
					w.WriteHeader(status)
					_, _ = fmt.Fprintf(w, `<Error><Code>%s</Code><Message>%s</Message></Error>`, code, message)
				}
				switch {
				case r.Method == http.MethodPost && query.Has("uploads"):
					if scenario == "start" {
						fail(http.StatusForbidden, "AccessDenied", "start rejected")
						return
					}
					_, _ = io.WriteString(w, `<InitiateMultipartUploadResult><UploadId>upload</UploadId></InitiateMultipartUploadResult>`)
				case r.Method == http.MethodPut && query.Get("uploadId") == "upload":
					parts++
					if parts == 2 && (scenario == "part" || scenario == "abort-failure") {
						fail(http.StatusForbidden, "AccessDenied", "part rejected")
						return
					}
					if scenario == "cancel" {
						cancel()
						select {
						case <-r.Context().Done():
						case <-time.After(3 * time.Second):
							t.Error("part request did not follow cancellation")
						}
						return
					}
					if scenario != "missing-etag" {
						w.Header().Set("ETag", `"part"`)
					}
					if scenario != "missing-checksum" {
						w.Header().Set("X-Amz-Checksum-Crc32", "AAAAAA==")
					}
				case r.Method == http.MethodPost && query.Get("uploadId") == "upload":
					completes++
					switch scenario {
					case "complete":
						fail(http.StatusForbidden, "AccessDenied", "complete rejected")
					case "ambiguous-complete":
						fail(http.StatusInternalServerError, "InternalError", "completion response lost")
					case "embedded-error":
						fail(http.StatusOK, "InternalError", "embedded completion error")
					case "collision-reuse", "collision-abort-failure":
						fail(http.StatusPreconditionFailed, "PreconditionFailed", "already exists")
					default:
						t.Error("completed after an earlier failure")
						fail(http.StatusBadRequest, "InvalidRequest", "unexpected completion")
					}
				case r.Method == http.MethodHead:
					w.Header().Set("X-Amz-Meta-Git-Remote-Aws-Push-Tip", tip)
					w.Header().Set("Content-Length", "67108883")
				case r.Method == http.MethodDelete && query.Get("uploadId") == "upload":
					aborts++
					if scenario == "abort-failure" || scenario == "collision-abort-failure" {
						fail(http.StatusForbidden, "AccessDenied", "abort rejected")
					} else if scenario == "ambiguous-complete" {
						fail(http.StatusNotFound, "NoSuchUpload", "already completed")
					} else {
						w.WriteHeader(http.StatusNoContent)
					}
				case r.Method == http.MethodDelete:
					deletes++
				default:
					t.Errorf("unexpected upload operation: %s %s", r.Method, r.URL)
					fail(http.StatusBadRequest, "InvalidRequest", "unexpected request")
				}
			}))
			defer server.Close()
			err := bundleUploadClients(server.URL).uploadBundle(ctx, "bucket", "repo/bundle", tip, file)
			if (err == nil) != (scenario == "collision-reuse") {
				t.Fatalf("unexpected upload outcome: %v", err)
			}
			message := map[string]string{
				"start": "start rejected", "part": "part rejected", "missing-etag": "no ETag", "missing-checksum": "CRC32",
				"complete": "complete rejected", "ambiguous-complete": "completion response lost", "embedded-error": "embedded completion error",
				"cancel": "canceled", "abort-failure": "part rejected", "collision-abort-failure": "abort rejected",
			}[scenario]
			if message != "" && !strings.Contains(err.Error(), message) {
				t.Errorf("primary error was lost: %v", err)
			}
			if scenario == "abort-failure" && !strings.Contains(err.Error(), "abort rejected") {
				t.Errorf("cleanup error was lost: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			wantAborts := 1
			if scenario == "start" {
				wantAborts = 0
			}
			wantCompletes := 0
			if strings.Contains(scenario, "complete") || scenario == "embedded-error" || strings.HasPrefix(scenario, "collision-") {
				wantCompletes = 1
			}
			if aborts != wantAborts || completes != wantCompletes || deletes != 0 {
				t.Errorf("wrong cleanup scope: aborts=%d completes=%d deletes=%d", aborts, completes, deletes)
			}
		})
	}
}

func TestBundleMultipartPartLimits(t *testing.T) {
	for _, size := range []int64{64<<20 + 1, 5<<30 + 1, (64 << 20) * 10000, (64<<20)*10000 + 1, (5 << 30) * 10000, (5<<30)*10000 + 1, math.MaxInt64} {
		part, err := multipartBundlePartSize(size)
		if size > (5<<30)*10000 {
			if err == nil {
				t.Errorf("accepted unsupported object size: %d", size)
			}
			continue
		}
		if err != nil || part < 5<<20 || part > 5<<30 || (size+part-1)/part > 10000 {
			t.Errorf("invalid part size for %d bytes: part=%d, err=%v", size, part, err)
		}
	}
}

func TestBundleMultipartRetryRewindsPart(t *testing.T) {
	const size = 1<<20 + 13
	file := sparseBundleFile(t, size)
	var mu sync.Mutex
	var attempts int
	var hashes []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Query().Has("uploads"):
			_, _ = io.WriteString(w, `<InitiateMultipartUploadResult><UploadId>upload</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == http.MethodPut:
			attempts++
			digest, checksum := sha256.New(), crc32.NewIEEE()
			n, err := io.Copy(io.MultiWriter(digest, checksum), r.Body)
			if err != nil || n != size || r.URL.Query().Get("partNumber") != "1" {
				t.Errorf("retry changed part boundaries: size=%d, err=%v, query=%s", n, err, r.URL.RawQuery)
			}
			hashes = append(hashes, string(digest.Sum(nil)))
			if attempts == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `<Error><Code>SlowDown</Code></Error>`)
				return
			}
			w.Header().Set("ETag", `"part"`)
			w.Header().Set("X-Amz-Checksum-Crc32", base64.StdEncoding.EncodeToString(binary.BigEndian.AppendUint32(nil, checksum.Sum32())))
		case r.Method == http.MethodPost && r.URL.Query().Get("uploadId") == "upload":
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = io.WriteString(w, `<CompleteMultipartUploadResult><ETag>"complete"</ETag></CompleteMultipartUploadResult>`)
		default:
			t.Errorf("unexpected retry operation: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	clients := bundleUploadClients(server.URL)
	options := clients.s3.Options()
	options.Retryer = retry.NewStandard(func(options *retry.StandardOptions) {
		options.MaxAttempts = 5
		options.Backoff = retry.BackoffDelayerFunc(func(int, error) (time.Duration, error) { return 0, nil })
	})
	clients.s3 = s3.New(options)
	if err := clients.multipartUploadBundle(t.Context(), "bucket", "repo/bundle", strings.Repeat("a", 40), file, size); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 2 || hashes[0] != hashes[1] {
		t.Fatalf("SDK retry did not replay the exact part: attempts=%d hashes=%x", attempts, hashes)
	}
}
