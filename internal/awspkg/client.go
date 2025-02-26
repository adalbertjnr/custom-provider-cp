package awspkg

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

var ErrAwsClient = errors.New("cannot open aws client")

func AWSClientConnector(ctx context.Context) func(region string) (aws.Config, error) {
	return func(region string) (aws.Config, error) {
		cfg, err := config.LoadDefaultConfig(
			ctx,
			config.WithRegion(region),
		)
		if err != nil {
			return aws.Config{}, err
		}
		return cfg, nil
	}
}

func EC2Connect(c aws.Config) *ec2.Client {
	return ec2.NewFromConfig(c)
}
