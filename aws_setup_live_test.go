package main

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Every live test's resources come from the helper's ensure=y setup. Verify
// the configuration that AWS actually applied to a fresh bucket and table.
func TestEnsureSetupAWS(t *testing.T) {
	table, bucket, _ := getTestBucketAndTable(t)
	ctx := t.Context()
	clients := testAWSClients()
	name, owner := aws.String(bucket), aws.String(os.Getenv("GIT_REMOTE_AWS_TEST_ACCOUNT"))
	asJSON := func(value any) string {
		data, _ := json.Marshal(value)
		return string(data)
	}
	setupTag := func(key, value *string) bool {
		return aws.ToString(key) == "libaws.infraset" && aws.ToString(value) == ""
	}

	head, err := clients.s3.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: name, ExpectedBucketOwner: owner})
	if err != nil {
		t.Fatal(err)
	}
	if region := clients.s3.Options().Region; aws.ToString(head.BucketRegion) != region {
		t.Fatalf("bucket region %q, want %q", aws.ToString(head.BucketRegion), region)
	}
	block, err := clients.s3.GetPublicAccessBlock(ctx, &s3.GetPublicAccessBlockInput{Bucket: name, ExpectedBucketOwner: owner})
	if err != nil {
		t.Fatal(err)
	}
	if c := block.PublicAccessBlockConfiguration; !aws.ToBool(c.BlockPublicAcls) || !aws.ToBool(c.IgnorePublicAcls) ||
		!aws.ToBool(c.BlockPublicPolicy) || !aws.ToBool(c.RestrictPublicBuckets) {
		t.Fatalf("public access is not fully blocked: %s", asJSON(c))
	}
	encryption, err := clients.s3.GetBucketEncryption(ctx, &s3.GetBucketEncryptionInput{Bucket: name, ExpectedBucketOwner: owner})
	if err != nil {
		t.Fatal(err)
	}
	if rules := encryption.ServerSideEncryptionConfiguration.Rules; len(rules) != 1 || rules[0].ApplyServerSideEncryptionByDefault == nil ||
		rules[0].ApplyServerSideEncryptionByDefault.SSEAlgorithm != s3types.ServerSideEncryptionAes256 || rules[0].BlockedEncryptionTypes == nil ||
		!slices.Equal(rules[0].BlockedEncryptionTypes.EncryptionType, []s3types.EncryptionType{s3types.EncryptionTypeSseC}) {
		t.Fatalf("bucket encryption is not SSE-S3 with SSE-C blocked: %s", asJSON(rules))
	}
	versioning, err := clients.s3.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: name, ExpectedBucketOwner: owner})
	if err != nil {
		t.Fatal(err)
	}
	if versioning.Status != s3types.BucketVersioningStatusEnabled {
		t.Fatalf("bucket versioning is %q", versioning.Status)
	}
	policy, err := clients.s3.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: name, ExpectedBucketOwner: owner})
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Statement []struct {
			Effect, Principal, Action string
			Resource                  []string
			Condition                 map[string]map[string]string
		}
	}
	if err := json.Unmarshal([]byte(aws.ToString(policy.Policy)), &document); err != nil {
		t.Fatal(err)
	}
	if s := document.Statement; len(s) != 1 || s[0].Effect != "Deny" || s[0].Principal != "*" || s[0].Action != "s3:*" ||
		s[0].Condition["Bool"]["aws:SecureTransport"] != "false" || len(s[0].Resource) != 2 ||
		!strings.HasSuffix(s[0].Resource[0], ":s3:::"+bucket) || s[0].Resource[1] != s[0].Resource[0]+"/*" {
		t.Fatalf("bucket policy does not deny insecure transport to the whole bucket: %s", aws.ToString(policy.Policy))
	}
	bucketTags, err := clients.s3.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: name, ExpectedBucketOwner: owner})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(bucketTags.TagSet, func(tag s3types.Tag) bool { return setupTag(tag.Key, tag.Value) }) {
		t.Fatalf("bucket lacks the setup tag: %s", asJSON(bucketTags.TagSet))
	}

	described, err := clients.dynamodb.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(table)})
	if err != nil {
		t.Fatal(err)
	}
	if d := described.Table; d.TableStatus != ddbtypes.TableStatusActive || d.BillingModeSummary == nil ||
		d.BillingModeSummary.BillingMode != ddbtypes.BillingModePayPerRequest || len(d.KeySchema) != 1 ||
		aws.ToString(d.KeySchema[0].AttributeName) != "id" || d.KeySchema[0].KeyType != ddbtypes.KeyTypeHash ||
		len(d.AttributeDefinitions) != 1 || d.AttributeDefinitions[0].AttributeType != ddbtypes.ScalarAttributeTypeS ||
		d.StreamSpecification != nil && aws.ToBool(d.StreamSpecification.StreamEnabled) {
		t.Fatalf("table is not on-demand, keyed by string id, and stream-free: %s", asJSON(d))
	}
	ttl, err := clients.dynamodb.DescribeTimeToLive(ctx, &dynamodb.DescribeTimeToLiveInput{TableName: aws.String(table)})
	if err != nil {
		t.Fatal(err)
	}
	if status := ttl.TimeToLiveDescription.TimeToLiveStatus; status != ddbtypes.TimeToLiveStatusDisabled {
		t.Fatalf("table TTL is %q", status)
	}
	tableTags, err := clients.dynamodb.ListTagsOfResource(ctx, &dynamodb.ListTagsOfResourceInput{ResourceArn: described.Table.TableArn})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(tableTags.Tags, func(tag ddbtypes.Tag) bool { return setupTag(tag.Key, tag.Value) }) {
		t.Fatalf("table lacks the setup tag: %s", asJSON(tableTags.Tags))
	}
}
