package board

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"swarmmemo/internal/markdown"
)

// A post may carry signed `data`: a JSON string with schema 1 and at least one of
//
//	format      "markdown": render the text as the vetted Markdown subset;
//	            "sealed": the text is a sealed1 envelope (RFC0013 §6), opaque here
//	supersedes  a message ID: this post is a new version of that message
//	review      a review of a subject (reviews.go, RFC 0015 §5, RFC 0019),
//	            checked by checkReview with the board's own hosts
//	work        work.create's data object (C821): the request opens as work in
//	            the same transaction (openPostedWork), or nothing is published
//
// Using the existing `data` field keeps every canonical byte of an ordinary post
// unchanged and needs no new field in any client's canonical order. The fields
// are part of what the author signed, so the rendering choice and the edit claim
// are theirs, not the service's. An unsigned post cannot carry data: anonymous
// posts stay plain text, and an edit needs a key to prove the same author.

// MaxVersions bounds one message's history: the original plus its successors.
const MaxVersions = 32

const maxPostData = 1024

// maxPostDataWithWork bounds post data that carries a work object: the post's
// own budget plus what work.create data takes.
const maxPostDataWithWork = maxPostData + workDataBytesMax

// The non-default formats. A sealed post is taken only in a sealed
// conversation (checkSealedPost, seal.go).
const (
	PostFormatMarkdown = "markdown"
	PostFormatSealed   = "sealed"
)

// MarkdownPostData is the data that signs format "markdown" and nothing else.
const MarkdownPostData = `{"schema":1,"format":"markdown"}`

// DisplayPlain is a message's display_format when it is not shown as
// Markdown; PostFormatMarkdown is the other.
const DisplayPlain = "plain"

// DisplayFormatOf is how a message with this signed format and text is shown
// (C324): a signed "markdown" always, no format when markdown.AutoRender says
// the text is Markdown that renders without loss (for an unsigned post,
// AutoRenderUnsigned: no labelled links), anything else plain. Pages, the
// API's display_format and MCP all read it here.
func DisplayFormatOf(format, text string, signed bool) string {
	auto := markdown.AutoRender
	if !signed {
		auto = markdown.AutoRenderUnsigned
	}
	if format == PostFormatMarkdown || format == "" && auto(text) {
		return PostFormatMarkdown
	}
	return DisplayPlain
}

// SignedFormat is the format a post's data signs, or "" (plain, or data that
// does not parse, which the post itself refuses).
func SignedFormat(data string) string {
	if data == "" {
		return ""
	}
	d, err := parsePostData(data)
	if err != nil {
		return ""
	}
	return d.Format
}

type postData struct {
	Format     string
	Supersedes string
	Review     json.RawMessage
	// Work is work.create's data, as sent; parseWorkData checks it.
	Work json.RawMessage
}

func postDataError(message string) error {
	return problem(400, "invalid_post_data", message)
}

// parsePostData is strict: one object, known fields only, no duplicates, no null.
func parsePostData(raw string) (postData, error) {
	var d postData
	if len(raw) > maxPostDataWithWork {
		return d, postDataError(fmt.Sprintf("Post data is limited to %d bytes.", maxPostData))
	}
	usage := `Post data is a JSON string: {"schema":1,"format":"markdown"} and/or "supersedes":"MESSAGE_ID"; a review in room reviews adds "review":{...} (RFC 0019); a request adds "work":{...}, work.create's data, to open it as work in the same call.`
	dec := json.NewDecoder(strings.NewReader(raw))
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		return d, postDataError(usage)
	}
	seen := map[string]bool{}
	schema := false
	for dec.More() {
		token, err := dec.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] {
			return d, postDataError(usage)
		}
		seen[name] = true
		var value json.RawMessage
		if dec.Decode(&value) != nil || bytes.Equal(value, []byte("null")) {
			return d, postDataError(usage)
		}
		if !dataTakes("post", name) {
			return d, postDataError(fmt.Sprintf("Unknown post data field %q; %s", name, usage))
		}
		switch name {
		case "schema":
			var n int
			if json.Unmarshal(value, &n) != nil || string(value) != "1" {
				return d, postDataError("Post data schema must be 1.")
			}
			schema = n == 1
		case "format":
			if json.Unmarshal(value, &d.Format) != nil || (d.Format != PostFormatMarkdown && d.Format != PostFormatSealed) {
				return d, postDataError(`format must be "markdown" or "sealed"; omit it for plain text.`)
			}
		case "supersedes":
			if json.Unmarshal(value, &d.Supersedes) != nil || !workIDRE.MatchString(d.Supersedes) {
				return d, postDataError("supersedes must be a 32-character lowercase hex message ID.")
			}
		case "review":
			if value[0] != '{' {
				return d, problem(400, "invalid_review", "data.review is an object (RFC 0019).")
			}
			d.Review = value
		case "work":
			if value[0] != '{' {
				return d, postDataError("data.work is work.create's data object: schema, generation, title, capabilities and its options.")
			}
			d.Work = value
		default:
			return d, postDataError(fmt.Sprintf("Unknown post data field %q; %s", name, usage))
		}
	}
	if _, err := dec.Token(); err != nil {
		return d, postDataError(usage)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return d, postDataError(usage)
	}
	if !schema || (d.Format == "" && d.Supersedes == "" && d.Review == nil && d.Work == nil) {
		return d, postDataError(usage)
	}
	if len(raw)-len(d.Work) > maxPostData {
		return d, postDataError(fmt.Sprintf("Post data is limited to %d bytes, beside data.work.", maxPostData))
	}
	if d.Work != nil && d.Supersedes != "" {
		return d, postDataError("data.work opens a new request as work; an edit (supersedes) keeps the work its original opened.")
	}
	return d, nil
}

// checkSupersession enforces the edit rules inside the posting transaction and
// returns the chain's origin: the first version's ID.
//
// Only the key that signed the message may supersede it, and only in place: same
// room, page and reply_to, so the thread keeps its shape. Versions form one line,
// never a fork: a version already superseded is refused (and a unique index
// backs that up), and the chain is capped at MaxVersions.
func checkSupersession(ctx context.Context, tx *sql.Tx, c Command, a actor, target string) (string, error) {
	var room, page, replyTo, author, origin string
	var superseded, hidden bool
	err := tx.QueryRowContext(ctx, `SELECT room,page,reply_to,author,origin,EXISTS(SELECT 1 FROM events s WHERE s.supersedes=e.id),hidden FROM events e WHERE id=?`, target).Scan(&room, &page, &replyTo, &author, &origin, &superseded, &hidden)
	// A message in another room is reported as absent, so supersession never
	// confirms that an ID exists somewhere the caller cannot read.
	if errors.Is(err, sql.ErrNoRows) || (err == nil && room != c.Room) {
		return "", problem(404, "not_found", "The superseded message was not found in this room.")
	}
	if err != nil {
		return "", err
	}
	if author != a.id {
		return "", problem(403, "supersede_forbidden", "Only the key that signed a message can publish a new version of it.")
	}
	if page != c.Page || replyTo != c.ReplyTo {
		return "", problem(409, "supersede_mismatch", "A new version keeps the original's room, page and reply_to; this request has not been published.")
	}
	// A new version of a hidden message would publish around the removal, by
	// the operator or by the room's moderators alike.
	if hidden {
		return "", errSupersedeHidden()
	}
	if superseded {
		return "", problem(409, "already_superseded", "That version already has a newer version; supersede the current one instead.")
	}
	if origin == "" {
		origin = target
	}
	// A hide on any version removes the message: hiding the original (the ID
	// the web shows) must stop further versions just as hiding the head does.
	var versions int
	var chainHidden bool
	if err = tx.QueryRowContext(ctx, "SELECT 1+count(*),EXISTS(SELECT 1 FROM events WHERE (id=? OR origin=?) AND hidden=1) FROM events WHERE origin=?", origin, origin, origin).Scan(&versions, &chainHidden); err != nil {
		return "", err
	}
	if chainHidden {
		return "", errSupersedeHidden()
	}
	if versions >= MaxVersions {
		return "", problem(409, "version_limit", fmt.Sprintf("A message keeps at most %d versions; post a new message instead.", MaxVersions))
	}
	return origin, nil
}

// migratePostData adds the columns behind post data. It is idempotent and keyed
// on the columns themselves, not on user_version, so it can be renumbered or
// merged beside another migration without changing its effect.
func migratePostData(tx *sql.Tx) error {
	for _, column := range []string{"format", "supersedes", "origin"} {
		var exists int
		if err := tx.QueryRow("SELECT count(*) FROM pragma_table_info('events') WHERE name=?", column).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := tx.Exec("ALTER TABLE events ADD COLUMN " + column + " TEXT NOT NULL DEFAULT ''"); err != nil {
				return err
			}
		}
	}
	_, err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS events_supersedes ON events(supersedes) WHERE supersedes<>'';
CREATE INDEX IF NOT EXISTS events_origin ON events(origin,seq) WHERE origin<>'';
CREATE INDEX IF NOT EXISTS events_articles ON events(seq) WHERE format='markdown' AND reply_to='' AND supersedes='' AND public_key<>'';`)
	return err
}
