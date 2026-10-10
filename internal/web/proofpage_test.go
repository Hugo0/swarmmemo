package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/ots"
)

// pendingCalendar is an OpenTimestamps calendar that accepts every digest and
// never confirms it.
func pendingCalendar(t *testing.T) *httptest.Server {
	cal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/digest" {
			http.NotFound(w, r)
			return
		}
		digest, _ := io.ReadAll(io.LimitReader(r.Body, 64))
		uri := []byte("https://calendar.invalid")
		var out bytes.Buffer
		(&ots.Timestamp{Msg: digest, Attestations: []ots.Attestation{{Tag: ots.TagPending, Payload: append([]byte{byte(len(uri))}, uri...)}}}).Serialize(&out)
		w.Write(out.Bytes())
	}))
	t.Cleanup(cal.Close)
	return cal
}

// confirmedStore answers every proof as anchored in one Bitcoin block, the
// state a real anchor reaches an hour or two after its checkpoint.
// blockTime is the block's own timestamp; 0 is a block whose header is not
// read yet.
type confirmedStore struct {
	*board.Store
	blockTime int64
}

func (s confirmedStore) ReadLogProof(ctx context.Context, index int64, message string, size int64) (board.LogInclusion, error) {
	p, err := s.Store.ReadLogProof(ctx, index, message, size)
	if err == nil {
		p.Anchor = &board.LogAnchor{Size: p.Checkpoint.Size, State: "confirmed", BitcoinHeight: 912345, CheckpointAt: p.Checkpoint.CreatedAt,
			SubmittedAt: p.Checkpoint.CreatedAt, ConfirmedAt: p.Checkpoint.CreatedAt + 4500, Calendars: []string{"a", "b", "c"},
			BlockTime: s.blockTime, Explorer: board.ExplorerBlockURL + "912345"}
	}
	return p, err
}

func body(t *testing.T, s board.Service, path string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w.Code, w.Body.String()
}

func wantAll(t *testing.T, state, page string, want ...string) {
	t.Helper()
	for _, s := range want {
		if !strings.Contains(page, s) {
			t.Errorf("%s: the proof page lacks %q", state, s)
		}
	}
}

// TestProofPageStates: /e/ID/proof tells a post's three steps in each state
// the log passes through: not yet in a checkpoint (a page, not an error), in
// a checkpoint with its anchor pending, and confirmed in a Bitcoin block.
func TestProofPageStates(t *testing.T) {
	f := newArticleFixture(t)
	id := f.post(board.Command{Text: "Proof of life\n\nA second line."})

	code, page := body(t, f.store, "/e/"+id+"/proof")
	if code != 200 {
		t.Fatalf("not logged: %d", code)
	}
	wantAll(t, "not logged", page, "<title>On the record: Proof of life · SwarmMemo</title>", `<meta name="robots" content="noindex">`,
		`id="step-posted"`, "Signed by its key", "Text SHA-256", `class="proof-step is-pending" id="step-logged"`,
		"Waiting for the next checkpoint, signed within about 15 minutes", "Follows the checkpoint, usually 1–2 hours after it.",
		"What this proves", "does not prove who is behind a key", `href="/api/log/proof?message=`+id+`"`,
		"python3 verify_log.py message "+id, `href="/verify"`, `href="/e/`+id+`">← Back to the post`)
	if strings.Contains(page, "Entry ") {
		t.Error("not logged: the page names a log entry")
	}

	if _, err := f.store.SignCheckpoint(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, page = body(t, f.store, "/e/"+id+"/proof")
	wantAll(t, "logged", page, `class="proof-step is-done" id="step-logged"`, "of the log (entries are numbered from 0), in the checkpoint of", "To detect a fork, compare checkpoints",
		`class="proof-step is-pending" id="step-anchored"`, "The checkpoint goes next to OpenTimestamps, a public service that commits fingerprints into Bitcoin blocks,",
		"Until then, this proof rests on the log's signature alone.")

	if err := f.store.AnchorCheckpoints(t.Context(), &ots.Client{Calendars: []string{pendingCalendar(t).URL}}, 3); err != nil {
		t.Fatal(err)
	}
	_, page = body(t, f.store, "/e/"+id+"/proof")
	wantAll(t, "anchor pending", page, "The checkpoint went to 1 OpenTimestamps calendar ", "OpenTimestamps is a public service that commits fingerprints into Bitcoin blocks", "usually in a block 1–2 hours after the checkpoint", `class="proof-step is-pending" id="step-anchored"`)
	if strings.Contains(page, "mempool.space") {
		t.Error("anchor pending: the page links a block")
	}

	// Block time unknown: only our checker's time, labeled as ours.
	_, page = body(t, confirmedStore{Store: f.store}, "/e/"+id+"/proof")
	wantAll(t, "confirmed", page, `class="proof-step is-done" id="step-anchored"`, `<a href="https://mempool.space/block/912345" rel="noopener noreferrer nofollow">block 912345</a>, through`,
		"a public service that commits fingerprints into Bitcoin blocks.", " Our checker saw it confirmed at <time", "and the block, this proof rests on the log's signature alone.")
	if strings.Contains(page, "mined") || strings.Contains(page, ", confirmed <time") {
		t.Error("confirmed: the page names a block time it does not have")
	}
	// Block time known: the block's own timestamp first, ours after.
	_, page = body(t, confirmedStore{Store: f.store, blockTime: 1759900000}, "/e/"+id+"/proof")
	wantAll(t, "block time", page, `block 912345</a>, mined <time datetime="2025-10-08T05:06:40Z">`, "by the block's own timestamp, through", " Our checker saw it confirmed at <time")

	// What went into Bitcoin, and proofs at the anchored size: a later
	// checkpoint does not move the page off the anchored one; the JSON
	// proves inclusion there and a consistency proof links it to the latest.
	anchored, err := f.store.ReadLogCheckpoint(t.Context(), -1)
	if err != nil {
		t.Fatal(err)
	}
	f.post(board.Command{Text: "A later post"})
	if _, err := f.store.SignCheckpoint(t.Context()); err != nil {
		t.Fatal(err)
	}
	latest, err := f.store.ReadLogCheckpoint(t.Context(), -1)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Size <= anchored.Size {
		t.Fatalf("no later checkpoint: %d after %d", latest.Size, anchored.Size)
	}
	n, l := strconv.FormatInt(anchored.Size, 10), strconv.FormatInt(latest.Size, 10)
	sum := sha256.Sum256([]byte(anchored.Note))
	noteSHA := hex.EncodeToString(sum[:])
	_, page = body(t, f.store, "/e/"+id+"/proof")
	wantAll(t, "anchored size", page, "in the checkpoint of "+n+" entries",
		`Tree root <code title="`+anchored.RootHex+`" data-copy="`+anchored.RootHex+`"`, ">"+anchored.RootHex[:16]+"…</code>",
		`Bitcoin commits to the SHA-256 of the <a href="/api/log/checkpoint/note?size=`+n+`" rel="nofollow">signed checkpoint note</a> of `+n+` entries, which contains the tree root: <code title="`+noteSHA+`"`,
		">"+noteSHA[:16]+"…</code>",
		`href="/api/log/proof?message=`+id+`&amp;size=`+n+`"`, "inclusion proof against the checkpoint of "+n+" entries above",
		`href="/api/log/consistency?from=`+n+`&amp;to=`+l+`"`, "is a prefix of the latest, of "+l+" entries",
		"python3 verify_log.py consistency "+n)
	// The linked answers exist: the same store calls the API serves.
	if p, err := f.store.ReadLogProof(t.Context(), -1, id, anchored.Size); err != nil || p.Checkpoint.Size != anchored.Size {
		t.Errorf("proof at the anchored size: %v, size %d", err, p.Checkpoint.Size)
	}
	if _, err := f.store.ReadLogConsistency(t.Context(), anchored.Size, latest.Size); err != nil {
		t.Errorf("consistency %s→%s: %v", n, l, err)
	}
}

// TestProofPageMissingHiddenAndEdited: a private or unknown message is a 404
// exactly as /e/ID; a hidden one keeps its page, without its text, with the
// hide as a later entry; an edited one lists its versions.
func TestProofPageMissingHiddenAndEdited(t *testing.T) {
	f := newArticleFixture(t)
	f.signed(f.key, board.Command{Operation: "room.create", Room: "proof-circle", Visibility: "private"})
	secret := f.signed(f.key, board.Command{Operation: "post", Room: "proof-circle", Text: "members only"}).Receipt.ID
	for _, path := range []string{"/e/" + secret + "/proof", "/e/" + strings.Repeat("0", 32) + "/proof", "/e/nope/proof"} {
		if code, _ := body(t, f.store, path); code != 404 {
			t.Errorf("%s: %d, want 404", path, code)
		}
		if code, _ := body(t, f.store, strings.TrimSuffix(path, "/proof")); code != 404 {
			t.Errorf("%s: /e/ID is not a 404 either", path)
		}
	}

	hidden := f.post(board.Command{Text: "Soon removed"})
	if err := f.store.Moderate(t.Context(), hidden, "Spam.", true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.SignCheckpoint(t.Context()); err != nil {
		t.Fatal(err)
	}
	code, page := body(t, f.store, "/e/"+hidden+"/proof")
	if code != 200 {
		t.Fatalf("hidden: %d", code)
	}
	wantAll(t, "hidden", page, "On the record: Removed message", "This message has been removed. Spam.", "The log keeps the text's fingerprint and the removal.",
		"Later entries about this post", "Hidden · <time", `id="step-logged"`)
	if strings.Contains(page, "Soon removed") {
		t.Error("hidden: the page shows the removed text")
	}

	original := f.post(board.Command{Text: "# Draft\n\nFirst.", Data: markdownData})
	edit := f.post(board.Command{Text: "# Draft\n\nSecond.", Data: supersedes(original)})
	_, page = body(t, f.store, "/e/"+original+"/proof")
	wantAll(t, "edited", page, "<strong>Original</strong> · this page", `href="/e/`+edit+`/proof">Edit 1</a>`)
}

// TestProofLinksAndDetails: the details' proof link opens the page, an
// article titled "Proof" keeps a slug that is not the page's, and every
// public memo carries its collapsed details, rendered from the page's own data:
// the key, channel, edits and exact text the byline leaves out.
func TestProofLinksAndDetails(t *testing.T) {
	f := newArticleFixture(t)
	id := f.post(board.Command{Text: "A plain public post"})
	_, page := body(t, f.store, "/e/"+id)
	if strings.Contains(page, `href="/api/log/proof`) {
		t.Error("the post page links the JSON instead of the proof page")
	}
	wantAll(t, "details", page, `<details class="tip memo-info" data-info-id="`+id+`">`, `<summary aria-label="Message details"`,
		`<dt>ID</dt><dd><code data-copy="`+id+`"`, "<dt>Room</dt><dd>#guides/main</dd>", "<dt>Signed</dt><dd>yes, key <code data-copy=",
		"<dt>Edits</dt><dd>none</dd>", `<dd class="memo-info-log"><a href="/e/`+id+`/proof">see the proof page</a>`,
		`<a class="memo-plain" href="/e/`+id+`/text" rel="nofollow"`, `<svg class="sm-icon sm-icon-info"`)
	if strings.Contains(page, "<details class=\"tip memo-info\" data-info-id=\""+id+"\" open") {
		t.Error("details render open")
	}
	_, room := body(t, f.store, "/r/guides")
	wantAll(t, "listing", room, `data-info-id="`+id+`"`)

	titled := f.post(board.Command{Text: "# Proof\n\nAn article named Proof.", Data: markdownData})
	_, page = body(t, f.store, "/e/"+titled)
	wantAll(t, "article", page, `<link rel="canonical" href="https://swarmmemo.com/e/`+titled+`/proof-1">`, `<dd class="memo-info-log"><a href="/e/`+titled+`/proof">`)
	f.post(board.Command{Text: "# Proof\n\nRevised.", Data: supersedes(titled)})
	_, page = body(t, f.store, "/e/"+titled)
	wantAll(t, "edited article", page, `<a class="memo-edited" href="/e/`+titled+`/history">2 versions</a>, the last <time`)
}

// TestPostConfirmationTracksTheLog: the panel after posting says where the
// post goes next and links its proof page; the memo details built for a live
// memo match the server's.
func TestPostConfirmationTracksTheLog(t *testing.T) {
	js, err := files.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"'Goes into the public log within 15 min and is anchored to Bitcoin within about 2 h · '", "link('', 'track it', memoPath + '/proof')",
		"function memoInfo(event)", "'see the proof page'", "/api/log/proof?message="} {
		if !bytes.Contains(js, []byte(want)) {
			t.Errorf("app.js lacks %s", want)
		}
	}
}
