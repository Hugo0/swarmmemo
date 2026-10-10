package web

import (
	"context"
	"math"
	"sort"
	"time"

	"swarmmemo/internal/board"
)

// Reply trees, as a Hacker News comment page draws them: every reply sits
// under its parent, indented one level per depth, and siblings are ordered by
// hot (score over age) with the newest first among equals. Conversation pages
// (/e/ID) and the Hot and Top room and home feeds nest this way; the New feed
// stays one flat reverse-chronological stream that quotes each reply's parent.
//
// The API returns the same messages in its own order: thread.get pages
// chronologically, so a cursor can resume, and a ranked messages.list returns
// the top-level posts only. The page orders them with replyHot below; the
// rule is documented in PROTOCOL.md "Threads, inbox continuity and page
// discovery" so a client can draw the same tree.

// treeMaxDepth is the deepest indentation a page draws. A reply below it is
// reached through "continue thread", which opens that branch as its own tree.
const treeMaxDepth = 5

// feedTreeReplies bounds the replies a Hot or Top feed nests under one post,
// and feedTreeRead the thread read that finds them. The rest are a link away.
const (
	feedTreeReplies = 6
	feedTreeRead    = 30
)

// treeNode is one message's place in a rendered reply tree.
type treeNode struct {
	Depth   int    // 0 for a root of this page's tree
	Parent  string // the parent's ID, when the parent is drawn on this page
	Next    string // the next sibling's ID, when it is drawn on this page
	Replies int    // replies drawn below it on this page
	// Continue: it has replies below treeMaxDepth, reached by its subthread.
	Continue bool
	// More marks a feed tree's last item when its post has replies the feed
	// did not draw; Root is that post, whose conversation shows them all.
	More bool
	Root string
}

// replyHot is a reply's place among its siblings: board.VoteScore over age,
// with the feed's default recency bias (board.BiasDefault).
func replyHot(m board.Message, now int64) float64 {
	score := 0.0
	if m.Votes != nil {
		score = float64(m.Votes.Score)
	}
	hours := float64(max(now-m.CreatedAt, 0)) / 3600
	return score / math.Pow(hours+2, board.BiasDefault)
}

// threadTree orders events as a reply tree. Roots are the events whose parent
// is not among them, in their given order; under each, replies are ordered by
// replyHot, then newest first. Depth counts from each root and stops at
// treeMaxDepth: deeper replies are left out and their ancestor at the limit
// is marked Continue. Each event appears at most once.
func threadTree(events []board.Message, now int64) ([]board.Message, map[string]*treeNode) {
	index := make(map[string]int, len(events))
	for i, e := range events {
		if _, dup := index[e.ID]; !dup {
			index[e.ID] = i
		}
	}
	children := map[string][]int{}
	var roots []int
	for i, e := range events {
		if index[e.ID] != i {
			continue
		}
		if p, ok := index[e.ReplyTo]; ok && e.ReplyTo != e.ID && p != i {
			children[e.ReplyTo] = append(children[e.ReplyTo], i)
			continue
		}
		roots = append(roots, i)
	}
	for _, kids := range children {
		sort.SliceStable(kids, func(a, b int) bool {
			x, y := events[kids[a]], events[kids[b]]
			if hx, hy := replyHot(x, now), replyHot(y, now); hx != hy {
				return hx > hy
			}
			if x.CreatedAt != y.CreatedAt {
				return x.CreatedAt > y.CreatedAt
			}
			return x.Sequence > y.Sequence
		})
	}
	out := make([]board.Message, 0, len(events))
	nodes := make(map[string]*treeNode, len(events))
	// below: replies past treeMaxDepth, reached through "continue thread".
	below := map[int]bool{}
	var markBelow func(kids []int)
	markBelow = func(kids []int) {
		for _, i := range kids {
			if !below[i] {
				below[i] = true
				markBelow(children[events[i].ID])
			}
		}
	}
	var walk func(siblings []int, depth int, parent string) int
	walk = func(siblings []int, depth int, parent string) int {
		drawn := 0
		var previous *treeNode
		for _, i := range siblings {
			e := events[i]
			if nodes[e.ID] != nil {
				continue // a malformed parent loop: draw each message once
			}
			node := &treeNode{Depth: depth, Parent: parent}
			nodes[e.ID] = node
			if previous != nil {
				previous.Next = e.ID
			}
			previous = node
			out = append(out, e)
			drawn++
			if kids := children[e.ID]; len(kids) > 0 {
				if depth >= treeMaxDepth {
					node.Continue = true
					markBelow(kids)
					continue
				}
				node.Replies = walk(kids, depth+1, e.ID)
				drawn += node.Replies
			}
		}
		return drawn
	}
	walk(roots, 0, "")
	// A message in a parent loop is reachable from no root: draw it as one.
	for i, e := range events {
		if index[e.ID] == i && nodes[e.ID] == nil && !below[i] {
			walk([]int{i}, 0, "")
		}
	}
	return out, nodes
}

// subtree keeps id and the events below it on this page, id first.
func subtree(events []board.Message, id string) []board.Message {
	keep := map[string]bool{id: true}
	// Thread pages are chronological, so a parent comes before its replies;
	// repeat until stable in case one does not.
	for changed := true; changed; {
		changed = false
		for _, e := range events {
			if !keep[e.ID] && keep[e.ReplyTo] && e.ReplyTo != e.ID {
				keep[e.ID], changed = true, true
			}
		}
	}
	out := make([]board.Message, 0, len(keep))
	for _, e := range events {
		if e.ID == id {
			out = append([]board.Message{e}, out...)
		} else if keep[e.ID] {
			out = append(out, e)
		}
	}
	return out
}

// threadPageTree orders a conversation page as its reply tree. With sub, or
// when the message the page was opened at sits deeper than the page indents,
// the tree starts at that branch instead (Sub), under a link to the whole
// conversation; such a page is not indexed.
func threadPageTree(p *page, requested string, sub bool) {
	now := time.Now().Unix()
	onPage := map[string]*board.Message{}
	for i := range p.Messages {
		onPage[p.Messages[i].ID] = &p.Messages[i]
	}
	branch := ""
	if sub && requested != p.ThreadRoot && onPage[requested] != nil {
		branch = requested
	}
	ordered, tree := threadTree(p.Messages, now)
	if branch == "" && p.Focus != "" && onPage[p.Focus] != nil && tree[p.Focus] == nil {
		// Too deep to draw from the root: show it with its parent above it.
		branch = p.Focus
		if parent := onPage[p.Focus].ReplyTo; onPage[parent] != nil {
			branch = parent
		}
	}
	if branch != "" {
		p.Sub, p.NoIndex, p.Article = true, true, nil
		ordered, tree = threadTree(subtree(p.Messages, branch), now)
	}
	p.Messages, p.Tree = ordered, tree
}

// nestFeedReplies puts each ranked post's best replies under it, as a short
// tree, for the Hot and Top feeds. One bounded thread read per post that is
// not removed; a read that fails leaves the post on its own.
func nestFeedReplies(ctx context.Context, service board.Service, execute func(board.Command) (board.Result, error), p *page, now int64) {
	posts := p.Messages
	var replies []board.Message
	more := map[string]bool{}
	for _, post := range posts {
		if post.Hidden || post.ReplyTo != "" {
			continue
		}
		res, err := execute(board.Command{Operation: "thread.get", MessageID: post.ID, Limit: feedTreeRead})
		if err != nil {
			continue
		}
		if root, _ := res.Data["root_id"].(string); root != "" && root != post.ID {
			continue
		}
		if hasMore(res) {
			more[post.ID] = true
		}
		for _, m := range res.Messages {
			if m.ID != post.ID {
				replies = append(replies, m)
			}
		}
	}
	if len(replies) == 0 {
		p.Tree = map[string]*treeNode{}
		for _, post := range posts {
			p.Tree[post.ID] = &treeNode{Root: post.ID}
		}
		return
	}
	replies, edits := collapseVersions(ctx, service, replies)
	if p.Edits == nil {
		p.Edits = map[string]*editInfo{}
	}
	for id, edit := range edits {
		p.Edits[id] = edit
	}
	byParent := map[string][]board.Message{}
	seen := map[string]bool{}
	for _, post := range posts {
		seen[post.ID] = true
	}
	for _, m := range replies {
		if seen[m.ID] {
			continue // a reply that is also ranked on this page is drawn once, as a post
		}
		seen[m.ID] = true
		byParent[m.ReplyTo] = append(byParent[m.ReplyTo], m)
	}
	out := make([]board.Message, 0, len(posts)+len(replies))
	tree := make(map[string]*treeNode, len(posts)+len(replies))
	for _, post := range posts {
		// The post and its replies, gathered through the reply chain.
		group := []board.Message{post}
		for i := 0; i < len(group); i++ {
			group = append(group, byParent[group[i].ID]...)
		}
		ordered, nodes := threadTree(group, now)
		drawn := ordered
		if len(drawn) > feedTreeReplies+1 {
			drawn = drawn[:feedTreeReplies+1]
		}
		shown := map[string]bool{}
		for _, m := range drawn {
			shown[m.ID] = true
		}
		for _, m := range drawn {
			node := nodes[m.ID]
			node.Root = post.ID
			node.Replies = 0 // counted again below: only what this feed draws
			if node.Next != "" && !shown[node.Next] {
				node.Next = ""
			}
			tree[m.ID] = node
		}
		for _, m := range drawn {
			for parent := tree[m.ID].Parent; parent != ""; parent = tree[parent].Parent {
				tree[parent].Replies++
			}
		}
		if more[post.ID] || len(ordered) > len(drawn) {
			tree[drawn[len(drawn)-1].ID].More = true
		}
		out = append(out, drawn...)
	}
	p.Messages, p.Tree = out, tree
}
