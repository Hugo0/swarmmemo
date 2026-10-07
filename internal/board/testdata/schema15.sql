-- Production schema 15: the DDL of the production database's sqlite_master on
-- 2026-10-07 (1.40.x: before 1.41's notary_public_keys and the receiver dedupe
-- columns), in creation order, without SQLite's and Litestream's own tables.
-- Schema only, no rows. TestSchema16UpgradeMatchesFresh migrates it.
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE identities (
 id TEXT PRIMARY KEY, public_key TEXT UNIQUE NOT NULL, account TEXT NOT NULL,
 handle TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, last_seen INTEGER NOT NULL,
 successor TEXT NOT NULL DEFAULT '', custody TEXT NOT NULL DEFAULT 'self' CHECK(custody IN ('self','hosted')));
CREATE UNIQUE INDEX handles ON identities(handle) WHERE handle <> '';
CREATE INDEX identity_account ON identities(account);
CREATE TABLE rooms (
 name TEXT PRIMARY KEY, visibility TEXT NOT NULL CHECK(visibility IN ('public','private')),
 owner TEXT NOT NULL, created_at INTEGER NOT NULL, private_access_epoch TEXT NOT NULL DEFAULT '');
CREATE TABLE members (room TEXT NOT NULL, account TEXT NOT NULL,
 PRIMARY KEY(room,account), FOREIGN KEY(room) REFERENCES rooms(name));
CREATE TABLE events (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, display_seq INTEGER NOT NULL, id TEXT UNIQUE NOT NULL,
 room TEXT NOT NULL REFERENCES rooms(name), page TEXT NOT NULL, text TEXT NOT NULL,
 kind TEXT NOT NULL, author TEXT NOT NULL, account TEXT NOT NULL,
 handle TEXT NOT NULL, public_key TEXT NOT NULL, signature TEXT NOT NULL,
 payload TEXT NOT NULL, created_at INTEGER NOT NULL, hash TEXT NOT NULL,
 reply_to TEXT NOT NULL, recipient TEXT NOT NULL,
 hidden INTEGER NOT NULL DEFAULT 0, reason TEXT NOT NULL DEFAULT '', format TEXT NOT NULL DEFAULT '', supersedes TEXT NOT NULL DEFAULT '', origin TEXT NOT NULL DEFAULT '', hidden_by TEXT NOT NULL DEFAULT '', via TEXT NOT NULL DEFAULT '');
CREATE INDEX events_room_seq ON events(room,seq);
CREATE INDEX events_author ON events(account,seq);
CREATE INDEX events_recipient ON events(recipient,seq);
CREATE TABLE counters (scope TEXT PRIMARY KEY, value INTEGER NOT NULL);
CREATE TABLE blobs (
 id TEXT PRIMARY KEY,room TEXT NOT NULL REFERENCES rooms(name),account TEXT NOT NULL,
 filename TEXT NOT NULL,media_type TEXT NOT NULL,hash TEXT NOT NULL,size INTEGER NOT NULL,
 created_at INTEGER NOT NULL,expires_at INTEGER NOT NULL,deleted INTEGER NOT NULL DEFAULT 0,
 data BLOB);
CREATE INDEX blobs_expiry ON blobs(expires_at) WHERE deleted=0;
CREATE TABLE event_attachments (
 event_id TEXT NOT NULL REFERENCES events(id),blob_id TEXT NOT NULL REFERENCES blobs(id),
 position INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(event_id,blob_id));
CREATE INDEX attachments_blob ON event_attachments(blob_id,event_id);
CREATE TABLE changes (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, event_id TEXT NOT NULL REFERENCES events(id),
 changed_at INTEGER NOT NULL, urgent INTEGER NOT NULL DEFAULT 0);
CREATE INDEX changes_time ON changes(changed_at,seq);
CREATE TABLE export_changes (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, change_seq INTEGER UNIQUE NOT NULL REFERENCES changes(seq),
 event_id TEXT NOT NULL REFERENCES events(id), ready_at INTEGER NOT NULL);
CREATE TABLE requests (
 actor TEXT NOT NULL, request_key TEXT NOT NULL, digest TEXT NOT NULL, result TEXT NOT NULL,
 created_at INTEGER NOT NULL, PRIMARY KEY(actor,request_key));
CREATE TABLE quota (
 actor TEXT NOT NULL, day INTEGER NOT NULL, used INTEGER NOT NULL DEFAULT 0,
 incoming INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(actor,day));
CREATE TABLE reports (
 id TEXT PRIMARY KEY, event_id TEXT NOT NULL REFERENCES events(id), actor TEXT NOT NULL,
 reason TEXT NOT NULL, created_at INTEGER NOT NULL, resolved INTEGER NOT NULL DEFAULT 0,
 UNIQUE(event_id,actor));
CREATE TABLE audit (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, operation TEXT NOT NULL, actor TEXT NOT NULL,
 target TEXT NOT NULL, detail TEXT NOT NULL, created_at INTEGER NOT NULL);
CREATE TABLE leases (
 room TEXT NOT NULL REFERENCES rooms(name), name TEXT NOT NULL, account TEXT NOT NULL,
 fence INTEGER NOT NULL, expires_at INTEGER NOT NULL, PRIMARY KEY(room,name));
CREATE INDEX events_reply ON events(reply_to,room,seq);
CREATE INDEX events_page_directory ON events(room,page,seq) WHERE hidden=0;
CREATE TABLE peer_cards (
 account TEXT PRIMARY KEY, description TEXT NOT NULL, capabilities TEXT NOT NULL,
 availability TEXT NOT NULL, author TEXT NOT NULL REFERENCES identities(id),
 public_key TEXT NOT NULL, signature TEXT NOT NULL, payload TEXT NOT NULL,
 published_at INTEGER NOT NULL, expires_at INTEGER NOT NULL);
CREATE INDEX peer_expiry ON peer_cards(expires_at,account);
CREATE TABLE peer_capabilities (
 account TEXT NOT NULL REFERENCES peer_cards(account) ON DELETE CASCADE,
 capability TEXT NOT NULL, PRIMARY KEY(account,capability));
CREATE INDEX peer_capability ON peer_capabilities(capability,account);
CREATE TABLE works (
 id TEXT PRIMARY KEY REFERENCES events(id), requester TEXT NOT NULL,
 title TEXT NOT NULL, capabilities TEXT NOT NULL, state TEXT NOT NULL
 CHECK(state IN ('open','claimed','submitted','accepted','cancelled')),
 generation TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
 deadline INTEGER NOT NULL, fence INTEGER NOT NULL DEFAULT 0,
 worker TEXT NOT NULL DEFAULT '', claim_expires_at INTEGER NOT NULL DEFAULT 0,
 result_id TEXT NOT NULL DEFAULT '', history_seq INTEGER NOT NULL DEFAULT 0, attempt_grant_id TEXT NOT NULL DEFAULT '', reviewer TEXT NOT NULL DEFAULT '', eligibility TEXT NOT NULL DEFAULT '');
CREATE TABLE work_transitions (
 work_id TEXT NOT NULL REFERENCES works(id), sequence INTEGER NOT NULL,
 operation TEXT NOT NULL, author TEXT NOT NULL, public_key TEXT NOT NULL,
 signature TEXT NOT NULL, payload TEXT NOT NULL, accepted_at INTEGER NOT NULL,
 fence INTEGER NOT NULL, generation TEXT NOT NULL, state TEXT NOT NULL, delegation_id TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(work_id,sequence));
CREATE TABLE delegations (
 child_id TEXT PRIMARY KEY,public_key TEXT UNIQUE NOT NULL,parent_account TEXT NOT NULL,
 issuer_id TEXT NOT NULL REFERENCES identities(id),issuer_key TEXT NOT NULL,
 room TEXT NOT NULL REFERENCES rooms(name),operations TEXT NOT NULL,generation TEXT NOT NULL,
 created_at INTEGER NOT NULL,expires_at INTEGER NOT NULL,ceiling_bytes INTEGER NOT NULL,
 used_bytes INTEGER NOT NULL DEFAULT 0,revoked_at INTEGER NOT NULL DEFAULT 0,
 payload TEXT NOT NULL,signature TEXT NOT NULL,proof TEXT NOT NULL,
 revoke_payload TEXT NOT NULL DEFAULT '',revoke_signature TEXT NOT NULL DEFAULT '',
 revoke_generation TEXT NOT NULL DEFAULT '');
CREATE INDEX delegations_parent ON delegations(parent_account,child_id);
CREATE TABLE event_delegations (
 event_id TEXT PRIMARY KEY REFERENCES events(id),grant_id TEXT NOT NULL REFERENCES delegations(child_id));
CREATE TABLE private_read_grants (
 child_id TEXT PRIMARY KEY, public_key TEXT UNIQUE NOT NULL, owner_account TEXT NOT NULL,
 issuer_id TEXT NOT NULL REFERENCES identities(id), issuer_key TEXT NOT NULL,
 room TEXT NOT NULL REFERENCES rooms(name), generation TEXT NOT NULL, access_epoch TEXT NOT NULL,
 created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, revoked_at INTEGER NOT NULL DEFAULT 0,
 payload TEXT NOT NULL, signature TEXT NOT NULL, proof TEXT NOT NULL,
 revoke_payload TEXT NOT NULL DEFAULT '', revoke_signature TEXT NOT NULL DEFAULT '', revoke_generation TEXT NOT NULL DEFAULT '');
CREATE INDEX private_read_owner ON private_read_grants(owner_account, child_id);
CREATE INDEX private_read_room ON private_read_grants(room, child_id);
CREATE TABLE webhook_subscriptions (
 id TEXT PRIMARY KEY, account TEXT NOT NULL, created_by TEXT NOT NULL,
 url TEXT NOT NULL, secret TEXT NOT NULL, state TEXT NOT NULL CHECK(state IN ('pending','active','disabled')),
 challenge TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL,
 confirmed_at INTEGER NOT NULL DEFAULT 0, disabled_at INTEGER NOT NULL DEFAULT 0,
 failures INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '',
 UNIQUE(account,url));
CREATE INDEX webhook_account ON webhook_subscriptions(account,id);
CREATE INDEX webhook_active ON webhook_subscriptions(state,account);
CREATE TABLE webhook_deliveries (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT UNIQUE NOT NULL,
 subscription TEXT NOT NULL REFERENCES webhook_subscriptions(id) ON DELETE CASCADE,
 event_id TEXT NOT NULL, kind TEXT NOT NULL, body TEXT NOT NULL,
 attempts INTEGER NOT NULL DEFAULT 0, next_at INTEGER NOT NULL,
 leased_until INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL,
 UNIQUE(subscription,event_id));
CREATE INDEX webhook_queue ON webhook_deliveries(next_at,seq);
CREATE TABLE webhook_rates (
 account TEXT NOT NULL, hour INTEGER NOT NULL, count INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(account,hour));
CREATE TABLE identity_links (
 agent TEXT NOT NULL REFERENCES identities(id), kind TEXT NOT NULL, value TEXT NOT NULL,
 proof TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL CHECK(state IN ('claimed','proof_attached','verified','lapsed')),
 created_at INTEGER NOT NULL, checked_at INTEGER NOT NULL DEFAULT 0, lapsed_at INTEGER NOT NULL DEFAULT 0,
 failures INTEGER NOT NULL DEFAULT 0, attempted_at INTEGER NOT NULL DEFAULT 0,
 next_check_at INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(agent,kind,value));
CREATE INDEX identity_link_checks ON identity_links(next_check_at) WHERE next_check_at>0;
CREATE TABLE room_policies (
 room TEXT PRIMARY KEY REFERENCES rooms(name),
 write_policy TEXT NOT NULL CHECK(write_policy IN ('open','members','owner')),
 reply_policy TEXT NOT NULL CHECK(reply_policy IN ('anyone','members','none')),
 rules TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL, write_via TEXT NOT NULL DEFAULT '', front_page TEXT NOT NULL DEFAULT '', closed INTEGER NOT NULL DEFAULT 0, closes_at INTEGER NOT NULL DEFAULT 0, max_messages INTEGER NOT NULL DEFAULT 0, top_level_per_day INTEGER NOT NULL DEFAULT 0);
CREATE TABLE room_moderators (
 room TEXT NOT NULL REFERENCES rooms(name), account TEXT NOT NULL, added_at INTEGER NOT NULL,
 PRIMARY KEY(room,account));
CREATE TABLE room_moderation_log (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, room TEXT NOT NULL REFERENCES rooms(name),
 action TEXT NOT NULL, actor TEXT NOT NULL, target TEXT NOT NULL, reason TEXT NOT NULL,
 detail TEXT NOT NULL, public_key TEXT NOT NULL, signature TEXT NOT NULL, payload TEXT NOT NULL,
 created_at INTEGER NOT NULL);
CREATE INDEX room_moderation_log_room ON room_moderation_log(room,seq);
CREATE TABLE room_styles (
 room TEXT PRIMARY KEY REFERENCES rooms(name), css TEXT NOT NULL, sha256 TEXT NOT NULL, updated_at INTEGER NOT NULL);
CREATE UNIQUE INDEX events_supersedes ON events(supersedes) WHERE supersedes<>'';
CREATE INDEX events_origin ON events(origin,seq) WHERE origin<>'';
CREATE INDEX events_articles ON events(seq) WHERE format='markdown' AND reply_to='' AND supersedes='' AND public_key<>'';
CREATE TABLE event_forwards (
 event_id TEXT PRIMARY KEY REFERENCES events(id), mode TEXT NOT NULL, origin_service TEXT NOT NULL,
 origin_id TEXT NOT NULL, origin_author TEXT NOT NULL, origin_ref TEXT NOT NULL);
CREATE INDEX event_forwards_origin ON event_forwards(origin_service,origin_id);
CREATE TABLE votes (
 event_id TEXT NOT NULL REFERENCES events(id), account TEXT NOT NULL,
 value INTEGER NOT NULL CHECK(value IN (-1,1)), created_at INTEGER NOT NULL,
 PRIMARY KEY(event_id,account));
CREATE INDEX votes_account ON votes(account,created_at);
CREATE TABLE event_scores (
 event_id TEXT PRIMARY KEY REFERENCES events(id),
 ups INTEGER NOT NULL, downs INTEGER NOT NULL, score INTEGER NOT NULL);
CREATE INDEX event_scores_score ON event_scores(score,event_id);
CREATE TABLE honors (
 account TEXT NOT NULL, title TEXT NOT NULL, awarded_at INTEGER NOT NULL,
 PRIMARY KEY(account,title));
CREATE TABLE tier_grants (
 account TEXT PRIMARY KEY, tier INTEGER NOT NULL CHECK(tier IN (1,2)), reason TEXT NOT NULL,
 granted_at INTEGER NOT NULL, revoked_at INTEGER NOT NULL DEFAULT 0);
CREATE TABLE tier_grant_log (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, account TEXT NOT NULL, tier INTEGER NOT NULL,
 action TEXT NOT NULL CHECK(action IN ('grant','revoke')), reason TEXT NOT NULL, created_at INTEGER NOT NULL);
CREATE TABLE params (
 namespace TEXT NOT NULL, version INTEGER NOT NULL, body TEXT NOT NULL, sha256 TEXT NOT NULL,
 effective_at INTEGER NOT NULL, created_at INTEGER NOT NULL, actor TEXT NOT NULL, reason TEXT NOT NULL,
 PRIMARY KEY(namespace,version));
CREATE TABLE allowance_days (
 resource TEXT NOT NULL, day INTEGER NOT NULL, budget INTEGER NOT NULL, budget_effective INTEGER NOT NULL,
 unallocated INTEGER NOT NULL, spent_nonpaid INTEGER NOT NULL DEFAULT 0, spent_paid INTEGER NOT NULL DEFAULT 0,
 spill_done_at INTEGER NOT NULL DEFAULT 0, opened_at INTEGER NOT NULL, params_version INTEGER NOT NULL,
 PRIMARY KEY(resource,day));
CREATE TABLE allowance_pools (
 resource TEXT NOT NULL, day INTEGER NOT NULL, tier INTEGER NOT NULL CHECK(tier BETWEEN 0 AND 4),
 size INTEGER NOT NULL, want INTEGER NOT NULL, spill_in INTEGER NOT NULL DEFAULT 0, spill_out INTEGER NOT NULL DEFAULT 0,
 claimed INTEGER NOT NULL DEFAULT 0, lent INTEGER NOT NULL DEFAULT 0, borrowed INTEGER NOT NULL DEFAULT 0,
 expected_units INTEGER NOT NULL, expected_weight INTEGER NOT NULL, claimed_weight INTEGER NOT NULL DEFAULT 0,
 claimants INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(resource,day,tier));
CREATE TABLE allowance_claims (
 resource TEXT NOT NULL, day INTEGER NOT NULL, subject TEXT NOT NULL, tier INTEGER NOT NULL,
 weight INTEGER NOT NULL, root TEXT NOT NULL, entitlement INTEGER NOT NULL, granted INTEGER NOT NULL,
 source TEXT NOT NULL, lot_id INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL,
 PRIMARY KEY(resource,day,subject));
CREATE INDEX allowance_claims_root ON allowance_claims(resource,day,root);
CREATE INDEX allowance_claims_subject ON allowance_claims(resource,subject,day);
CREATE TABLE allowance_client_spend (
 resource TEXT NOT NULL, day INTEGER NOT NULL, subject TEXT NOT NULL, client TEXT NOT NULL,
 spent INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(resource,day,subject,client));
CREATE TABLE allowance_usage (
 resource TEXT NOT NULL, day INTEGER NOT NULL, subject TEXT NOT NULL,
 spent INTEGER NOT NULL DEFAULT 0, incoming INTEGER NOT NULL DEFAULT 0, outgoing INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(resource,day,subject));
CREATE TABLE ledger_lots (
 id INTEGER PRIMARY KEY AUTOINCREMENT, account TEXT NOT NULL, resource TEXT NOT NULL,
 bucket TEXT NOT NULL CHECK(bucket IN ('free','granted','earned','paid')),
 origin_tier INTEGER NOT NULL, origin_account TEXT NOT NULL, hops INTEGER NOT NULL DEFAULT 0,
 issued_day INTEGER NOT NULL, expires_at INTEGER NOT NULL DEFAULT 0, half_life_days INTEGER NOT NULL DEFAULT 0,
 decayed_day INTEGER NOT NULL, initial INTEGER NOT NULL, remaining INTEGER NOT NULL CHECK(remaining >= 0),
 held INTEGER NOT NULL DEFAULT 0 CHECK(held >= 0 AND held <= remaining),
 state TEXT NOT NULL CHECK(state IN ('live','spent','expired')), created_at INTEGER NOT NULL);
CREATE INDEX ledger_lots_live ON ledger_lots(account,resource,expires_at,id) WHERE state='live';
CREATE INDEX ledger_lots_expiry ON ledger_lots(expires_at) WHERE state='live' AND expires_at>0;
CREATE INDEX ledger_lots_decay ON ledger_lots(decayed_day) WHERE state='live' AND half_life_days>0;
CREATE TABLE ledger_entries (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, day INTEGER NOT NULL, created_at INTEGER NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN ('claim','grant','earn','topup','spend','reserve','commit','refund',
  'transfer_out','transfer_in','fee','expire','decay','forfeit','dividend','spill','lever','shadow')),
 account TEXT NOT NULL, counterparty TEXT NOT NULL DEFAULT '', resource TEXT NOT NULL,
 bucket TEXT NOT NULL DEFAULT '', lot_id INTEGER NOT NULL DEFAULT 0, amount INTEGER NOT NULL,
 hold_id TEXT NOT NULL DEFAULT '', service TEXT NOT NULL DEFAULT '', op TEXT NOT NULL DEFAULT '',
 public_ref TEXT NOT NULL DEFAULT '', params_version INTEGER NOT NULL, detail TEXT NOT NULL DEFAULT '');
CREATE INDEX ledger_entries_account ON ledger_entries(account,seq);
CREATE INDEX ledger_entries_day ON ledger_entries(day,kind);
CREATE TABLE ledger_holds (
 id TEXT PRIMARY KEY, account TEXT NOT NULL, resource TEXT NOT NULL, max_units INTEGER NOT NULL,
 used_units INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL CHECK(state IN ('held','committed','refunded','expired')),
 request_key TEXT NOT NULL, service TEXT NOT NULL, method TEXT NOT NULL,
 created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, UNIQUE(account,request_key));
CREATE INDEX ledger_holds_open ON ledger_holds(expires_at) WHERE state='held';
CREATE TABLE ledger_hold_parts (
 hold_id TEXT NOT NULL REFERENCES ledger_holds(id), lot_id INTEGER NOT NULL REFERENCES ledger_lots(id),
 units INTEGER NOT NULL, PRIMARY KEY(hold_id,lot_id));
CREATE TABLE ledger_transfers (
 id TEXT PRIMARY KEY, from_account TEXT NOT NULL, to_account TEXT NOT NULL, resource TEXT NOT NULL,
 amount INTEGER NOT NULL, fee INTEGER NOT NULL, hold_id TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL CHECK(state IN ('pending','done','cancelled')), created_at INTEGER NOT NULL,
 execute_at INTEGER NOT NULL, done_at INTEGER NOT NULL DEFAULT 0, cancelled_by TEXT NOT NULL DEFAULT '');
CREATE INDEX ledger_transfers_due ON ledger_transfers(execute_at) WHERE state='pending';
CREATE INDEX ledger_transfers_from ON ledger_transfers(from_account,created_at);
CREATE INDEX ledger_transfers_to ON ledger_transfers(to_account,created_at);
CREATE INDEX ledger_transfers_created ON ledger_transfers(created_at);
CREATE TABLE account_breakers (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, account TEXT NOT NULL, reason TEXT NOT NULL,
 cancel_key TEXT NOT NULL DEFAULT '', started_at INTEGER NOT NULL,
 transfer_until INTEGER NOT NULL, trust_until INTEGER NOT NULL);
CREATE INDEX account_breakers_account ON account_breakers(account,trust_until);
CREATE TABLE service_calls (
 id TEXT PRIMARY KEY, account TEXT NOT NULL, service TEXT NOT NULL, method TEXT NOT NULL,
 request_key TEXT NOT NULL, hold_id TEXT NOT NULL, resource TEXT NOT NULL, cost INTEGER NOT NULL DEFAULT 0,
 max_cost INTEGER NOT NULL DEFAULT 0, mode TEXT NOT NULL DEFAULT 'local' CHECK(mode IN ('local','remote','async')),
 state TEXT NOT NULL CHECK(state IN ('running','done','failed','unknown')), public TEXT NOT NULL DEFAULT '{}',
 body TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
 prices_version INTEGER NOT NULL, created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL DEFAULT 0,
 finished_at INTEGER NOT NULL DEFAULT 0, UNIQUE(account,request_key));
CREATE INDEX service_calls_account ON service_calls(account,created_at);
CREATE INDEX service_calls_running ON service_calls(expires_at) WHERE state='running';
CREATE TABLE service_jobs (
 id TEXT PRIMARY KEY, call_id TEXT NOT NULL REFERENCES service_calls(id), service TEXT NOT NULL,
 account TEXT NOT NULL, data TEXT NOT NULL DEFAULT '',
 due_at INTEGER NOT NULL, state TEXT NOT NULL CHECK(state IN ('scheduled','running','done','failed','cancelled')),
 attempts INTEGER NOT NULL DEFAULT 0, leased_until INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL);
CREATE INDEX service_jobs_due ON service_jobs(due_at) WHERE state='scheduled';
CREATE INDEX service_jobs_leased ON service_jobs(leased_until) WHERE state='running';
CREATE TABLE memory_items (
 account TEXT NOT NULL, key TEXT NOT NULL, value TEXT NOT NULL,
 visibility TEXT NOT NULL CHECK(visibility IN ('private','public')), bytes INTEGER NOT NULL,
 version INTEGER NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, PRIMARY KEY(account,key));
CREATE TABLE memory_usage (account TEXT PRIMARY KEY, items INTEGER NOT NULL, bytes INTEGER NOT NULL);
CREATE TABLE wakeups (
 id TEXT PRIMARY KEY, account TEXT NOT NULL, key TEXT NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN ('time','reply','mention','room')), room TEXT NOT NULL DEFAULT '',
 due_at INTEGER NOT NULL DEFAULT 0, until INTEGER NOT NULL DEFAULT 0, from_seq INTEGER NOT NULL DEFAULT 0,
 state TEXT NOT NULL CHECK(state IN ('active','fired','cancelled','expired')),
 created_at INTEGER NOT NULL, finished_at INTEGER NOT NULL DEFAULT 0, event TEXT NOT NULL DEFAULT '', every INTEGER NOT NULL DEFAULT 0, start_at INTEGER NOT NULL DEFAULT 0, max_fires INTEGER NOT NULL DEFAULT 0, fired_count INTEGER NOT NULL DEFAULT 0);
CREATE UNIQUE INDEX wakeups_key ON wakeups(account,key) WHERE state='active';
CREATE INDEX wakeups_account ON wakeups(account,state,created_at);
CREATE INDEX wakeups_due ON wakeups(due_at) WHERE state='active' AND kind='time';
CREATE INDEX wakeups_until ON wakeups(until) WHERE state='active' AND kind<>'time';
CREATE INDEX wakeups_watch ON wakeups(kind,account) WHERE state='active';
CREATE INDEX wakeups_room ON wakeups(room) WHERE state='active' AND kind='room';
CREATE TABLE wakeup_notices (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, wakeup TEXT NOT NULL, account TEXT NOT NULL, kind TEXT NOT NULL,
 event TEXT NOT NULL DEFAULT '', room TEXT NOT NULL DEFAULT '', due_at INTEGER NOT NULL DEFAULT 0,
 fired_at INTEGER NOT NULL, event_seq INTEGER NOT NULL, late INTEGER NOT NULL DEFAULT 0);
CREATE INDEX wakeup_notices_account ON wakeup_notices(account,seq);
CREATE TABLE wakeup_scan (id INTEGER PRIMARY KEY CHECK(id=1), after_seq INTEGER NOT NULL);
CREATE TABLE notary_receipts (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, hash TEXT NOT NULL UNIQUE, time INTEGER NOT NULL, account TEXT NOT NULL,
 key_id TEXT NOT NULL DEFAULT '', payload TEXT NOT NULL DEFAULT '', signature TEXT NOT NULL DEFAULT '');
CREATE INDEX notary_receipts_account ON notary_receipts(account,time);
CREATE TABLE inference_spend (
 upstream TEXT NOT NULL, day INTEGER NOT NULL, units INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(upstream,day));
CREATE TABLE x402_payments (
 id TEXT PRIMARY KEY, account TEXT NOT NULL, request_key TEXT NOT NULL, resource TEXT NOT NULL,
 day INTEGER NOT NULL, amount INTEGER NOT NULL CHECK(amount > 0), network TEXT NOT NULL, asset TEXT NOT NULL,
 pay_to TEXT NOT NULL, nonce TEXT NOT NULL UNIQUE, valid_before INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('signed','paid','rejected','unknown','failed')),
 http_status INTEGER NOT NULL DEFAULT 0, response_bytes INTEGER NOT NULL DEFAULT 0,
 transaction_hash TEXT NOT NULL DEFAULT '', reason TEXT NOT NULL DEFAULT '',
 allowlist_version INTEGER NOT NULL, created_at INTEGER NOT NULL, finished_at INTEGER NOT NULL DEFAULT 0,
 UNIQUE(account,request_key));
CREATE INDEX x402_payments_day ON x402_payments(day,account);
CREATE TABLE public_data_cache (
 dataset TEXT NOT NULL, key TEXT NOT NULL, body TEXT NOT NULL, source_url TEXT NOT NULL,
 fetched_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, PRIMARY KEY(dataset,key));
CREATE TABLE runs_code (sha256 TEXT PRIMARY KEY, language TEXT NOT NULL, code TEXT NOT NULL, first_seen INTEGER NOT NULL);
CREATE TABLE runs_log (
 run_id TEXT PRIMARY KEY, account TEXT NOT NULL, request_key TEXT NOT NULL, created_at INTEGER NOT NULL, finished_at INTEGER NOT NULL DEFAULT 0,
 language TEXT NOT NULL, code_sha256 TEXT NOT NULL, input TEXT NOT NULL, input_sha256 TEXT NOT NULL,
 network_requested INTEGER NOT NULL, network TEXT NOT NULL DEFAULT 'off', network_note TEXT NOT NULL DEFAULT '',
 cpu_ms_limit INTEGER NOT NULL, wall_ms_limit INTEGER NOT NULL, status TEXT NOT NULL, error TEXT NOT NULL DEFAULT '',
 output TEXT NOT NULL DEFAULT '', output_sha256 TEXT NOT NULL DEFAULT '', egress TEXT NOT NULL DEFAULT '', egress_sha256 TEXT NOT NULL DEFAULT '',
 cpu_ms INTEGER NOT NULL DEFAULT 0, cpu_source TEXT NOT NULL DEFAULT '', wall_ms INTEGER NOT NULL DEFAULT 0,
 egress_bytes INTEGER NOT NULL DEFAULT 0, cost INTEGER NOT NULL DEFAULT 0,
 code_decision TEXT NOT NULL DEFAULT '', egress_decision TEXT NOT NULL DEFAULT '', flagged INTEGER NOT NULL DEFAULT 0,
 receipt TEXT NOT NULL DEFAULT '', UNIQUE(account,request_key));
CREATE INDEX runs_log_account ON runs_log(account,created_at);
CREATE INDEX runs_log_flagged ON runs_log(created_at) WHERE flagged=1;
CREATE TABLE runs_usage (day INTEGER NOT NULL, account TEXT NOT NULL, runs INTEGER NOT NULL DEFAULT 0,
 cpu_ms INTEGER NOT NULL DEFAULT 0, egress_bytes INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(day,account));
CREATE TABLE runs_reviews (code_sha256 TEXT PRIMARY KEY, state TEXT NOT NULL CHECK(state IN ('held','approved','blocked')),
 reason TEXT NOT NULL DEFAULT '', account TEXT NOT NULL, created_at INTEGER NOT NULL, decided_at INTEGER NOT NULL DEFAULT 0);
CREATE TABLE runs_network_blocks (account TEXT PRIMARY KEY, reason TEXT NOT NULL, run_id TEXT NOT NULL, created_at INTEGER NOT NULL,
 lifted_at INTEGER NOT NULL DEFAULT 0);
CREATE TABLE trust_runs (
 id INTEGER PRIMARY KEY AUTOINCREMENT, as_of INTEGER NOT NULL, params_version INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('running','done','aborted')), inputs TEXT NOT NULL,
 nodes INTEGER NOT NULL DEFAULT 0, edges INTEGER NOT NULL DEFAULT 0, work INTEGER NOT NULL DEFAULT 0,
 capture_bound TEXT NOT NULL DEFAULT '', output_sha256 TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
 started_at INTEGER NOT NULL, finished_at INTEGER NOT NULL DEFAULT 0);
CREATE TABLE trust_scores (
 run_id INTEGER NOT NULL REFERENCES trust_runs(id), account TEXT NOT NULL, root TEXT NOT NULL,
 proof_collateral INTEGER NOT NULL, flow_a INTEGER NOT NULL, flow_b INTEGER NOT NULL, flow INTEGER NOT NULL,
 collateral INTEGER NOT NULL, tier INTEGER NOT NULL, weight_ppm INTEGER NOT NULL, parts TEXT NOT NULL,
 PRIMARY KEY(run_id,account));
CREATE INDEX trust_scores_account ON trust_scores(account,run_id);
CREATE TABLE trust_current (account TEXT PRIMARY KEY, run_id INTEGER NOT NULL);
CREATE TABLE trust_evidence (
 id TEXT PRIMARY KEY, run_id INTEGER NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('funnel','ring')),
 members TEXT NOT NULL, detail TEXT NOT NULL, detector_version INTEGER NOT NULL,
 created_at INTEGER NOT NULL, lifted_at INTEGER NOT NULL DEFAULT 0, lift_reason TEXT NOT NULL DEFAULT '');
CREATE TABLE trust_penalties (
 id INTEGER PRIMARY KEY AUTOINCREMENT, account TEXT NOT NULL, evidence_id TEXT NOT NULL REFERENCES trust_evidence(id),
 fraction_ppm INTEGER NOT NULL, starts_at INTEGER NOT NULL, ends_at INTEGER NOT NULL, UNIQUE(account,evidence_id));
CREATE INDEX trust_penalties_account ON trust_penalties(account,ends_at);
CREATE TABLE trust_sponsorships (
 invitee TEXT PRIMARY KEY, sponsor TEXT NOT NULL, record_seq INTEGER NOT NULL,
 created_at INTEGER NOT NULL, ends_at INTEGER NOT NULL, high_water INTEGER NOT NULL DEFAULT 0);
CREATE TABLE trust_dividends (
 run_id INTEGER NOT NULL, sponsor TEXT NOT NULL, invitee TEXT NOT NULL, units INTEGER NOT NULL,
 paid INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(run_id,sponsor,invitee));
CREATE INDEX trust_sponsorships_sponsor ON trust_sponsorships(sponsor);
CREATE INDEX trust_dividends_sponsor ON trust_dividends(sponsor,run_id);
CREATE TABLE trust_run_snapshots (
 run_id INTEGER PRIMARY KEY REFERENCES trust_runs(id), sha256 TEXT NOT NULL, bytes INTEGER NOT NULL, gz BLOB NOT NULL);
CREATE TABLE endorsement_records (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL CHECK(kind IN ('vote','vouch')),
 class TEXT NOT NULL CHECK(class IN ('signed','unsigned')), event_id TEXT NOT NULL DEFAULT '',
 target_account TEXT NOT NULL DEFAULT '', voter_account TEXT NOT NULL, voter_id TEXT NOT NULL,
 public_key TEXT NOT NULL, value INTEGER NOT NULL CHECK(value IN (-1,0,1)), sponsor INTEGER NOT NULL DEFAULT 0,
 signed_payload TEXT NOT NULL, signature TEXT NOT NULL, via TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL);
CREATE INDEX endorsement_voter ON endorsement_records(voter_account,kind,created_at);
CREATE INDEX endorsement_event ON endorsement_records(event_id) WHERE kind='vote';
CREATE INDEX endorsement_target ON endorsement_records(target_account) WHERE kind='vouch';
CREATE TABLE vouches (
 voter_account TEXT NOT NULL, target_account TEXT NOT NULL, value INTEGER NOT NULL, sponsor INTEGER NOT NULL,
 record_seq INTEGER NOT NULL, created_at INTEGER NOT NULL, PRIMARY KEY(voter_account,target_account));
CREATE INDEX vouches_target ON vouches(target_account,value);
CREATE TABLE levers (
 name TEXT PRIMARY KEY, state TEXT NOT NULL CHECK(state IN ('pulled','released')), args TEXT NOT NULL,
 pulled_at INTEGER NOT NULL, until INTEGER NOT NULL DEFAULT 0, released_at INTEGER NOT NULL DEFAULT 0,
 actor TEXT NOT NULL, reason TEXT NOT NULL);
CREATE TABLE lever_log (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, action TEXT NOT NULL CHECK(action IN ('pull','release','expire')),
 args TEXT NOT NULL, actor TEXT NOT NULL, reason TEXT NOT NULL, created_at INTEGER NOT NULL);
CREATE TABLE blocked_prefixes (
 id INTEGER PRIMARY KEY AUTOINCREMENT, cidr TEXT NOT NULL, bits INTEGER NOT NULL, keyed_hash TEXT NOT NULL,
 reason TEXT NOT NULL, created_at INTEGER NOT NULL, until INTEGER NOT NULL DEFAULT 0, released_at INTEGER NOT NULL DEFAULT 0);
CREATE TABLE moderation_decisions (
 id TEXT PRIMARY KEY, surface TEXT NOT NULL, subject TEXT NOT NULL, agent TEXT NOT NULL DEFAULT '',
 action TEXT NOT NULL, proposed TEXT NOT NULL, category TEXT NOT NULL DEFAULT '', p REAL NOT NULL DEFAULT 0,
 scores TEXT NOT NULL DEFAULT '{}', policy_version INTEGER NOT NULL, policy_sha256 TEXT NOT NULL DEFAULT '',
 model TEXT NOT NULL DEFAULT '', reason TEXT NOT NULL DEFAULT '', burst INTEGER NOT NULL DEFAULT 0,
 hard INTEGER NOT NULL DEFAULT 0, degraded TEXT NOT NULL DEFAULT '', content_sha256 TEXT NOT NULL DEFAULT '',
 content_bytes INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL);
CREATE INDEX moderation_decisions_surface ON moderation_decisions(surface,created_at);
CREATE INDEX moderation_decisions_agent ON moderation_decisions(surface,agent,created_at);
CREATE INDEX moderation_decisions_subject ON moderation_decisions(subject);
CREATE INDEX moderation_decisions_day ON moderation_decisions(created_at);
CREATE TABLE moderation_queue (
 id TEXT PRIMARY KEY, decision_id TEXT NOT NULL UNIQUE REFERENCES moderation_decisions(id),
 surface TEXT NOT NULL, subject TEXT NOT NULL, cause TEXT NOT NULL CHECK(cause IN ('flag','hold','burst')),
 state TEXT NOT NULL CHECK(state IN ('pending','approved','rejected')), content TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, resolved_at INTEGER NOT NULL DEFAULT 0, resolved_by TEXT NOT NULL DEFAULT '',
 note TEXT NOT NULL DEFAULT '');
CREATE INDEX moderation_queue_pending ON moderation_queue(created_at) WHERE state='pending';
CREATE INDEX moderation_queue_subject ON moderation_queue(subject);
CREATE TABLE moderation_jobs (
 id TEXT PRIMARY KEY, surface TEXT NOT NULL, subject TEXT NOT NULL, agent TEXT NOT NULL DEFAULT '',
 room TEXT NOT NULL DEFAULT '', signed INTEGER NOT NULL DEFAULT 0, content TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL CHECK(state IN ('pending','running','done','failed')), attempts INTEGER NOT NULL DEFAULT 0,
 leased_until INTEGER NOT NULL DEFAULT 0, decision_id TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL);
CREATE INDEX moderation_jobs_due ON moderation_jobs(created_at) WHERE state IN ('pending','running');
CREATE TABLE moderation_spend (
 day INTEGER PRIMARY KEY, spent_microusd INTEGER NOT NULL DEFAULT 0, calls INTEGER NOT NULL DEFAULT 0,
 tokens INTEGER NOT NULL DEFAULT 0);
CREATE TABLE moderation_domains (
 site TEXT PRIMARY KEY, state TEXT NOT NULL CHECK(state IN ('seen','allowed','denied')),
 first_seen INTEGER NOT NULL, first_agent TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL);
CREATE TABLE moderation_alerts (
 id INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, surface TEXT NOT NULL DEFAULT '',
 detail TEXT NOT NULL, created_at INTEGER NOT NULL);
CREATE INDEX moderation_alerts_kind ON moderation_alerts(kind,surface,created_at);
CREATE TABLE moderation_screen_spend (
 day INTEGER PRIMARY KEY, spent_microusd INTEGER NOT NULL DEFAULT 0, calls INTEGER NOT NULL DEFAULT 0,
 tokens INTEGER NOT NULL DEFAULT 0);
CREATE TABLE event_quality (
 event_id TEXT PRIMARY KEY REFERENCES events(id),
 quality REAL NOT NULL CHECK(quality>=0 AND quality<=1),
 model TEXT NOT NULL, scored_at INTEGER NOT NULL);
CREATE TABLE x402_catalogue (
 id TEXT PRIMARY KEY, bundler TEXT NOT NULL, url TEXT NOT NULL, method TEXT NOT NULL, pay_to TEXT NOT NULL,
 amount INTEGER NOT NULL, query TEXT NOT NULL DEFAULT '[]', body INTEGER NOT NULL DEFAULT 0,
 category TEXT NOT NULL DEFAULT '', summary TEXT NOT NULL DEFAULT '', source TEXT NOT NULL DEFAULT '',
 payers INTEGER NOT NULL DEFAULT 0, calls INTEGER NOT NULL DEFAULT 0, curated INTEGER NOT NULL DEFAULT 0,
 first_seen INTEGER NOT NULL, last_seen INTEGER NOT NULL);
CREATE INDEX x402_catalogue_seen ON x402_catalogue(last_seen);
CREATE TABLE x402_resource_days (
 resource TEXT NOT NULL, day INTEGER NOT NULL, ok INTEGER NOT NULL DEFAULT 0, failed INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(resource, day));
CREATE INDEX x402_resource_days_day ON x402_resource_days(day);
CREATE TABLE x402_vetted (
 id TEXT PRIMARY KEY, url TEXT NOT NULL, method TEXT NOT NULL, pay_to TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('vetted','unvetted')), changed_at INTEGER NOT NULL, reason TEXT NOT NULL DEFAULT '');
CREATE TABLE x402_denied (
 kind TEXT NOT NULL CHECK(kind IN ('pay_to','url')), value TEXT NOT NULL, reason TEXT NOT NULL,
 denied_at INTEGER NOT NULL, cleared_at INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(kind, value));
CREATE TABLE event_flags (
 event_id TEXT PRIMARY KEY REFERENCES events(id),
 root TEXT NOT NULL, flagged_at INTEGER NOT NULL);
CREATE INDEX event_flags_root ON event_flags(root);
CREATE INDEX events_front1 ON events(seq) WHERE substr(room,1,1)<>'@' AND room NOT IN ('bounties','sandbox','boards','commerce');
CREATE INDEX events_front_top1 ON events(seq) WHERE reply_to='' AND supersedes='' AND hidden=0 AND kind NOT IN ('simulation','imported') AND substr(room,1,1)<>'@' AND room NOT IN ('bounties','sandbox','boards','commerce');
CREATE INDEX events_top1 ON events(seq) WHERE reply_to='' AND supersedes='' AND hidden=0 AND kind NOT IN ('simulation','imported');
CREATE INDEX room_policies_front ON room_policies(front_page);
CREATE TABLE x402_summary_screens (
 hash TEXT PRIMARY KEY, verdict TEXT NOT NULL CHECK(verdict IN ('pass','flag')), screened_at INTEGER NOT NULL);
CREATE TABLE room_invites (
 secret_sha256 TEXT PRIMARY KEY, room TEXT NOT NULL REFERENCES rooms(name),
 created_by TEXT NOT NULL, created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL,
 used_by TEXT NOT NULL DEFAULT '', used_at INTEGER NOT NULL DEFAULT 0, target TEXT NOT NULL DEFAULT '');
CREATE INDEX room_invites_room ON room_invites(room,expires_at);
CREATE INDEX ledger_holds_account_open ON ledger_holds(account,service) WHERE state='held';
CREATE TABLE conversations (
 room TEXT PRIMARY KEY REFERENCES rooms(name), kind TEXT NOT NULL CHECK(kind IN ('dm','group')),
 pair TEXT NOT NULL DEFAULT '', sealed INTEGER NOT NULL DEFAULT 0,
 member_epoch INTEGER NOT NULL DEFAULT 1, seal_epoch INTEGER NOT NULL DEFAULT 0,
 last_seq INTEGER NOT NULL DEFAULT 0, message_count INTEGER NOT NULL DEFAULT 0, created_by TEXT NOT NULL, created_at INTEGER NOT NULL,
 created_key TEXT NOT NULL DEFAULT '', created_signature TEXT NOT NULL DEFAULT '', created_payload TEXT NOT NULL DEFAULT '');
CREATE UNIQUE INDEX conversations_pair ON conversations(pair) WHERE pair<>'';
CREATE TABLE conversation_members (
 room TEXT NOT NULL REFERENCES rooms(name), account TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('active','requested','declined','left','removed')),
 role TEXT NOT NULL DEFAULT 'member' CHECK(role IN ('owner','member')), added_by TEXT NOT NULL DEFAULT '',
 acknowledged INTEGER NOT NULL DEFAULT 0, read_seq INTEGER NOT NULL DEFAULT 0, request_posts INTEGER NOT NULL DEFAULT 0,
 postage_hold TEXT NOT NULL DEFAULT '', changed_at INTEGER NOT NULL, PRIMARY KEY(room,account));
CREATE INDEX conversation_members_account ON conversation_members(account,state,room);
CREATE INDEX members_account ON members(account,room);
CREATE TABLE contact_blocks (account TEXT NOT NULL, blocked TEXT NOT NULL, created_at INTEGER NOT NULL, PRIMARY KEY(account,blocked));
CREATE TABLE messaging_settings (account TEXT PRIMARY KEY, settings TEXT NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE message_screens (event_id TEXT PRIMARY KEY REFERENCES events(id),
 state TEXT NOT NULL CHECK(state IN ('pass','flag','unscreened')), scores TEXT NOT NULL DEFAULT '{}', model TEXT NOT NULL DEFAULT '',
 payer TEXT NOT NULL DEFAULT '', cost INTEGER NOT NULL DEFAULT 0, screened_at INTEGER NOT NULL);
CREATE TABLE hosted_keys (
 account TEXT PRIMARY KEY, public_key TEXT NOT NULL UNIQUE, sealed_private_key TEXT NOT NULL, kek_id TEXT NOT NULL,
 created_at INTEGER NOT NULL, network TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('active','claimed','suspended')), claimed_at INTEGER NOT NULL DEFAULT 0,
 recovery_sha256 TEXT NOT NULL);
CREATE TABLE hosted_tokens (
 token_sha256 TEXT PRIMARY KEY, token_id TEXT UNIQUE NOT NULL, account TEXT NOT NULL REFERENCES hosted_keys(account),
 label TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, last_used_at INTEGER NOT NULL DEFAULT 0, revoked_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX hosted_tokens_account ON hosted_tokens(account,revoked_at);
CREATE TABLE hosted_issuance (day INTEGER NOT NULL, network TEXT NOT NULL, count INTEGER NOT NULL, PRIMARY KEY(day,network));
CREATE TABLE seal_epochs (room TEXT NOT NULL REFERENCES rooms(name), epoch INTEGER NOT NULL, member_epoch INTEGER NOT NULL,
 created_by TEXT NOT NULL, public_key TEXT NOT NULL, signature TEXT NOT NULL, payload TEXT NOT NULL, created_at INTEGER NOT NULL,
 PRIMARY KEY(room,epoch));
CREATE TABLE seal_wraps (room TEXT NOT NULL, epoch INTEGER NOT NULL, account TEXT NOT NULL, kid TEXT NOT NULL,
 enc TEXT NOT NULL, ct TEXT NOT NULL, PRIMARY KEY(room,epoch,account));
CREATE TABLE moderation_conversation_spend (
 day INTEGER PRIMARY KEY, spent_microusd INTEGER NOT NULL DEFAULT 0, calls INTEGER NOT NULL DEFAULT 0,
 tokens INTEGER NOT NULL DEFAULT 0);
CREATE INDEX hosted_keys_recovery ON hosted_keys(recovery_sha256);
CREATE TABLE oauth_clients (
 client_id TEXT PRIMARY KEY, client_name TEXT NOT NULL, redirect_uris TEXT NOT NULL,
 created_at INTEGER NOT NULL, network TEXT NOT NULL);
CREATE INDEX oauth_clients_network ON oauth_clients(network,created_at);
CREATE INDEX oauth_clients_created ON oauth_clients(created_at);
CREATE TABLE oauth_codes (
 code_sha256 TEXT PRIMARY KEY, client_id TEXT NOT NULL, client_name TEXT NOT NULL, redirect_uri TEXT NOT NULL,
 code_challenge TEXT NOT NULL, resource TEXT NOT NULL, scope TEXT NOT NULL, account TEXT NOT NULL,
 created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, used_at INTEGER NOT NULL DEFAULT 0, family_id TEXT NOT NULL DEFAULT '');
CREATE TABLE oauth_families (
 family_id TEXT PRIMARY KEY, account TEXT NOT NULL, client_id TEXT NOT NULL, client_name TEXT NOT NULL,
 resource TEXT NOT NULL, scope TEXT NOT NULL, created_at INTEGER NOT NULL,
 access_sha256 TEXT NOT NULL UNIQUE, access_expires_at INTEGER NOT NULL,
 refresh_sha256 TEXT NOT NULL UNIQUE, refresh_expires_at INTEGER NOT NULL,
 rotated_at INTEGER NOT NULL DEFAULT 0, revoked_at INTEGER NOT NULL DEFAULT 0, revoked_reason TEXT NOT NULL DEFAULT '');
CREATE INDEX oauth_families_account ON oauth_families(account,revoked_at);
CREATE TABLE oauth_refresh_used (refresh_sha256 TEXT PRIMARY KEY, family_id TEXT NOT NULL, used_at INTEGER NOT NULL);
CREATE TABLE key_backups (
 account TEXT PRIMARY KEY, key_id TEXT NOT NULL, scheme TEXT NOT NULL,
 credential_sha256 TEXT NOT NULL, salt TEXT NOT NULL, iv TEXT NOT NULL, ciphertext TEXT NOT NULL,
 label TEXT NOT NULL DEFAULT '', version INTEGER NOT NULL,
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE frames_tools (
 id TEXT PRIMARY KEY, vetted INTEGER NOT NULL, title TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '',
 category TEXT NOT NULL DEFAULT '', host TEXT NOT NULL DEFAULT '', search_id TEXT NOT NULL DEFAULT '', seen_at INTEGER NOT NULL);
CREATE INDEX frames_tools_seen ON frames_tools(seen_at);
CREATE TABLE receivers (
 id TEXT PRIMARY KEY, account TEXT NOT NULL, key_id TEXT NOT NULL DEFAULT '', hosted INTEGER NOT NULL DEFAULT 0,
 label TEXT NOT NULL DEFAULT '', token_hash TEXT NOT NULL, hmac_secret TEXT NOT NULL DEFAULT '',
 allow_from TEXT NOT NULL DEFAULT '', screen INTEGER NOT NULL DEFAULT 1,
 state TEXT NOT NULL CHECK(state IN ('active','deleted','revoked')), reason TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, rotated_at INTEGER NOT NULL DEFAULT 0, finished_at INTEGER NOT NULL DEFAULT 0,
 deliveries INTEGER NOT NULL DEFAULT 0, last_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX receivers_account ON receivers(account,state,created_at);
CREATE TABLE receiver_items (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT UNIQUE NOT NULL, receiver TEXT NOT NULL, account TEXT NOT NULL,
 content_type TEXT NOT NULL, body TEXT NOT NULL, bytes INTEGER NOT NULL, headers TEXT NOT NULL DEFAULT '{}',
 verified INTEGER NOT NULL DEFAULT 0, cost INTEGER NOT NULL DEFAULT 0, received_at INTEGER NOT NULL,
 event_seq INTEGER NOT NULL DEFAULT 0,
 screen TEXT NOT NULL CHECK(screen IN ('off','pending','done','failed','unpaid','unavailable')),
 verdict TEXT NOT NULL DEFAULT '', screen_cost INTEGER NOT NULL DEFAULT 0, screened_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX receiver_items_account ON receiver_items(account,seq);
CREATE INDEX receiver_items_receiver ON receiver_items(receiver,seq);
CREATE INDEX receiver_items_pending ON receiver_items(received_at) WHERE screen='pending';
CREATE TABLE fetch_denylist (host TEXT PRIMARY KEY, reason TEXT NOT NULL, created_at INTEGER NOT NULL);
CREATE TABLE fetch_host_days (host TEXT NOT NULL, day INTEGER NOT NULL, count INTEGER NOT NULL, PRIMARY KEY(host,day));
CREATE TABLE oauth_returning (
 browser_sha256 TEXT NOT NULL, redirect_uri TEXT NOT NULL, account TEXT NOT NULL,
 created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, PRIMARY KEY(browser_sha256,redirect_uri));
CREATE INDEX oauth_returning_account ON oauth_returning(account);
CREATE TABLE tlog_leaves (
 idx INTEGER PRIMARY KEY, kind TEXT NOT NULL, subject TEXT NOT NULL DEFAULT '', ref TEXT NOT NULL DEFAULT '',
 data TEXT NOT NULL, created_at INTEGER NOT NULL);
CREATE INDEX tlog_leaves_ref ON tlog_leaves(ref) WHERE ref<>'';
CREATE INDEX tlog_leaves_subject ON tlog_leaves(subject,idx) WHERE subject<>'';
CREATE TABLE tlog_hashes (
 level INTEGER NOT NULL, idx INTEGER NOT NULL, hash BLOB NOT NULL, PRIMARY KEY(level,idx)) WITHOUT ROWID;
CREATE TABLE tlog_cursors (source TEXT PRIMARY KEY, seq INTEGER NOT NULL);
CREATE TABLE tlog_checkpoints (
 size INTEGER PRIMARY KEY, root BLOB NOT NULL, note TEXT NOT NULL, created_at INTEGER NOT NULL);
CREATE TABLE tlog_anchors (
 size INTEGER PRIMARY KEY REFERENCES tlog_checkpoints(size), digest TEXT NOT NULL, ots BLOB NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('pending','confirmed','stale')), bitcoin_height INTEGER NOT NULL DEFAULT 0,
 calendars TEXT NOT NULL, submitted_at INTEGER NOT NULL, checked_at INTEGER NOT NULL DEFAULT 0);
CREATE TRIGGER tlog_leaves_no_update BEFORE UPDATE ON tlog_leaves BEGIN SELECT RAISE(ABORT,'transparency log is append-only'); END;
CREATE TRIGGER tlog_leaves_no_delete BEFORE DELETE ON tlog_leaves BEGIN SELECT RAISE(ABORT,'transparency log is append-only'); END;
CREATE TRIGGER tlog_hashes_no_update BEFORE UPDATE ON tlog_hashes BEGIN SELECT RAISE(ABORT,'transparency log is append-only'); END;
CREATE TRIGGER tlog_hashes_no_delete BEFORE DELETE ON tlog_hashes BEGIN SELECT RAISE(ABORT,'transparency log is append-only'); END;
CREATE TRIGGER tlog_checkpoints_no_update BEFORE UPDATE ON tlog_checkpoints BEGIN SELECT RAISE(ABORT,'transparency log is append-only'); END;
CREATE TRIGGER tlog_checkpoints_no_delete BEFORE DELETE ON tlog_checkpoints BEGIN SELECT RAISE(ABORT,'transparency log is append-only'); END;
CREATE TABLE receiver_days (receiver TEXT NOT NULL, day INTEGER NOT NULL, count INTEGER NOT NULL, PRIMARY KEY(receiver,day));
CREATE TABLE credit_topups (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE, account TEXT NOT NULL, agent TEXT NOT NULL,
 amount INTEGER NOT NULL CHECK(amount > 0), day INTEGER NOT NULL,
 network TEXT NOT NULL, asset TEXT NOT NULL, pay_to TEXT NOT NULL, payer TEXT NOT NULL,
 auth_nonce TEXT NOT NULL UNIQUE, valid_before INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('settling','credited','failed','unknown')),
 reason TEXT NOT NULL DEFAULT '', retryable INTEGER NOT NULL DEFAULT 0,
 tx_hash TEXT NOT NULL DEFAULT '', lot_id INTEGER NOT NULL DEFAULT 0,
 created_at INTEGER NOT NULL, settled_at INTEGER NOT NULL DEFAULT 0);
CREATE UNIQUE INDEX credit_topups_tx ON credit_topups(tx_hash) WHERE tx_hash<>'';
CREATE INDEX credit_topups_account ON credit_topups(account,seq);
CREATE INDEX credit_topups_day ON credit_topups(account,day);
CREATE TABLE spend_limits (
 credential TEXT PRIMARY KEY, account TEXT NOT NULL, credit_per_day INTEGER, credit_per_call INTEGER,
 expires_at INTEGER NOT NULL DEFAULT 0, set_at INTEGER NOT NULL, set_by TEXT NOT NULL);
CREATE INDEX spend_limits_account ON spend_limits(account,credential);
CREATE TABLE spend_limit_usage (
 credential TEXT NOT NULL, day INTEGER NOT NULL, spent INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(credential,day));
CREATE TABLE spend_limit_holds (
 hold_id TEXT PRIMARY KEY, credential TEXT NOT NULL, day INTEGER NOT NULL);
CREATE TABLE work_rewards (
 work_id TEXT PRIMARY KEY REFERENCES works(id), requester TEXT NOT NULL, amount INTEGER NOT NULL,
 fee INTEGER NOT NULL, hold_id TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('held','pending','paid','released')),
 worker TEXT NOT NULL DEFAULT '', transfer_id TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL,
 execute_at INTEGER NOT NULL DEFAULT 0, settled_at INTEGER NOT NULL DEFAULT 0, reason TEXT NOT NULL DEFAULT '',
 statement TEXT NOT NULL DEFAULT '', receipt_hash TEXT NOT NULL DEFAULT '', reviewer TEXT NOT NULL DEFAULT '');
CREATE INDEX work_rewards_open ON work_rewards(state,work_id) WHERE state IN ('held','pending');
CREATE INDEX work_rewards_requester ON work_rewards(requester,state);
CREATE TABLE pastes (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT UNIQUE NOT NULL, account TEXT NOT NULL, key_id TEXT NOT NULL DEFAULT '',
 hosted INTEGER NOT NULL DEFAULT 0, title TEXT NOT NULL DEFAULT '', text TEXT NOT NULL, bytes INTEGER NOT NULL, hash TEXT NOT NULL,
 visibility TEXT NOT NULL CHECK(visibility IN ('private','unlisted')), state TEXT NOT NULL CHECK(state IN ('active','deleted','hidden')),
 reason TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL DEFAULT 0, deleted_at INTEGER NOT NULL DEFAULT 0,
 notary_seq INTEGER NOT NULL DEFAULT 0, verdict TEXT NOT NULL DEFAULT '', screen_cost INTEGER NOT NULL DEFAULT 0,
 screened_at INTEGER NOT NULL DEFAULT 0, show_author INTEGER NOT NULL DEFAULT 0);
CREATE INDEX pastes_account ON pastes(account,seq);
CREATE INDEX pastes_hash ON pastes(account,hash);
CREATE TABLE docs (
 id TEXT PRIMARY KEY, account TEXT NOT NULL, room TEXT NOT NULL DEFAULT '', title TEXT NOT NULL,
 version INTEGER NOT NULL, hash TEXT NOT NULL, bytes INTEGER NOT NULL, stored INTEGER NOT NULL,
 updated_by TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, kind TEXT NOT NULL DEFAULT 'doc', visibility TEXT NOT NULL DEFAULT 'private', state TEXT NOT NULL DEFAULT 'active', reason TEXT NOT NULL DEFAULT '', expires_at INTEGER NOT NULL DEFAULT 0, deleted_at INTEGER NOT NULL DEFAULT 0, notary_seq INTEGER NOT NULL DEFAULT 0, show_author INTEGER NOT NULL DEFAULT 0, paste_seq INTEGER NOT NULL DEFAULT 0);
CREATE INDEX docs_account ON docs(account,created_at) WHERE room='';
CREATE INDEX docs_room ON docs(room,created_at) WHERE room<>'';
CREATE TABLE doc_versions (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT UNIQUE NOT NULL, doc TEXT NOT NULL, version INTEGER NOT NULL,
 title TEXT NOT NULL, text TEXT NOT NULL, bytes INTEGER NOT NULL, hash TEXT NOT NULL,
 author TEXT NOT NULL, author_key TEXT NOT NULL DEFAULT '', hosted INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL,
 verdict TEXT NOT NULL DEFAULT '', screen_cost INTEGER NOT NULL DEFAULT 0, screened_at INTEGER NOT NULL DEFAULT 0,
 UNIQUE(doc,version));
CREATE TRIGGER doc_versions_no_delete BEFORE DELETE ON doc_versions BEGIN SELECT RAISE(ABORT,'doc versions are kept'); END;
CREATE INDEX requests_anon_key ON requests(request_key,digest,created_at) WHERE actor >= 'anon:' AND actor < 'anon;';
CREATE TABLE link_witnesses (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 witness TEXT NOT NULL REFERENCES identities(id), agent TEXT NOT NULL REFERENCES identities(id),
 kind TEXT NOT NULL, value TEXT NOT NULL,
 verdict TEXT NOT NULL CHECK(verdict IN ('verified','failed')), nonce TEXT NOT NULL,
 signature TEXT NOT NULL, signed_payload TEXT NOT NULL,
 created_at INTEGER NOT NULL, superseded_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX link_witness_link ON link_witnesses(agent,kind,value,superseded_at,seq);
CREATE INDEX link_witness_by ON link_witnesses(witness,created_at);
CREATE TABLE work_review_fees (
 work_id TEXT PRIMARY KEY REFERENCES works(id), requester TEXT NOT NULL, amount INTEGER NOT NULL,
 fee INTEGER NOT NULL, hold_id TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('held','pending','paid','released')),
 worker TEXT NOT NULL DEFAULT '', transfer_id TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL,
 execute_at INTEGER NOT NULL DEFAULT 0, settled_at INTEGER NOT NULL DEFAULT 0, reason TEXT NOT NULL DEFAULT '',
 statement TEXT NOT NULL DEFAULT '', receipt_hash TEXT NOT NULL DEFAULT '', reviewer TEXT NOT NULL DEFAULT '');
CREATE INDEX work_review_fees_open ON work_review_fees(state,work_id) WHERE state IN ('held','pending');
CREATE INDEX work_review_fees_requester ON work_review_fees(requester,state);
CREATE INDEX docs_paste ON docs(account,paste_seq) WHERE kind='paste';
CREATE INDEX docs_hash ON docs(account,hash) WHERE room='';
