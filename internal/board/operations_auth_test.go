package board

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

// matrixOps is the union of 1.13.0 and 1.14.0 operation names; matrix* helpers build
// the matrix fixture.
var matrixOps = []string{"post", "messages.list", "message.get", "thread.get", "updates.get", "room.pages", "rooms.list", "room.get", "room.create", "room.member.add", "room.member.remove", "room.policy.set", "room.moderator.add", "room.moderator.remove", "room.owner.transfer", "room.hide", "room.restore", "room.style.set", "room.style.clear", "room.style.check", "room.modlog", "agent.register", "agent.rotate", "agent.get", "agents.list", "agent.profile.publish", "agent.profile.remove", "identity.link", "identity.unlink", "blob.put", "blob.get", "blob.delete", "quota.get", "credit.transfer", "report", "stats", "export", "lease.acquire", "lease.release", "work.create", "work.claim", "work.renew", "work.submit", "work.accept", "work.reject", "work.cancel", "work.get", "works.list", "work.history", "delegation.create", "delegation.revoke", "delegation.get", "delegations.list", "private_read.create", "private_read.revoke", "private_read.get", "private_read.list", "webhook.create", "webhook.delete", "webhook.list"}

type matrixFixture struct {
	s                                  *Store
	owner, other, third, child, child2 ed25519.PrivateKey
	child3, rotated                    ed25519.PrivateKey
	grant                              *DelegationContext
	m, m2, m3, blob                    string
}

func matrixPub(k ed25519.PrivateKey) string {
	return base64.RawURLEncoding.EncodeToString(k.Public().(ed25519.PublicKey))
}

func matrixSetup(t *testing.T) *matrixFixture {
	f := &matrixFixture{s: openTest(t, Config{}), owner: keyFor(201), other: keyFor(202), third: keyFor(204), child: keyFor(203), child2: keyFor(205), child3: keyFor(206), rotated: keyFor(207)}
	s := f.s
	for _, k := range []ed25519.PrivateKey{f.owner, f.other, f.third} {
		register(t, s, k)
	}
	run(t, s, signed(f.owner, Command{Operation: "room.create", Room: "pub", Visibility: "public"}))
	run(t, s, signed(f.owner, Command{Operation: "room.create", Room: "priv", Visibility: "private"}))
	f.m = createTestWork(t, s, f.owner, "pub", "request", 3600)
	f.m2 = run(t, s, signed(f.owner, Command{Operation: "post", Room: "pub", Text: "plain"})).Receipt.ID
	f.m3 = run(t, s, signed(f.owner, Command{Operation: "post", Room: "pub", Kind: "request", Text: "second brief"})).Receipt.ID
	blob := run(t, s, signed(f.owner, Command{Operation: "blob.put", Room: "pub", Data: base64.RawURLEncoding.EncodeToString([]byte("hello")), Filename: "a.txt", MediaType: "text/plain"}))
	f.blob = blob.Data["blob"].(Attachment).ID
	run(t, s, signed(f.owner, Command{Operation: "lease.acquire", Room: "pub", Target: "l1", TTL: 60}))
	f.grant = enroll(t, s, f.owner, f.child, "pub", 3600, 1<<20)
	privateReadEnroll(t, s, f.owner, f.child3, "priv")
	return f
}

// matrixBase builds the unsigned command for op; proofKey signs the proof field if any.
func matrixBase(f *matrixFixture, op string) (Command, ed25519.PrivateKey) {
	s := f.s
	gen := func(extra map[string]any) string {
		d := map[string]any{"schema": 1, "generation": s.generation}
		for k, v := range extra {
			d[k] = v
		}
		b, _ := json.Marshal(d)
		return string(b)
	}
	c := Command{Operation: op, RequestID: "req-" + randomID()}
	var proof ed25519.PrivateKey
	switch op {
	case "post":
		c.Room, c.Text = "pub", "hello"
	case "messages.list", "room.pages", "room.get", "room.modlog", "works.list":
		c.Room = "pub"
	case "message.get", "thread.get":
		c.MessageID = f.m2
	case "updates.get":
		c.Target = keyID(f.owner)
	case "room.create":
		c.Room, c.Visibility = "newroom", "public"
	case "room.member.add", "room.member.remove":
		c.Room, c.Target = "priv", keyID(f.other)
	case "room.policy.set":
		c.Room, c.Data = "pub", `{"write":"owner"}`
	case "room.moderator.add", "room.moderator.remove", "room.owner.transfer":
		c.Room, c.Target = "pub", keyID(f.other)
	case "room.hide", "room.restore", "report":
		c.MessageID, c.Reason = f.m2, "reason"
	case "room.style.set", "room.style.check":
		c.Room, c.Data = "pub", `{"css":".x{color:red}"}`
	case "room.style.clear":
		c.Room = "pub"
	case "agent.register":
		c.Handle = "h" + randomID()[:8]
	case "agent.rotate":
		c.Target, proof = matrixPub(f.rotated), f.rotated
	case "agent.get":
		c.Target = keyID(f.owner)
	case "agent.profile.publish":
		c.Data = testPeerData
	case "identity.link", "identity.unlink":
		c.Data = linkJSON("url", "https://example.org/agent")
	case "blob.put":
		c.Room, c.Data, c.Filename, c.MediaType = "pub", base64.RawURLEncoding.EncodeToString([]byte("world")), "b.txt", "text/plain"
	case "blob.get":
		c.MessageID = f.blob
	case "blob.delete":
		c.MessageID, c.Reason = f.blob, "reason"
	case "credit.transfer":
		c.Target, c.Amount = keyID(f.third), 1000
	case "lease.acquire":
		c.Room, c.Target, c.TTL = "pub", "l2", 60
	case "lease.release":
		c.Room, c.Target, c.Amount = "pub", "l1", 1
	case "work.create":
		c.MessageID, c.Data = f.m3, gen(map[string]any{"title": "t", "capabilities": []string{"go"}})
	case "work.claim", "work.renew", "work.submit", "work.accept", "work.reject", "work.cancel":
		c.MessageID, c.Data = f.m, gen(nil)
	case "work.get", "work.history":
		c.MessageID = f.m
	case "delegation.create":
		c.Room, c.Target, c.TTL, c.Amount = "pub", matrixPub(f.child2), 3600, 1<<20
		b, _ := json.Marshal(map[string]any{"schema": 1, "generation": s.generation, "operations": grantOps(), "disclosure": "public"})
		c.Data, proof = string(b), f.child2
	case "delegation.revoke":
		c.Target, c.Data = keyID(f.child), gen(nil)
	case "delegation.get":
		c.Target = keyID(f.child)
	case "private_read.create":
		var epoch string
		_ = s.db.QueryRow("SELECT private_access_epoch FROM rooms WHERE name='priv'").Scan(&epoch)
		b, _ := json.Marshal(map[string]any{"schema": 1, "generation": s.generation, "access_epoch": epoch, "disclosure": "private"})
		c.Room, c.Target, c.Data, proof = "priv", matrixPub(f.child2), string(b), f.child2
	case "private_read.revoke":
		c.Room, c.Target, c.Data = "priv", keyID(f.child3), gen(nil)
	case "private_read.get":
		c.Room, c.Target = "priv", keyID(f.child3)
	case "private_read.list":
		c.Room = "priv"
	case "webhook.create":
		c.Data = `{"schema":1,"url":"https://hooks.example.org/other"}`
	case "webhook.delete":
		c.Target = randomID()
	}
	return c, proof
}

func matrixSign(f *matrixFixture, key ed25519.PrivateKey, c Command, proof ed25519.PrivateKey) Command {
	c.Timestamp = f.s.now().Unix()
	c = signed(key, c)
	if proof != nil {
		c.Proof = base64.RawURLEncoding.EncodeToString(ed25519.Sign(proof, Canonical(f.s.config.ServiceID, c)))
	}
	return c
}

func matrixGlobal(f *matrixFixture) int64 {
	var used int64
	_ = f.s.db.QueryRow("SELECT coalesce(sum(used),0) FROM quota WHERE actor='global'").Scan(&used)
	return used
}

func matrixOutcome(f *matrixFixture, c Command) string {
	before := matrixGlobal(f)
	_, err := f.s.Execute(testContext, c, "test-origin")
	after := matrixGlobal(f)
	code := "OK"
	var e *Error
	if errors.As(err, &e) {
		code = e.Code
	} else if err != nil {
		code = "ERR(" + err.Error() + ")"
	}
	if code == "OK" {
		return fmt.Sprintf("OK/%+d", after-before)
	}
	return code
}

// v113Unsigned and v113Delegable are what 1.13.0 (fe93f7b) accepted unsigned
// and from a scoped worker key, measured with this same matrix. Operations
// added since are listed with their intent.
var v113Unsigned = "agent.get agents.list blob.get delegation.get export message.get messages.list post quota.get report room.get room.pages rooms.list stats thread.get updates.get work.get work.history works.list"
var addedUnsigned = "room.modlog room.style.check" // public reads; check stores nothing
var v113Delegable = "post messages.list message.get thread.get room.get room.pages works.list work.get work.history work.claim work.renew work.submit"

// freeMutations succeed without spending allowance, as they did in 1.13.0: a
// revoke must work even when the budget is exhausted.
var freeMutations = "delegation.revoke private_read.revoke"

func listed(list, op string) bool { return strings.Contains(" "+list+" ", " "+op+" ") }

// TestOperationAuthorityMatrix executes every operation unsigned, signed by a
// key that owns nothing, through a scoped worker key of the owner, and signed
// by the owner, and holds the results to the operation table and to 1.13.0.
func TestOperationAuthorityMatrix(t *testing.T) {
	known := map[string]bool{}
	var lines []string
	for _, op := range Operations() {
		known[op.Name] = true
		got := map[string]string{}
		row := []string{op.Name}
		for _, mode := range []string{"unsigned", "nonowner", "delegated", "owner"} {
			f := matrixSetup(t)
			c, proof := matrixBase(f, op.Name)
			switch mode {
			case "nonowner":
				c = matrixSign(f, f.other, c, proof)
			case "delegated":
				copied := *f.grant
				c.Delegation = &copied
				if strings.HasPrefix(op.Name, "work.") && op.Mutation {
					c.Data = `{"schema":1,"generation":"` + f.grant.Generation + `"}`
				}
				c = matrixSign(f, f.child, c, proof)
			case "owner":
				c = matrixSign(f, f.owner, c, proof)
			}
			got[mode] = matrixOutcome(f, c)
			row = append(row, mode+"="+got[mode])
		}
		line := strings.Join(row, " ")
		lines = append(lines, line)
		unsignedOK := strings.HasPrefix(got["unsigned"], "OK")
		if op.Signed && unsignedOK {
			t.Errorf("%s is Signed but accepted an unsigned command", op.Name)
		}
		if unsignedOK && !listed(v113Unsigned, op.Name) && !listed(addedUnsigned, op.Name) {
			t.Errorf("%s newly accepts unsigned commands", op.Name)
		}
		if op.Delegable != listed(v113Delegable, op.Name) {
			t.Errorf("%s: Delegable=%v differs from 1.13.0", op.Name, op.Delegable)
		}
		// A worker key may always read its own grant (delegation.get of itself).
		if !op.Delegable && strings.HasPrefix(got["delegated"], "OK") && op.Name != "delegation.get" {
			t.Errorf("%s is not Delegable but a worker key ran it", op.Name)
		}
		if op.Mutation && got["owner"] == "OK/+0" && !listed(freeMutations, op.Name) {
			t.Errorf("%s succeeded without spending allowance", op.Name)
		}
		if op.Mutation && got["unsigned"] == "OK/+0" {
			t.Errorf("%s: an unsigned write spent no allowance", op.Name)
		}
		// Room governance: never by a key that neither owns nor moderates.
		if strings.HasPrefix(op.Name, "room.") && op.Signed && op.Name != "room.create" {
			if n := got["nonowner"]; n != "owner_required" && n != "moderator_required" && n != "not_found" {
				t.Errorf("room operation not refused to a non-owner: %s", line)
			}
		}
	}
	for _, op := range matrixOps {
		if !known[op] {
			t.Errorf("operation %s missing from the table", op)
		}
	}
	sort.Strings(lines)
	if path := os.Getenv("OPS_MATRIX_OUT"); path != "" {
		_ = os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	}
}

// A scoped worker key can post as its parent in one room; supersession must
// not let it, an anonymous caller or another key replace the parent's messages.
func TestWorkerKeyCannotSupersedeParentPost(t *testing.T) {
	f := matrixSetup(t)
	edit := `{"schema":1,"supersedes":"` + f.m2 + `"}`
	copied := *f.grant
	fails(t, f.s, matrixSign(f, f.child, Command{Operation: "post", Room: "pub", Text: "rewritten", Visibility: "public", Data: edit, Delegation: &copied}, nil), "supersede_forbidden")
	fails(t, f.s, Command{Operation: "post", Room: "pub", Text: "anon edit", Data: edit}, "signature_required")
	fails(t, f.s, matrixSign(f, f.other, Command{Operation: "post", Room: "pub", Text: "other edit", Data: edit}, nil), "supersede_forbidden")
}
