package sqlite

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/config"
)

func TestRewritePlaceholders(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"none", "SELECT 1", "SELECT 1"},
		{"single", "WHERE id = $1", "WHERE id = ?1"},
		{"several", "VALUES ($1,$2,$3)", "VALUES (?1,?2,?3)"},
		{"double digits", "LIMIT $10 OFFSET $11", "LIMIT ?10 OFFSET ?11"},
		{"repeated", "a = $1 OR b = $1", "a = ?1 OR b = ?1"},
		// A literal must survive untouched, otherwise a stop word containing a
		// dollar sign would silently turn into a bind parameter.
		{"string literal", "SET note = 'costs $5' WHERE id = $1", "SET note = 'costs $5' WHERE id = ?1"},
		{"escaped quote", "SET a = 'it''s $9' WHERE id = $2", "SET a = 'it''s $9' WHERE id = ?2"},
		{"quoted identifier", `SELECT "$1 col" WHERE id = $1`, `SELECT "$1 col" WHERE id = ?1`},
		{"line comment", "-- $3 stays\nWHERE id = $1", "-- $3 stays\nWHERE id = ?1"},
		{"block comment", "/* $7 */ WHERE id = $1", "/* $7 */ WHERE id = ?1"},
		{"lone dollar", "SELECT '$' || x", "SELECT '$' || x"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := rewritePlaceholders(tc.in); got != tc.want {
				t.Errorf("rewritePlaceholders(%q)\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
		})
	}
}

// The storage layout must sort chronologically as plain text, because every
// "due yet?" query compares timestamps as strings.
func TestTimeLayoutSortsChronologically(t *testing.T) {
	base := time.Date(2026, 8, 15, 10, 4, 5, 0, time.UTC)
	instants := []time.Time{
		base,
		base.Add(9 * time.Millisecond),
		base.Add(12 * time.Millisecond),
		base.Add(120 * time.Millisecond),
		base.Add(time.Second),
		base.Add(time.Hour),
	}

	for i := 1; i < len(instants); i++ {
		prev, curr := formatTime(instants[i-1]), formatTime(instants[i])
		if !(prev < curr) {
			t.Errorf("text order disagrees with time order:\n %q (%s)\n %q (%s)",
				prev, instants[i-1], curr, instants[i])
		}
	}
}

func TestTimeRoundTrip(t *testing.T) {
	// A non-UTC zone confirms the value is normalised on the way in.
	almaty, err := time.LoadLocation("Asia/Almaty")
	if err != nil {
		t.Fatal(err)
	}

	want := time.Date(2026, 8, 15, 18, 30, 45, 123456789, almaty)
	got, err := parseTime(formatTime(want))
	if err != nil {
		t.Fatalf("parseTime: %v", err)
	}
	if !got.Equal(want) {
		t.Errorf("round trip changed the instant: got %s, want %s", got, want)
	}
	if got.Location() != time.UTC {
		t.Errorf("expected UTC, got %s", got.Location())
	}
}

func TestParseTimeAcceptsOtherLayouts(t *testing.T) {
	for _, in := range []string{
		"2026-08-15T10:04:05.000000000Z",
		"2026-08-15T10:04:05Z",
		"2026-08-15 10:04:05",
		"2026-08-15",
	} {
		if _, err := parseTime(in); err != nil {
			t.Errorf("parseTime(%q): %v", in, err)
		}
	}
	if _, err := parseTime("not a time"); err == nil {
		t.Error("expected an error for unparseable input")
	}
}

// openTestDB gives each test its own database file.
func openTestDB(t *testing.T) *DB {
	t.Helper()

	db, err := Connect(context.Background(), config.Database{
		Path:     filepath.Join(t.TempDir(), "test.db"),
		MaxConns: 4,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// The bridge exists because database/sql cannot scan SQLite's stored text into
// these types on its own.
func TestScanBridgesDomainTypes(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.Exec(ctx, `CREATE TABLE t (
		id TEXT PRIMARY KEY DEFAULT (gen_random_uuid()),
		created_at TEXT NOT NULL DEFAULT (now()),
		deleted_at TEXT,
		owner_id TEXT,
		keywords TEXT NOT NULL DEFAULT '[]',
		payload TEXT
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	owner := uuid.New()
	moment := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	keywords := []string{"STOP", "ТОҚТАТУ"}
	payload := json.RawMessage(`{"a":1}`)

	if _, err := db.Exec(ctx,
		`INSERT INTO t (created_at, deleted_at, owner_id, keywords, payload) VALUES ($1,$2,$3,$4,$5)`,
		moment, nil, owner, keywords, payload); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var (
		id        uuid.UUID
		createdAt time.Time
		deletedAt *time.Time
		ownerID   *uuid.UUID
		gotWords  []string
		gotJSON   json.RawMessage
	)
	err := db.QueryRow(ctx, `SELECT id, created_at, deleted_at, owner_id, keywords, payload FROM t`).
		Scan(&id, &createdAt, &deletedAt, &ownerID, &gotWords, &gotJSON)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	if id == uuid.Nil {
		t.Error("gen_random_uuid() did not populate the id")
	}
	if !createdAt.Equal(moment) {
		t.Errorf("created_at: got %s, want %s", createdAt, moment)
	}
	if deletedAt != nil {
		t.Errorf("deleted_at: got %v, want nil", deletedAt)
	}
	if ownerID == nil || *ownerID != owner {
		t.Errorf("owner_id: got %v, want %s", ownerID, owner)
	}
	if len(gotWords) != 2 || gotWords[0] != "STOP" || gotWords[1] != "ТОҚТАТУ" {
		t.Errorf("keywords: got %v, want %v", gotWords, keywords)
	}
	if string(gotJSON) != string(payload) {
		t.Errorf("payload: got %s, want %s", gotJSON, payload)
	}
}

func TestNowIsComparableAgainstStoredTimestamps(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.Exec(ctx, `CREATE TABLE jobs (due_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO jobs (due_at) VALUES ($1), ($2)`,
		time.Now().Add(-time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	var due int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE due_at <= now()`).Scan(&due); err != nil {
		t.Fatal(err)
	}
	if due != 1 {
		t.Errorf("expected exactly one due job, got %d", due)
	}
}

func TestTimestampAddShiftsByStepOffset(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	base := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	var got time.Time
	if err := db.QueryRow(ctx, `SELECT ts_add($1, $2)`, base, -3600).Scan(&got); err != nil {
		t.Fatalf("ts_add: %v", err)
	}
	if want := base.Add(-time.Hour); !got.Equal(want) {
		t.Errorf("ts_add: got %s, want %s", got, want)
	}
}

func TestToLocalDateUsesTheZone(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// 20:00 UTC is already the next day in Almaty (UTC+5).
	stamp := time.Date(2026, 8, 15, 20, 0, 0, 0, time.UTC)

	var day string
	if err := db.QueryRow(ctx, `SELECT to_local_date($1, $2)`, stamp, "Asia/Almaty").Scan(&day); err != nil {
		t.Fatalf("to_local_date: %v", err)
	}
	if day != "2026-08-16" {
		t.Errorf("to_local_date: got %s, want 2026-08-16", day)
	}
}

func TestInTxRollsBackOnError(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.Exec(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}

	wantErr := errSentinel{}
	err := db.InTx(ctx, func(tx Querier) error {
		if _, err := tx.Exec(ctx, `INSERT INTO t (id) VALUES (1)`); err != nil {
			return err
		}
		return wantErr
	})
	if err != wantErr {
		t.Fatalf("InTx returned %v, want the callback's error", err)
	}

	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM t`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("expected the insert to be rolled back, found %d rows", n)
	}
}

type errSentinel struct{}

func (errSentinel) Error() string { return "sentinel" }

func TestConstraintClassification(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.Exec(ctx, `
		CREATE TABLE admins (id TEXT PRIMARY KEY, email TEXT NOT NULL);
		CREATE UNIQUE INDEX admins_email_key ON admins (lower(email));
		CREATE TABLE messages (id TEXT PRIMARY KEY, external_id TEXT, admin_id TEXT REFERENCES admins (id));
		CREATE UNIQUE INDEX messages_external_id_key ON messages (external_id) WHERE external_id IS NOT NULL;`,
	); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec(ctx, `INSERT INTO admins VALUES ('1','a@x.com')`); err != nil {
		t.Fatal(err)
	}

	// An expression index: SQLite reports the index name.
	_, err := db.Exec(ctx, `INSERT INTO admins VALUES ('2','A@X.COM')`)
	if !IsUniqueViolation(err) {
		t.Errorf("expected a unique violation, got %v", err)
	}
	if !IsUniqueViolation(err, "admins_email_key") {
		t.Errorf("expected the violation to be attributed to admins_email_key, got %v", err)
	}
	if IsUniqueViolation(err, "messages_external_id_key") {
		t.Error("violation attributed to the wrong index")
	}

	// A plain-column index: SQLite reports table.column instead of the name,
	// which is exactly what the lookup table exists to translate.
	if _, err := db.Exec(ctx, `INSERT INTO messages (id, external_id) VALUES ('1','e1')`); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(ctx, `INSERT INTO messages (id, external_id) VALUES ('2','e1')`)
	if !IsUniqueViolation(err, "messages_external_id_key") {
		t.Errorf("expected the violation to be attributed to messages_external_id_key, got %v", err)
	}

	_, err = db.Exec(ctx, `INSERT INTO messages (id, admin_id) VALUES ('3','missing')`)
	if !IsForeignKeyViolation(err) {
		t.Errorf("expected a foreign key violation, got %v", err)
	}
	if IsUniqueViolation(err) {
		t.Error("a foreign key violation was classified as a unique violation")
	}
}

func TestQueryRowReportsNoRows(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.Exec(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}

	var id int
	err := db.QueryRow(ctx, `SELECT id FROM t WHERE id = $1`, 42).Scan(&id)
	if !IsNoRows(err) {
		t.Errorf("expected ErrNoRows, got %v", err)
	}
}

func TestExecReportsRowsAffected(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.Exec(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO t (id, v) VALUES (1,'a'), (2,'b'), (3,'c')`); err != nil {
		t.Fatal(err)
	}

	res, err := db.Exec(ctx, `UPDATE t SET v = 'z' WHERE id < $1`, 3)
	if err != nil {
		t.Fatal(err)
	}
	if res.RowsAffected() != 2 {
		t.Errorf("RowsAffected: got %d, want 2", res.RowsAffected())
	}

	res, err = db.Exec(ctx, `DELETE FROM t WHERE id = $1`, 99)
	if err != nil {
		t.Fatal(err)
	}
	if res.RowsAffected() != 0 {
		t.Errorf("RowsAffected on no match: got %d, want 0", res.RowsAffected())
	}
}
