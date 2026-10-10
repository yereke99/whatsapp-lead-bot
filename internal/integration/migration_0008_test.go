//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ayran/whatsapp-automation/internal/config"
	"github.com/ayran/whatsapp-automation/internal/storage/sqlite"
	"github.com/ayran/whatsapp-automation/migrations"
)

type recoveryTableImage struct {
	columns []string
	rows    [][]any
}

// Read every original column, including timestamps and provider IDs. Counts
// alone cannot detect an upgrade that silently rewrites customer history.
func recoverySnapshot(t *testing.T, db *sqlite.DB, schema map[string]recoveryTableImage) map[string]recoveryTableImage {
	t.Helper()
	ctx := context.Background()
	if schema == nil {
		schema = map[string]recoveryTableImage{}
		rows, err := db.SQL().QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			schema[name] = recoveryTableImage{}
		}
		rows.Close()
	}
	out := map[string]recoveryTableImage{}
	for table, image := range schema {
		columns := "*"
		if len(image.columns) > 0 {
			quoted := make([]string, len(image.columns))
			for i, column := range image.columns {
				quoted[i] = fmt.Sprintf("%q", column)
			}
			columns = strings.Join(quoted, ",")
		}
		query := "SELECT " + columns + " FROM " + fmt.Sprintf("%q", table)
		if table == "schema_migrations" {
			query += " WHERE version < 8"
		}
		query += " ORDER BY 1"
		rows, err := db.SQL().QueryContext(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		columnNames, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		current := recoveryTableImage{columns: columnNames}
		for rows.Next() {
			values := make([]any, len(columnNames))
			dest := make([]any, len(values))
			for i := range values {
				dest[i] = &values[i]
			}
			if err := rows.Scan(dest...); err != nil {
				t.Fatal(err)
			}
			current.rows = append(current.rows, values)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		out[table] = current
	}
	return out
}

func TestMigration0008PreservesEveryExistingValueAcrossRestarts(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Connect(ctx, config.Database{Path: filepath.Join(t.TempDir(), "upgrade.db"), MaxConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	previous, _ := schemaBefore(t, "0008")
	if err := db.Migrate(ctx, os.DirFS(previous), discardLogger()); err != nil {
		t.Fatal(err)
	}
	// Representative customer data on the exact previous production schema.
	// Duplicate event aliases also existed in old versions: an upgrade must
	// tolerate them without deleting or rewriting either historical record.
	statements := []string{
		`INSERT INTO admins (id,email,password_hash,role) VALUES ('admin','owner@example.test','hash','OWNER')`,
		`INSERT INTO admin_sessions (admin_id,token_hash,csrf_token,expires_at) VALUES ('admin','session','csrf',ts_add(now(),3600))`,
		`INSERT INTO contacts (id,phone,chat_id,first_contact_at) VALUES ('contact','77011234567','77011234567@c.us',now())`,
		`INSERT INTO message_templates (id,name,type,body) VALUES ('template','welcome','TEXT','preserve this exact text')`,
		`INSERT INTO campaigns (id,name,event_start_at,status,whatsapp_instance_id) VALUES ('campaign','existing campaign',ts_add(now(),86400),'ACTIVE',1)`,
		`INSERT INTO campaign_triggers (id,campaign_id,keyword,normalized_keyword,match_mode) VALUES ('trigger','campaign','JOIN','join','EXACT')`,
		`INSERT INTO campaign_steps (id,campaign_id,name,message_template_id,offset_seconds,schedule_kind,order_index) VALUES ('step','campaign','welcome','template',2,'ON_TRIGGER',1)`,
		`INSERT INTO campaign_contacts (id,campaign_id,contact_id,trigger_id,trigger_keyword) VALUES ('enrollment','campaign','contact','trigger','JOIN')`,
		`INSERT INTO scheduled_messages (id,campaign_id,contact_id,enrollment_id,campaign_step_id,scheduled_at,status) VALUES ('job','campaign','contact','enrollment','step',now(),'SENT')`,
		`INSERT INTO messages (id,contact_id,campaign_id,scheduled_message_id,direction,type,text,external_id,status,sent_at) VALUES ('reply','contact','campaign','job','OUTGOING','TEXT','already sent','external-reply','SENT',now())`,
		`UPDATE scheduled_messages SET message_id = 'reply', sent_at = now() WHERE id = 'job'`,
		`INSERT INTO messages (contact_id,direction,type,text,external_id,status) VALUES ('contact','INCOMING','TEXT','JOIN','external-incoming','RECEIVED')`,
		`INSERT INTO webhook_events (dedupe_key,event_type,external_id,payload,status,whatsapp_instance_id) VALUES ('incoming:external-incoming','incomingMessageReceived','external-incoming','{}','PROCESSED',1)`,
		`INSERT INTO webhook_events (dedupe_key,event_type,external_id,payload,status,whatsapp_instance_id) VALUES ('primary:incoming:external-incoming','incomingMessageReceived','external-incoming','{}','FAILED',1)`,
		`INSERT INTO audit_logs (admin_id,action,summary) VALUES ('admin','old_action','keep audit history')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(ctx, statement); err != nil {
			t.Fatalf("seed previous schema: %v", err)
		}
	}
	before := recoverySnapshot(t, db, nil)
	for restart := 0; restart < 5; restart++ {
		if err := db.Migrate(ctx, migrations.FS, discardLogger()); err != nil {
			t.Fatal(err)
		}
		after := recoverySnapshot(t, db, before)
		if !reflect.DeepEqual(before, after) {
			for table, want := range before {
				if !reflect.DeepEqual(want, after[table]) {
					t.Errorf("existing values changed in %s on restart %d", table, restart)
				}
			}
		}
	}
	var checkpoints, invalidForeignKeys int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM webhook_events WHERE trigger_result IS NOT NULL OR next_attempt_at IS NOT NULL`).Scan(&checkpoints); err != nil {
		t.Fatal(err)
	}
	if checkpoints != 0 {
		t.Fatal("upgrade invented historical business checkpoints")
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM pragma_foreign_key_check`).Scan(&invalidForeignKeys); err != nil {
		t.Fatal(err)
	}
	if invalidForeignKeys != 0 {
		t.Fatal("upgrade damaged foreign keys")
	}
	var integrity string
	if err := db.QueryRow(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity=%s error=%v", integrity, err)
	}
	var synchronous int
	if err := db.QueryRow(ctx, `PRAGMA synchronous`).Scan(&synchronous); err != nil || synchronous != 2 {
		t.Fatalf("durable commit pragma=%d error=%v", synchronous, err)
	}
}
