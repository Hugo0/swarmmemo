package board

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"swarmmemo/internal/ledger"
	"swarmmemo/internal/moderation"
	"swarmmemo/internal/services"
	"swarmmemo/internal/trust"
)

// migrateSchema brings a database at version (0 for a new file) to
// SchemaVersion inside Open's migration transaction. Open calls it only
// when version < SchemaVersion, so a current database runs no DDL at all.
//
// Up to schema 15 most of the schema was created at every start, outside
// the version: CREATE ... IF NOT EXISTS for whole tables and indexes, and
// columns added when pragma_table_info lacked them. Schema 16 folds all of
// it in. Every step below is still keyed on what exists (IF NOT EXISTS, the
// column itself), so the same sequence builds a new database and upgrades
// any older one, and the order of the column steps is the order production
// added them: an upgraded database and a new one end with the same
// sqlite_master, byte for byte (TestSchema16UpgradeMatchesFresh, and
// testdata/schema16.sql pins it).
//
// Added at start, without a version, since schema 15 (the 1.30.0 binary):
// tables docs, doc_versions (and its no-delete trigger), pastes, receivers,
// receiver_items, receiver_days, notary_public_keys, link_witnesses,
// work_rewards, work_review_fees, spend_limits, spend_limit_holds,
// spend_limit_usage, oauth_returning, fetch_denylist, fetch_host_days,
// credit_topups; columns works.reviewer, works.eligibility,
// work_rewards.reviewer, room_policies.top_level_per_day, wakeups.every,
// start_at, max_fires, fired_count, docs.kind ... paste_seq,
// receivers.dedupe_header, duplicates, receiver_items.dedupe_key; and their
// indexes. Before that, created after the migration committed:
// credit_topups (openTopup), hosted_keys_recovery (openHosted) and the
// moderation tables (moderation.New, only with MODERATION on); all three
// are here now, so the schema no longer depends on a flag.
//
// A later change to the schema is a new version: raise SchemaVersion, so an
// existing database runs this whole keyed sequence once more. A new table or
// index goes into its fragment (IF NOT EXISTS), a new column into a keyed
// column step after the existing ones of its table, and anything that
// rewrites data behind `if version < N`. Then pin the new schema
// (TestSchemaPinned in schema16_test.go says how).
func migrateSchema(tx *sql.Tx, version int) error {
	if version == 1 {
		if _, err := tx.Exec("ALTER TABLE changes ADD COLUMN urgent INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
	}
	if version > 0 && version < 3 {
		if _, err := tx.Exec(`ALTER TABLE events ADD COLUMN display_seq INTEGER NOT NULL DEFAULT 0;
 WITH numbering AS (SELECT e.seq,row_number() OVER(PARTITION BY CASE WHEN r.visibility='public' THEN 'public' ELSE 'room:'||r.name END ORDER BY e.seq) AS n FROM events e JOIN rooms r ON r.name=e.room)
 UPDATE events SET display_seq=(SELECT n FROM numbering WHERE numbering.seq=events.seq);`); err != nil {
			return err
		}
	}
	// Schema 9 is the SwarmMemo 1.0 vocabulary consolidation. Directory visibility
	// in readAgents is decided by matching operation names in the audit table, so
	// every historical audit row has to be renamed with the operations themselves;
	// otherwise every agent that became public through an 8-era identity.register
	// or peer.publish would silently vanish from the directory.
	if version > 0 && version < 9 {
		for _, rename := range [][2]string{
			{"identity.register", "agent.register"},
			{"identity.rotate", "agent.rotate"},
			{"peer.publish", "agent.profile.publish"},
			{"peer.remove", "agent.profile.remove"},
		} {
			if _, err := tx.Exec("UPDATE audit SET operation=? WHERE operation=?", rename[1], rename[0]); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(schema + peerSchema + workSchema + delegationSchema + webhookSchema + identityLinkSchema + roomPolicySchema + roomStyleSchema + forwardSchema + voteSchema + qualitySchema + honorSchema + inviteSchema +
		// RFC0012 §7 fragments, in this fixed order.
		design0Schema + ledger.Schema + services.Schema + trust.Schema + endorsementSchema +
		// RFC0013 (conversation_schema.go).
		conversationSchema +
		// Cross-network anonymous retries (anonretry.go): an index only.
		anonRetrySchema +
		// T56 OAuth for the hosted MCP assistant profile (oauth.go).
		oauthSchema +
		// RFC0014 §5 passkey key backups (keybackup.go).
		keyBackupSchema +
		// Identity link witnesses (identitywitness.go).
		identityWitnessSchema +
		// Schema 16: what used to be created after the migration committed.
		// Credit top-ups (topup.go), recovery lookups of hosted keys
		// (hosted.go) and the moderation engine's tables, which exist
		// whatever MODERATION says and stay empty while it is off.
		topupSchema + hostedRecoveryIndex + moderation.Schema +
		// Schema 17: MCP Events subscriptions and queue (mcpevents.go).
		mcpEventSchema); err != nil {
		return err
	}
	// Schema 8: rooms.private_access_epoch.
	if err := migratePrivateRead(tx); err != nil {
		return err
	}
	// Schema 11: signed post data (format, supersession).
	if err := migratePostData(tx); err != nil {
		return err
	}
	// Schema 12: events.hidden_by, who hid a message.
	if err := migrateHiddenBy(tx); err != nil {
		return err
	}
	// Schema 14: message provenance (events.via), room_policies.write_via
	// and room_policies.front_page.
	if err := migrateVia(tx); err != nil {
		return err
	}
	// Read indexes for the front page and rankings, and the ranking's flag
	// table (frontpage.go).
	if err := migrateReadIndexes(tx); err != nil {
		return err
	}
	// RFC0013: identities.custody and the room limits (conversation_schema.go).
	if err := migrateConversations(tx); err != nil {
		return err
	}
	// room_policies.top_level_per_day, after the room limits: the order
	// production added them in.
	if err := migrateRoomPolicyColumns(tx); err != nil {
		return err
	}
	// The services' later columns: recurring wake-ups, pastes.show_author
	// and the fold of pastes into docs with its one-time, id-keyed copy,
	// the receiver dedupe columns and index (services.MigrateSchema).
	if err := services.MigrateSchema(tx); err != nil {
		return err
	}
	// Schema 13: room styles (RFC0011), a table created above.
	// 1.24: x402_vetted.reason, who vetted ('' before: the operator).
	// 1.37: works.reviewer (the named reviewer's account) and
	// work_rewards.reviewer (the key whose verdict paid it).
	// 1.39: works.eligibility (who may claim; '' is open).
	for _, column := range []struct{ table, name string }{{"works", "attempt_grant_id"}, {"work_transitions", "delegation_id"}, {"x402_vetted", "reason"}, {"works", "reviewer"}, {"work_rewards", "reviewer"}, {"works", "eligibility"}} {
		var exists int
		if err := tx.QueryRow("SELECT count(*) FROM pragma_table_info(?) WHERE name=?", column.table, column.name).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := tx.Exec("ALTER TABLE " + column.table + " ADD COLUMN " + column.name + " TEXT NOT NULL DEFAULT ''"); err != nil {
				return err
			}
		}
	}
	// Development schema 3 briefly had attachment references without ordering.
	var positionColumns int
	if err := tx.QueryRow("SELECT count(*) FROM pragma_table_info('event_attachments') WHERE name='position'").Scan(&positionColumns); err != nil {
		return err
	}
	if positionColumns == 0 {
		if _, err := tx.Exec("ALTER TABLE event_attachments ADD COLUMN position INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
	}
	// Schema 15: the transparency log's tables, once every source table
	// exists; upkeepData backfills them.
	if _, err := tx.Exec(tlogSchema); err != nil {
		return err
	}
	_, err := tx.Exec(fmt.Sprintf("PRAGMA user_version=%d;", SchemaVersion))
	return err
}

// upkeepData runs at every start, after migrateSchema, in the same
// transaction. Each step is keyed on the data itself, so a second start
// changes nothing: the legacy blob expiry fix (once, recorded in meta), the
// display counters of rooms that predate them, and the transparency log's
// catch-up from its source cursors (the whole public history the first
// time; whatever a write path left for the background job after).
func upkeepData(tx *sql.Tx) error {
	if _, err := extendLegacyBlobs(tx, time.Now().Unix()); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO counters(scope,value) SELECT CASE WHEN r.visibility='public' THEN 'public' ELSE 'room:'||r.name END,max(e.display_seq) FROM events e JOIN rooms r ON r.name=e.room GROUP BY 1`); err != nil {
		return err
	}
	for {
		n, err := tlogCatchUp(context.Background(), tx)
		if err != nil {
			return fmt.Errorf("transparency log backfill: %w", err)
		}
		if n == 0 {
			break
		}
	}
	if _, err := tlogBackfillIdentity(context.Background(), tx); err != nil {
		return fmt.Errorf("transparency log identity backfill: %w", err)
	}
	return nil
}
