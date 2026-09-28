package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestLeaseAWSScratchPreflight(t *testing.T) {
	if os.Getenv("GIT_REMOTE_AWS_SCRATCH_CHILD") == "1" {
		getTestBucketAndTable(t)
		t.Fatal("unsafe scratch setup passed its account guard")
	}
	for _, scenario := range []struct {
		name, account, want string
		identityError       bool
		identityCalls       int32
	}{
		{name: "missing account", want: "set GIT_REMOTE_AWS_TEST_ACCOUNT"},
		{name: "wrong account", account: "210987654321", want: "wrong aws account", identityCalls: 1},
		{name: "identity failure", account: "123456789012", want: "identify scratch account", identityError: true, identityCalls: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var identityCalls, unexpected atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.Header.Get("X-Amz-Target") != "" || r.ParseForm() != nil || r.Form.Get("Action") != "GetCallerIdentity" {
					unexpected.Add(1)
					w.WriteHeader(http.StatusForbidden)
					return
				}
				identityCalls.Add(1)
				w.Header().Set("Content-Type", "text/xml")
				if scenario.identityError {
					w.WriteHeader(http.StatusForbidden)
					_, _ = io.WriteString(w, `<ErrorResponse><Error><Code>AccessDenied</Code><Message>identity unavailable</Message></Error></ErrorResponse>`)
					return
				}
				_, _ = io.WriteString(w, `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Account>123456789012</Account><Arn>arn:aws:iam::123456789012:user/test</Arn><UserId>test</UserId></GetCallerIdentityResult></GetCallerIdentityResponse>`)
			}))
			defer server.Close()
			child := groupCommand(t.Context(), os.Args[0], "-test.run=^TestLeaseAWSScratchPreflight$", "-test.timeout=10s")
			child.Env = append(testEnvironment(),
				"GIT_REMOTE_AWS_SCRATCH_CHILD=1", "GIT_REMOTE_AWS_TEST_ACCOUNT="+scenario.account,
				"AWS_ACCESS_KEY_ID=test", "AWS_SECRET_ACCESS_KEY=test", "AWS_SESSION_TOKEN=",
				"AWS_REGION=us-east-1", "AWS_CONFIG_FILE=/dev/null", "AWS_SHARED_CREDENTIALS_FILE=/dev/null", "AWS_PROFILE=",
				"AWS_EC2_METADATA_DISABLED=true", "AWS_IGNORE_CONFIGURED_ENDPOINT_URLS=false",
				"AWS_ENDPOINT_URL="+server.URL, "AWS_ENDPOINT_URL_STS="+server.URL,
				"AWS_ENDPOINT_URL_S3="+server.URL, "AWS_ENDPOINT_URL_DYNAMODB="+server.URL,
			)
			output, err := child.CombinedOutput()
			if err == nil || !strings.Contains(string(output), scenario.want) {
				t.Fatalf("account guard: %v, want failure containing %q\n%s", err, scenario.want, output)
			}
			if identityCalls.Load() != scenario.identityCalls || unexpected.Load() != 0 {
				t.Fatalf("unsafe account reached provider operations: identity=%d, other=%d", identityCalls.Load(), unexpected.Load())
			}
		})
	}
}

type cleanupHTTP func(*http.Request) (*http.Response, error)

func (f cleanupHTTP) Do(r *http.Request) (*http.Response, error) { return f(r) }

func cleanupClients(send cleanupHTTP) *awsClients {
	cfg := aws.Config{Region: "us-east-1", Credentials: aws.AnonymousCredentials{}, HTTPClient: send, Retryer: func() aws.Retryer { return aws.NopRetryer{} }}
	return &awsClients{s3: s3.NewFromConfig(cfg), dynamodb: dynamodb.NewFromConfig(cfg)}
}

func cleanupResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestLeaseAWSCleanupTable(t *testing.T) {
	for _, scenario := range []struct {
		name, code, want string
		calls            int
	}{
		{name: "creating then active", code: "ResourceInUseException", calls: 2},
		{name: "absent", code: "ResourceNotFoundException", calls: 1},
		{name: "access denied", code: "AccessDeniedException", want: "AccessDeniedException", calls: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				clients := cleanupClients(func(r *http.Request) (*http.Response, error) {
					if r.Header.Get("X-Amz-Target") == "DynamoDB_20120810.DeleteTable" {
						calls++
						if calls == 1 {
							return cleanupResponse(http.StatusBadRequest, `{"__type":"`+scenario.code+`"}`), nil
						}
						return cleanupResponse(http.StatusOK, `{}`), nil
					}
					if r.Method != http.MethodGet || !r.URL.Query().Has("versions") {
						t.Errorf("unexpected cleanup request: %s %s", r.Method, r.URL)
					}
					return cleanupResponse(http.StatusNotFound, `<Error><Code>NoSuchBucket</Code></Error>`), nil
				})
				err := deleteTestResources(clients, "123456789012", "git-remote-aws-test-cleanup")
				if scenario.want == "" && err != nil || scenario.want != "" && (err == nil || !strings.Contains(err.Error(), scenario.want)) || calls != scenario.calls {
					t.Fatalf("cleanup: %v, %d calls; want %q, %d calls", err, calls, scenario.want, scenario.calls)
				}
			})
		})
	}
}

func TestLeaseAWSCleanupIndependentBudgets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		deleted := false
		clients := cleanupClients(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("X-Amz-Target") == "DynamoDB_20120810.DeleteTable" {
				if err := r.Context().Err(); err != nil {
					return nil, err
				}
				deleted = true
				return cleanupResponse(http.StatusOK, `{}`), nil
			}
			<-r.Context().Done()
			return nil, r.Context().Err()
		})
		err := deleteTestResources(clients, "123456789012", "git-remote-aws-test-cleanup")
		if !errors.Is(err, context.DeadlineExceeded) || !deleted {
			t.Fatalf("bucket timeout suppressed table cleanup or was lost: deleted=%v, error=%v", deleted, err)
		}
	})
}

func TestLeaseAWSCleanupTableTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		clients := cleanupClients(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("X-Amz-Target") == "DynamoDB_20120810.DeleteTable" {
				calls++
				return cleanupResponse(http.StatusBadRequest, `{"__type":"ResourceInUseException"}`), nil
			}
			return cleanupResponse(http.StatusNotFound, `<Error><Code>NoSuchBucket</Code></Error>`), nil
		})
		err := deleteTestResources(clients, "123456789012", "git-remote-aws-test-cleanup")
		if !errors.Is(err, context.DeadlineExceeded) || calls < 2 {
			t.Fatalf("cleanup did not retry to its deadline: calls=%d, error=%v", calls, err)
		}
	})
}
