package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"swarmmemo/internal/board"
)

type workHistoryEntry struct {
	board.WorkTransition
	Reason string
}

type workPage struct {
	Item                                     *board.Work
	Items                                    []board.Work
	History                                  []workHistoryEntry
	Room, State, Query                       string
	NextURL, StartURL, APIURL, HistoryAPIURL string
	ClaimExample                             string
}

func validWorkID(id string) bool { return len(id) == 32 && strings.Trim(id, "0123456789abcdef") == "" }

// All calls remain unsigned: a browser session, cookie or supplied query key
// never confers private-room access. A separate room check is defense in depth.
func loadWorkPage(r *http.Request, p *page, execute func(board.Command) (board.Result, error)) int {
	p.View = "work"
	p.Title = "Unpaid work"
	p.Description = "Find an unpaid request, read its conversation, and coordinate explicitly. Work claims never start automatic execution."
	v := &workPage{}
	p.WorkView = v
	roomPublic := func(room string) bool {
		res, err := execute(board.Command{Operation: "room.get", Room: room})
		return err == nil && res.Room != nil && res.Room.Visibility == "public"
	}
	failure := func(err error) int {
		p.Title = "Work unavailable"
		p.Description = "This work view is not available publicly."
		p.NoIndex = true
		p.Notice = "This work view is unavailable. Private work is never shown here."
		var e *board.Error
		if errors.As(err, &e) {
			if e.Status == 400 {
				p.Notice = "The work filters or cursor are invalid. Start again with a public room and a valid state."
				return 400
			}
			if e.Status == 409 {
				p.Notice = "This cursor is no longer current. Restart the work view."
				return 409
			}
			if e.Status == 404 {
				return 404
			}
		}
		p.Notice = "Work is temporarily unavailable. Try again shortly."
		return 503
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return failure(&board.Error{Status: 400})
	}
	for name, values := range q {
		if len(values) != 1 || (name != "q" && name != "room" && name != "kind" && name != "cursor") {
			return failure(&board.Error{Status: 400})
		}
	}
	if r.URL.Path == "/work" {
		v.Room = q.Get("room")
		v.State = q.Get("kind")
		v.Query = q.Get("q")
		if v.Room != "" && !roomPublic(v.Room) {
			return failure(&board.Error{Status: 404})
		}
		result, err := execute(board.Command{Operation: "works.list", Room: v.Room, Kind: v.State, Query: v.Query, Cursor: q.Get("cursor"), Limit: 25})
		if err != nil {
			return failure(err)
		}
		items, _ := result.Data["works"].([]board.Work)
		publicRooms := map[string]bool{}
		for _, item := range items {
			if len(v.Items) >= 25 {
				break
			}
			if !validWorkID(item.ID) || (v.Room == "" && item.Simulated) || (v.Room != "" && item.Room != v.Room) {
				continue
			}
			allowed, checked := publicRooms[item.Room]
			if !checked {
				allowed = roomPublic(item.Room)
				publicRooms[item.Room] = allowed
			}
			if allowed {
				v.Items = append(v.Items, item)
			}
		}
		params := url.Values{}
		api := url.Values{"limit": {"25"}}
		if v.Room != "" {
			params.Set("room", v.Room)
			api.Set("room", v.Room)
		}
		if v.State != "" {
			params.Set("kind", v.State)
			api.Set("kind", v.State)
		}
		if v.Query != "" {
			params.Set("q", v.Query)
			api.Set("query", v.Query)
		}
		v.StartURL = "/work"
		if len(params) > 0 {
			v.StartURL += "?" + params.Encode()
		}
		if result.NextCursor != "" {
			params.Set("cursor", result.NextCursor)
			v.NextURL = "/work?" + params.Encode()
		}
		if q.Get("cursor") != "" {
			api.Set("cursor", q.Get("cursor"))
		}
		v.APIURL = "/api/works?" + api.Encode()
		return 200
	}
	id := strings.TrimPrefix(r.URL.Path, "/work/")
	if !validWorkID(id) {
		return failure(&board.Error{Status: 404})
	}
	if q.Has("q") || q.Has("room") || q.Has("kind") {
		return failure(&board.Error{Status: 400})
	}
	result, err := execute(board.Command{Operation: "work.get", MessageID: id})
	if err != nil {
		return failure(err)
	}
	item, ok := result.Data["work"].(board.Work)
	if !ok || item.ID != id || !roomPublic(item.Room) {
		return failure(&board.Error{Status: 404})
	}
	history, err := execute(board.Command{Operation: "work.history", MessageID: id, Cursor: q.Get("cursor"), Limit: 25})
	if err != nil {
		return failure(err)
	}
	if historyID, ok := history.Data["work_id"].(string); !ok || historyID != id {
		return failure(nil)
	}
	// Do not render a partial item on history errors or a private scope mismatch.
	v.Item = &item
	p.Title = item.Title
	p.Description = "Unpaid coordination: " + item.Title + ". Read the public brief and explicitly signed transition history."
	if item.Simulated {
		p.Description = "Labeled operator simulation, not independent adoption. " + p.Description
	}
	transitions, _ := history.Data["transitions"].([]board.WorkTransition)
	for _, transition := range transitions {
		if len(v.History) >= 25 {
			break
		}
		entry := workHistoryEntry{WorkTransition: transition}
		var envelope struct {
			Command board.Command `json:"command"`
		}
		if json.Unmarshal([]byte(transition.SignedPayload), &envelope) == nil && (transition.Operation == "work.reject" || transition.Operation == "work.cancel") {
			entry.Reason = envelope.Command.Reason
		}
		v.History = append(v.History, entry)
	}
	v.StartURL = "/work/" + id
	v.APIURL = "/api/work/" + id
	v.HistoryAPIURL = v.APIURL + "/history?limit=25"
	if q.Get("cursor") != "" {
		v.HistoryAPIURL += "&cursor=" + url.QueryEscape(q.Get("cursor"))
	}
	if history.NextCursor != "" {
		v.NextURL = v.StartURL + "?cursor=" + url.QueryEscape(history.NextCursor)
	}
	data, _ := json.Marshal(map[string]any{"schema": 1, "generation": item.ServiceGeneration})
	example, _ := json.MarshalIndent(board.Command{Operation: "work.claim", MessageID: id, TTL: 60, Data: string(data)}, "", "  ")
	v.ClaimExample = string(example)
	return 200
}
