package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/nathants/go-dynamolock"
)

type migrationInput struct {
	Account, Region, Table, Bucket, ID, Backup string
	Apply, WritersStopped                      bool
}

func (in migrationInput) validate() error {
	if in.Account == "" || in.Region == "" || in.Table == "" || in.Bucket == "" {
		return errors.New("--account, --region, --table, and --bucket are required")
	}
	if in.Apply && (!in.WritersStopped || in.Backup == "") {
		return errors.New("--apply requires --writers-stopped and a new --backup path")
	}
	if in.ID != "" && !strings.HasPrefix(in.ID, in.Bucket+"/") {
		return errors.New("--id must belong to --bucket")
	}
	return nil
}

func migrateDynamolock(args []string, output io.Writer) error {
	var in migrationInput
	flags := flag.NewFlagSet("--migrate-dynamolock", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&in.Account, "account", "", "expected AWS account ID")
	flags.StringVar(&in.Region, "region", "", "AWS region")
	flags.StringVar(&in.Table, "table", "", "repository DynamoDB table")
	flags.StringVar(&in.Bucket, "bucket", "", "migrate only this bucket's repository records")
	flags.StringVar(&in.ID, "id", "", "optional exact BUCKET/REPOSITORY item")
	flags.StringVar(&in.Backup, "backup", "", "new private file for original DynamoDB JSON records")
	flags.BoolVar(&in.Apply, "apply", false, "write changes; default is read-only")
	flags.BoolVar(&in.WritersStopped, "writers-stopped", false, "confirm ALL old/new writers are stopped")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional migration arguments")
	}
	if err := in.validate(); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(in.Region))
	if err != nil {
		return err
	}
	return runMigration(ctx, cfg, in, output)
}

var legacyRepoFields = []string{"branch", "bundles", "uid", "unix"}

// Preserve the exact raw payload, including absent versus explicitly empty data.
// Unknown fields and uncleared ownership require manual reconciliation, not loss.
func migratedRepoItem(item map[string]types.AttributeValue) (map[string]types.AttributeValue, error) {
	id, ok := item["id"].(*types.AttributeValueMemberS)
	if !ok || id.Value == "" {
		return nil, errors.New("item must have a nonempty string id")
	}
	legacy := false
	for _, name := range legacyRepoFields {
		legacy = legacy || item[name] != nil
	}
	for name := range item {
		if name == "id" || (legacy && slices.Contains(legacyRepoFields, name)) || (!legacy && name == "data") {
			continue
		}
		return nil, fmt.Errorf("%s: unknown attributes, mixed schemas, or uncleared ownership", id.Value)
	}
	var data map[string]types.AttributeValue
	if legacy {
		switch uid := item["uid"].(type) {
		case nil:
		case *types.AttributeValueMemberS:
			if uid.Value != "" {
				return nil, fmt.Errorf("%s: legacy lock is held", id.Value)
			}
		case *types.AttributeValueMemberNULL:
			if !uid.Value {
				return nil, fmt.Errorf("%s: invalid legacy owner", id.Value)
			}
		default:
			return nil, fmt.Errorf("%s: invalid legacy owner", id.Value)
		}
		if unix, exists := item["unix"]; exists {
			value, ok := unix.(*types.AttributeValueMemberN)
			if !ok || value.Value != "0" {
				return nil, fmt.Errorf("%s: legacy heartbeat is not cleared", id.Value)
			}
		}
		for _, name := range []string{"branch", "bundles"} {
			if value, ok := item[name]; ok {
				if data == nil {
					data = make(map[string]types.AttributeValue)
				}
				data[name] = value
			}
		}
	} else if value, exists := item["data"]; exists {
		m, ok := value.(*types.AttributeValueMemberM)
		if !ok {
			return nil, fmt.Errorf("%s: data must be a map", id.Value)
		}
		data = m.Value
		if data == nil {
			data = map[string]types.AttributeValue{}
		}
	}
	for name, value := range data {
		if _, ok := value.(*types.AttributeValueMemberS); !ok || (name != "branch" && name != "bundles") {
			return nil, fmt.Errorf("%s: unknown or non-string repository payload field %s", id.Value, name)
		}
	}
	result := map[string]types.AttributeValue{"id": id}
	if data != nil {
		result["data"] = &types.AttributeValueMemberM{Value: data}
	}
	if _, err := dynamolock.UnmarshalItem[RepoMeta](result); err != nil {
		return nil, err
	}
	return result, nil
}

func migrationCall[T any](ctx context.Context, request func(context.Context) (T, error)) (T, error) {
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return request(bounded)
}

func scanMigration(ctx context.Context, client *dynamodb.Client, in migrationInput) (map[string]map[string]types.AttributeValue, error) {
	items := make(map[string]map[string]types.AttributeValue)
	input := &dynamodb.ScanInput{TableName: aws.String(in.Table), ConsistentRead: aws.Bool(true)}
	seen := make(map[string]bool)
	for {
		page, err := migrationCall(ctx, func(ctx context.Context) (*dynamodb.ScanOutput, error) {
			return client.Scan(ctx, input)
		})
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			id, ok := item["id"].(*types.AttributeValueMemberS)
			if !ok || id.Value == "" {
				return nil, errors.New("table contains an item without a string id")
			}
			if !strings.HasPrefix(id.Value, in.Bucket+"/") || (in.ID != "" && id.Value != in.ID) {
				continue
			}
			if _, exists := items[id.Value]; exists {
				return nil, fmt.Errorf("duplicate item during scan: %s", id.Value)
			}
			items[id.Value] = item
		}
		if len(page.LastEvaluatedKey) == 0 {
			break
		}
		marker, err := attributevalue.MarshalMapJSON(page.LastEvaluatedKey)
		if err != nil {
			return nil, err
		}
		if seen[string(marker)] {
			return nil, errors.New("scan cursor repeated")
		}
		seen[string(marker)] = true
		input.ExclusiveStartKey = page.LastEvaluatedKey
	}
	if in.ID != "" && items[in.ID] == nil {
		return nil, fmt.Errorf("requested item not found: %s", in.ID)
	}
	return items, nil
}

func migrationUpdate(table string, before, after map[string]types.AttributeValue) *dynamodb.UpdateItemInput {
	names := map[string]string{"#id": "id", "#data": "data"}
	values := make(map[string]types.AttributeValue)
	conditions := []string{"attribute_exists(#id)", "attribute_not_exists(#data)"}
	for _, name := range []string{"branch", "bundles", "uid", "unix", "owner_token", "expires_at"} {
		alias := "#" + name
		names[alias] = name
		if value, exists := before[name]; exists {
			values[":"+name] = value
			conditions = append(conditions, alias+" = :"+name)
		} else {
			conditions = append(conditions, "attribute_not_exists("+alias+")")
		}
	}
	update := "REMOVE #branch, #bundles, #uid, #unix"
	if data, exists := after["data"]; exists {
		values[":data"] = data
		update = "SET #data = :data " + update
	}
	return &dynamodb.UpdateItemInput{
		TableName: aws.String(table), Key: map[string]types.AttributeValue{"id": before["id"]},
		ConditionExpression: aws.String(strings.Join(conditions, " AND ")), UpdateExpression: aws.String(update),
		ExpressionAttributeNames: names, ExpressionAttributeValues: values, ReturnValues: types.ReturnValueAllNew,
	}
}

type migrationBackup struct {
	Account, Region, TableARN, Bucket string
	Items                             []json.RawMessage
}

func saveMigrationBackup(filename string, backup migrationBackup) error {
	encoded, err := json.MarshalIndent(backup, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	f, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(encoded); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(filename))
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	if err := directory.Sync(); err != nil {
		return err
	}
	actual, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	if !bytes.Equal(actual, encoded) {
		return errors.New("backup verification failed")
	}
	return nil
}

func runMigration(ctx context.Context, cfg aws.Config, in migrationInput, output io.Writer) error {
	if err := in.validate(); err != nil {
		return err
	}
	// Never retry an ambiguous migration write. Reconcile it with a strong read.
	cfg.Region, cfg.Retryer = in.Region, func() aws.Retryer { return aws.NopRetryer{} }
	identity, err := migrationCall(ctx, func(ctx context.Context) (*sts.GetCallerIdentityOutput, error) {
		return sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	})
	if err != nil {
		return err
	}
	if aws.ToString(identity.Account) != in.Account {
		return fmt.Errorf("wrong AWS account: %s, expected %s", aws.ToString(identity.Account), in.Account)
	}
	client := dynamodb.NewFromConfig(cfg)
	description, err := migrationCall(ctx, func(ctx context.Context) (*dynamodb.DescribeTableOutput, error) {
		return client.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(in.Table)})
	})
	if err != nil {
		return err
	}
	table := description.Table
	if table == nil || table.TableStatus != types.TableStatusActive || len(table.KeySchema) != 1 ||
		aws.ToString(table.KeySchema[0].AttributeName) != "id" || table.KeySchema[0].KeyType != types.KeyTypeHash {
		return errors.New("table must be ACTIVE with only an id partition key")
	}
	stringID := false
	for _, attr := range table.AttributeDefinitions {
		stringID = stringID || (aws.ToString(attr.AttributeName) == "id" && attr.AttributeType == types.ScalarAttributeTypeS)
	}
	if !stringID {
		return errors.New("table must have a string id partition key")
	}
	ttl, err := migrationCall(ctx, func(ctx context.Context) (*dynamodb.DescribeTimeToLiveOutput, error) {
		return client.DescribeTimeToLive(ctx, &dynamodb.DescribeTimeToLiveInput{TableName: aws.String(in.Table)})
	})
	if err != nil {
		return err
	}
	if ttl.TimeToLiveDescription == nil || ttl.TimeToLiveDescription.TimeToLiveStatus != types.TimeToLiveStatusDisabled {
		return errors.New("repository table TTL must be DISABLED; lease expiry is not deletion")
	}
	before, err := scanMigration(ctx, client, in)
	if err != nil {
		return err
	}
	after := make(map[string]map[string]types.AttributeValue)
	var ids, changed []string
	for id, item := range before {
		next, err := migratedRepoItem(item)
		if err != nil {
			return err
		}
		after[id] = next
		ids = append(ids, id)
		if !reflect.DeepEqual(item, next) {
			changed = append(changed, id)
		}
	}
	slices.Sort(ids)
	slices.Sort(changed)
	if _, err := fmt.Fprintf(output, "%s: %d selected, %d need migration\n", aws.ToString(table.TableArn), len(ids), len(changed)); err != nil {
		return err
	}
	if !in.Apply || len(changed) == 0 {
		return nil
	}
	backup := migrationBackup{Account: in.Account, Region: in.Region, TableARN: aws.ToString(table.TableArn), Bucket: in.Bucket}
	for _, id := range ids {
		encoded, err := attributevalue.MarshalMapJSON(before[id])
		if err != nil {
			return err
		}
		backup.Items = append(backup.Items, encoded)
	}
	if err := saveMigrationBackup(in.Backup, backup); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(output, "saved pre-migration records:", in.Backup); err != nil {
		return err
	}
	for _, id := range changed {
		request := migrationUpdate(in.Table, before[id], after[id])
		result, err := migrationCall(ctx, func(ctx context.Context) (*dynamodb.UpdateItemOutput, error) {
			return client.UpdateItem(ctx, request)
		})
		var actual map[string]types.AttributeValue
		if err != nil {
			read, readErr := migrationCall(ctx, func(ctx context.Context) (*dynamodb.GetItemOutput, error) {
				return client.GetItem(ctx, &dynamodb.GetItemInput{TableName: request.TableName, Key: request.Key, ConsistentRead: aws.Bool(true)})
			})
			if readErr != nil || !reflect.DeepEqual(read.Item, after[id]) {
				return fmt.Errorf("%s: migration write failed; keep writers stopped: %w", id, errors.Join(err, readErr))
			}
			actual = read.Item
		} else {
			actual = result.Attributes
		}
		if !reflect.DeepEqual(actual, after[id]) {
			return fmt.Errorf("%s: post-write verification failed; keep writers stopped and inspect backup", id)
		}
		if _, err := fmt.Fprintln(output, "migrated", id); err != nil {
			return err
		}
	}
	actual, err := scanMigration(ctx, client, in)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, after) {
		return errors.New("final snapshot differs; keep writers stopped and inspect backup")
	}
	_, err = fmt.Fprintf(output, "verified %d records; S3 objects were not modified\n", len(after))
	return err
}
