package nostr_test

import (
	"context"
	"encoding/json"
	"runtime"
	"sync"
	"testing"
	"time"

	"swarmmemo/internal/nostr"
	"swarmmemo/internal/nostr/fakerelay"
)

func TestValidateRelayURL(t *testing.T) {
	for _, ok := range []string{"wss://relay.example", "wss://relay.example/path", "ws://127.0.0.1:7000", "ws://localhost:1", "ws://[::1]:9"} {
		if _, err := nostr.ValidateRelayURL(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "relay.example", "https://relay.example", "ws://relay.example", "ws://10.0.0.1", "wss://user:pw@relay.example", "wss://relay.example/?x=1", "wss://relay.example/#f", "wss://"} {
		if _, err := nostr.ValidateRelayURL(bad); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
	many := make([]string, nostr.MaxRelays+1)
	for i := range many {
		many[i] = "wss://relay.example"
	}
	if _, err := nostr.NewPool(nostr.PoolConfig{Relays: many}); err == nil {
		t.Error("accepted too many relays")
	}
}

func signed(t *testing.T, content string) []byte {
	t.Helper()
	k, err := nostr.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	e := nostr.Event{CreatedAt: time.Now().Unix(), Kind: 1, Tags: [][]string{{"t", "swarmmemo"}}, Content: content}
	if err := k.Sign(&e); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(e)
	return raw
}

func TestPoolSubscribesDeliversPublishesAndReconnects(t *testing.T) {
	relay := fakerelay.New()
	defer relay.Close()
	var mu sync.Mutex
	var got []string
	pool, err := nostr.NewPool(nostr.PoolConfig{
		Relays: []string{relay.URL()}, Since: 10 * time.Minute,
		Filter: map[string]any{"kinds": []int{1}, "#t": []string{"swarmmemo"}},
		OnEvent: func(_ string, raw json.RawMessage) {
			mu.Lock()
			got = append(got, string(raw))
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()
	if !relay.Wait(5*time.Second, func() bool { return relay.Subscribed() == 1 }) {
		t.Fatal("no subscription")
	}
	var filter map[string]any
	_ = json.Unmarshal(relay.Filters()[0], &filter)
	since, _ := filter["since"].(float64)
	if filter["#t"] == nil || time.Since(time.Unix(int64(since), 0)) < 9*time.Minute {
		t.Fatalf("filter %v", filter)
	}
	event := signed(t, "one")
	relay.SendEvent(event)
	relay.SendRaw([]byte(`["EVENT","someone-else",{}]`)) // another subscription: ignored
	relay.SendRaw([]byte(`not json`))
	relay.SendRaw([]byte(`["NOTICE","hello"]`))
	if !relay.Wait(5*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 1 && pool.Stats.Notices.Load() == 1 }) {
		t.Fatal("event not delivered")
	}
	if pool.Stats.BadFrames.Load() != 2 {
		t.Fatalf("bad frames %d", pool.Stats.BadFrames.Load())
	}
	var e nostr.Event
	_ = json.Unmarshal(event, &e)
	if err := pool.Publish(e); err != nil {
		t.Fatal(err)
	}
	if !relay.Wait(5*time.Second, func() bool { return len(relay.Published()) == 1 && pool.Stats.Accepted.Load() == 1 }) {
		t.Fatal("publish not delivered or not acknowledged")
	}
	// A dropped connection comes back, with a new subscription.
	relay.Drop()
	if !relay.Wait(10*time.Second, func() bool { return pool.Stats.Connects.Load() >= 2 && relay.Subscribed() == 1 }) {
		t.Fatal("no reconnect")
	}
	// A relay closing the subscription also gets a fresh one.
	relay.SendRaw([]byte(`["CLOSED","` + relay.SubscriptionID() + `","restricted"]`))
	if !relay.Wait(10*time.Second, func() bool { return pool.Stats.Closed.Load() == 1 && pool.Stats.Connects.Load() >= 3 }) {
		t.Fatal("closed subscription was not reopened")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestPublishNeverBlocksAndStopsCleanly(t *testing.T) {
	before := runtime.NumGoroutine()
	// Nothing listens here: every dial fails and the outbox fills.
	pool, err := nostr.NewPool(nostr.PoolConfig{Relays: []string{"ws://127.0.0.1:1"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()
	e := nostr.Event{Kind: 1, Tags: [][]string{}}
	start := time.Now()
	for i := 0; i < 1000; i++ {
		_ = pool.Publish(e)
	}
	if time.Since(start) > time.Second {
		t.Fatal("publish blocked")
	}
	if pool.Stats.Dropped.Load() == 0 {
		t.Fatal("a full outbox dropped nothing")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	time.Sleep(100 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("goroutines %d -> %d", before, after)
	}
}
