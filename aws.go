package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

type awsClients struct {
	s3       *s3.Client
	dynamodb *dynamodb.Client
	sts      *sts.Client
}

func newAWSClients(ctx context.Context) (*awsClients, error) {
	// SDK v2 always loads shared config and uses regional STS endpoints. Keep
	// the previous five-attempt policy without mutating the process environment.
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRetryMaxAttempts(5))
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration: %w", err)
	}
	return &awsClients{s3: s3.NewFromConfig(cfg), dynamodb: dynamodb.NewFromConfig(cfg), sts: sts.NewFromConfig(cfg)}, nil
}

func (clients *awsClients) ensureResources(ctx context.Context, bucket, table string, ensure bool) error {
	if err := clients.ensureBucket(ctx, bucket, ensure); err != nil {
		return err
	}
	return clients.ensureTable(ctx, table, ensure)
}

func (clients *awsClients) bucketExists(ctx context.Context, bucket string) (bool, error) {
	probe, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := clients.s3.HeadBucket(probe, &s3.HeadBucketInput{Bucket: aws.String(bucket)}, func(options *s3.Options) {
		// Preserve the anonymous region probe: an existing private bucket can
		// return 403 plus its region without granting bucket-list permission.
		options.Credentials = nil
		options.UsePathStyle = true
	})
	if err == nil || out != nil && aws.ToString(out.BucketRegion) != "" {
		return true, nil
	}
	var response *smithyhttp.ResponseError
	if errors.As(err, &response) && response.HTTPResponse() != nil && response.HTTPResponse().Header.Get("X-Amz-Bucket-Region") != "" {
		return true, nil
	}
	// HeadBucket has no XML error body. Its exact 404 outcome denotes absence;
	// other statuses and transport errors never authorize resource creation.
	if errors.As(err, &response) && response.HTTPStatusCode() == http.StatusNotFound {
		return false, nil
	}
	return false, fmt.Errorf("inspect S3 bucket %q: %w", bucket, err)
}

func (clients *awsClients) ensureBucket(ctx context.Context, bucket string, ensure bool) error {
	exists, err := clients.bucketExists(ctx, bucket)
	if err != nil || exists {
		return err
	}
	if !ensure {
		return fmt.Errorf("S3 bucket %q does not exist; provision it or use ensure=y with setup permissions", bucket)
	}
	identity, err := clients.sts.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fmt.Errorf("identify bucket creator: %w", err)
	}
	caller, err := arn.Parse(aws.ToString(identity.Arn))
	if err != nil || caller.Partition == "" || aws.ToString(identity.Account) == "" {
		return fmt.Errorf("STS returned an invalid bucket-creator identity")
	}
	input := &s3.CreateBucketInput{Bucket: aws.String(bucket)}
	region := clients.s3.Options().Region
	if region != "us-east-1" {
		input.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{LocationConstraint: s3types.BucketLocationConstraint(region)}
	}
	fmt.Fprintln(os.Stderr, "creating private s3 bucket:", bucket)
	// Retrying CreateBucket in us-east-1 can reset an existing bucket's ACL.
	// An uncertain result is an error, not permission to repeat the mutation.
	_, err = clients.s3.CreateBucket(ctx, input, func(options *s3.Options) {
		options.Retryer = aws.NopRetryer{}
		options.RetryMaxAttempts = 1
	})
	var owned *s3types.BucketAlreadyOwnedByYou
	if errors.As(err, &owned) {
		return nil // Another creator won; never converge its existing configuration.
	}
	if err != nil {
		return fmt.Errorf("create S3 bucket %q: %w", bucket, err)
	}
	if err := clients.configureNewBucket(ctx, bucket, identity.Account, caller.Partition); err != nil {
		return fmt.Errorf("S3 bucket %q was created but setup is incomplete; review its configuration before use: %w", bucket, err)
	}
	fmt.Fprintln(os.Stderr, "created private s3 bucket:", bucket)
	return nil
}

func (clients *awsClients) configureNewBucket(ctx context.Context, bucket string, account *string, partition string) error {
	waiter := s3.NewBucketExistsWaiter(clients.s3, func(options *s3.BucketExistsWaiterOptions) {
		options.MinDelay, options.MaxDelay = 2*time.Second, 2*time.Second
	})
	if err := waiter.Wait(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket), ExpectedBucketOwner: account}, 2*time.Minute); err != nil {
		return fmt.Errorf("wait for new S3 bucket %q: %w", bucket, err)
	}
	_, err := clients.s3.PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{
		Bucket: aws.String(bucket), ExpectedBucketOwner: account,
		PublicAccessBlockConfiguration: &s3types.PublicAccessBlockConfiguration{
			BlockPublicAcls: aws.Bool(true), IgnorePublicAcls: aws.Bool(true),
			BlockPublicPolicy: aws.Bool(true), RestrictPublicBuckets: aws.Bool(true),
		},
	})
	if err != nil {
		return err
	}
	_, err = clients.s3.PutBucketEncryption(ctx, &s3.PutBucketEncryptionInput{
		Bucket: aws.String(bucket), ExpectedBucketOwner: account,
		ServerSideEncryptionConfiguration: &s3types.ServerSideEncryptionConfiguration{
			Rules: []s3types.ServerSideEncryptionRule{{
				ApplyServerSideEncryptionByDefault: &s3types.ServerSideEncryptionByDefault{SSEAlgorithm: s3types.ServerSideEncryptionAes256},
				BlockedEncryptionTypes:             &s3types.BlockedEncryptionTypes{EncryptionType: []s3types.EncryptionType{s3types.EncryptionTypeSseC}},
				BucketKeyEnabled:                   aws.Bool(false),
			}},
		},
	})
	if err != nil {
		return err
	}
	bucketARN := "arn:" + partition + ":s3:::" + bucket
	policy, err := json.Marshal(map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{{
			"Sid": "DenyInsecureTransport", "Effect": "Deny", "Principal": "*", "Action": "s3:*",
			"Resource":  []string{bucketARN, bucketARN + "/*"},
			"Condition": map[string]any{"Bool": map[string]string{"aws:SecureTransport": "false"}},
		}},
	})
	if err != nil {
		return err
	}
	_, err = clients.s3.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: aws.String(bucket), ExpectedBucketOwner: account, Policy: aws.String(string(policy))})
	if err != nil {
		return err
	}
	// Keep the setup tag used by existing deployments, without importing the
	// provisioning library or treating its tags as authority to alter a bucket.
	_, err = clients.s3.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{
		Bucket: aws.String(bucket), ExpectedBucketOwner: account,
		Tagging: &s3types.Tagging{TagSet: []s3types.Tag{{Key: aws.String("libaws.infraset"), Value: aws.String("")}}},
	})
	if err != nil {
		return err
	}
	return clients.enableBucketVersioning(ctx, bucket, account)
}

func (clients *awsClients) ensureTable(ctx context.Context, table string, ensure bool) error {
	_, err := clients.dynamodb.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(table)})
	if err == nil {
		return nil // Existing schema, billing, TTL, tags and records remain untouched.
	}
	var missing *ddbtypes.ResourceNotFoundException
	if !errors.As(err, &missing) {
		return fmt.Errorf("inspect DynamoDB table %q: %w", table, err)
	}
	if !ensure {
		return fmt.Errorf("DynamoDB table %q does not exist; provision it or use ensure=y with setup permissions", table)
	}
	fmt.Fprintln(os.Stderr, "creating private dynamodb table:", table)
	_, err = clients.dynamodb.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(table), BillingMode: ddbtypes.BillingModePayPerRequest,
		KeySchema:            []ddbtypes.KeySchemaElement{{AttributeName: aws.String("id"), KeyType: ddbtypes.KeyTypeHash}},
		AttributeDefinitions: []ddbtypes.AttributeDefinition{{AttributeName: aws.String("id"), AttributeType: ddbtypes.ScalarAttributeTypeS}},
		Tags:                 []ddbtypes.Tag{{Key: aws.String("libaws.infraset"), Value: aws.String("")}},
	})
	var inUse *ddbtypes.ResourceInUseException
	if err != nil && !errors.As(err, &inUse) {
		return fmt.Errorf("create DynamoDB table %q: %w", table, err)
	}
	if err := clients.waitForTable(ctx, table); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "created private dynamodb table:", table)
	return nil
}

func (clients *awsClients) waitForTable(ctx context.Context, table string) error {
	waiter := dynamodb.NewTableExistsWaiter(clients.dynamodb, func(options *dynamodb.TableExistsWaiterOptions) {
		options.MinDelay, options.MaxDelay = 2*time.Second, 2*time.Second
	})
	if err := waiter.Wait(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(table)}, 5*time.Minute); err != nil {
		return fmt.Errorf("wait for DynamoDB table %q: %w", table, err)
	}
	return nil
}
