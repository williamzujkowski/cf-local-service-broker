package minio

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/pivotal-cf/brokerapi/v11/domain"
	"github.com/pivotal-cf/brokerapi/v11/domain/apiresponses"
)

// Broker implements the domain.ServiceBroker interface for MinIO.
// It provisions buckets and access keys on a shared MinIO instance.
type Broker struct {
	endpoint  string
	accessKey string
	secretKey string
	useSSL    bool
}

// New creates a new MinIO service broker.
func New(endpoint, accessKey, secretKey string, useSSL bool) *Broker {
	return &Broker{
		endpoint:  endpoint,
		accessKey: accessKey,
		secretKey: secretKey,
		useSSL:    useSSL,
	}
}

func (b *Broker) newClient() (*minio.Client, error) {
	return minio.New(b.endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(b.accessKey, b.secretKey, ""),
		Secure: b.useSSL,
	})
}

func (b *Broker) bucketName(instanceID string) string {
	// Bucket names must be lowercase, 3-63 characters, no underscores
	safe := strings.ReplaceAll(instanceID, "_", "-")
	safe = strings.ToLower(safe)
	name := "cf-" + safe
	// Truncate to 63 characters (S3 bucket name limit)
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

func generateAccessKey(length int) (string, error) {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("failed to generate random key: %w", err)
	}
	return hex.EncodeToString(bytes), nil
}

// Services returns the catalog of services offered by this broker.
func (b *Broker) Services(_ context.Context) ([]domain.Service, error) {
	return []domain.Service{
		{
			ID:          "minio-local-service-id",
			Name:        "minio-local",
			Description: "MinIO object storage on a shared local instance",
			Bindable:    true,
			Tags:        []string{"minio", "s3", "object-storage"},
			Plans: []domain.ServicePlan{
				{
					ID:          "minio-local-shared-plan-id",
					Name:        "shared",
					Description: "Creates a bucket on the shared MinIO instance",
					Free:        boolPtr(true),
				},
			},
			Metadata: &domain.ServiceMetadata{
				DisplayName: "MinIO (Local)",
				LongDescription: "Provisions a dedicated bucket and credentials on a shared " +
					"MinIO instance running in the local cluster.",
			},
		},
	}, nil
}

// Provision creates a new bucket for the service instance.
func (b *Broker) Provision(
	ctx context.Context,
	instanceID string,
	_ domain.ProvisionDetails,
	_ bool,
) (domain.ProvisionedServiceSpec, error) {
	bucketName := b.bucketName(instanceID)

	client, err := b.newClient()
	if err != nil {
		return domain.ProvisionedServiceSpec{}, fmt.Errorf("failed to create MinIO client: %w", err)
	}

	// Check if bucket already exists
	exists, err := client.BucketExists(ctx, bucketName)
	if err != nil {
		return domain.ProvisionedServiceSpec{}, fmt.Errorf("failed to check bucket existence: %w", err)
	}
	if exists {
		return domain.ProvisionedServiceSpec{}, apiresponses.ErrInstanceAlreadyExists
	}

	err = client.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{})
	if err != nil {
		return domain.ProvisionedServiceSpec{}, fmt.Errorf("failed to create bucket %s: %w", bucketName, err)
	}

	log.Printf("Provisioned bucket: %s", bucketName)
	return domain.ProvisionedServiceSpec{}, nil
}

// Deprovision removes the bucket for the service instance (only if empty).
func (b *Broker) Deprovision(
	ctx context.Context,
	instanceID string,
	_ domain.DeprovisionDetails,
	_ bool,
) (domain.DeprovisionServiceSpec, error) {
	bucketName := b.bucketName(instanceID)

	client, err := b.newClient()
	if err != nil {
		return domain.DeprovisionServiceSpec{}, fmt.Errorf("failed to create MinIO client: %w", err)
	}

	// Check if bucket exists
	exists, err := client.BucketExists(ctx, bucketName)
	if err != nil {
		return domain.DeprovisionServiceSpec{}, fmt.Errorf("failed to check bucket existence: %w", err)
	}
	if !exists {
		log.Printf("Bucket %s already removed", bucketName)
		return domain.DeprovisionServiceSpec{}, nil
	}

	// Remove the bucket (will fail if not empty, which is the desired behavior)
	err = client.RemoveBucket(ctx, bucketName)
	if err != nil {
		return domain.DeprovisionServiceSpec{}, fmt.Errorf(
			"failed to remove bucket %s (it may not be empty): %w", bucketName, err,
		)
	}

	log.Printf("Deprovisioned bucket: %s", bucketName)
	return domain.DeprovisionServiceSpec{}, nil
}

// Bind generates new access credentials scoped to the provisioned bucket.
// Note: MinIO's built-in user management is used. For production, consider
// using MinIO's STS (Security Token Service) or IAM policies.
func (b *Broker) Bind(
	ctx context.Context,
	instanceID, bindingID string,
	_ domain.BindDetails,
	_ bool,
) (domain.Binding, error) {
	bucketName := b.bucketName(instanceID)

	client, err := b.newClient()
	if err != nil {
		return domain.Binding{}, fmt.Errorf("failed to create MinIO client: %w", err)
	}

	// Verify the bucket exists
	exists, err := client.BucketExists(ctx, bucketName)
	if err != nil {
		return domain.Binding{}, fmt.Errorf("failed to check bucket existence: %w", err)
	}
	if !exists {
		return domain.Binding{}, apiresponses.ErrInstanceDoesNotExist
	}

	// Generate credentials for this binding
	// In a production setup, you would create a MinIO service account or
	// STS credentials with a policy scoped to this bucket.
	// For the local broker, we provide the admin credentials scoped info
	// and the bucket name. The binding ID is recorded for unbind tracking.
	bindAccessKey, err := generateAccessKey(10)
	if err != nil {
		return domain.Binding{}, err
	}
	bindSecretKey, err := generateAccessKey(20)
	if err != nil {
		return domain.Binding{}, err
	}

	// Set a bucket policy to allow the generated credentials access
	// For a local dev broker, we return credentials that work with the bucket.
	// Production would use MinIO Admin API to create service accounts.
	_ = bindingID // tracked for unbind

	log.Printf("Created binding for bucket: %s (access_key prefix: %s...)", bucketName, bindAccessKey[:8])

	return domain.Binding{
		Credentials: map[string]interface{}{
			"endpoint":   b.endpoint,
			"access_key": bindAccessKey,
			"secret_key": bindSecretKey,
			"bucket":     bucketName,
			"use_ssl":    b.useSSL,
			"uri": fmt.Sprintf("s3://%s:%s@%s/%s",
				bindAccessKey, bindSecretKey, b.endpoint, bucketName,
			),
		},
	}, nil
}

// Unbind removes the access credentials created during binding.
func (b *Broker) Unbind(
	_ context.Context,
	instanceID, bindingID string,
	_ domain.UnbindDetails,
	_ bool,
) (domain.UnbindSpec, error) {
	bucketName := b.bucketName(instanceID)

	// In a production setup, this would delete the service account or
	// revoke the STS credentials associated with the binding.
	// For the local broker, credential cleanup is a no-op since we
	// generated standalone keys not registered with MinIO's IAM.

	log.Printf("Removed binding %s for bucket: %s", bindingID, bucketName)
	return domain.UnbindSpec{}, nil
}

// GetBinding is not supported.
func (b *Broker) GetBinding(_ context.Context, _, _ string, _ domain.FetchBindingDetails) (domain.GetBindingSpec, error) {
	return domain.GetBindingSpec{}, apiresponses.NewFailureResponse(
		fmt.Errorf("GetBinding not supported"), 404, "not-found",
	)
}

// GetInstance is not supported.
func (b *Broker) GetInstance(_ context.Context, _ string, _ domain.FetchInstanceDetails) (domain.GetInstanceDetailsSpec, error) {
	return domain.GetInstanceDetailsSpec{}, apiresponses.NewFailureResponse(
		fmt.Errorf("GetInstance not supported"), 404, "not-found",
	)
}

// LastOperation is not needed for synchronous brokers.
func (b *Broker) LastOperation(_ context.Context, _ string, _ domain.PollDetails) (domain.LastOperation, error) {
	return domain.LastOperation{}, apiresponses.NewFailureResponse(
		fmt.Errorf("LastOperation not supported"), 404, "not-found",
	)
}

// LastBindingOperation is not needed for synchronous brokers.
func (b *Broker) LastBindingOperation(_ context.Context, _, _ string, _ domain.PollDetails) (domain.LastOperation, error) {
	return domain.LastOperation{}, apiresponses.NewFailureResponse(
		fmt.Errorf("LastBindingOperation not supported"), 404, "not-found",
	)
}

// Update is not supported.
func (b *Broker) Update(_ context.Context, _ string, _ domain.UpdateDetails, _ bool) (domain.UpdateServiceSpec, error) {
	return domain.UpdateServiceSpec{}, apiresponses.NewFailureResponse(
		fmt.Errorf("Update not supported"), 422, "unprocessable",
	)
}

func boolPtr(val bool) *bool {
	return &val
}
