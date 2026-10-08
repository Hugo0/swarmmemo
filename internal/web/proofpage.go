package web

// /e/ID/proof: a public post's place in the transparency log
// (board/transparency.go), told for people. The same facts as
// /api/log/proof?message=ID, as three steps: posted, in a signed checkpoint,
// anchored to Bitcoin. A post newer than the latest checkpoint is simply not
// there yet, so the page says when it will be instead of failing. Private
// and unknown messages are a 404, exactly as /e/ID.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"swarmmemo/internal/board"
)

// proofSuffix names the proof page at /e/ID/proof; like historySuffix, a
// slug never equals it.
const proofSuffix = "proof"

// logProofReader is the store's message proof, the one /api/log/proof serves.
type logProofReader interface {
	ReadLogProof(ctx context.Context, index int64, message string, size int64) (board.LogInclusion, error)
}

// proofView is /e/ID/proof.
type proofView struct {
	ID, Title, PostPath, JSONPath string
	Message                       board.Message
	// Logged: the post's leaf is in a signed checkpoint, Leaf, of
	// CheckpointSize leaves signed at CheckpointAt (the first checkpoint
	// that covers it when it is anchored, else the latest).
	Logged                       bool
	Leaf                         int64
	CheckpointSize, CheckpointAt int64
	// Anchor is that checkpoint's Bitcoin anchor; nil until it is submitted.
	Anchor *board.LogAnchor
	// Explorer is the confirmed block's page on a public block explorer.
	Explorer string
	// Related are the hides and restores logged about this message, and
	// Versions its edits, oldest first, when there is more than one.
	Related  []proofEntry
	Versions []proofEntry
}

// proofEntry is one related log entry or version.
type proofEntry struct {
	Label, Reason, Href string
	At, Leaf            int64
	Current             bool
}

// Confirmed reports a Bitcoin-confirmed anchor.
func (v *proofView) Confirmed() bool { return v.Anchor != nil && v.Anchor.State == "confirmed" }

// leafFields are the parts of a leaf the page names.
type leafFields struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Op     string `json:"op"`
	At     int64  `json:"at"`
	Reason string `json:"reason"`
}

// moderationLabels names a moderation leaf's op for people.
var moderationLabels = map[string]string{"hide": "Hidden", "restore": "Restored"}

func loadProof(r *http.Request, p *page, service board.Service, id string) int {
	versions, okV := service.(versionReader)
	logs, okL := service.(logProofReader)
	p.View = "missing"
	if !okV || !okL || !validMessageID(id) {
		return 404
	}
	all, err := versions.PublicVersions(r.Context(), id)
	if err != nil {
		return 503
	}
	var m *board.Message
	for i := range all {
		if all[i].ID == id {
			m = &all[i]
		}
	}
	if m == nil {
		return 404
	}
	v := &proofView{ID: id, Message: *m, PostPath: "/e/" + url.PathEscape(id), JSONPath: "/api/log/proof?message=" + url.QueryEscape(id)}
	v.Title = postTitle(*m)
	if m.Hidden || v.Title == "" {
		v.Title = "Removed message"
	}
	proof, err := logs.ReadLogProof(r.Context(), -1, id, -1)
	var be *board.Error
	switch {
	case err == nil:
		v.Logged, v.Leaf = true, proof.Leaf.Index
		v.CheckpointSize, v.CheckpointAt = proof.Checkpoint.Size, proof.Checkpoint.CreatedAt
		if a := proof.Anchor; a != nil {
			v.Anchor = a
			v.CheckpointSize, v.CheckpointAt = a.Size, a.CheckpointAt
			if a.State == "confirmed" {
				v.Explorer = a.Explorer
			}
		}
		for _, rel := range proof.Related {
			var f leafFields
			if json.Unmarshal([]byte(rel.Leaf.Data), &f) != nil {
				continue
			}
			label := moderationLabels[f.Op]
			if label == "" {
				label = f.Op
			}
			v.Related = append(v.Related, proofEntry{Label: label, Reason: f.Reason, At: f.At, Leaf: rel.Leaf.Index,
				Href: "/api/log/proof?leaf=" + strconv.FormatInt(rel.Leaf.Index, 10)})
		}
	case errors.As(err, &be) && (be.Code == "not_logged" || be.Code == "no_checkpoint"):
		// Newer than the latest checkpoint: the next one takes it in.
	default:
		return 503
	}
	if len(all) > 1 {
		for i, ver := range all {
			label := "Edit " + strconv.Itoa(i)
			if i == 0 {
				label = "Original"
			}
			v.Versions = append(v.Versions, proofEntry{Label: label, At: ver.CreatedAt, Current: ver.ID == id,
				Href: "/e/" + url.PathEscape(ver.ID) + "/" + proofSuffix})
		}
	}
	p.View, p.NoIndex = "proof", true
	p.RoomName, p.PageName = m.Room, m.Page
	p.Title = "On the record: " + v.Title
	p.Description = "Where this post stands in SwarmMemo's public, Bitcoin-anchored log, and how to check it yourself."
	p.Canonical = v.PostPath + "/" + proofSuffix
	p.Proof = v
	return 200
}

// recordPage is the proof page of an agent's first log entry when that entry
// is a post; "" when it is a key event (or not in a checkpoint yet), whose
// proof stays the JSON the record links.
func recordPage(ctx context.Context, service board.Service, rec *board.AgentRecord) string {
	logs, ok := service.(logProofReader)
	if rec == nil || !ok {
		return ""
	}
	proof, err := logs.ReadLogProof(ctx, rec.FirstLeaf, "", -1)
	var f leafFields
	if err != nil || json.Unmarshal([]byte(proof.Leaf.Data), &f) != nil || f.Kind != "message" || !validMessageID(f.ID) {
		return ""
	}
	return "/e/" + f.ID + "/" + proofSuffix
}
