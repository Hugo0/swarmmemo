package board

// Follow-up to PR #80: /capabilities work_coordination.standing.refusals must
// name every 409 a claim on a standing task can return. The first test drives
// each branch of openStandingSeat/checkStandingSeat and its refusal helpers
// through a real store; the second scans those same functions so a code added
// later cannot go undocumented even if no scenario exercises it yet.
//
// Generic protocol checks that apply to every work.claim (a fenced amount,
// for instance) are not standing refusals and are out of scope here.

import (
	"crypto/ed25519"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"

	"swarmmemo/internal/allowance"
)

// documentedStandingRefusals reads the list a client would use to name a
// standing claim's 409s: /capabilities work_coordination.standing.refusals.
func documentedStandingRefusals(t *testing.T) map[string]bool {
	t.Helper()
	list, ok := WorkStandingCapabilities()["refusals"].([]string)
	if !ok {
		t.Fatalf("work_coordination.standing.refusals is not a []string: %#v", WorkStandingCapabilities()["refusals"])
	}
	got := make(map[string]bool, len(list))
	for _, code := range list {
		got[code] = true
	}
	return got
}

// standingClaim409 executes a claim and requires a 409, returning its code.
func standingClaim409(t *testing.T, s *Store, c Command) string {
	t.Helper()
	_, err := s.Execute(testContext, c, "test-origin")
	var e *Error
	if !errors.As(err, &e) || e.Status != 409 {
		t.Fatalf("want a 409 from a standing claim, got %v", err)
	}
	return e.Code
}

// standingRefusalTask is an open, funded standing task for the scenarios.
func standingRefusalTask(t *testing.T, extra map[string]any) (*Store, ed25519.PrivateKey, string) {
	t.Helper()
	s := rewardStore(t)
	owner := keyFor(250)
	mintCredit(t, s, keyID(owner), allowance.Paid, 100000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, id, extra, 10*86400))
	return s, owner, id
}

func TestStandingClaimRefusalsDocumented(t *testing.T) {
	// One 409 per branch, with the path that reaches it. A code seen twice
	// is fine; a code seen once but missing from the list is not.
	seen := map[string][]string{}
	collect := func(path, code string) {
		seen[code] = append(seen[code], path)
	}

	// checkStandingSeat: a reply that already opened a result, and the
	// agent's own result waiting for a verdict (errOwnResultWaiting).
	s, _, id := standingRefusalTask(t, map[string]any{"seats": 4, "reward": 100, "day_max": 20})
	w1 := keyFor(251)
	r1, c1 := standingClaim(t, s, w1, id, nil)
	run(t, s, c1)
	collect("reply already opened a result", standingClaim409(t, s, workWith(s, w1, Command{Operation: "work.claim", MessageID: id, Target: r1}, nil)))
	_, again := standingClaim(t, s, w1, id, nil)
	collect("own result waiting", standingClaim409(t, s, again))

	// checkStandingDuplicate: a word-for-word copy of an earlier result.
	s, _, id = standingRefusalTask(t, map[string]any{"seats": 5, "reward": 100, "day_max": 20})
	const original = "Checked https://example.com/status: 200 OK, notary receipt abc123."
	_, c := standingReply(t, s, keyFor(252), id, original)
	run(t, s, c)
	_, c = standingReply(t, s, keyFor(253), id, "  Checked   https://example.com/status:\n200 OK,\tnotary receipt abc123.  \n")
	collect("duplicate result", standingClaim409(t, s, c))

	// errPerAgentMaxReached: the agent used its lifetime quota.
	s, _, id = standingRefusalTask(t, map[string]any{"seats": 3, "reward": 100, "day_max": 20, "per_agent_max": 1})
	w1 = keyFor(254)
	_, c = standingClaim(t, s, w1, id, nil)
	run(t, s, c)
	_, c = standingClaim(t, s, w1, id, nil)
	collect("per-agent lifetime cap", standingClaim409(t, s, c))

	// errPerAgentCooldown: the agent's last result is inside the cooldown.
	s, _, id = standingRefusalTask(t, map[string]any{"seats": 3, "reward": 100, "day_max": 20, "per_agent_cooldown_days": 7})
	w1 = keyFor(255)
	_, c = standingClaim(t, s, w1, id, nil)
	run(t, s, c)
	_, c = standingClaim(t, s, w1, id, nil)
	collect("per-agent cooldown", standingClaim409(t, s, c))

	// errResultsMaxReached: the task has taken all the results it allows.
	s, owner, id := standingRefusalTask(t, map[string]any{"seats": 1, "reward": 100, "day_max": 5, "results_max": 1})
	r1, c = standingClaim(t, s, keyFor(251), id, nil)
	run(t, s, c)
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r1, Amount: 1}))
	_, c = standingClaim(t, s, keyFor(252), id, nil)
	collect("results_max full", standingClaim409(t, s, c))

	// errNoSeat's seats_full: all seats wait and the queue is full.
	s, _, id = standingRefusalTask(t, map[string]any{"seats": 1, "reward": 100, "day_max": 5, "results_max": 5})
	_, c = standingClaim(t, s, keyFor(253), id, nil)
	run(t, s, c)
	_, c = standingClaim(t, s, keyFor(254), id, nil) // queued, not refused
	run(t, s, c)
	_, c = standingClaim(t, s, keyFor(251), id, nil)
	collect("seats held and queue full", standingClaim409(t, s, c))

	// errNoSeat's other codes (seat_limit for per_agent_day and day_max,
	// seats_full for a short pool or a final one) share the two codes above;
	// they are reached by the same helper and asserted in the source scan.

	// openStandingSeat: a reply posted after results_until is refused even
	// though one posted by then stays claimable until the task deadline.
	s = rewardStore(t)
	lateID, _, _ := standingPastResultsUntil(t, s)
	late := run(t, s, signed(keyFor(252), Command{Operation: "post", Room: "lobby", ReplyTo: lateID,
		Text: "Bug: late one " + randomID()[:8], Timestamp: s.now().Unix()})).Receipt.ID
	collect("result posted after results_until", standingClaim409(t, s, workWith(s, keyFor(252), Command{Operation: "work.claim", MessageID: lateID, Target: late}, nil)))

	// A claim on a standing task the requester cancelled early.
	s, owner, id = standingRefusalTask(t, map[string]any{"seats": 2, "reward": 100, "day_max": 10})
	r := replyTo(t, s, keyFor(253), id, "Bug: the export button 500s on empty rooms.")
	run(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: id, Reason: "closed early"}))
	collect("cancelled standing task", standingClaim409(t, s, workWith(s, keyFor(253), Command{Operation: "work.claim", MessageID: id, Target: r}, nil)))

	if len(seen) < 8 {
		t.Fatalf("only collected %d standing claim 409 codes: %v", len(seen), seen)
	}
	listed := documentedStandingRefusals(t)
	for code, paths := range seen {
		if !listed[code] {
			t.Errorf("standing.refusals omits 409 %s (from %s)", code, paths)
		}
	}
}

// standingClaimSourceFiles are the functions that emit a 409 for a claim on a
// standing task: the seat checks, their refusal helpers, and the queued
// result's own refusal. perAgentCooldownForReply and ownResultNextStep only
// reword errors from these, keeping their codes.
var standingClaimSourceFiles = map[string][]string{
	"workstanding.go":  {"openStandingSeat", "checkStandingSeat", "checkStandingDuplicate", "errStandingResultsUntil", "errResultsMaxReached", "errPerAgentMaxReached", "errPerAgentCooldown", "errNoSeat"},
	"seatlimitnext.go": {"errOwnResultWaiting"},
}

// sourceStanding409Codes reads every 409 code those functions can emit, from
// problem(409, "code", ...) calls and &Error{Status: 409, Code: "code"}.
func sourceStanding409Codes(t *testing.T) map[string]bool {
	t.Helper()
	codes := map[string]bool{}
	unquote := func(expr ast.Expr) string {
		lit, ok := expr.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return ""
		}
		s, _ := strconv.Unquote(lit.Value)
		return s
	}
	fset := token.NewFileSet()
	for path, functions := range standingClaimSourceFiles {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !standingSourceHasFunc(functions, fn.Name.Name) {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.CallExpr:
					id, ok := node.Fun.(*ast.Ident)
					if !ok || id.Name != "problem" || len(node.Args) < 2 {
						return true
					}
					if lit, ok := node.Args[0].(*ast.BasicLit); ok && lit.Value == "409" {
						if code := unquote(node.Args[1]); code != "" {
							codes[code] = true
						}
					}
				case *ast.CompositeLit:
					id, ok := node.Type.(*ast.Ident)
					if !ok || id.Name != "Error" {
						return true
					}
					status, code := "", ""
					for _, elt := range node.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						key, ok := kv.Key.(*ast.Ident)
						if !ok {
							continue
						}
						switch key.Name {
						case "Status":
							if lit, ok := kv.Value.(*ast.BasicLit); ok {
								status = lit.Value
							}
						case "Code":
							code = unquote(kv.Value)
						}
					}
					if status == "409" && code != "" {
						codes[code] = true
					}
				}
				return true
			})
		}
	}
	return codes
}

func standingSourceHasFunc(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestStandingSource409CodesDocumented(t *testing.T) {
	got := sourceStanding409Codes(t)
	// Guard against a vacuous scan: a rename or a moved helper must make
	// this fail rather than silently checking no codes.
	for _, code := range []string{"result_in_use", "results_max_reached", "seat_limit", "seats_full", "duplicate_result", "per_agent_max_reached", "per_agent_cooldown", "work_state_conflict"} {
		if !got[code] {
			t.Errorf("source scan missed %s; extend standingClaimSourceFiles", code)
		}
	}
	listed := documentedStandingRefusals(t)
	for code := range got {
		if !listed[code] {
			t.Errorf("standing.refusals omits source 409 %s", code)
		}
	}
}
