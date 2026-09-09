package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/nathants/go-dynamolock"
	"github.com/nathants/libaws/lib"
)

// Use the exact migration CLI used for production, including its backup and
// account guard. It must be idempotent, without recreating an existing backup.
func migrateTestRepository(t *testing.T, table, bucket, prefix string) {
	t.Helper()
	args := []string{"--migrate-dynamolock",
		"--account", os.Getenv("GIT_REMOTE_AWS_TEST_ACCOUNT"), "--region", lib.Region(),
		"--table", table, "--bucket", bucket, "--id", bucket + "/" + prefix,
		"--backup", filepath.Join(t.TempDir(), "metadata.json"), "--apply", "--writers-stopped"}
	for range 2 {
		cmd := exec.CommandContext(t.Context(), "git-remote-aws", args...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("migrate repository: %v\n%s", err, output)
		}
	}
}

func TestDynamolockMigration(t *testing.T) {
	table, bucket, prefix := getTestBucketAndTable(t)
	defer cleanupAws(table, bucket, prefix)
	client := lib.DynamoDBClient()
	id := bucket + "/" + prefix
	old := map[string]types.AttributeValue{
		"id":      &types.AttributeValueMemberS{Value: id},
		"uid":     &types.AttributeValueMemberS{Value: ""},
		"unix":    &types.AttributeValueMemberN{Value: "0"},
		"branch":  &types.AttributeValueMemberS{Value: "master"},
		"bundles": &types.AttributeValueMemberS{Value: prefix + "/bundles_" + strings.Repeat("a", 40)},
	}
	_, err := client.PutItem(t.Context(), &dynamodb.PutItemInput{
		TableName: aws.String(table), Item: old,
		ConditionExpression: aws.String("attribute_not_exists(id)"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dynamolock.Read[RepoMeta](t.Context(), client, table, id); !errors.Is(err, dynamolock.ErrInvalidRecord) {
		t.Fatalf("legacy record unexpectedly readable: %v", err)
	}
	migrateTestRepository(t, table, bucket, prefix)
	want := &RepoMeta{Branch: "master", BundlesS3Key: prefix + "/bundles_" + strings.Repeat("a", 40)}
	got, err := dynamolock.Read[RepoMeta](t.Context(), client, table, id)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("migrated metadata = %+v, %v; want %+v", got, err, want)
	}
	lease, got, err := dynamolock.Lock[RepoMeta](t.Context(), client, &dynamolock.LockInput{
		Table: table, ID: id, RequireExisting: true,
		HeartbeatMaxAge: 10 * time.Second, HeartbeatInterval: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := lease.Release(ctx); err != nil {
			t.Error(err)
		}
	}()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("acquisition changed migrated metadata: %+v", got)
	}
	got.BundlesS3Key = prefix + "/bundles_" + strings.Repeat("b", 40)
	if err := lease.Commit(lease.Context(), got); err != nil {
		t.Fatal(err)
	}
	item, err := client.GetItem(t.Context(), &dynamodb.GetItemInput{
		TableName: aws.String(table), Key: map[string]types.AttributeValue{"id": old["id"]},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(item.Item) != 2 || item.Item["data"] == nil || item.Item["id"] == nil {
		t.Fatalf("committed record still has flat fields or ownership: %v", item.Item)
	}
	actual, err := dynamolock.UnmarshalItem[RepoMeta](item.Item)
	if err != nil || !reflect.DeepEqual(actual, got) {
		t.Fatalf("committed record = %+v, %v; want %+v", actual, err, got)
	}
}

// Submit the real migration update after a competing writer changes the item.
// DynamoDB, not a local condition evaluator, must reject every stale preimage.
func TestDynamolockMigrationRejectsStalePreimage(t *testing.T) {
	table, bucket, prefix := getTestBucketAndTable(t)
	defer cleanupAws(table, bucket, prefix)
	client := lib.DynamoDBClient()
	for _, scenario := range []string{"payload", "legacy-owner", "lease-owner", "deleted", "migrated"} {
		t.Run(scenario, func(t *testing.T) {
			before := migrationItem(t, oldMigrationJSON)
			before["id"] = &types.AttributeValueMemberS{Value: bucket + "/" + prefix}
			key := map[string]types.AttributeValue{"id": before["id"]}
			if _, err := client.PutItem(t.Context(), &dynamodb.PutItemInput{TableName: aws.String(table), Item: before}); err != nil {
				t.Fatal(err)
			}
			after, err := migratedRepoItem(before)
			if err != nil {
				t.Fatal(err)
			}
			stale := migrationUpdate(table, before, after)
			switch scenario {
			case "deleted":
				_, err = client.DeleteItem(t.Context(), &dynamodb.DeleteItemInput{TableName: aws.String(table), Key: key})
			case "migrated":
				_, err = client.UpdateItem(t.Context(), migrationUpdate(table, before, after))
			default:
				update := "SET #field = :value"
				field := "bundles"
				values := map[string]types.AttributeValue{":value": &types.AttributeValueMemberS{Value: "competing state"}}
				names := map[string]string{"#field": field}
				if scenario == "legacy-owner" {
					names["#field"] = "uid"
					update += ", #heartbeat = :heartbeat"
					names["#heartbeat"] = "unix"
					values[":heartbeat"] = &types.AttributeValueMemberN{Value: "123"}
				} else if scenario == "lease-owner" {
					names["#field"] = "owner_token"
					update += ", #heartbeat = :heartbeat"
					names["#heartbeat"] = "expires_at"
					values[":heartbeat"] = &types.AttributeValueMemberN{Value: "9223372036854775807"}
				}
				_, err = client.UpdateItem(t.Context(), &dynamodb.UpdateItemInput{TableName: aws.String(table), Key: key, UpdateExpression: aws.String(update), ExpressionAttributeNames: names, ExpressionAttributeValues: values})
			}
			if err != nil {
				t.Fatal(err)
			}
			read := func() map[string]types.AttributeValue {
				t.Helper()
				out, err := client.GetItem(t.Context(), &dynamodb.GetItemInput{TableName: aws.String(table), Key: key, ConsistentRead: aws.Bool(true)})
				if err != nil {
					t.Fatal(err)
				}
				return out.Item
			}
			competing := read()
			_, err = client.UpdateItem(t.Context(), stale)
			var rejected *types.ConditionalCheckFailedException
			if !errors.As(err, &rejected) {
				t.Fatalf("DynamoDB accepted stale %s migration: %v", scenario, err)
			}
			if actual := read(); !reflect.DeepEqual(actual, competing) {
				t.Fatalf("rejected migration changed competing %s state: before=%v after=%v", scenario, competing, actual)
			}
		})
	}
}
