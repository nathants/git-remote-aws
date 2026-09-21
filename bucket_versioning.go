package main

import (
	"context"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Versioning is bucket-wide. It is the only existing-bucket setting push may
// change; reads never call this preflight. No object versions are read or deleted.
func (clients *awsClients) ensureBucketVersioning(ctx context.Context, bucket string) error {
	out, err := clients.s3.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(bucket)})
	if err != nil {
		return fmt.Errorf("inspect S3 bucket %q versioning (requires s3:GetBucketVersioning): %w", bucket, err)
	}
	switch out.Status {
	case s3types.BucketVersioningStatusEnabled:
		return nil
	case "", s3types.BucketVersioningStatusSuspended:
		return clients.enableBucketVersioning(ctx, bucket, nil)
	default:
		return fmt.Errorf("S3 bucket %q has unknown versioning status %q", bucket, out.Status)
	}
}

func (clients *awsClients) enableBucketVersioning(ctx context.Context, bucket string, owner *string) error {
	_, err := clients.s3.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: aws.String(bucket), ExpectedBucketOwner: owner,
		VersioningConfiguration: &s3types.VersioningConfiguration{Status: s3types.BucketVersioningStatusEnabled},
	})
	if err != nil {
		return fmt.Errorf("enable S3 bucket %q versioning (requires s3:PutBucketVersioning): %w", bucket, err)
	}
	fmt.Fprintf(os.Stderr, "enabled versioning on s3://%s\n", bucket)
	return nil
}
