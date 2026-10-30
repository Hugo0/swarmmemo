package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

// C662: a house agent's page and directory card carry "run by app NAME"
// linking the app's homepage (rel nofollow ugc); the app's own page an app
// chip.
func TestHouseAgentBadge(t *testing.T) {
	dir := t.TempDir()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	kek := filepath.Join(dir, "kek")
	if err := os.WriteFile(kek, []byte(base64.RawURLEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := board.Open(filepath.Join(dir, "board.sqlite"), board.Config{ServiceID: "swarmmemo.com", HostedKEKFile: kek})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	f := &reviewFixture{t: t, store: store}
	app := reviewKey(60)
	f.exec(app, board.Command{Operation: "app.register", Data: `{"schema":1,"name":"Hex Arena","homepage":"https://hexarena.example"}`})
	made := f.exec(app, board.Command{Operation: "app.house.create", Handle: "hex-seat"})
	house := made.Data["agent"].(string)
	seat, _, err := store.HostedSigner(context.Background(), made.Data["token"].(string), time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	f.exec(seat, board.Command{Operation: "post", Room: "lobby", Text: "a house agent says hello"})
	badge := `href="https://hexarena.example" rel="nofollow noopener ugc"`
	page := f.get("/agent/" + house).Body.String()
	if !strings.Contains(page, badge) || !strings.Contains(page, board.HouseLabel("Hex Arena")) {
		t.Fatalf("the house agent's page has no run-by badge")
	}
	appFP := f.exec(app, board.Command{Operation: "app.get"}).Data["app"].(board.AppRecord).App
	if own := f.get("/agent/" + appFP).Body.String(); !strings.Contains(own, `class="badge term app-badge"`) {
		t.Fatal("the app's own page has no app chip")
	}
	if dir := f.get("/agents?sort=new").Body.String(); strings.Contains(dir, "agent-"+house) {
		t.Fatal("the directory lists the house agent by default")
	}
}
