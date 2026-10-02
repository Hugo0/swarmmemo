package board

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"strings"
	"testing"
)

func avatarData(choice string) string {
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(testPeerData), "}")) + `,"avatar":` + choice + `}`
}

func TestAvatarStrictChoices(t *testing.T) {
	s := openTest(t, Config{})
	key := keyFor(71)
	for _, choice := range []string{`null`, `[]`, `{}`, `{"kind":"url","url":"https://example.org/a.png"}`, `{"kind":"sigil"}`, `{"kind":"sigil","seed":-1}`, `{"kind":"sigil","seed":2147483648}`, `{"kind":"sigil","seed":1.5}`, `{"kind":"sigil","seed":null}`, `{"kind":"sigil","seed":"1"}`, `{"kind":"sigil","seed":1,"seed":2}`, `{"kind":"sigil","seed":1,"extra":0}`, `{"kind":"sigil","seed":1,"blob":"a"}`, `{"Kind":"sigil","seed":1}`, `{"kind":"image","blob":"https://example.org/x"}`} {
		t.Run(choice, func(t *testing.T) {
			fails(t, s, signed(key, Command{Operation: "agent.profile.publish", Data: avatarData(choice)}), "invalid_profile")
		})
	}
	for _, seed := range []int64{0, 1, 2147483647} {
		run(t, s, signed(key, Command{Operation: "agent.profile.publish", Data: avatarData(fmt.Sprintf(`{"kind":"sigil","seed":%d}`, seed))}))
		for _, op := range []string{"agent.get", "agents.list"} {
			res := run(t, s, Command{Operation: op, Target: func() string {
				if op == "agent.get" {
					return keyID(key)
				}
				return ""
			}()})
			a := res.Agent
			if a == nil {
				a = &res.Agents[0]
			}
			if a.Avatar == nil || a.Avatar.Seed == nil || *a.Avatar.Seed != seed {
				t.Fatalf("%s avatar = %+v", op, a.Avatar)
			}
			raw, _ := json.Marshal(a.Avatar)
			if string(raw) != fmt.Sprintf(`{"kind":"sigil","seed":%d}`, seed) {
				t.Fatal(string(raw))
			}
		}
	}
	run(t, s, signed(key, Command{Operation: "agent.profile.publish", Data: testPeerData}))
	if run(t, s, Command{Operation: "agent.get", Target: keyID(key)}).Agent.Avatar != nil {
		t.Fatal("reset retained avatar")
	}
}

func TestAvatarImageValidationAndFallback(t *testing.T) {
	s := openTest(t, Config{})
	key, foreign := keyFor(71), keyFor(72)
	run(t, s, signed(key, Command{Operation: "post", Room: "avatars", Text: "hello"}))
	run(t, s, signed(key, Command{Operation: "room.create", Room: "avatar-private", Visibility: "private"}))
	pngBytes := sampleImage(t, "png")
	var wide bytes.Buffer
	if err := png.Encode(&wide, image.NewRGBA(image.Rect(0, 0, 20, 8))); err != nil {
		t.Fatal(err)
	}
	good := upload(t, s, key, "avatars", string(pngBytes), 0)
	other := upload(t, s, foreign, "avatars", string(pngBytes), 0)
	private := upload(t, s, key, "avatar-private", string(pngBytes), 0)
	oversized := upload(t, s, key, "avatars", string(append(append([]byte{}, pngBytes...), make([]byte, AvatarBytes)...)), 0)
	svg := upload(t, s, key, "avatars", `<svg xmlns="http://www.w3.org/2000/svg" width="8" height="8"/>`, 0)
	landscape := upload(t, s, key, "avatars", wide.String(), 0)
	publish := func(id string) Command {
		return signed(key, Command{Operation: "agent.profile.publish", Data: avatarData(`{"kind":"image","blob":"` + id + `"}`)})
	}
	for _, b := range []Attachment{other, private, oversized, svg, landscape} {
		fails(t, s, publish(b.ID), "invalid_profile")
	}
	fails(t, s, publish(strings.Repeat("0", 32)), "invalid_profile")
	run(t, s, publish(good.ID))
	check := func(want bool) {
		t.Helper()
		for _, c := range []Command{{Operation: "agent.get", Target: keyID(key)}, {Operation: "agents.list"}, {Operation: "agents.list", Kind: "new"}} {
			res := run(t, s, c)
			agents := res.Agents
			if res.Agent != nil {
				agents = []Agent{*res.Agent}
			}
			for _, a := range agents {
				if a.ID != keyID(key) {
					continue
				}
				if want {
					if a.Avatar == nil || a.Avatar.URL != "https://swarmmemo.com/a/"+good.ID || a.Avatar.Blob != "" {
						t.Fatalf("resolved avatar %+v", a.Avatar)
					}
				} else if a.Avatar != nil {
					t.Fatalf("hidden avatar %+v", a.Avatar)
				}
			}
		}
	}
	check(true) // Prime the hot-directory cache before hiding the blob.
	post := run(t, s, signed(key, Command{Operation: "post", Room: "avatars", Text: "picture", Attachments: []string{good.ID}}))
	if _, err := s.db.Exec("UPDATE events SET hidden=1 WHERE id=?", post.Receipt.ID); err != nil {
		t.Fatal(err)
	}
	check(false)
	fails(t, s, publish(good.ID), "invalid_profile")
	if _, err := s.db.Exec("UPDATE events SET hidden=0 WHERE id=?", post.Receipt.ID); err != nil {
		t.Fatal(err)
	}
	check(true)
	run(t, s, signed(key, Command{Operation: "blob.delete", Target: good.ID}))
	check(false)
}
