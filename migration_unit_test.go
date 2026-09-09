package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const oldMigrationJSON = `{"id":{"S":"bucket/repo"},"uid":{"S":""},"unix":{"N":"0"},"branch":{"S":"main"},"bundles":{"S":"repo/bundles_abc"}}`
const newMigrationJSON = `{"id":{"S":"bucket/repo"},"data":{"M":{"branch":{"S":"main"},"bundles":{"S":"repo/bundles_abc"}}}}`
const migrationTableJSON = `{"Table":{"TableArn":"arn:aws:dynamodb:us-east-1:123456789012:table/table","TableStatus":"ACTIVE","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}]}}`

func migrationItem(t *testing.T, text string) map[string]types.AttributeValue {
	t.Helper()
	item, err := attributevalue.UnmarshalMapJSON([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func TestMigrationEnvelope(t *testing.T) {
	before := migrationItem(t, oldMigrationJSON)
	original := migrationItem(t, oldMigrationJSON)
	got, err := migratedRepoItem(before)
	if err != nil || !reflect.DeepEqual(got, migrationItem(t, newMigrationJSON)) || !reflect.DeepEqual(before, original) {
		t.Fatalf("migration changed payload or source: %v %v", got, err)
	}
	for _, text := range []string{newMigrationJSON, `{"id":{"S":"bucket/repo"}}`, `{"id":{"S":"bucket/repo"},"data":{"M":{}}}`} {
		item := migrationItem(t, text)
		got, err := migratedRepoItem(item)
		if err != nil || !reflect.DeepEqual(got, item) {
			t.Fatalf("existing envelope changed: %v %v", got, err)
		}
	}
	for _, update := range []struct {
		field string
		value types.AttributeValue
	}{
		{"id", &types.AttributeValueMemberN{Value: "1"}},
		{"uid", &types.AttributeValueMemberS{Value: "held"}},
		{"unix", &types.AttributeValueMemberN{Value: "1"}},
		{"branch", &types.AttributeValueMemberN{Value: "1"}},
		{"data", &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{}}},
		{"extra", &types.AttributeValueMemberS{Value: "preserve me"}},
		{"owner_token", &types.AttributeValueMemberS{Value: "held"}},
	} {
		item := migrationItem(t, oldMigrationJSON)
		item[update.field] = update.value
		if _, err := migratedRepoItem(item); err == nil {
			t.Errorf("invalid %s accepted", update.field)
		}
	}
	for _, text := range []string{
		`{"id":{"S":"key"},"data":{"S":"not a map"}}`,
		`{"id":{"S":"key"},"data":{"M":{"other":{"S":"data"}}}}`,
		`{"id":{"S":"key"},"expires_at":{"N":"1"}}`,
	} {
		if _, err := migratedRepoItem(migrationItem(t, text)); err == nil {
			t.Errorf("invalid envelope accepted: %s", text)
		}
	}
	got, err = migratedRepoItem(migrationItem(t, `{"id":{"S":"key"},"uid":{"NULL":true},"unix":{"N":"0"}}`))
	if err != nil || len(got) != 1 {
		t.Fatalf("absent payload changed: %v %v", got, err)
	}
}

func TestMigrationUpdateFencesPreimage(t *testing.T) {
	before, after := migrationItem(t, oldMigrationJSON), migrationItem(t, newMigrationJSON)
	request := migrationUpdate("table", before, after)
	if aws.ToString(request.UpdateExpression) != "SET #data = :data REMOVE #branch, #bundles, #uid, #unix" ||
		!reflect.DeepEqual(request.Key, map[string]types.AttributeValue{"id": before["id"]}) {
		t.Fatalf("wrong update: %+v", request)
	}
	for _, name := range legacyRepoFields {
		if !strings.Contains(*request.ConditionExpression, "#"+name+" = :"+name) ||
			!reflect.DeepEqual(request.ExpressionAttributeValues[":"+name], before[name]) {
			t.Fatalf("unfenced legacy field %s", name)
		}
	}
	for _, name := range []string{"data", "owner_token", "expires_at"} {
		if !strings.Contains(*request.ConditionExpression, "attribute_not_exists(#"+name+")") {
			t.Fatalf("unfenced modern field %s", name)
		}
	}
	delete(before, "uid")
	delete(before, "unix")
	request = migrationUpdate("table", before, after)
	if !strings.Contains(*request.ConditionExpression, "attribute_not_exists(#uid)") || !strings.Contains(*request.ConditionExpression, "attribute_not_exists(#unix)") {
		t.Fatal("absent ownership was not fenced")
	}
}

type migrationHTTP func(*http.Request) (*http.Response, error)

func (f migrationHTTP) Do(r *http.Request) (*http.Response, error) { return f(r) }

type migrationStep struct {
	operation, response string
	status              int
	inspect             func([]byte)
}

func migrationConfig(t *testing.T, steps []migrationStep) aws.Config {
	t.Helper()
	i := 0
	t.Cleanup(func() {
		if i != len(steps) {
			t.Errorf("provider calls = %d, want %d", i, len(steps))
		}
	})
	return aws.Config{Region: "us-east-1", Credentials: aws.AnonymousCredentials{}, HTTPClient: migrationHTTP(func(r *http.Request) (*http.Response, error) {
		if i >= len(steps) {
			t.Error("unexpected provider request")
			return nil, errors.New("unexpected provider request")
		}
		step := steps[i]
		i++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		operation := strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "DynamoDB_20120810.")
		contentType := "application/x-amz-json-1.0"
		if operation == "" && strings.Contains(string(body), "Action=GetCallerIdentity") {
			operation, contentType = "GetCallerIdentity", "text/xml"
		}
		if operation != step.operation {
			t.Errorf("provider operation = %s, want %s", operation, step.operation)
		}
		if step.inspect != nil {
			step.inspect(body)
		}
		status := step.status
		if status == 0 {
			status = 200
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(step.response)), Request: r}, nil
	})}
}

func migrationPreflight() []migrationStep {
	return []migrationStep{
		{operation: "GetCallerIdentity", response: `<GetCallerIdentityResponse><GetCallerIdentityResult><Account>123456789012</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`},
		{operation: "DescribeTable", response: migrationTableJSON},
		{operation: "DescribeTimeToLive", response: `{"TimeToLiveDescription":{"TimeToLiveStatus":"DISABLED"}}`},
	}
}

func migrationTestInput(t *testing.T) migrationInput {
	return migrationInput{Account: "123456789012", Region: "us-east-1", Table: "table", Bucket: "bucket", Apply: true, WritersStopped: true, Backup: filepath.Join(t.TempDir(), "backup.json")}
}

func TestMigrationGuardsBeforeMutation(t *testing.T) {
	for _, scenario := range []string{"account", "schema", "ttl", "held", "backup", "confirmation"} {
		t.Run(scenario, func(t *testing.T) {
			in := migrationTestInput(t)
			steps := migrationPreflight()
			switch scenario {
			case "account":
				in.Account = "wrong"
				steps = steps[:1]
			case "schema":
				steps = steps[:2]
				steps[1].response = strings.ReplaceAll(migrationTableJSON, `"HASH"`, `"RANGE"`)
			case "ttl":
				steps[2].response = `{"TimeToLiveDescription":{"TimeToLiveStatus":"ENABLED"}}`
			case "held":
				bad := strings.ReplaceAll(oldMigrationJSON, `"uid":{"S":""}`, `"uid":{"S":"held"}`)
				bad = strings.ReplaceAll(bad, "bucket/repo", "bucket/held")
				steps = append(steps, migrationStep{operation: "Scan", response: `{"Items":[` + oldMigrationJSON + `,` + bad + `]}`})
			case "backup":
				in.Backup = ""
				steps = nil
			case "confirmation":
				in.WritersStopped = false
				steps = nil
			default:
				t.Fatal(scenario)
			}
			if err := runMigration(t.Context(), migrationConfig(t, steps), in, io.Discard); err == nil {
				t.Fatal("guard did not reject migration")
			}
			if in.Backup != "" {
				if _, err := os.Stat(in.Backup); !os.IsNotExist(err) {
					t.Fatalf("backup written before validation: %v", err)
				}
			}
		})
	}
}

func TestMigrationPreviewAndIdempotency(t *testing.T) {
	for _, preview := range []bool{true, false} {
		t.Run(map[bool]string{true: "preview", false: "already-migrated"}[preview], func(t *testing.T) {
			in := migrationTestInput(t)
			in.Apply = !preview
			item := newMigrationJSON
			if preview {
				item = oldMigrationJSON
			}
			steps := append(migrationPreflight(), migrationStep{operation: "Scan", response: `{"Items":[` + item + `]}`})
			if err := runMigration(t.Context(), migrationConfig(t, steps), in, io.Discard); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(in.Backup); !os.IsNotExist(err) {
				t.Fatalf("preview/no-op created a backup: %v", err)
			}
		})
	}
}

func TestMigrationWritesAndReconciliation(t *testing.T) {
	for _, scenario := range []string{"success", "lost-response", "competing-migration", "unconfirmed-write", "changed-final", "existing-backup"} {
		t.Run(scenario, func(t *testing.T) {
			in := migrationTestInput(t)
			steps := append(migrationPreflight(), migrationStep{operation: "Scan", response: `{"Items":[` + oldMigrationJSON + `]}`})
			if scenario == "existing-backup" {
				if err := os.WriteFile(in.Backup, []byte("original backup"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				write := migrationStep{operation: "UpdateItem", response: `{"Attributes":` + newMigrationJSON + `}`, inspect: func(body []byte) {
					data, err := os.ReadFile(in.Backup)
					if err != nil {
						t.Fatal("write preceded backup", err)
					}
					var saved migrationBackup
					if err := json.Unmarshal(data, &saved); err != nil || len(saved.Items) != 1 {
						t.Fatal("invalid backup", err)
					}
					if !reflect.DeepEqual(migrationItem(t, string(saved.Items[0])), migrationItem(t, oldMigrationJSON)) {
						t.Fatal("backup did not preserve raw attributes")
					}
					stat, err := os.Stat(in.Backup)
					if err != nil || stat.Mode().Perm() != 0600 {
						t.Fatal("backup permissions", err)
					}
					var request struct{ ConditionExpression string }
					if err := json.Unmarshal(body, &request); err != nil || !strings.Contains(request.ConditionExpression, "#bundles = :bundles") {
						t.Fatal("SDK write was not conditional", err)
					}
				}}
				if scenario == "lost-response" || scenario == "unconfirmed-write" {
					write.status, write.response = 500, `{"__type":"InternalServerError","message":"lost"}`
				}
				if scenario == "competing-migration" {
					write.status, write.response = 400, `{"__type":"ConditionalCheckFailedException"}`
				}
				steps = append(steps, write)
				if scenario == "lost-response" || scenario == "competing-migration" {
					steps = append(steps, migrationStep{operation: "GetItem", response: `{"Item":` + newMigrationJSON + `}`})
				} else if scenario == "unconfirmed-write" {
					steps = append(steps, migrationStep{operation: "GetItem", response: `{"Item":` + oldMigrationJSON + `}`})
				}
				if scenario != "unconfirmed-write" {
					final := `{"Items":[` + newMigrationJSON + `]}`
					if scenario == "changed-final" {
						final = `{"Items":[]}`
					}
					steps = append(steps, migrationStep{operation: "Scan", response: final})
				}
			}
			err := runMigration(t.Context(), migrationConfig(t, steps), in, io.Discard)
			wantSuccess := scenario == "success" || scenario == "lost-response" || scenario == "competing-migration"
			if (err == nil) != wantSuccess {
				t.Fatalf("migration result: %v", err)
			}
			if scenario == "existing-backup" {
				data, err := os.ReadFile(in.Backup)
				if err != nil || string(data) != "original backup" {
					t.Fatal("existing backup overwritten", err)
				}
			}
		})
	}
}

func TestMigrationScanPagination(t *testing.T) {
	cursor := `{"id":{"S":"foreign/repo"}}`
	steps := []migrationStep{
		{operation: "Scan", response: `{"Items":[` + cursor + `],"LastEvaluatedKey":` + cursor + `}`},
		{operation: "Scan", response: `{"Items":[` + oldMigrationJSON + `]}`, inspect: func(body []byte) {
			if !strings.Contains(string(body), `"ExclusiveStartKey":`+cursor) || !strings.Contains(string(body), `"ConsistentRead":true`) {
				t.Fatalf("scan did not preserve cursor/consistency: %s", body)
			}
		}},
	}
	items, err := readMigrationSnapshot(t.Context(), dynamodb.NewFromConfig(migrationConfig(t, steps)), migrationTestInput(t))
	if err != nil || len(items) != 1 || items["bucket/repo"] == nil {
		t.Fatalf("wrong selected records: %v %v", items, err)
	}
}

func TestMigrationCLIRequiresExplicitTarget(t *testing.T) {
	for _, args := range [][]string{nil, {"--apply"}, {"unexpected"}, {"--account", "account", "--region", "region", "--table", "table", "--bucket", "bucket", "--id", "other/repo"}} {
		if err := migrateDynamolock(args, io.Discard); err == nil {
			t.Fatalf("invalid arguments accepted: %v", args)
		}
	}
}

type migrationFailWriter struct{}

func (migrationFailWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestMigrationOutputFailureStopsBeforeWrites(t *testing.T) {
	in := migrationTestInput(t)
	steps := append(migrationPreflight(), migrationStep{operation: "Scan", response: `{"Items":[` + oldMigrationJSON + `]}`})
	err := runMigration(t.Context(), migrationConfig(t, steps), in, migrationFailWriter{})
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("output failure was ignored: %v", err)
	}
	if _, err := os.Stat(in.Backup); !os.IsNotExist(err) {
		t.Fatalf("backup created after output failed: %v", err)
	}
}

func TestMigrationExactIDNeverScans(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing", true: "missing"}[missing], func(t *testing.T) {
			in := migrationTestInput(t)
			in.ID = "bucket/repo"
			response := `{"Item":` + oldMigrationJSON + `}`
			if missing {
				response = `{}`
			}
			read := migrationStep{operation: "GetItem", response: response, inspect: func(body []byte) {
				var request struct {
					ConsistentRead bool
					Key            json.RawMessage
				}
				if err := json.Unmarshal(body, &request); err != nil || !request.ConsistentRead || string(request.Key) != `{"id":{"S":"bucket/repo"}}` {
					t.Fatalf("not a strongly consistent keyed read: %s (%v)", body, err)
				}
			}}
			steps := append(migrationPreflight(), read)
			if !missing {
				steps = append(steps, migrationStep{operation: "UpdateItem", response: `{"Attributes":` + newMigrationJSON + `}`})
				read.response = `{"Item":` + newMigrationJSON + `}`
				steps = append(steps, read)
			}
			err := runMigration(t.Context(), migrationConfig(t, steps), in, io.Discard)
			if (err != nil) != missing {
				t.Fatalf("exact-ID migration: %v", err)
			}
		})
	}
}
