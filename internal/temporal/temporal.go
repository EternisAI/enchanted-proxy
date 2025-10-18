package temporal

import (
	"context"
	"crypto/tls"
	"fmt"

	"github.com/eternisai/enchanted-proxy/internal/config"
	"go.temporal.io/sdk/client"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type TemporalClientConfig struct {
	Address     string
	Namespace   string
	CloudAPIKey string
	TaskQueue   string
}

// NewTemporalClientFromConfig creates a new Temporal client from the application config
func NewTemporalClientFromConfig(cfg *config.Config) (client.Client, error) {
	clientConfig := TemporalClientConfig{
		Address:     cfg.TemporalAddress,
		Namespace:   cfg.TemporalNamespace,
		CloudAPIKey: cfg.TemporalCloudAPIKey,
		TaskQueue:   cfg.TemporalTaskQueue,
	}
	return CreateTemporalClient(clientConfig)
}

func CreateTemporalClient(config TemporalClientConfig) (client.Client, error) {
	clientOptions := client.Options{
		HostPort:  config.Address,
		Namespace: config.Namespace,
	}

	// Only configure TLS and credentials if using Temporal Cloud
	if config.CloudAPIKey != "" {
		clientOptions.ConnectionOptions = client.ConnectionOptions{
			TLS: &tls.Config{
				MinVersion: tls.VersionTLS12,
			},
			DialOptions: []grpc.DialOption{
				grpc.WithUnaryInterceptor(
					func(ctx context.Context, method string, req any, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
						return invoker(
							metadata.AppendToOutgoingContext(ctx, "temporal-namespace", config.Namespace),
							method,
							req,
							reply,
							cc,
							opts...,
						)
					},
				),
			},
		}
		clientOptions.Credentials = client.NewAPIKeyStaticCredentials(config.CloudAPIKey)
	}

	c, err := client.Dial(clientOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to create temporal client: %w", err)
	}

	return c, nil
}
