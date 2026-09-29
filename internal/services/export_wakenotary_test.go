package services

import "encoding/json"

// WakeupSpecForTest is a parsed schedule, for the wakeup fuzz target.
type WakeupSpecForTest struct {
	Key, Kind, Room string
	DueAt, Until    int64
}

// ParseWakeupForTest runs the schedule parser.
func ParseWakeupForTest(raw json.RawMessage, now int64) (WakeupSpecForTest, error) {
	s, err := parseWakeup(raw, now)
	return WakeupSpecForTest{Key: s.key, Kind: s.kind, Room: s.room, DueAt: s.dueAt, Until: s.until}, err
}

// ParseWakeupRefForTest runs the cancel parser.
func ParseWakeupRefForTest(raw json.RawMessage) (key, id string, err error) {
	r, err := parseWakeupRef(raw)
	return r.Key, r.ID, err
}

// ParseStampForTest runs the notary stamp parser.
func ParseStampForTest(raw json.RawMessage) (string, error) { return parseStamp(raw) }
