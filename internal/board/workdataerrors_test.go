package board

import (
	"errors"
	"strings"
	"testing"
)

// C77: an invalid_work_data refusal names the field it is about, by the
// services' UnknownArg rule (short plain names echoed, anything else
// described), hints at a likely intended field, and names a type error.
func TestWorkDataErrorsNameTheField(t *testing.T) {
	s := openTest(t, Config{})
	owner := keyFor(92)
	id := run(t, s, signed(owner, Command{Operation: "post", Kind: "request", Text: "root"})).Receipt.ID
	base := workCommand(s, owner, Command{Operation: "work.create", MessageID: id})
	message := func(op, data string) string {
		t.Helper()
		c := base
		c.Operation, c.Data, c.Nonce = op, data, ""
		if op == "work.claim" {
			c.TTL = 600
		}
		_, err := s.Execute(testContext, signed(owner, c), "test-origin")
		var e *Error
		if !errors.As(err, &e) || e.Code != "invalid_work_data" || e.Status != 400 {
			t.Fatalf("%s %s: want invalid_work_data 400, got %v", op, data, err)
		}
		if !strings.HasSuffix(e.Message, workDataRule) {
			t.Fatalf("message lost the rule: %q", e.Message)
		}
		return strings.TrimSuffix(e.Message, " "+workDataRule)
	}
	with := func(field string) string {
		return strings.Replace(base.Data, `"schema":1`, `"schema":1,`+field, 1)
	}
	gen := `{"schema":1,"generation":"` + s.generation + `",`
	for _, tc := range []struct{ op, data, want string }{
		{"work.create", with(`"extra":true`), "extra is not a field work.create data takes."},
		{"work.create", with(`"deadline":3600`), "deadline is not a field work.create data takes; did you mean ttl? It goes beside data, not in it."},
		{"work.create", with(`"bounty":5`), "bounty is not a field work.create data takes; did you mean reward?"},
		// A hint names only a field the operation takes.
		{"work.claim", gen + `"name":"x"}`, "name is not a field work.claim data takes."},
		{"work.claim", gen + `"result_hash":"x"}`, "result_hash is not a field work.claim data takes; did you mean result_sha256?"},
		{"work.claim", gen + `"title":"x"}`, "title is not a field work.claim data takes."},
		// A name that is not short and plain is described, never echoed.
		{"work.create", with(`"<script>":1`), "A field was sent that work.create data does not take."},
		{"work.create", with(`"` + strings.Repeat("a", 65) + `":1`), "A field was sent that work.create data does not take."},
		// Type errors name the field and the type, never the value.
		{"work.create", with(`"reward":"lots"`), "reward must be an integer."},
		{"work.create", with(`"reward":1.5`), "reward must be an integer."},
		{"work.create", strings.Replace(base.Data, `["review","go"]`, `["review",7]`, 1), "capabilities must be an array of strings."},
		{"work.create", strings.Replace(base.Data, `"schema":1`, `"schema":null`, 1), "schema must be an integer."},
	} {
		if got := message(tc.op, tc.data); got != tc.want {
			t.Errorf("%s %s:\n got %q\nwant %q", tc.op, tc.data, got, tc.want)
		}
	}
	// A refusal with nothing to name keeps the plain rule.
	c := base
	c.Data, c.Nonce = `{}`, ""
	_, err := s.Execute(testContext, signed(owner, c), "test-origin")
	var e *Error
	if !errors.As(err, &e) || e.Message != workDataRule {
		t.Fatalf("want the plain rule, got %v", err)
	}
}

func TestWorksListDataNamesTheField(t *testing.T) {
	for raw, want := range map[string]string{
		`{"schema":1,"agent":"x"}`:         "agent is not a field works.list data takes. ",
		`{"schema":1,"<b>":"x"}`:           "A field was sent that works.list data does not take. ",
		`{"schema":1,"eligible_for":7}`:    "eligible_for must be a string. ",
		`{"schema":"1","eligible_for":""}`: "schema must be an integer. ",
		`{"schema":2,"eligible_for":""}`:   "",
	} {
		_, _, err := worksListData(raw)
		var e *Error
		if !errors.As(err, &e) || e.Code != "invalid_work_data" || e.Message != want+WorksListDataRule {
			t.Errorf("%s: got %v", raw, err)
		}
	}
}
