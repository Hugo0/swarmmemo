package board

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type graphFixture struct {
	t     testing.TB
	store *Store
	seq   int
	now   time.Time
}

func newGraphFixture(t testing.TB) *graphFixture {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "graph.db"), Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	f := &graphFixture{t: t, store: store, now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	store.now = func() time.Time { return f.now }
	return f
}

func (f *graphFixture) room(name, visibility string) {
	f.t.Helper()
	if _, err := f.store.db.Exec("INSERT INTO rooms(name,visibility,owner,created_at) VALUES(?,?,?,0) ON CONFLICT(name) DO UPDATE SET visibility=excluded.visibility", name, visibility, "owner"); err != nil {
		f.t.Fatal(err)
	}
}

// post inserts one event and returns its ID. author "" is anonymous.
func (f *graphFixture) post(room, author, text, replyTo, recipient string, at int64, hidden int) string {
	f.t.Helper()
	f.seq++
	id := fmt.Sprintf("%032x", f.seq)
	key, name := "", "anonymous"
	if author != "" {
		key, name = "key-"+author, author
	}
	if _, err := f.store.db.Exec(`INSERT INTO events(display_seq,id,room,page,text,kind,author,account,handle,public_key,signature,payload,created_at,hash,reply_to,recipient,hidden,origin) VALUES(?,?,?,'main',?,'note',?,?,'',?,'','',?,'',?,?,?,?)`, f.seq, id, room, text, name, name, key, at, replyTo, recipient, hidden, id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func fp(c byte) string { return strings.Repeat(string(c), 64) }

func decodeGraph(t *testing.T, s *GraphSnapshot) graphDocument {
	t.Helper()
	var d graphDocument
	if err := json.Unmarshal(s.JSON, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// TestGraphNeverCarriesPrivateOrSealedData proves the graph is built from
// visible public messages only: nothing from a private room, a conversation
// (DM or group, sealed or not), an addressed message or a hidden post, and
// never any message text, appears in it.
func TestGraphNeverCarriesPrivateOrSealedData(t *testing.T) {
	f := newGraphFixture(t)
	alice, bob, secret, dmOnly, sealedOnly, hiddenOnly, addressedOnly := fp('a'), fp('b'), fp('c'), fp('d'), fp('e'), fp('f'), fp('9')
	f.room("lobby", "public")
	f.room("help", "public")
	f.room("vault", "private")
	f.room("~aaaaaaaaaaaaaaaaaaaaaaaaaa", "private")
	f.room("~bbbbbbbbbbbbbbbbbbbbbbbbbb", "private")
	if _, err := f.store.db.Exec("INSERT INTO conversations(room,kind,sealed,created_by,created_at) VALUES('~aaaaaaaaaaaaaaaaaaaaaaaaaa','dm',0,'x',0),('~bbbbbbbbbbbbbbbbbbbbbbbbbb','group',1,'x',0)"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.Exec("INSERT INTO identities(id,public_key,account,handle,created_at,last_seen) VALUES(?,?,?,'alice-h',0,0),(?,?,?,'secret-h',0,0)", alice, "pk-a", alice, secret, "pk-c", secret); err != nil {
		t.Fatal(err)
	}
	base := f.now.Add(-time.Hour).Unix()
	root := f.post("lobby", alice, "PUBLIC-TEXT-1", "", "", base, 0)
	f.post("lobby", bob, "PUBLIC-TEXT-2", root, "", base+1, 0)
	f.post("help", "", "PUBLIC-TEXT-3", "", "", base+2, 0)
	privateRoot := f.post("vault", secret, "PRIVATE-TEXT", "", "", base+3, 0)
	f.post("lobby", bob, "PUBLIC-REPLY-TO-PRIVATE", privateRoot, "", base+4, 0) // parent private: no edge
	f.post("~aaaaaaaaaaaaaaaaaaaaaaaaaa", dmOnly, "DM-TEXT", "", "", base+5, 0)
	f.post("~bbbbbbbbbbbbbbbbbbbbbbbbbb", sealedOnly, "SEALED-CIPHERTEXT", "", "", base+6, 0)
	f.post("lobby", hiddenOnly, "HIDDEN-TEXT", root, "", base+7, 1)
	f.post("lobby", addressedOnly, "ADDRESSED-TEXT", root, alice, base+8, 0)

	snap, err := f.store.ReadGraph(context.Background(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	body := string(snap.JSON)
	for _, banned := range []string{"TEXT", "CIPHERTEXT", secret, dmOnly, sealedOnly, hiddenOnly, addressedOnly, "vault", "~", "secret-h", "key-"} {
		if strings.Contains(body, banned) {
			t.Errorf("graph contains %q: %s", banned, body)
		}
	}
	d := decodeGraph(t, snap)
	if d.Meta.Messages != 4 || len(d.Rooms) != 2 || d.Rooms[0] != "help" || d.Rooms[1] != "lobby" {
		t.Fatalf("messages %d rooms %v", d.Meta.Messages, d.Rooms)
	}
	// alice, bob, the anonymous pool of help, then the two rooms.
	if got := d.Nodes.Kind; fmt.Sprint(got) != "[0 0 1 2 2]" {
		t.Fatalf("kinds %v", got)
	}
	if d.Nodes.Label[0] != "alice-h" || d.Nodes.Label[1] != bob[:12] || d.Nodes.Label[2] != "anonymous · help" || d.Nodes.Key[2] != nil {
		t.Fatalf("labels %v keys %v", d.Nodes.Label, d.Nodes.Key)
	}
	if fmt.Sprint(d.Nodes.Posts) != "[1 2 1 1 3]" {
		t.Fatalf("posts %v", d.Nodes.Posts)
	}
	if r := d.Edges.Reply; len(r.Src) != 1 || r.Src[0] != 1 || r.Dst[0] != 0 || r.W[0] != 1 {
		t.Fatalf("reply edges %+v", r)
	}
	if m := d.Edges.Member; len(m.Src) != 3 {
		t.Fatalf("member edges %+v", m)
	}

	// A private room asked for by name is empty, not an error that confirms it.
	f.now = f.now.Add(time.Minute)
	snap, err = f.store.ReadGraph(context.Background(), "vault", 0)
	if err != nil {
		t.Fatal(err)
	}
	if d := decodeGraph(t, snap); d.Meta.Messages != 0 || len(d.Nodes.Kind) != 0 || strings.Contains(string(snap.JSON), secret) {
		t.Fatalf("private room graph: %s", snap.JSON)
	}
}

func TestGraphRoomSinceAndCache(t *testing.T) {
	f := newGraphFixture(t)
	f.room("lobby", "public")
	f.room("help", "public")
	base := f.now.Add(-time.Hour).Unix()
	a := f.post("lobby", fp('a'), "x", "", "", base, 0)
	f.post("help", fp('b'), "x", a, "", base+10, 0)
	f.post("help", fp('c'), "x", "", "", base+20, 0)
	ctx := context.Background()
	one, err := f.store.ReadGraph(ctx, "help", 0)
	if err != nil {
		t.Fatal(err)
	}
	if d := decodeGraph(t, one); d.Meta.Messages != 2 || len(d.Edges.Reply.Src) != 0 || d.Meta.Room != "help" {
		t.Fatalf("room graph %s", one.JSON)
	}
	recent, err := f.store.ReadGraph(ctx, "", base+10)
	if err != nil {
		t.Fatal(err)
	}
	if d := decodeGraph(t, recent); d.Meta.Messages != 2 || d.Meta.T0 != base+10 {
		t.Fatalf("since graph %s", recent.JSON)
	}
	// Within the TTL the same snapshot is shared; a new post shows after it.
	f.post("help", fp('d'), "x", "", "", base+30, 0)
	again, _ := f.store.ReadGraph(ctx, "help", 0)
	if again != one {
		t.Fatal("graph recomputed within its TTL")
	}
	f.now = f.now.Add(GraphTTL)
	later, _ := f.store.ReadGraph(ctx, "help", 0)
	if d := decodeGraph(t, later); d.Meta.Messages != 3 || later.ETag == one.ETag {
		t.Fatalf("graph after TTL %s", later.JSON)
	}
}

// TestGraphScalesTo100kMessages builds the graph over 100,000 public
// messages, the size the prototype was measured on, inside a generous bound
// that still catches an accidental quadratic step.
func TestGraphScalesTo100kMessages(t *testing.T) {
	if testing.Short() {
		t.Skip("large fixture")
	}
	f := newGraphFixture(t)
	seedGraph(t, f, 100_000)
	start := time.Now()
	snap, err := f.store.ReadGraph(context.Background(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	if d := decodeGraph(t, snap); d.Meta.Messages != 100_000 {
		t.Fatalf("messages %d", d.Meta.Messages)
	}
	t.Logf("100k messages: %v, %d bytes", took, len(snap.JSON))
	if took > 15*time.Second {
		t.Fatalf("graph over 100k messages took %v", took)
	}
}

func BenchmarkGraph100k(b *testing.B) {
	f := newGraphFixture(b)
	seedGraph(b, f, 100_000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := f.store.computeGraph(context.Background(), "", 0, f.now); err != nil {
			b.Fatal(err)
		}
	}
}

// seedGraph inserts n public messages from 2,000 identities in 20 rooms,
// a third of them replies, in one transaction.
func seedGraph(t testing.TB, f *graphFixture, n int) {
	for r := 0; r < 20; r++ {
		f.room(fmt.Sprintf("room-%d", r), "public")
	}
	tx, err := f.store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO events(display_seq,id,room,page,text,kind,author,account,handle,public_key,signature,payload,created_at,hash,reply_to,recipient,hidden,origin) VALUES(?,?,?,'main','text','note',?,?,'',?,'','',?,'',?,'',0,?)`)
	if err != nil {
		t.Fatal(err)
	}
	base := f.now.Add(-90 * 24 * time.Hour).Unix()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%032x", i+1)
		author := fmt.Sprintf("%064x", i*7919%2000+1)
		key := "key-" + author
		if i%10 == 0 {
			author, key = "anonymous", ""
		}
		parent := ""
		if i%3 == 1 {
			parent = fmt.Sprintf("%032x", i*31%(i)+1)
		}
		if _, err = stmt.Exec(i+1, id, fmt.Sprintf("room-%d", i*13%20), author, author, key, base+int64(i)*60, parent, id); err != nil {
			t.Fatal(err)
		}
	}
	if err = stmt.Close(); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
