//go:build integration

// Integration tests for the Provision/Bind/Unbind/Deprovision lifecycle.
// Build-tagged so the default `go test ./...` stays hermetic (no DB).
// Run against a real Postgres:
//
//	PG_TEST_DSN='host=localhost port=5432 user=postgres password=postgres sslmode=disable' \
//	    go test -tags=integration ./internal/broker/postgres/
//
// PG_TEST_DSN must point at the cluster's `postgres` database with an
// admin-level role — same shape as the broker's runtime admin connection.
// The role identified by PG_TEST_ADMIN_USER (default `postgres`) is what
// the broker will REASSIGN OWNED to on unbind in the legacy fallback path.
//
// Coverage matrix:
//
//	TestProvisionCreatesSharedOwnerRole          — shared-owner-role on, Provision side-effects
//	TestTwoBindingsShareTableAccess              — the bug from issue #10: cross-binding access works
//	TestUnbindLeavesSharedOwnerIntact            — unbind drops only the per-binding role
//	TestDeprovisionRemovesSharedOwnerRole        — full cleanup
//	TestSharedOwnerFlagOffFallsBackToPerBinding  — feature flag opt-out preserved
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pivotal-cf/brokerapi/v11/domain"

	_ "github.com/lib/pq"
)

// brokerFromDSN parses PG_TEST_DSN ("host=... port=... user=... password=... ...")
// into the host/port/user/pass quadruple the Broker constructor wants.
// We deliberately don't take a full DSN — Broker.connectAdmin builds its own.
func brokerFromDSN(t *testing.T, sharedOwner bool) (*Broker, string) {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping integration test")
	}
	parts := strings.Fields(dsn)
	kv := make(map[string]string, len(parts))
	for _, p := range parts {
		i := strings.Index(p, "=")
		if i < 0 {
			continue
		}
		kv[p[:i]] = p[i+1:]
	}
	host := kv["host"]
	if host == "" {
		host = "localhost"
	}
	port := kv["port"]
	if port == "" {
		port = "5432"
	}
	user := kv["user"]
	if user == "" {
		user = "postgres"
	}
	pass := kv["password"]
	return NewWithOptions(host, port, user, pass, sharedOwner), user
}

// newInstanceID returns a short, time-derived id we can sanitize cleanly.
// Real CF passes UUIDs; the broker's sanitizer handles both.
func newInstanceID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// roleExists checks pg_roles for a role name.
func roleExists(t *testing.T, db *sql.DB, role string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM pg_roles WHERE rolname = $1", role).Scan(&n); err != nil {
		t.Fatalf("roleExists query failed: %v", err)
	}
	return n > 0
}

// dbExists checks pg_database for a database name.
func dbExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM pg_database WHERE datname = $1", name).Scan(&n); err != nil {
		t.Fatalf("dbExists query failed: %v", err)
	}
	return n > 0
}

// dbOwner returns the owning role of a database.
func dbOwner(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var owner string
	err := db.QueryRow(`SELECT pg_catalog.pg_get_userbyid(datdba) FROM pg_database WHERE datname = $1`, name).Scan(&owner)
	if err != nil {
		t.Fatalf("dbOwner query failed: %v", err)
	}
	return owner
}

func TestProvisionCreatesSharedOwnerRole(t *testing.T) {
	b, _ := brokerFromDSN(t, true)
	ctx := context.Background()

	instanceID := newInstanceID("prov")
	dbName := b.dbName(instanceID)
	ownerRole := b.ownerRoleName(instanceID)

	if _, err := b.Provision(ctx, instanceID, domain.ProvisionDetails{PlanID: SharedPlanID}, false); err != nil {
		t.Fatalf("Provision failed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = b.Deprovision(ctx, instanceID, domain.DeprovisionDetails{}, false)
	})

	admin, err := b.connectAdmin()
	if err != nil {
		t.Fatalf("connectAdmin: %v", err)
	}
	defer admin.Close()

	if !dbExists(t, admin, dbName) {
		t.Errorf("database %s not created", dbName)
	}
	if !roleExists(t, admin, ownerRole) {
		t.Errorf("shared owner role %s not created", ownerRole)
	}
	if got := dbOwner(t, admin, dbName); got != ownerRole {
		t.Errorf("db owner: got %q, want %q", got, ownerRole)
	}

	// Owner role must be NOLOGIN (it's a group) — otherwise it's an
	// undocumented login surface.
	var canLogin bool
	if err := admin.QueryRow("SELECT rolcanlogin FROM pg_roles WHERE rolname = $1", ownerRole).Scan(&canLogin); err != nil {
		t.Fatalf("rolcanlogin lookup: %v", err)
	}
	if canLogin {
		t.Errorf("shared owner role %s should be NOLOGIN", ownerRole)
	}
}

func TestTwoBindingsShareTableAccess(t *testing.T) {
	b, _ := brokerFromDSN(t, true)
	ctx := context.Background()

	instanceID := newInstanceID("share")
	if _, err := b.Provision(ctx, instanceID, domain.ProvisionDetails{PlanID: SharedPlanID}, false); err != nil {
		t.Fatalf("Provision failed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = b.Deprovision(ctx, instanceID, domain.DeprovisionDetails{}, false)
	})

	bindingA := newInstanceID("binda")
	bindingB := newInstanceID("bindb")

	bA, err := b.Bind(ctx, instanceID, bindingA, domain.BindDetails{}, false)
	if err != nil {
		t.Fatalf("Bind A failed: %v", err)
	}
	t.Cleanup(func() { _, _ = b.Unbind(ctx, instanceID, bindingA, domain.UnbindDetails{}, false) })

	bB, err := b.Bind(ctx, instanceID, bindingB, domain.BindDetails{}, false)
	if err != nil {
		t.Fatalf("Bind B failed: %v", err)
	}
	t.Cleanup(func() { _, _ = b.Unbind(ctx, instanceID, bindingB, domain.UnbindDetails{}, false) })

	// Username in credentials should still be the per-binding role,
	// not the shared owner — apps connect with their own creds.
	if got := bA.Credentials.(map[string]interface{})["username"]; got != b.roleName(bindingA) {
		t.Errorf("Binding A username: got %v, want %s", got, b.roleName(bindingA))
	}
	if got := bB.Credentials.(map[string]interface{})["username"]; got != b.roleName(bindingB) {
		t.Errorf("Binding B username: got %v, want %s", got, b.roleName(bindingB))
	}

	// Connect as binding A, CREATE TABLE foo, INSERT.
	connA, err := sql.Open("postgres", bA.Credentials.(map[string]interface{})["uri"].(string)+"?sslmode=disable")
	if err != nil {
		t.Fatalf("open conn A: %v", err)
	}
	defer connA.Close()
	if _, err := connA.Exec("CREATE TABLE shared_test_foo (id INT PRIMARY KEY, note TEXT)"); err != nil {
		t.Fatalf("CREATE TABLE as binding A failed: %v", err)
	}
	if _, err := connA.Exec("INSERT INTO shared_test_foo VALUES (1, 'hello-from-A')"); err != nil {
		t.Fatalf("INSERT as binding A failed: %v", err)
	}

	// Connect as binding B, SELECT — this is the bug from #10. Without
	// the shared-owner-role model this fails with permission denied.
	connB, err := sql.Open("postgres", bB.Credentials.(map[string]interface{})["uri"].(string)+"?sslmode=disable")
	if err != nil {
		t.Fatalf("open conn B: %v", err)
	}
	defer connB.Close()

	var note string
	if err := connB.QueryRow("SELECT note FROM shared_test_foo WHERE id = 1").Scan(&note); err != nil {
		t.Fatalf("SELECT as binding B failed (this is the bug from issue #10): %v", err)
	}
	if note != "hello-from-A" {
		t.Errorf("SELECT as binding B: got %q, want %q", note, "hello-from-A")
	}

	// And the reverse direction: B writes, A reads.
	if _, err := connB.Exec("INSERT INTO shared_test_foo VALUES (2, 'hello-from-B')"); err != nil {
		t.Fatalf("INSERT as binding B failed: %v", err)
	}
	if err := connA.QueryRow("SELECT note FROM shared_test_foo WHERE id = 2").Scan(&note); err != nil {
		t.Fatalf("SELECT (B's row) as binding A failed: %v", err)
	}
	if note != "hello-from-B" {
		t.Errorf("SELECT B's row as binding A: got %q, want %q", note, "hello-from-B")
	}

	// Confirm the table is owned by the shared role, not either binding.
	admin, err := b.connectAdminDB(b.dbName(instanceID))
	if err != nil {
		t.Fatalf("connectAdminDB: %v", err)
	}
	defer admin.Close()
	var tableOwner string
	if err := admin.QueryRow(`SELECT tableowner FROM pg_tables WHERE tablename = 'shared_test_foo'`).Scan(&tableOwner); err != nil {
		t.Fatalf("table owner lookup: %v", err)
	}
	if tableOwner != b.ownerRoleName(instanceID) {
		t.Errorf("shared_test_foo owner: got %q, want %q (the shared owner)", tableOwner, b.ownerRoleName(instanceID))
	}
}

func TestUnbindLeavesSharedOwnerIntact(t *testing.T) {
	b, _ := brokerFromDSN(t, true)
	ctx := context.Background()

	instanceID := newInstanceID("unbind")
	if _, err := b.Provision(ctx, instanceID, domain.ProvisionDetails{PlanID: SharedPlanID}, false); err != nil {
		t.Fatalf("Provision failed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = b.Deprovision(ctx, instanceID, domain.DeprovisionDetails{}, false)
	})

	bindingID := newInstanceID("b1")
	if _, err := b.Bind(ctx, instanceID, bindingID, domain.BindDetails{}, false); err != nil {
		t.Fatalf("Bind failed: %v", err)
	}

	admin, err := b.connectAdmin()
	if err != nil {
		t.Fatalf("connectAdmin: %v", err)
	}
	defer admin.Close()

	bindRole := b.roleName(bindingID)
	ownerRole := b.ownerRoleName(instanceID)

	if !roleExists(t, admin, bindRole) {
		t.Fatalf("precondition: per-binding role %s should exist after Bind", bindRole)
	}
	if !roleExists(t, admin, ownerRole) {
		t.Fatalf("precondition: shared owner role %s should exist after Provision", ownerRole)
	}

	if _, err := b.Unbind(ctx, instanceID, bindingID, domain.UnbindDetails{}, false); err != nil {
		t.Fatalf("Unbind failed: %v", err)
	}

	if roleExists(t, admin, bindRole) {
		t.Errorf("per-binding role %s should be gone after Unbind", bindRole)
	}
	if !roleExists(t, admin, ownerRole) {
		t.Errorf("shared owner role %s should survive Unbind (only Deprovision removes it)", ownerRole)
	}
}

func TestDeprovisionRemovesSharedOwnerRole(t *testing.T) {
	b, _ := brokerFromDSN(t, true)
	ctx := context.Background()

	instanceID := newInstanceID("deprov")
	if _, err := b.Provision(ctx, instanceID, domain.ProvisionDetails{PlanID: SharedPlanID}, false); err != nil {
		t.Fatalf("Provision failed: %v", err)
	}

	admin, err := b.connectAdmin()
	if err != nil {
		t.Fatalf("connectAdmin: %v", err)
	}
	defer admin.Close()

	dbName := b.dbName(instanceID)
	ownerRole := b.ownerRoleName(instanceID)
	if !dbExists(t, admin, dbName) || !roleExists(t, admin, ownerRole) {
		t.Fatalf("precondition: db + owner role should exist after Provision")
	}

	if _, err := b.Deprovision(ctx, instanceID, domain.DeprovisionDetails{}, false); err != nil {
		t.Fatalf("Deprovision failed: %v", err)
	}

	if dbExists(t, admin, dbName) {
		t.Errorf("database %s should be gone after Deprovision", dbName)
	}
	if roleExists(t, admin, ownerRole) {
		t.Errorf("shared owner role %s should be gone after Deprovision", ownerRole)
	}
}

func TestSharedOwnerFlagOffFallsBackToPerBinding(t *testing.T) {
	// Operators upgrading an existing broker who don't want to rebind
	// every service instance can set POSTGRES_BROKER_SHARED_OWNER_ROLE=false.
	// In that mode the broker must behave exactly as it did before #10:
	// no owner role created, db owner stays as the admin role.
	b, adminUser := brokerFromDSN(t, false)
	ctx := context.Background()

	instanceID := newInstanceID("legacy")
	if _, err := b.Provision(ctx, instanceID, domain.ProvisionDetails{PlanID: SharedPlanID}, false); err != nil {
		t.Fatalf("Provision failed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = b.Deprovision(ctx, instanceID, domain.DeprovisionDetails{}, false)
	})

	admin, err := b.connectAdmin()
	if err != nil {
		t.Fatalf("connectAdmin: %v", err)
	}
	defer admin.Close()

	dbName := b.dbName(instanceID)
	ownerRole := b.ownerRoleName(instanceID)
	if !dbExists(t, admin, dbName) {
		t.Errorf("database %s not created", dbName)
	}
	if roleExists(t, admin, ownerRole) {
		t.Errorf("legacy mode should NOT create shared owner role %s", ownerRole)
	}
	if got := dbOwner(t, admin, dbName); got != adminUser {
		t.Errorf("legacy mode db owner: got %q, want %q (the admin role)", got, adminUser)
	}

	// A Bind in legacy mode also must not grant any owner role —
	// confirm the only roles created are the per-binding one.
	bindingID := newInstanceID("legb")
	if _, err := b.Bind(ctx, instanceID, bindingID, domain.BindDetails{}, false); err != nil {
		t.Fatalf("Bind in legacy mode failed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = b.Unbind(ctx, instanceID, bindingID, domain.UnbindDetails{}, false)
	})

	// pg_auth_members rows for the per-binding role should be empty —
	// no group membership granted.
	bindRole := b.roleName(bindingID)
	var memberships int
	if err := admin.QueryRow(`
		SELECT COUNT(*) FROM pg_auth_members am
		JOIN pg_roles r ON r.oid = am.member
		WHERE r.rolname = $1`, bindRole).Scan(&memberships); err != nil {
		t.Fatalf("pg_auth_members query failed: %v", err)
	}
	if memberships != 0 {
		t.Errorf("legacy mode: per-binding role %s should have no group memberships, got %d", bindRole, memberships)
	}
}
