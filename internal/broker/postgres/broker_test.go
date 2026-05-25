// Pure-function tests for the postgres broker. DB-touching paths
// (Provision, Bind, Deprovision, Unbind, enablePgvectorExtension) are
// covered by the integration suite tracked in the follow-up issue —
// they need a real Postgres and we intentionally keep `go test` hermetic.
package postgres

import (
	"context"
	"strings"
	"testing"
)

func TestServicesReturnsBothPlans(t *testing.T) {
	b := New("h", "5432", "admin", "pw")
	services, err := b.Services(context.Background())
	if err != nil {
		t.Fatalf("Services returned error: %v", err)
	}
	if len(services) != 1 {
		t.Fatalf("want 1 service, got %d", len(services))
	}
	svc := services[0]
	if svc.ID != ServiceID {
		t.Errorf("service id: want %q, got %q", ServiceID, svc.ID)
	}
	if len(svc.Plans) != 2 {
		t.Fatalf("want 2 plans (shared + pgvector), got %d", len(svc.Plans))
	}

	plansByID := make(map[string]string, len(svc.Plans))
	for _, p := range svc.Plans {
		plansByID[p.ID] = p.Name
	}
	if plansByID[SharedPlanID] != "shared" {
		t.Errorf("shared plan: want name=shared at id=%s, got name=%q", SharedPlanID, plansByID[SharedPlanID])
	}
	if plansByID[PgvectorPlanID] != "pgvector" {
		t.Errorf("pgvector plan: want name=pgvector at id=%s, got name=%q", PgvectorPlanID, plansByID[PgvectorPlanID])
	}
}

func TestServicesPgvectorPlanMentionsImageRequirement(t *testing.T) {
	// If you change the wording, also update README and broker LongDescription.
	b := New("h", "5432", "admin", "pw")
	services, _ := b.Services(context.Background())
	var pgvDesc string
	for _, p := range services[0].Plans {
		if p.ID == PgvectorPlanID {
			pgvDesc = p.Description
			break
		}
	}
	if !strings.Contains(strings.ToLower(pgvDesc), "pgvector") {
		t.Errorf("pgvector plan description should mention 'pgvector': %q", pgvDesc)
	}
	if !strings.Contains(strings.ToLower(pgvDesc), "image") {
		t.Errorf("pgvector plan description should mention image requirement so operators wire the right backing image: %q", pgvDesc)
	}
}

func TestPlanUsesPgvector(t *testing.T) {
	cases := []struct {
		planID string
		want   bool
	}{
		{PgvectorPlanID, true},
		{SharedPlanID, false},
		{"", false},
		{"unknown-plan-id", false},
	}
	for _, tc := range cases {
		if got := planUsesPgvector(tc.planID); got != tc.want {
			t.Errorf("planUsesPgvector(%q) = %v, want %v", tc.planID, got, tc.want)
		}
	}
}

func TestSanitizeIdentifier(t *testing.T) {
	cases := map[string]string{
		"abc":                "abc",
		"abc-def":            "abc_def",
		"my-instance-id-123": "my_instance_id_123",
		"bad;DROP TABLE":     "badDROPTABLE",
		"a.b.c":              "abc",
		"under_score_ok":     "under_score_ok",
		"":                   "",
	}
	for input, want := range cases {
		if got := sanitizeIdentifier(input); got != want {
			t.Errorf("sanitizeIdentifier(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestValidateIdentifier(t *testing.T) {
	good := []string{"abc", "ABC_123", "cf_my_instance"}
	for _, g := range good {
		if err := validateIdentifier(g); err != nil {
			t.Errorf("validateIdentifier(%q) returned unexpected error: %v", g, err)
		}
	}
	bad := []string{"", "abc-def", "abc;drop", "abc def", "abc.def", `abc"def`, "abc'def"}
	for _, b := range bad {
		if err := validateIdentifier(b); err == nil {
			t.Errorf("validateIdentifier(%q) returned nil error, want error", b)
		}
	}
}

func TestQuoteIdentifierEscapesDoubleQuotes(t *testing.T) {
	cases := map[string]string{
		"abc":           `"abc"`,
		`a"b`:           `"a""b"`,
		`weird"name"x"`: `"weird""name""x"""`,
	}
	for input, want := range cases {
		if got := quoteIdentifier(input); got != want {
			t.Errorf("quoteIdentifier(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestQuoteLiteralEscapesSingleQuotes(t *testing.T) {
	cases := map[string]string{
		"abc":           `'abc'`,
		`a'b`:           `'a''b'`,
		`O'Brien`:       `'O''Brien'`,
		"":              `''`,
		`it's a 'test'`: `'it''s a ''test'''`,
	}
	for input, want := range cases {
		if got := quoteLiteral(input); got != want {
			t.Errorf("quoteLiteral(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestGeneratePasswordReturnsExpectedLength(t *testing.T) {
	// hex.EncodeToString doubles byte count.
	for _, n := range []int{8, 16, 32} {
		pw, err := generatePassword(n)
		if err != nil {
			t.Fatalf("generatePassword(%d) returned error: %v", n, err)
		}
		if len(pw) != n*2 {
			t.Errorf("generatePassword(%d) returned %d hex chars, want %d", n, len(pw), n*2)
		}
	}
}

func TestDbNameAndRoleNamePrefixWithCF(t *testing.T) {
	b := New("h", "5432", "admin", "pw")
	if got := b.dbName("abc-123"); got != "cf_abc_123" {
		t.Errorf("dbName: got %q, want cf_abc_123", got)
	}
	if got := b.roleName("xyz-456"); got != "cf_xyz_456" {
		t.Errorf("roleName: got %q, want cf_xyz_456", got)
	}
}

func TestOwnerRoleNameDerivesFromInstance(t *testing.T) {
	b := New("h", "5432", "admin", "pw")

	// Deterministic: same instance id -> same owner role.
	first := b.ownerRoleName("abc-123")
	second := b.ownerRoleName("abc-123")
	if first != second {
		t.Errorf("ownerRoleName not deterministic: %q vs %q", first, second)
	}

	// Suffix convention so operators can spot it in pg_roles.
	if !strings.HasSuffix(first, "_owner") {
		t.Errorf("ownerRoleName(%q)=%q should end with _owner", "abc-123", first)
	}

	// Sanity: prefix is the db name so the relationship is obvious.
	if got, want := first, b.dbName("abc-123")+"_owner"; got != want {
		t.Errorf("ownerRoleName(%q)=%q, want %q", "abc-123", got, want)
	}

	// Postgres identifier limit is 63 bytes — even with a worst-case CF
	// UUID instance id we should stay under it.
	worstCase := b.ownerRoleName("ffffffff-ffff-ffff-ffff-ffffffffffff")
	if len(worstCase) > 63 {
		t.Errorf("ownerRoleName for UUID is %d bytes (%q), exceeds Postgres NAMEDATALEN-1=63", len(worstCase), worstCase)
	}

	// Validates as a SQL identifier — the broker hands it straight to
	// CREATE ROLE / ALTER DATABASE OWNER TO.
	if err := validateIdentifier(worstCase); err != nil {
		t.Errorf("ownerRoleName for UUID failed validateIdentifier: %v", err)
	}
}

func TestNewDefaultsSharedOwnerOn(t *testing.T) {
	// New() is the convenience constructor used by simple deploys; the
	// shared-owner-role model should be ON by default per issue #10.
	b := New("h", "5432", "admin", "pw")
	if !b.sharedOwnerRole {
		t.Errorf("New() should default sharedOwnerRole=true; got false")
	}
}

func TestNewWithOptionsCarriesSharedOwnerFlag(t *testing.T) {
	cases := []struct {
		name string
		in   bool
	}{
		{"on", true},
		{"off", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := NewWithOptions("h", "5432", "admin", "pw", tc.in)
			if b.sharedOwnerRole != tc.in {
				t.Errorf("sharedOwnerRole: got %v, want %v", b.sharedOwnerRole, tc.in)
			}
		})
	}
}
