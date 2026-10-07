package httpapi

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// C26: an unsigned call's request_id is one retry key across every network
// without a key. A caller whose address moves between tries (a VPN or WARP
// egress that hands each connection a different IPv6 /48) once got a new,
// charged call for each exact retry and ran a different call under a used
// request_id instead of 409. Within the network that made the call a retry
// returns the first answer; from any other network the request_id is taken:
// 409 idempotency_conflict, nothing run, charged or revealed.

// scopeCall GETs a notary stamp with text and request_id from remote.
func scopeCall(t *testing.T, s *Server, remote, text, id string) (int, map[string]any, string) {
	t.Helper()
	w := secReq(s, "GET", "/call/notary/stamp?text="+text+"&request_id="+id, "", remote, nil)
	return w.Code, decodeResult(t, w.Body.Bytes()), w.Body.String()
}

// spent is the credit this network has left today without a key; a charge
// lowers it.
func spent(t *testing.T, s *Server, remote string) float64 {
	t.Helper()
	w := secReq(s, "GET", "/api/allowance", "", remote, nil)
	resources, _ := dig(decodeResult(t, w.Body.Bytes()), "data", "resources").([]any)
	for _, r := range resources {
		if m, _ := r.(map[string]any); m["resource"] == "credit" {
			remaining, _ := m["remaining"].(float64)
			return remaining
		}
	}
	t.Fatalf("no credit in the allowance: %s", w.Body.String())
	return 0
}

func conflictWithoutAnswer(t *testing.T, what string, code int, raw, callID string) {
	t.Helper()
	if code != 409 || !strings.Contains(raw, `"idempotency_conflict"`) || strings.Contains(raw, callID) || strings.Contains(raw, `"receipt"`) {
		t.Fatalf("%s: want 409 idempotency_conflict without the first answer, got %d %s", what, code, raw)
	}
}

// One network (two addresses of one IPv6 /48): limit1, limit1, limit2,
// limit1 is one call, one charge, a 409 and the first answer twice.
func TestCallRequestIDOneNetwork(t *testing.T) {
	_, s := anonCallServer(t, 2000)
	id := "c2156a96436b97f44e5b5e0d8bd3b90f"
	a, b := "[2a09:bac5:58f0:1::7]:1", "[2a09:bac5:58f0:2::9]:1"
	code, first, raw := scopeCall(t, s, a, "one", id)
	callID, _ := dig(first, "data", "call", "id").(string)
	if code != 200 || callID == "" {
		t.Fatalf("first: %d %s", code, raw)
	}
	used := spent(t, s, a)
	if code, again, raw := scopeCall(t, s, b, "one", id); code != 200 || dig(again, "data", "call", "id") != callID {
		t.Fatalf("exact retry from the same /48: %d %s", code, raw)
	}
	code, _, raw = scopeCall(t, s, a, "two", id)
	conflictWithoutAnswer(t, "a different call, same network", code, raw, callID)
	if code, again, raw := scopeCall(t, s, a, "one", id); code != 200 || dig(again, "data", "call", "id") != callID {
		t.Fatalf("exact retry after the conflict: %d %s", code, raw)
	}
	if after := spent(t, s, a); after != used {
		t.Fatalf("charged again: %v then %v", used, after)
	}
}

// Two networks (two /48s of one WARP-style egress, then IPv4): the
// request_id stays taken. Exact or different, a call from another network
// is 409, nothing is charged to anyone, and the first answer is not shown.
func TestCallRequestIDAcrossNetworks(t *testing.T) {
	_, s := anonCallServer(t, 2000)
	id := "d98362541c1e2c30e65b7e2d16cf8183"
	home := "[2a09:bac5:58f0::1]:1"
	others := []string{"[2a09:bac5:5aa4::1]:1", "[2a09:bac1:1234::1]:1", "203.0.113.9:1"}
	code, first, raw := scopeCall(t, s, home, "one", id)
	callID, _ := dig(first, "data", "call", "id").(string)
	if code != 200 || callID == "" {
		t.Fatalf("first: %d %s", code, raw)
	}
	used := spent(t, s, home)
	for _, other := range others {
		before := spent(t, s, other)
		for _, text := range []string{"one", "two", "three"} {
			code, _, raw := scopeCall(t, s, other, text, id)
			conflictWithoutAnswer(t, fmt.Sprintf("%s from %s", text, other), code, raw, callID)
		}
		if after := spent(t, s, other); after != before {
			t.Fatalf("%s was charged for a taken request_id: %v then %v", other, before, after)
		}
	}
	if code, again, raw := scopeCall(t, s, home, "one", id); code != 200 || dig(again, "data", "call", "id") != callID {
		t.Fatalf("the home network's exact retry: %d %s", code, raw)
	}
	code, _, raw = scopeCall(t, s, home, "two", id)
	conflictWithoutAnswer(t, "a different call from home", code, raw, callID)
	if after := spent(t, s, home); after != used {
		t.Fatalf("home charged again: %v then %v", used, after)
	}
}

// Concurrent duplicates, from one network and from several, make one call.
func TestCallRequestIDConcurrent(t *testing.T) {
	_, s := anonCallServer(t, 20000)
	id := "a7a755be018e6ebf60a4b781317dd1c3"
	remotes := []string{"[2a09:bac5:58f0::1]:1", "[2a09:bac5:58f0::2]:1", "[2a09:bac5:5aa4::1]:1", "203.0.113.9:1", "198.51.100.3:1"}
	var mu sync.Mutex
	calls := map[string]bool{}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := secReq(s, "GET", "/call/notary/stamp?text=same&request_id="+id, "", remotes[i%len(remotes)], nil)
			body := decodeResult(t, w.Body.Bytes())
			mu.Lock()
			defer mu.Unlock()
			switch w.Code {
			case 200:
				calls[fmt.Sprint(dig(body, "data", "call", "id"))] = true
			case 409:
			default:
				t.Errorf("concurrent %d: %d %s", i, w.Code, w.Body.String())
			}
		}(i)
	}
	wg.Wait()
	if len(calls) != 1 {
		t.Fatalf("one request_id made %d calls: %v", len(calls), calls)
	}
}
