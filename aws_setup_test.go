package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestLeaseAWSSetupDoesNotMistakeErrorsForAbsence(t *testing.T) {
	for _, service := range []string{"s3", "dynamodb"} {
		for _, ensure := range []string{"", "y"} {
			t.Run(service+"/ensure="+ensure, func(t *testing.T) {
				var mu sync.Mutex
				var mutations int
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if target := r.Header.Get("X-Amz-Target"); target != "" {
						w.Header().Set("Content-Type", "application/x-amz-json-1.0")
						if strings.HasSuffix(target, ".DescribeTable") {
							if service == "dynamodb" {
								w.WriteHeader(http.StatusBadRequest)
								_, _ = io.WriteString(w, `{"__type":"AccessDeniedException","message":"access denied, not missing"}`)
							} else {
								_, _ = io.WriteString(w, `{"Table":{"TableStatus":"ACTIVE"}}`)
							}
							return
						}
					} else if r.Method == http.MethodHead {
						if service == "s3" {
							w.WriteHeader(http.StatusForbidden)
						} else {
							w.Header().Set("X-Amz-Bucket-Region", "us-east-1")
						}
						return
					} else if r.URL.Query().Get("Action") == "GetCallerIdentity" || r.Method == http.MethodPost {
						_, _ = io.WriteString(w, `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Account>123456789012</Account><Arn>arn:aws:iam::123456789012:user/test</Arn><UserId>test</UserId></GetCallerIdentityResult></GetCallerIdentityResponse>`)
						return
					}
					mu.Lock()
					mutations++
					mu.Unlock()
					w.WriteHeader(http.StatusForbidden)
				}))
				defer server.Close()
				dir := t.TempDir()
				runAt(dir, "git", "init", "-q")
				child := testHelperCommand(t, dir, server.URL, "10s")
				child.Env = append(child.Env, "ensure="+ensure, "AWS_ENDPOINT_URL_STS="+server.URL)
				child.Stdin = strings.NewReader("capabilities\n\n")
				output, err := child.CombinedOutput()
				if err == nil {
					t.Fatal("initialization hid a provider error")
				}
				for _, misleading := range []string{"did not exist", "creating private", "created private"} {
					if strings.Contains(string(output), misleading) {
						t.Errorf("provider failure was treated as absence: %s", output)
					}
				}
				mu.Lock()
				defer mu.Unlock()
				if mutations != 0 {
					t.Errorf("provider failure authorized %d mutations", mutations)
				}
				if !strings.Contains(string(output), "403") && !strings.Contains(string(output), "AccessDenied") {
					t.Errorf("original provider failure was lost: %s", output)
				}
			})
		}
	}
}

func TestLeaseAWSSetupCreatesOnlyMissingResources(t *testing.T) {
	for _, scenario := range []struct {
		name                                string
		bucketMissing, tableMissing, ensure bool
		createError, waitError, setupError  bool
		versioningError                     bool
	}{
		{name: "existing", ensure: true},
		{name: "missing without ensure", bucketMissing: true, tableMissing: true},
		{name: "bucket only", bucketMissing: true, ensure: true},
		{name: "table only", tableMissing: true, ensure: true},
		{name: "both", bucketMissing: true, tableMissing: true, ensure: true},
		{name: "ambiguous bucket create", bucketMissing: true, ensure: true, createError: true},
		{name: "incomplete bucket readiness", bucketMissing: true, ensure: true, waitError: true},
		{name: "incomplete bucket setup", bucketMissing: true, ensure: true, setupError: true},
		{name: "incomplete bucket versioning", bucketMissing: true, ensure: true, versioningError: true},
	} {
		for _, region := range []string{"us-east-1", "eu-west-1"} {
			t.Run(scenario.name+"/"+region, func(t *testing.T) {
				var mu sync.Mutex
				bucketExists, tableExists := !scenario.bucketMissing, !scenario.tableMissing
				calls := make(map[string]int)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					mu.Lock()
					defer mu.Unlock()
					if target := r.Header.Get("X-Amz-Target"); target != "" {
						operation := strings.TrimPrefix(target, "DynamoDB_20120810.")
						calls[operation]++
						w.Header().Set("Content-Type", "application/x-amz-json-1.0")
						switch operation {
						case "DescribeTable":
							if tableExists {
								_, _ = io.WriteString(w, `{"Table":{"TableStatus":"ACTIVE"}}`)
							} else {
								w.WriteHeader(http.StatusBadRequest)
								_, _ = io.WriteString(w, `{"__type":"ResourceNotFoundException"}`)
							}
						case "CreateTable":
							if tableExists {
								t.Error("attempted to create an existing table")
							}
							var input struct {
								TableName, BillingMode string
								KeySchema              []struct{ AttributeName, KeyType string }
								AttributeDefinitions   []struct{ AttributeName, AttributeType string }
							}
							if err := json.Unmarshal(body, &input); err != nil {
								t.Error(err)
							}
							if input.TableName != "table" || input.BillingMode != "PAY_PER_REQUEST" || len(input.KeySchema) != 1 || input.KeySchema[0].AttributeName != "id" || input.KeySchema[0].KeyType != "HASH" || len(input.AttributeDefinitions) != 1 || input.AttributeDefinitions[0].AttributeName != "id" || input.AttributeDefinitions[0].AttributeType != "S" {
								t.Errorf("incompatible table creation: %s", body)
							}
							tableExists = true
							_, _ = io.WriteString(w, `{"TableDescription":{"TableStatus":"CREATING"}}`)
						default:
							t.Errorf("setup changed table configuration or records: %s", operation)
							w.WriteHeader(http.StatusBadRequest)
						}
						return
					}
					if r.Method == http.MethodPost {
						calls["identity"]++
						_, _ = io.WriteString(w, `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Account>123456789012</Account><Arn>arn:aws:iam::123456789012:user/test</Arn><UserId>test</UserId></GetCallerIdentityResult></GetCallerIdentityResponse>`)
						return
					}
					if r.Method == http.MethodHead {
						calls["head"]++
						if bucketExists {
							w.Header().Set("X-Amz-Bucket-Region", region)
							if scenario.waitError {
								w.WriteHeader(http.StatusForbidden)
							}
						} else {
							w.WriteHeader(http.StatusNotFound)
						}
						return
					}
					if r.Method != http.MethodPut {
						t.Errorf("unexpected setup request: %s %s", r.Method, r.URL)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					if !scenario.bucketMissing {
						t.Error("setup mutated an existing bucket")
					}
					operation := "create bucket"
					for _, key := range []string{"publicAccessBlock", "encryption", "policy", "tagging", "versioning"} {
						if r.URL.Query().Has(key) {
							operation = key
						}
					}
					calls[operation]++
					if operation == "create bucket" {
						var input struct{ LocationConstraint string }
						if len(body) != 0 {
							if err := xml.Unmarshal(body, &input); err != nil {
								t.Error(err)
							}
						}
						if region == "us-east-1" && input.LocationConstraint != "" || region != "us-east-1" && input.LocationConstraint != region {
							t.Errorf("wrong bucket region: %s", body)
						}
						if scenario.createError {
							w.WriteHeader(http.StatusInternalServerError)
							_, _ = io.WriteString(w, `<Error><Code>InternalError</Code><Message>unknown create outcome</Message></Error>`)
							return
						}
						bucketExists = true
						return
					}
					if r.Header.Get("X-Amz-Expected-Bucket-Owner") != "123456789012" {
						t.Error("new-bucket configuration is not owner-scoped")
					}
					if scenario.setupError || scenario.versioningError && operation == "versioning" {
						w.WriteHeader(http.StatusForbidden)
						_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code></Error>`)
						return
					}
					switch operation {
					case "versioning":
						var config struct {
							Status    string
							MFADelete string `xml:"MfaDelete"`
						}
						if err := xml.Unmarshal(body, &config); err != nil || config.Status != "Enabled" || config.MFADelete != "" {
							t.Errorf("wrong versioning configuration: %s (%v)", body, err)
						}
					case "publicAccessBlock":
						var policy struct{ BlockPublicAcls, IgnorePublicAcls, BlockPublicPolicy, RestrictPublicBuckets bool }
						if err := xml.Unmarshal(body, &policy); err != nil || !policy.BlockPublicAcls || !policy.IgnorePublicAcls || !policy.BlockPublicPolicy || !policy.RestrictPublicBuckets {
							t.Errorf("bucket privacy changed: %s (%v)", body, err)
						}
					case "encryption":
						var policy struct {
							Rules []struct {
								Default struct{ SSEAlgorithm string } `xml:"ApplyServerSideEncryptionByDefault"`
								Blocked struct {
									Type []string `xml:"EncryptionType"`
								} `xml:"BlockedEncryptionTypes"`
							} `xml:"Rule"`
						}
						if err := xml.Unmarshal(body, &policy); err != nil || len(policy.Rules) != 1 || policy.Rules[0].Default.SSEAlgorithm != "AES256" || len(policy.Rules[0].Blocked.Type) != 1 || policy.Rules[0].Blocked.Type[0] != "SSE-C" {
							t.Errorf("bucket encryption policy changed: %s (%v)", body, err)
						}
					case "policy":
						var policy struct {
							Statement []struct {
								Effect, Principal, Action string
								Resource                  []string
								Condition                 map[string]map[string]string
							}
						}
						if err := json.Unmarshal(body, &policy); err != nil || len(policy.Statement) != 1 || policy.Statement[0].Effect != "Deny" || policy.Statement[0].Principal != "*" || policy.Statement[0].Action != "s3:*" || strings.Join(policy.Statement[0].Resource, ",") != "arn:aws:s3:::bucket,arn:aws:s3:::bucket/*" || policy.Statement[0].Condition["Bool"]["aws:SecureTransport"] != "false" {
							t.Errorf("bucket transport policy changed: %s (%v)", body, err)
						}
					case "tagging":
						if !strings.Contains(string(body), "libaws.infraset") {
							t.Errorf("setup tag changed: %s", body)
						}
					default:
						t.Errorf("unexpected new-bucket configuration: %s", operation)
					}
				}))
				defer server.Close()
				dir := t.TempDir()
				runAt(dir, "git", "init", "-q")
				child := testHelperCommand(t, dir, server.URL, "15s")
				ensure := ""
				if scenario.ensure {
					ensure = "y"
				}
				child.Env = append(child.Env, "ensure="+ensure, "AWS_REGION="+region, "AWS_DEFAULT_REGION="+region, "AWS_ENDPOINT_URL_STS="+server.URL)
				child.Stdin = strings.NewReader("capabilities\n\n")
				output, err := child.CombinedOutput()
				wantError := !scenario.ensure || scenario.createError || scenario.waitError || scenario.setupError || scenario.versioningError
				if (err != nil) != wantError {
					t.Fatalf("setup result: %v\n%s", err, output)
				}
				if scenario.waitError || scenario.setupError || scenario.versioningError {
					for _, message := range []string{"was created but setup is incomplete", "before use", "StatusCode: 403"} {
						if !strings.Contains(string(output), message) {
							t.Errorf("post-create failure lost %q: %s", message, output)
						}
					}
				}
				mu.Lock()
				defer mu.Unlock()
				if scenario.bucketMissing && scenario.ensure && calls["create bucket"] != 1 {
					t.Errorf("create must not be repeated: %v", calls)
				}
				if !wantError && scenario.bucketMissing {
					for _, operation := range []string{"publicAccessBlock", "encryption", "policy", "tagging", "versioning"} {
						if calls[operation] != 1 {
							t.Errorf("missing new-bucket configuration %s: %v", operation, calls)
						}
					}
				}
				if !wantError && scenario.tableMissing && calls["CreateTable"] != 1 {
					t.Errorf("table was not created: %v", calls)
				}
				if scenario.createError || scenario.waitError {
					for _, operation := range []string{"publicAccessBlock", "encryption", "policy", "tagging", "versioning", "CreateTable"} {
						if calls[operation] != 0 {
							t.Errorf("setup continued after creation/readiness failure: %v\n%s", calls, output)
						}
					}
				}
				if scenario.waitError || scenario.setupError || scenario.versioningError {
					if !bucketExists || strings.Contains(string(output), "created private s3 bucket") {
						t.Errorf("incomplete setup removed the bucket or reported success: %v\n%s", calls, output)
					}
				}
				if scenario.waitError && (calls["head"] != 2 || !strings.Contains(string(output), "HeadBucket")) {
					t.Errorf("failure did not occur in the post-create waiter: %v\n%s", calls, output)
				}
			})
		}
	}
}

func TestLeaseAWSConfigurationPreservesProfileEndpoints(t *testing.T) {
	var signed atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			if r.Header.Get("Authorization") != "" {
				t.Error("bucket existence probe unexpectedly requires list credentials")
			}
			w.Header().Set("X-Amz-Bucket-Region", "eu-west-1")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if !strings.Contains(r.Header.Get("Authorization"), "Credential=profile-key/") || !strings.Contains(r.Header.Get("Authorization"), "/eu-west-1/dynamodb/aws4_request") || r.Header.Get("X-Amz-Security-Token") != "profile-token" {
			t.Error("profile credentials, token or signing region were not preserved")
		}
		signed.Add(1)
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		switch r.Header.Get("X-Amz-Target") {
		case "DynamoDB_20120810.DescribeTable":
			_, _ = io.WriteString(w, `{"Table":{"TableStatus":"ACTIVE"}}`)
		case "DynamoDB_20120810.GetItem":
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected profile request: %s", r.Header.Get("X-Amz-Target"))
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	runAt(dir, "git", "init", "-q")
	config := filepath.Join(t.TempDir(), "config")
	credentials := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(config, []byte("[profile selected]\nregion = eu-west-1\nservices = local\n[services local]\ns3 =\n  endpoint_url = "+server.URL+"\ndynamodb =\n  endpoint_url = "+server.URL+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentials, []byte("[selected]\naws_access_key_id = profile-key\naws_secret_access_key = synthetic-profile-secret\naws_session_token = profile-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	child := testHelperCommand(t, dir, server.URL, "10s")
	child.Env = append(child.Env,
		"AWS_ACCESS_KEY_ID=", "AWS_SECRET_ACCESS_KEY=", "AWS_SESSION_TOKEN=",
		"AWS_REGION=", "AWS_DEFAULT_REGION=", "AWS_PROFILE=selected", "AWS_DEFAULT_PROFILE=",
		"AWS_CONFIG_FILE="+config, "AWS_SHARED_CREDENTIALS_FILE="+credentials,
		"AWS_ENDPOINT_URL=", "AWS_ENDPOINT_URL_S3=", "AWS_ENDPOINT_URL_DYNAMODB=",
		"AWS_IGNORE_CONFIGURED_ENDPOINT_URLS=false",
	)
	child.Stdin = strings.NewReader("list\n\n")
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("profile configuration failed: %v\n%s", err, output)
	}
	// DescribeTable, alias collision check, and repository pointer lookup.
	if signed.Load() != 3 {
		t.Fatalf("profile endpoints received %d signed calls, want 3", signed.Load())
	}
}

func TestLeaseAWSWaitCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		_, _ = io.WriteString(w, `{"Table":{"TableStatus":"CREATING"}}`)
		cancel()
	}))
	defer server.Close()
	for key, value := range map[string]string{
		"AWS_ACCESS_KEY_ID": "test", "AWS_SECRET_ACCESS_KEY": "test", "AWS_SESSION_TOKEN": "",
		"AWS_REGION": "us-east-1", "AWS_ENDPOINT_URL_DYNAMODB": server.URL,
		"AWS_CONFIG_FILE": "/dev/null", "AWS_SHARED_CREDENTIALS_FILE": "/dev/null", "AWS_PROFILE": "",
		"AWS_EC2_METADATA_DISABLED": "true", "AWS_IGNORE_CONFIGURED_ENDPOINT_URLS": "false",
	} {
		t.Setenv(key, value)
	}
	clients, err := newAWSClients(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if clients.dynamodb.Options().RetryMaxAttempts != 5 || clients.s3.Options().RetryMaxAttempts != 5 {
		t.Fatal("default request retry budget changed")
	}
	if err := clients.waitForTable(ctx, "table"); err == nil || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("table waiter ignored cancellation: %v", err)
	}
}
