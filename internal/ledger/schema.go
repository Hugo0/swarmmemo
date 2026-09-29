package ledger

// Schema is migration fragment B (RFC0012 §7), applied after design0Schema in
// the existing migration transaction. It only creates tables and indexes, so
// SchemaVersion does not change and an older binary ignores these tables.
//
// Beyond the §7 listing it adds allowance_usage (per-subject daily totals:
// spent, incoming, outgoing; the anonymous public view and quota.get read it),
// an index on claims by subject (the dormant-account breaker) and indexes on
// transfers by recipient and by time (stats).
const Schema = `
CREATE TABLE IF NOT EXISTS params (
 namespace TEXT NOT NULL, version INTEGER NOT NULL, body TEXT NOT NULL, sha256 TEXT NOT NULL,
 effective_at INTEGER NOT NULL, created_at INTEGER NOT NULL, actor TEXT NOT NULL, reason TEXT NOT NULL,
 PRIMARY KEY(namespace,version));
CREATE TABLE IF NOT EXISTS allowance_days (
 resource TEXT NOT NULL, day INTEGER NOT NULL, budget INTEGER NOT NULL, budget_effective INTEGER NOT NULL,
 unallocated INTEGER NOT NULL, spent_nonpaid INTEGER NOT NULL DEFAULT 0, spent_paid INTEGER NOT NULL DEFAULT 0,
 spill_done_at INTEGER NOT NULL DEFAULT 0, opened_at INTEGER NOT NULL, params_version INTEGER NOT NULL,
 PRIMARY KEY(resource,day));
CREATE TABLE IF NOT EXISTS allowance_pools (
 resource TEXT NOT NULL, day INTEGER NOT NULL, tier INTEGER NOT NULL CHECK(tier BETWEEN 0 AND 4),
 size INTEGER NOT NULL, want INTEGER NOT NULL, spill_in INTEGER NOT NULL DEFAULT 0, spill_out INTEGER NOT NULL DEFAULT 0,
 claimed INTEGER NOT NULL DEFAULT 0, lent INTEGER NOT NULL DEFAULT 0, borrowed INTEGER NOT NULL DEFAULT 0,
 expected_units INTEGER NOT NULL, expected_weight INTEGER NOT NULL, claimed_weight INTEGER NOT NULL DEFAULT 0,
 claimants INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(resource,day,tier));
CREATE TABLE IF NOT EXISTS allowance_claims (
 resource TEXT NOT NULL, day INTEGER NOT NULL, subject TEXT NOT NULL, tier INTEGER NOT NULL,
 weight INTEGER NOT NULL, root TEXT NOT NULL, entitlement INTEGER NOT NULL, granted INTEGER NOT NULL,
 source TEXT NOT NULL, lot_id INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL,
 PRIMARY KEY(resource,day,subject));
CREATE INDEX IF NOT EXISTS allowance_claims_root ON allowance_claims(resource,day,root);
CREATE INDEX IF NOT EXISTS allowance_claims_subject ON allowance_claims(resource,subject,day);
CREATE TABLE IF NOT EXISTS allowance_client_spend (
 resource TEXT NOT NULL, day INTEGER NOT NULL, subject TEXT NOT NULL, client TEXT NOT NULL,
 spent INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(resource,day,subject,client));
CREATE TABLE IF NOT EXISTS allowance_usage (
 resource TEXT NOT NULL, day INTEGER NOT NULL, subject TEXT NOT NULL,
 spent INTEGER NOT NULL DEFAULT 0, incoming INTEGER NOT NULL DEFAULT 0, outgoing INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(resource,day,subject));
CREATE TABLE IF NOT EXISTS ledger_lots (
 id INTEGER PRIMARY KEY AUTOINCREMENT, account TEXT NOT NULL, resource TEXT NOT NULL,
 bucket TEXT NOT NULL CHECK(bucket IN ('free','granted','earned','paid')),
 origin_tier INTEGER NOT NULL, origin_account TEXT NOT NULL, hops INTEGER NOT NULL DEFAULT 0,
 issued_day INTEGER NOT NULL, expires_at INTEGER NOT NULL DEFAULT 0, half_life_days INTEGER NOT NULL DEFAULT 0,
 decayed_day INTEGER NOT NULL, initial INTEGER NOT NULL, remaining INTEGER NOT NULL CHECK(remaining >= 0),
 held INTEGER NOT NULL DEFAULT 0 CHECK(held >= 0 AND held <= remaining),
 state TEXT NOT NULL CHECK(state IN ('live','spent','expired')), created_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS ledger_lots_live ON ledger_lots(account,resource,expires_at,id) WHERE state='live';
CREATE INDEX IF NOT EXISTS ledger_lots_expiry ON ledger_lots(expires_at) WHERE state='live' AND expires_at>0;
CREATE INDEX IF NOT EXISTS ledger_lots_decay ON ledger_lots(decayed_day) WHERE state='live' AND half_life_days>0;
CREATE TABLE IF NOT EXISTS ledger_entries (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, day INTEGER NOT NULL, created_at INTEGER NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN ('claim','grant','earn','topup','spend','reserve','commit','refund',
  'transfer_out','transfer_in','fee','expire','decay','forfeit','dividend','spill','lever','shadow')),
 account TEXT NOT NULL, counterparty TEXT NOT NULL DEFAULT '', resource TEXT NOT NULL,
 bucket TEXT NOT NULL DEFAULT '', lot_id INTEGER NOT NULL DEFAULT 0, amount INTEGER NOT NULL,
 hold_id TEXT NOT NULL DEFAULT '', service TEXT NOT NULL DEFAULT '', op TEXT NOT NULL DEFAULT '',
 public_ref TEXT NOT NULL DEFAULT '', params_version INTEGER NOT NULL, detail TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS ledger_entries_account ON ledger_entries(account,seq);
CREATE INDEX IF NOT EXISTS ledger_entries_day ON ledger_entries(day,kind);
CREATE TABLE IF NOT EXISTS ledger_holds (
 id TEXT PRIMARY KEY, account TEXT NOT NULL, resource TEXT NOT NULL, max_units INTEGER NOT NULL,
 used_units INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL CHECK(state IN ('held','committed','refunded','expired')),
 request_key TEXT NOT NULL, service TEXT NOT NULL, method TEXT NOT NULL,
 created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, UNIQUE(account,request_key));
CREATE INDEX IF NOT EXISTS ledger_holds_open ON ledger_holds(expires_at) WHERE state='held';
CREATE TABLE IF NOT EXISTS ledger_hold_parts (
 hold_id TEXT NOT NULL REFERENCES ledger_holds(id), lot_id INTEGER NOT NULL REFERENCES ledger_lots(id),
 units INTEGER NOT NULL, PRIMARY KEY(hold_id,lot_id));
CREATE TABLE IF NOT EXISTS ledger_transfers (
 id TEXT PRIMARY KEY, from_account TEXT NOT NULL, to_account TEXT NOT NULL, resource TEXT NOT NULL,
 amount INTEGER NOT NULL, fee INTEGER NOT NULL, hold_id TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL CHECK(state IN ('pending','done','cancelled')), created_at INTEGER NOT NULL,
 execute_at INTEGER NOT NULL, done_at INTEGER NOT NULL DEFAULT 0, cancelled_by TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS ledger_transfers_due ON ledger_transfers(execute_at) WHERE state='pending';
CREATE INDEX IF NOT EXISTS ledger_transfers_from ON ledger_transfers(from_account,created_at);
CREATE INDEX IF NOT EXISTS ledger_transfers_to ON ledger_transfers(to_account,created_at);
CREATE INDEX IF NOT EXISTS ledger_transfers_created ON ledger_transfers(created_at);
CREATE TABLE IF NOT EXISTS account_breakers (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, account TEXT NOT NULL, reason TEXT NOT NULL,
 cancel_key TEXT NOT NULL DEFAULT '', started_at INTEGER NOT NULL,
 transfer_until INTEGER NOT NULL, trust_until INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS account_breakers_account ON account_breakers(account,trust_until);
`
