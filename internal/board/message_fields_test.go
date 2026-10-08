package board

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

var updateMessageFields = flag.Bool("update-message-fields", false, "rewrite clients/message-fields.json from the Message struct")

// messageFieldsGolden is the wire field set of a message object, derived from
// the struct tags. The first-party clients validate messages against strict
// allowlists; their tests (clients/python/test_contract.py and
// clients/mcp/test_operations.py) check every allowlist covers this file, so
// a field added here fails CI, not the agents reading it.
const messageFieldsGolden = "../../clients/message-fields.json"

func jsonFields(value any) []string {
	var fields []string
	kind := reflect.TypeOf(value)
	for i := 0; i < kind.NumField(); i++ {
		name, _, _ := strings.Cut(kind.Field(i).Tag.Get("json"), ",")
		if kind.Field(i).IsExported() && name != "" && name != "-" {
			fields = append(fields, name)
		}
	}
	sort.Strings(fields)
	return fields
}

func TestMessageFieldsGolden(t *testing.T) {
	want := map[string][]string{"message": jsonFields(Message{}), "attachment": jsonFields(Attachment{}),
		"forwarded": jsonFields(Forwarded{}), "quality": jsonFields(Quality{}), "screen": jsonFields(MessageScreen{}), "votes": jsonFields(VoteCounts{})}
	encoded, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	if *updateMessageFields {
		if err = os.WriteFile(messageFieldsGolden, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(messageFieldsGolden)
	if err != nil || !bytes.Equal(got, encoded) {
		t.Fatalf("clients/message-fields.json is stale (%v); rerun go test ./internal/board -run TestMessageFieldsGolden -update-message-fields, then teach every client validator the new field\nwant:\n%s", err, encoded)
	}
}
