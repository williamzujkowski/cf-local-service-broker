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

// Service and plan IDs. Treat these as stable — changing them rebinds
// every existing service instance against the broker.
const (
	ServiceID      = "postgresql-local-service-id"
	SharedPlanID   = "postgresql-local-shared-plan-id"
	PgvectorPlanID = "postgresql-local-pgvector-plan-id"
)

// planUsesPgvector reports whether the given plan ID provisions databases
// with the pgvector extension enabled.
func planUsesPgvector(planID string) bool {
	return planID == PgvectorPlanID
}

// Broker implements the domain.ServiceBroker interface for PostgreSQL.
// It provisions databases and roles on a shared PostgreSQL instance.
type Broker struct {
	host            string
	port            string
	adminUser       string
	adminPass       string
	sharedOwnerRole bool
}

// New creates a new PostgreSQL service broker with the shared-owner-role
// model enabled. See NewWithOptions for the per-binding-only fallback.
func New(host, port, adminUser, adminPass string) *Broker {
	return NewWithOptions(host, port, adminUser, adminPass, true)
}

// NewWithOptions creates a new PostgreSQL service broker, allowing the
// caller to disable the shared-owner-role model. When sharedOwnerRole is
// false the broker falls back to the per-binding-only role behavior that
// shipped before issue #10 — useful for operators upgrading an existing
// broker who don't want to rebind every service instance immediately.
func NewWithOptions(host, port, adminUser, adminPass string, sharedOwnerRole bool) *Broker {
	return &Broker{
		host:            host,
		port:            port,
		adminUser:       adminUser,
		adminPass:       adminPass,
		sharedOwnerRole: sharedOwnerRole,
	}
}

func (b *Broker) connectAdmin() (*sql.DB, error) {
	return b.connectAdminDB("postgres")
}

// connectAdminDB opens an admin connection scoped to a specific database.
// Used to run per-database DDL such as CREATE EXTENSION, which is not
// reachable from the cluster's default `postgres` database.
func (b *Broker) connectAdminDB(dbName string) (*sql.DB, error) {
	connStr := fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		b.host, b.port, b.adminUser, b.adminPass, dbName,
	)
	return sql.Open("postgres", connStr)
}

// roleExists returns true if the named PG role exists. Used to gate
// v0.2.0 shared-owner code paths so instances provisioned under v0.1.x
// (which have no owner role) can still be unbound/deprovisioned cleanly.
func (b *Broker) roleExists(db *sql.DB, role string) (bool, error) {
	var exists bool
	err := db.QueryRow(
		"SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname = $1)",
		role,
	).Scan(&exists)
	return exists, err
}

func (b *Broker) dbName(instanceID string) string {
	safe := sanitizeIdentifier(instanceID)
	return "cf_" + safe
}

func (b *Broker) roleName(bindingID string) string {
	safe := sanitizeIdentifier(bindingID)
	return "cf_" + safe
}

// ownerRoleName derives the deterministic name of the per-instance
// shared owner role used by the shared-owner-role model (issue #10).
// Postgres identifiers are limited to 63 bytes; with the broker's
// `cf_<sanitized-instance-id>` database name (typically 39 chars for a
// UUID instance id) plus the `_owner` suffix (6 chars) we stay well
// inside that bound. The suffix is appended directly to the dbName so
// the relationship is obvious in pg_roles for an operator triaging.
func (b *Broker) ownerRoleName(instanceID string) string {
	return b.dbName(instanceID) + "_owner"
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
			ID:          ServiceID,
			Name:        "postgresql-local",
			Description: "PostgreSQL database on a shared local instance",
			Bindable:    true,
			Tags:        []string{"postgresql", "sql", "database"},
			Plans: []domain.ServicePlan{
				{
					ID:          SharedPlanID,
					Name:        "shared",
					Description: "Creates a database on the shared PostgreSQL instance",
					Free:        boolPtr(true),
				},
				{
					ID:   PgvectorPlanID,
					Name: "pgvector",
					Description: "Creates a database with the pgvector extension enabled. " +
						"Requires a pgvector-capable backing Postgres image " +
						"(e.g., pgvector/pgvector:pg16).",
					Free: boolPtr(true),
				},
			},
			Metadata: &domain.ServiceMetadata{
				DisplayName: "PostgreSQL (Local)",
				LongDescription: "Provisions a dedicated database and credentials on a shared " +
					"PostgreSQL instance running in the local cluster. " +
					"The 'pgvector' plan additionally enables the pgvector extension " +
					"on the new database.",
			},
		},
	}, nil
}

// Provision creates a new database for the service instance.
//
// When the shared-owner-role model is enabled (issue #10) it also creates
// a per-instance non-LOGIN group role (`<dbName>_owner`) and transfers
// database ownership to it. Subsequent Bind() calls grant that group to
// each per-binding login role and set it as their default current role so
// every CREATE TABLE/ALTER ends up owned by the shared role — giving
// multi-binding apps (API + worker) natural cross-binding object access.
//
// For the pgvector plan it additionally enables the pgvector extension
// inside the new database. The extension is created BEFORE ownership
// is transferred because CREATE EXTENSION must run as a superuser and
// we connect as the admin user. If extension creation fails, the empty
// database (and owner role, if created) are dropped so the caller can
// retry from a clean slate.
func (b *Broker) Provision(
	_ context.Context,
	instanceID string,
	details domain.ProvisionDetails,
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

	if planUsesPgvector(details.PlanID) {
		if extErr := b.enablePgvectorExtension(dbName); extErr != nil {
			// Best-effort cleanup so a retry doesn't trip ErrInstanceAlreadyExists.
			if _, dropErr := db.Exec(fmt.Sprintf("DROP DATABASE %s", quoteIdentifier(dbName))); dropErr != nil {
				log.Printf("Warning: failed to clean up database %s after pgvector setup failed: %v", dbName, dropErr)
			}
			return domain.ProvisionedServiceSpec{}, extErr
		}
	}

	if b.sharedOwnerRole {
		ownerRole := b.ownerRoleName(instanceID)
		if err := validateIdentifier(ownerRole); err != nil {
			// Roll back so retries see a clean slate.
			if _, dropErr := db.Exec(fmt.Sprintf("DROP DATABASE %s", quoteIdentifier(dbName))); dropErr != nil {
				log.Printf("Warning: failed to clean up database %s after owner role validation failed: %v", dbName, dropErr)
			}
			return domain.ProvisionedServiceSpec{}, fmt.Errorf("invalid owner role name: %w", err)
		}

		// NOLOGIN group role — bindings will be granted INTO it.
		if _, err := db.Exec(fmt.Sprintf(
			"CREATE ROLE %s NOLOGIN",
			quoteIdentifier(ownerRole),
		)); err != nil {
			// Roll back the empty database so retry from clean slate works.
			if _, dropErr := db.Exec(fmt.Sprintf("DROP DATABASE %s", quoteIdentifier(dbName))); dropErr != nil {
				log.Printf("Warning: failed to clean up database %s after owner role creation failed: %v", dbName, dropErr)
			}
			return domain.ProvisionedServiceSpec{}, fmt.Errorf("failed to create shared owner role %s: %w", ownerRole, err)
		}

		// Transfer database ownership so the shared role owns the DB itself
		// (and, transitively, schema-level defaults inherited from it).
		if _, err := db.Exec(fmt.Sprintf(
			"ALTER DATABASE %s OWNER TO %s",
			quoteIdentifier(dbName),
			quoteIdentifier(ownerRole),
		)); err != nil {
			// Roll back both the role and the DB.
			if _, dropErr := db.Exec(fmt.Sprintf("DROP DATABASE %s", quoteIdentifier(dbName))); dropErr != nil {
				log.Printf("Warning: failed to clean up database %s after ALTER OWNER failed: %v", dbName, dropErr)
			}
			if _, dropErr := db.Exec(fmt.Sprintf("DROP ROLE IF EXISTS %s", quoteIdentifier(ownerRole))); dropErr != nil {
				log.Printf("Warning: failed to clean up owner role %s after ALTER OWNER failed: %v", ownerRole, dropErr)
			}
			return domain.ProvisionedServiceSpec{}, fmt.Errorf("failed to transfer ownership of %s to %s: %w", dbName, ownerRole, err)
		}
	}

	log.Printf("Provisioned database: %s (plan=%s, shared_owner=%v)", dbName, details.PlanID, b.sharedOwnerRole)
	return domain.ProvisionedServiceSpec{}, nil
}

// enablePgvectorExtension reconnects to the named database as admin and
// runs CREATE EXTENSION IF NOT EXISTS vector. Requires the backing
// PostgreSQL image to bundle the pgvector extension
// (e.g., pgvector/pgvector:pg16). On a stock postgres image this will
// fail with a clear "extension \"vector\" is not available" message.
func (b *Broker) enablePgvectorExtension(dbName string) error {
	db, err := b.connectAdminDB(dbName)
	if err != nil {
		return fmt.Errorf("failed to connect to %s for pgvector setup: %w", dbName, err)
	}
	defer db.Close()

	if _, err := db.Exec("CREATE EXTENSION IF NOT EXISTS vector"); err != nil {
		return fmt.Errorf("failed to enable pgvector extension on %s: %w", dbName, err)
	}
	return nil
}

// Deprovision drops the database for the service instance and, when the
// shared-owner-role model is enabled, the per-instance owner role.
//
// Order matters: DROP DATABASE first so the objects owned by the shared
// role go away with the DB, then DROP ROLE succeeds without needing
// REASSIGN OWNED.
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

	if b.sharedOwnerRole {
		ownerRole := b.ownerRoleName(instanceID)
		// Validate before interpolating into DDL.
		if err := validateIdentifier(ownerRole); err != nil {
			log.Printf("Warning: skipping shared owner role drop for instance %s: %v", instanceID, err)
		} else if exists, rErr := b.roleExists(db, ownerRole); rErr != nil {
			// Probe failed — fall through to DROP ROLE IF EXISTS, which
			// is itself a no-op if the role isn't there.
			log.Printf("Warning: roleExists check for %s failed; attempting DROP ROLE IF EXISTS anyway: %v", ownerRole, rErr)
			if _, err := db.Exec(fmt.Sprintf(
				"DROP ROLE IF EXISTS %s",
				quoteIdentifier(ownerRole),
			)); err != nil {
				log.Printf("Warning: failed to drop shared owner role %s: %v", ownerRole, err)
			}
		} else if !exists {
			// v0.1.x-provisioned instance (#12) — no owner role was ever
			// created, so there's nothing to drop. Skip cleanly.
			log.Printf("INFO: owner role %s not found at Deprovision; v0.1.x compatibility path (nothing to drop)", ownerRole)
		} else if _, err := db.Exec(fmt.Sprintf(
			"DROP ROLE IF EXISTS %s",
			quoteIdentifier(ownerRole),
		)); err != nil {
			// Don't fail the deprovision — the DB is already gone. Worst
			// case the operator cleans up a stray empty role manually.
			log.Printf("Warning: failed to drop shared owner role %s: %v", ownerRole, err)
		}
	}

	log.Printf("Deprovisioned database: %s (shared_owner=%v)", dbName, b.sharedOwnerRole)
	return domain.DeprovisionServiceSpec{}, nil
}

// Bind creates a new role with access to the provisioned database and returns credentials.
//
// When the shared-owner-role model is enabled (issue #10) the per-binding
// role is also granted membership in the per-instance shared owner role
// (created in Provision) and has `SET ROLE = <owner>` set as a session
// default. The practical effect: every DDL issued by an app on this
// binding ends up owned by the shared role, so a second binding against
// the same instance can read/write the same objects without an
// admin-level reconcile.
//
// The returned credentials still name the per-binding role as `username`
// — apps connect with their own creds; the shared-role inheritance
// happens server-side via the ALTER ROLE SET ROLE default.
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

	if b.sharedOwnerRole {
		ownerRole := b.ownerRoleName(instanceID)
		if err := validateIdentifier(ownerRole); err != nil {
			return domain.Binding{}, fmt.Errorf("invalid owner role name: %w", err)
		}

		// Bind inherits group membership in the shared owner. INHERIT is
		// the default on CREATE ROLE so the binding immediately picks up
		// any privileges held by the owner (notably: ownership of every
		// object in the database).
		if _, err := db.Exec(fmt.Sprintf(
			"GRANT %s TO %s",
			quoteIdentifier(ownerRole),
			quoteIdentifier(roleName),
		)); err != nil {
			return domain.Binding{}, fmt.Errorf("failed to grant owner role %s to %s: %w", ownerRole, roleName, err)
		}

		// Auto-set role on login so every CREATE TABLE/ALTER TABLE the
		// app issues defaults the object owner to the shared role —
		// regardless of which binding's credentials the connection used.
		// This is the property that makes multi-binding (API + worker)
		// apps share an object graph without admin-level reconciles.
		if _, err := db.Exec(fmt.Sprintf(
			"ALTER ROLE %s SET ROLE = %s",
			quoteIdentifier(roleName),
			quoteIdentifier(ownerRole),
		)); err != nil {
			return domain.Binding{}, fmt.Errorf("failed to set default role on %s: %w", roleName, err)
		}
	}

	// Grant schema-level CREATE so the role can create tables. Postgres
	// 15+ revoked CREATE on the `public` schema from PUBLIC, so a fresh
	// role gets a permission-denied on `CREATE TABLE` without this.
	// Has to run against the per-binding DB (schema permissions are
	// per-database), not the admin DB we connected to above.
	//
	// Kept as defense-in-depth even with shared-owner-role enabled:
	// CREATE TABLE under the binding role's own identity (e.g. before
	// SET ROLE takes effect, or if an app explicitly RESET ROLEs) still
	// needs to work.
	dbConn, err := b.connectAdminDB(dbName)
	if err != nil {
		return domain.Binding{}, fmt.Errorf("failed to connect to %s for schema grant: %w", dbName, err)
	}
	defer dbConn.Close()
	_, err = dbConn.Exec(fmt.Sprintf(
		"GRANT ALL ON SCHEMA public TO %s",
		quoteIdentifier(roleName),
	))
	if err != nil {
		return domain.Binding{}, fmt.Errorf("failed to grant schema public to %s: %w", roleName, err)
	}

	if b.sharedOwnerRole {
		// Also grant schema-public to the shared owner so objects
		// CREATEd under SET ROLE land in `public` cleanly. The owner
		// already owns the database itself, but on Postgres 15+ that
		// doesn't transitively grant CREATE on `public`.
		ownerRole := b.ownerRoleName(instanceID)
		if _, err := dbConn.Exec(fmt.Sprintf(
			"GRANT ALL ON SCHEMA public TO %s",
			quoteIdentifier(ownerRole),
		)); err != nil {
			return domain.Binding{}, fmt.Errorf("failed to grant schema public to owner %s: %w", ownerRole, err)
		}
	}

	uri := fmt.Sprintf("postgres://%s:%s@%s:%s/%s",
		roleName, password, b.host, b.port, dbName,
	)

	log.Printf("Created binding: role=%s database=%s shared_owner=%v", roleName, dbName, b.sharedOwnerRole)

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

// Unbind drops the per-binding role created during Bind. The per-instance
// shared owner role (when enabled) is intentionally left intact — it's
// shared across every binding for the instance and is only removed by
// Deprovision.
//
// With the shared-owner-role model on, most objects the bound app
// created are already owned by the shared role (not the per-binding
// role), so REASSIGN OWNED becomes mostly a no-op. We keep it for safety
// — anything created with an explicit `RESET ROLE` would still be owned
// by the per-binding role and block DROP ROLE without it.
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

	// Decide whether to use the v0.2.0 shared-owner path or the v0.1.x
	// compatibility fallback. The fallback exists because an operator who
	// upgraded the broker from v0.1.x to v0.2.0 still has instances on
	// disk that were provisioned WITHOUT a shared owner role (see #12).
	// Unconditionally REVOKE/REASSIGN to the owner errors out and leaves
	// the binding/instance in a stuck state. Probing pg_roles up front
	// lets us pick the right path per-instance.
	useSharedOwner := false
	ownerRole := ""
	if b.sharedOwnerRole {
		ownerRole = b.ownerRoleName(instanceID)
		if vErr := validateIdentifier(ownerRole); vErr != nil {
			log.Printf("Warning: shared owner role name %s failed validation; using v0.1.x compatibility path: %v", ownerRole, vErr)
		} else if exists, rErr := b.roleExists(db, ownerRole); rErr != nil {
			log.Printf("Warning: roleExists check for %s failed; using v0.1.x compatibility path: %v", ownerRole, rErr)
		} else if exists {
			useSharedOwner = true
		} else {
			log.Printf("INFO: owner role %s not found; using v0.1.x compatibility path", ownerRole)
		}
	}

	// Revoke membership in the shared owner so the role has no remaining
	// grants and DROP ROLE doesn't trip on "role ... is a member of role".
	// Only meaningful when the owner role actually exists.
	if useSharedOwner {
		if _, e := db.Exec(fmt.Sprintf(
			"REVOKE %s FROM %s",
			quoteIdentifier(ownerRole),
			quoteIdentifier(roleName),
		)); e != nil {
			log.Printf("Warning: REVOKE owner %s FROM %s failed: %v", ownerRole, roleName, e)
		}
	}

	// Reassign + drop objects owned by the role inside its DB. Without
	// this, DROP ROLE fails with "cannot be dropped because some objects
	// depend on it" whenever the bound app created tables/sequences/etc.
	// REASSIGN preserves user data by transferring ownership to admin
	// (or, with shared-owner-role enabled AND the owner role actually
	// present, to the per-instance owner so the next binding still sees
	// it); DROP OWNED then removes the role's remaining grants
	// (including the schema-public grant added in Bind).
	//
	// Ownership + privileges are per-DB, so connect to the per-binding DB.
	if dbConn, dbErr := b.connectAdminDB(dbName); dbErr == nil {
		defer dbConn.Close()
		reassignTo := b.adminUser
		if useSharedOwner {
			reassignTo = ownerRole
		}
		if _, e := dbConn.Exec(fmt.Sprintf(
			"REASSIGN OWNED BY %s TO %s",
			quoteIdentifier(roleName),
			quoteIdentifier(reassignTo),
		)); e != nil {
			log.Printf("Warning: REASSIGN OWNED for %s in %s failed: %v", roleName, dbName, e)
		}
		if _, e := dbConn.Exec(fmt.Sprintf(
			"DROP OWNED BY %s",
			quoteIdentifier(roleName),
		)); e != nil {
			log.Printf("Warning: DROP OWNED for %s in %s failed: %v", roleName, dbName, e)
		}
	} else {
		// DB may have been deprovisioned ahead of this unbind. Continue
		// to DROP ROLE — if the role owns no surviving objects, it works.
		log.Printf("Warning: could not connect to %s during unbind: %v", dbName, dbErr)
	}

	// Drop the role
	_, err = db.Exec(fmt.Sprintf("DROP ROLE IF EXISTS %s", quoteIdentifier(roleName)))
	if err != nil {
		return domain.UnbindSpec{}, fmt.Errorf("failed to drop role %s: %w", roleName, err)
	}

	if useSharedOwner {
		log.Printf("Removed binding: role=%s database=%s (shared owner %s left intact)", roleName, dbName, ownerRole)
	} else {
		log.Printf("Removed binding: role=%s database=%s (no shared owner — v0.1.x compatibility path)", roleName, dbName)
	}
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
