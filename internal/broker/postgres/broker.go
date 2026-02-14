package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"regexp"
	"strings"

	"github.com/pivotal-cf/brokerapi/v11/domain"
	"github.com/pivotal-cf/brokerapi/v11/domain/apiresponses"

	// PostgreSQL driver
	_ "github.com/lib/pq"
)

// identifierPattern validates SQL identifiers to prevent injection.
// Only allows alphanumeric characters and underscores.
var identifierPattern = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

// Broker implements the domain.ServiceBroker interface for PostgreSQL.
// It provisions databases and roles on a shared PostgreSQL instance.
type Broker struct {
	host     string
	port     string
	adminUser string
	adminPass string
}

// New creates a new PostgreSQL service broker.
func New(host, port, adminUser, adminPass string) *Broker {
	return &Broker{
		host:      host,
		port:      port,
		adminUser: adminUser,
		adminPass: adminPass,
	}
}

func (b *Broker) connectAdmin() (*sql.DB, error) {
	connStr := fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=postgres sslmode=disable",
		b.host, b.port, b.adminUser, b.adminPass,
	)
	return sql.Open("postgres", connStr)
}

func (b *Broker) dbName(instanceID string) string {
	safe := sanitizeIdentifier(instanceID)
	return "cf_" + safe
}

func (b *Broker) roleName(bindingID string) string {
	safe := sanitizeIdentifier(bindingID)
	return "cf_" + safe
}

// sanitizeIdentifier replaces hyphens with underscores and removes any
// characters that are not alphanumeric or underscores.
func sanitizeIdentifier(id string) string {
	s := strings.ReplaceAll(id, "-", "_")
	// Remove anything that is not alphanumeric or underscore
	safe := regexp.MustCompile(`[^a-zA-Z0-9_]`).ReplaceAllString(s, "")
	return safe
}

func validateIdentifier(name string) error {
	if !identifierPattern.MatchString(name) {
		return fmt.Errorf("invalid identifier: %s", name)
	}
	return nil
}

func generatePassword(length int) (string, error) {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("failed to generate random password: %w", err)
	}
	return hex.EncodeToString(bytes), nil
}

// Services returns the catalog of services offered by this broker.
func (b *Broker) Services(_ context.Context) ([]domain.Service, error) {
	return []domain.Service{
		{
			ID:          "postgresql-local-service-id",
			Name:        "postgresql-local",
			Description: "PostgreSQL database on a shared local instance",
			Bindable:    true,
			Tags:        []string{"postgresql", "sql", "database"},
			Plans: []domain.ServicePlan{
				{
					ID:          "postgresql-local-shared-plan-id",
					Name:        "shared",
					Description: "Creates a database on the shared PostgreSQL instance",
					Free:        boolPtr(true),
				},
			},
			Metadata: &domain.ServiceMetadata{
				DisplayName: "PostgreSQL (Local)",
				LongDescription: "Provisions a dedicated database and credentials on a shared " +
					"PostgreSQL instance running in the local cluster.",
			},
		},
	}, nil
}

// Provision creates a new database for the service instance.
func (b *Broker) Provision(
	_ context.Context,
	instanceID string,
	_ domain.ProvisionDetails,
	_ bool,
) (domain.ProvisionedServiceSpec, error) {
	dbName := b.dbName(instanceID)
	if err := validateIdentifier(dbName); err != nil {
		return domain.ProvisionedServiceSpec{}, err
	}

	db, err := b.connectAdmin()
	if err != nil {
		return domain.ProvisionedServiceSpec{}, fmt.Errorf("failed to connect to PostgreSQL: %w", err)
	}
	defer db.Close()

	// Check if database already exists
	var exists bool
	err = db.QueryRow("SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)", dbName).Scan(&exists)
	if err != nil {
		return domain.ProvisionedServiceSpec{}, fmt.Errorf("failed to check database existence: %w", err)
	}
	if exists {
		return domain.ProvisionedServiceSpec{}, apiresponses.ErrInstanceAlreadyExists
	}

	// CREATE DATABASE cannot use parameterized queries, so we validate the identifier strictly
	_, err = db.Exec(fmt.Sprintf("CREATE DATABASE %s", quoteIdentifier(dbName)))
	if err != nil {
		return domain.ProvisionedServiceSpec{}, fmt.Errorf("failed to create database %s: %w", dbName, err)
	}

	log.Printf("Provisioned database: %s", dbName)
	return domain.ProvisionedServiceSpec{}, nil
}

// Deprovision drops the database for the service instance.
func (b *Broker) Deprovision(
	_ context.Context,
	instanceID string,
	_ domain.DeprovisionDetails,
	_ bool,
) (domain.DeprovisionServiceSpec, error) {
	dbName := b.dbName(instanceID)
	if err := validateIdentifier(dbName); err != nil {
		return domain.DeprovisionServiceSpec{}, err
	}

	db, err := b.connectAdmin()
	if err != nil {
		return domain.DeprovisionServiceSpec{}, fmt.Errorf("failed to connect to PostgreSQL: %w", err)
	}
	defer db.Close()

	// Terminate existing connections to the database
	_, err = db.Exec(
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()",
		dbName,
	)
	if err != nil {
		log.Printf("Warning: failed to terminate connections to %s: %v", dbName, err)
	}

	// DROP DATABASE cannot use parameterized queries
	_, err = db.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS %s", quoteIdentifier(dbName)))
	if err != nil {
		return domain.DeprovisionServiceSpec{}, fmt.Errorf("failed to drop database %s: %w", dbName, err)
	}

	log.Printf("Deprovisioned database: %s", dbName)
	return domain.DeprovisionServiceSpec{}, nil
}

// Bind creates a new role with access to the provisioned database and returns credentials.
func (b *Broker) Bind(
	_ context.Context,
	instanceID, bindingID string,
	_ domain.BindDetails,
	_ bool,
) (domain.Binding, error) {
	dbName := b.dbName(instanceID)
	roleName := b.roleName(bindingID)

	if err := validateIdentifier(dbName); err != nil {
		return domain.Binding{}, err
	}
	if err := validateIdentifier(roleName); err != nil {
		return domain.Binding{}, err
	}

	password, err := generatePassword(16)
	if err != nil {
		return domain.Binding{}, err
	}

	db, err := b.connectAdmin()
	if err != nil {
		return domain.Binding{}, fmt.Errorf("failed to connect to PostgreSQL: %w", err)
	}
	defer db.Close()

	// Create role with login and password
	// Role names and passwords cannot use parameterized queries in CREATE ROLE
	_, err = db.Exec(fmt.Sprintf(
		"CREATE ROLE %s WITH LOGIN PASSWORD %s",
		quoteIdentifier(roleName),
		quoteLiteral(password),
	))
	if err != nil {
		return domain.Binding{}, fmt.Errorf("failed to create role %s: %w", roleName, err)
	}

	// Grant all privileges on the database to the role
	_, err = db.Exec(fmt.Sprintf(
		"GRANT ALL PRIVILEGES ON DATABASE %s TO %s",
		quoteIdentifier(dbName),
		quoteIdentifier(roleName),
	))
	if err != nil {
		return domain.Binding{}, fmt.Errorf("failed to grant privileges: %w", err)
	}

	uri := fmt.Sprintf("postgres://%s:%s@%s:%s/%s",
		roleName, password, b.host, b.port, dbName,
	)

	log.Printf("Created binding: role=%s database=%s", roleName, dbName)

	return domain.Binding{
		Credentials: map[string]interface{}{
			"host":     b.host,
			"port":     b.port,
			"database": dbName,
			"username": roleName,
			"password": password,
			"uri":      uri,
		},
	}, nil
}

// Unbind drops the role created during binding.
func (b *Broker) Unbind(
	_ context.Context,
	instanceID, bindingID string,
	_ domain.UnbindDetails,
	_ bool,
) (domain.UnbindSpec, error) {
	dbName := b.dbName(instanceID)
	roleName := b.roleName(bindingID)

	if err := validateIdentifier(dbName); err != nil {
		return domain.UnbindSpec{}, err
	}
	if err := validateIdentifier(roleName); err != nil {
		return domain.UnbindSpec{}, err
	}

	db, err := b.connectAdmin()
	if err != nil {
		return domain.UnbindSpec{}, fmt.Errorf("failed to connect to PostgreSQL: %w", err)
	}
	defer db.Close()

	// Revoke privileges first
	_, err = db.Exec(fmt.Sprintf(
		"REVOKE ALL PRIVILEGES ON DATABASE %s FROM %s",
		quoteIdentifier(dbName),
		quoteIdentifier(roleName),
	))
	if err != nil {
		log.Printf("Warning: failed to revoke privileges for %s: %v", roleName, err)
	}

	// Drop the role
	_, err = db.Exec(fmt.Sprintf("DROP ROLE IF EXISTS %s", quoteIdentifier(roleName)))
	if err != nil {
		return domain.UnbindSpec{}, fmt.Errorf("failed to drop role %s: %w", roleName, err)
	}

	log.Printf("Removed binding: role=%s database=%s", roleName, dbName)
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

// quoteIdentifier quotes a PostgreSQL identifier to prevent SQL injection.
// It doubles any embedded double quotes per PostgreSQL quoting rules.
func quoteIdentifier(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// quoteLiteral quotes a PostgreSQL string literal to prevent SQL injection.
// It doubles any embedded single quotes per PostgreSQL quoting rules.
func quoteLiteral(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}

func boolPtr(b bool) *bool {
	return &b
}
