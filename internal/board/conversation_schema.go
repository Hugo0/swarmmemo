package board

import "database/sql"

// RFC 0013 schema: every table, index and column behind conversations, hosted
// identities, protections and sealed conversations, in one fragment and one
// migration (docs/rfcs/0013-conversations.md §2.1,
// §3.1, §5.2, §6). It is additive only: new tables and indexes, and columns
// keyed on pragma_table_info, so SchemaVersion does not change and a 1.23
// binary opens the database and ignores all of it.
//
// A conversation is a private room plus one conversations row. The invariant
// kept by setMemberState (conversation_members.go): a members row exists if
// and only if the conversation_members state is 'active', so every existing
// read, webhook and wake-up works unchanged.
const conversationSchema = `
CREATE TABLE IF NOT EXISTS conversations (
 room TEXT PRIMARY KEY REFERENCES rooms(name), kind TEXT NOT NULL CHECK(kind IN ('dm','group')),
 pair TEXT NOT NULL DEFAULT '', sealed INTEGER NOT NULL DEFAULT 0,
 member_epoch INTEGER NOT NULL DEFAULT 1, seal_epoch INTEGER NOT NULL DEFAULT 0,
 last_seq INTEGER NOT NULL DEFAULT 0, message_count INTEGER NOT NULL DEFAULT 0, created_by TEXT NOT NULL, created_at INTEGER NOT NULL,
 created_key TEXT NOT NULL DEFAULT '', created_signature TEXT NOT NULL DEFAULT '', created_payload TEXT NOT NULL DEFAULT '');
CREATE UNIQUE INDEX IF NOT EXISTS conversations_pair ON conversations(pair) WHERE pair<>'';
CREATE TABLE IF NOT EXISTS conversation_members (
 room TEXT NOT NULL REFERENCES rooms(name), account TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('active','requested','declined','left','removed')),
 role TEXT NOT NULL DEFAULT 'member' CHECK(role IN ('owner','member')), added_by TEXT NOT NULL DEFAULT '',
 acknowledged INTEGER NOT NULL DEFAULT 0, read_seq INTEGER NOT NULL DEFAULT 0, request_posts INTEGER NOT NULL DEFAULT 0,
 postage_hold TEXT NOT NULL DEFAULT '', changed_at INTEGER NOT NULL, PRIMARY KEY(room,account));
CREATE INDEX IF NOT EXISTS conversation_members_account ON conversation_members(account,state,room);
CREATE INDEX IF NOT EXISTS members_account ON members(account,room);
CREATE TABLE IF NOT EXISTS contact_blocks (account TEXT NOT NULL, blocked TEXT NOT NULL, created_at INTEGER NOT NULL, PRIMARY KEY(account,blocked));
CREATE TABLE IF NOT EXISTS messaging_settings (account TEXT PRIMARY KEY, settings TEXT NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS message_screens (event_id TEXT PRIMARY KEY REFERENCES events(id),
 state TEXT NOT NULL CHECK(state IN ('pass','flag','unscreened')), scores TEXT NOT NULL DEFAULT '{}', model TEXT NOT NULL DEFAULT '',
 payer TEXT NOT NULL DEFAULT '', cost INTEGER NOT NULL DEFAULT 0, screened_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS hosted_keys (
 account TEXT PRIMARY KEY, public_key TEXT NOT NULL UNIQUE, sealed_private_key TEXT NOT NULL, kek_id TEXT NOT NULL,
 created_at INTEGER NOT NULL, network TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('active','claimed','suspended')), claimed_at INTEGER NOT NULL DEFAULT 0,
 recovery_sha256 TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS hosted_tokens (
 token_sha256 TEXT PRIMARY KEY, token_id TEXT UNIQUE NOT NULL, account TEXT NOT NULL REFERENCES hosted_keys(account),
 label TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, last_used_at INTEGER NOT NULL DEFAULT 0, revoked_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS hosted_tokens_account ON hosted_tokens(account,revoked_at);
CREATE TABLE IF NOT EXISTS hosted_issuance (day INTEGER NOT NULL, network TEXT NOT NULL, count INTEGER NOT NULL, PRIMARY KEY(day,network));
CREATE TABLE IF NOT EXISTS seal_epochs (room TEXT NOT NULL REFERENCES rooms(name), epoch INTEGER NOT NULL, member_epoch INTEGER NOT NULL,
 created_by TEXT NOT NULL, public_key TEXT NOT NULL, signature TEXT NOT NULL, payload TEXT NOT NULL, created_at INTEGER NOT NULL,
 PRIMARY KEY(room,epoch));
CREATE TABLE IF NOT EXISTS seal_wraps (room TEXT NOT NULL, epoch INTEGER NOT NULL, account TEXT NOT NULL, kid TEXT NOT NULL,
 enc TEXT NOT NULL, ct TEXT NOT NULL, PRIMARY KEY(room,epoch,account));
`

// conversationColumns are the RFC 0013 columns on existing tables: who holds
// an identity's key (§2.4), the generic room limits room.policy.set takes
// (§3.1; 0 means unset), and the one account an invite is for (§3.2; ""
// is anyone holding the secret).
var conversationColumns = []struct{ table, name, definition string }{
	{"identities", "custody", "TEXT NOT NULL DEFAULT 'self' CHECK(custody IN ('self','hosted'))"},
	{"room_policies", "closed", "INTEGER NOT NULL DEFAULT 0"},
	{"room_policies", "closes_at", "INTEGER NOT NULL DEFAULT 0"},
	{"room_policies", "max_messages", "INTEGER NOT NULL DEFAULT 0"},
	{"room_invites", "target", "TEXT NOT NULL DEFAULT ''"},
}

// migrateConversations adds conversationColumns. Keyed on the columns
// themselves, not on user_version, so running it again changes nothing.
func migrateConversations(tx *sql.Tx) error {
	for _, column := range conversationColumns {
		var exists int
		if err := tx.QueryRow("SELECT count(*) FROM pragma_table_info(?) WHERE name=?", column.table, column.name).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := tx.Exec("ALTER TABLE " + column.table + " ADD COLUMN " + column.name + " " + column.definition); err != nil {
				return err
			}
		}
	}
	return nil
}
