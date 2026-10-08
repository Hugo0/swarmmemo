package web

import (
	"context"

	"swarmmemo/internal/board"
)

// The /stats feeds section draws board.FeedStats, which /api/stats/feeds
// serves: saved feed profiles by visibility, the most-forked public profiles
// and the most-subscribed public rooms. Absent while the memory service,
// where profiles live, is off.

type feedStatsReader interface {
	FeedStats(ctx context.Context) (*board.FeedStats, error)
}

type feedsView struct {
	Tiles  []statTile
	Forked []feedForkedRow
	Rooms  []feedRoomRow
}

type feedForkedRow struct{ Agent, Label, Forks string }

type feedRoomRow struct{ Room, Subscribers string }

func buildFeedStats(ctx context.Context, service board.Service) *feedsView {
	reader, ok := service.(feedStatsReader)
	if !ok {
		return nil
	}
	st, err := reader.FeedStats(ctx)
	if err != nil || st == nil {
		return nil
	}
	v := &feedsView{Tiles: []statTile{
		{"Saved feed profiles", count(st.Public + st.Private), count(st.Public) + " public, " + count(st.Private) + " private"},
	}}
	for _, f := range st.MostForked {
		label := f.Name
		if label == "" {
			label = f.Agent[:min(12, len(f.Agent))]
		}
		if f.Handle != "" {
			label = f.Handle + ": " + label
		}
		v.Forked = append(v.Forked, feedForkedRow{Agent: f.Agent, Label: label, Forks: count(f.Forks)})
	}
	for _, r := range st.MostRooms {
		v.Rooms = append(v.Rooms, feedRoomRow{Room: r.Room, Subscribers: count(r.Subscribers)})
	}
	return v
}
