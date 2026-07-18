module github.com/nathants/git-remote-aws

go 1.26

require (
	github.com/aws/aws-sdk-go-v2 v1.42.1
	github.com/aws/aws-sdk-go-v2/service/dynamodb v1.59.2
	github.com/aws/aws-sdk-go-v2/service/s3 v1.104.2
	github.com/gofrs/uuid/v5 v5.4.0
	github.com/nathants/go-dynamolock v0.0.0-20260717094340-75d92caa2db1
	github.com/nathants/go-libsodium v0.0.0-20260502104057-4e1a79aae4f3
	github.com/nathants/libaws v0.0.0-20260717093841-e394b53f4d4b
)

require (
	github.com/avast/retry-go v3.0.0+incompatible // indirect
	github.com/aws/aws-lambda-go v1.54.0 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.14 // indirect
	github.com/aws/aws-sdk-go-v2/config v1.32.27 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.19.26 // indirect
	github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue v1.20.50 // indirect
	github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression v1.8.50 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.18.30 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.30 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.30 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.4.31 // indirect
	github.com/aws/aws-sdk-go-v2/service/acm v1.41.1 // indirect
	github.com/aws/aws-sdk-go-v2/service/apigatewaymanagementapi v1.30.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/apigatewayv2 v1.35.8 // indirect
	github.com/aws/aws-sdk-go-v2/service/cloudwatch v1.61.1 // indirect
	github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs v1.78.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/codecommit v1.34.6 // indirect
	github.com/aws/aws-sdk-go-v2/service/costexplorer v1.65.3 // indirect
	github.com/aws/aws-sdk-go-v2/service/dynamodbstreams v1.34.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/ec2 v1.311.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/ecr v1.58.6 // indirect
	github.com/aws/aws-sdk-go-v2/service/ecs v1.86.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/eventbridge v1.46.8 // indirect
	github.com/aws/aws-sdk-go-v2/service/iam v1.54.7 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.13 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.9.23 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/endpoint-discovery v1.12.7 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.13.30 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.19.31 // indirect
	github.com/aws/aws-sdk-go-v2/service/lambda v1.94.1 // indirect
	github.com/aws/aws-sdk-go-v2/service/organizations v1.51.12 // indirect
	github.com/aws/aws-sdk-go-v2/service/pricing v1.42.9 // indirect
	github.com/aws/aws-sdk-go-v2/service/route53 v1.63.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/ses v1.35.4 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.2.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/sns v1.40.3 // indirect
	github.com/aws/aws-sdk-go-v2/service/sqs v1.44.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.31.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.36.8 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.43.5 // indirect
	github.com/aws/smithy-go v1.27.3 // indirect
	github.com/gofrs/uuid v4.4.0+incompatible // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/kr/pretty v0.2.1 // indirect
	github.com/mattn/go-isatty v0.0.22 // indirect
	github.com/mikesmitty/edkey v0.0.0-20170222072505-3356ea4e686a // indirect
	github.com/r3labs/diff/v2 v2.15.1 // indirect
	github.com/vmihailenco/msgpack v4.0.4+incompatible // indirect
	golang.org/x/crypto v0.53.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	google.golang.org/appengine v1.6.8 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

exclude github.com/stretchr/testify v1.5.1 // retains vulnerable gopkg.in/yaml.v2 in the selected graph
