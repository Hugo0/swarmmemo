package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"time"

	"swarmmemo/internal/board"
)

// GraphCacheControl lets browsers and proxies reuse /api/graph for as long as
// the server does (board.GraphTTL), revalidating by ETag after.
const GraphCacheControl = "public, max-age=30, stale-while-revalidate=30"

type graphStore interface {
	ReadGraph(ctx context.Context, room string, since int64) (*board.GraphSnapshot, error)
}

// graph serves /api/graph: the public identity, reply and room graph behind
// /graph, as compact parallel arrays and never with text.
func (s *Server) graph(w http.ResponseWriter, r *http.Request) {
	if !readMethod(r) {
		methodError(w)
		return
	}
	var room string
	var since int64
	for key, values := range r.URL.Query() {
		if len(values) != 1 {
			writeError(w, bad("Give each graph parameter at most once."))
			return
		}
		v := values[0]
		switch key {
		case "room":
			if v != "" && (!board.ValidRoomName(v) || board.IsConversationRoom(v)) {
				writeError(w, bad("room must be a public room name, as /api/rooms lists them."))
				return
			}
			room = v
		case "since":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 || n > time.Now().Unix()+86400 {
				writeError(w, bad("since must be a unix time in seconds, 0 or later and not in the future."))
				return
			}
			since = n
		case "format":
			if v != "json" {
				writeError(w, bad("The graph is JSON; format may only be json."))
				return
			}
		default:
			writeError(w, bad("The graph takes only the optional room and since parameters."))
			return
		}
	}
	store, ok := s.service.(graphStore)
	if !ok {
		writeError(w, &board.Error{Status: 503, Code: "stats_unavailable", Message: "The graph is not available from this service."})
		return
	}
	g, err := store.ReadGraph(r.Context(), room, since)
	if err != nil {
		writeError(w, &board.Error{Status: 503, Code: "storage_unavailable", Message: "The graph is temporarily unavailable. Retry shortly."})
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", GraphCacheControl)
	h.Set("ETag", g.ETag)
	http.ServeContent(w, r, "graph.json", g.Generated, bytes.NewReader(g.JSON))
}
