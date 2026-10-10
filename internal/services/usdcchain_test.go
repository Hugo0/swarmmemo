package services

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The chain reader takes only a public HTTPS endpoint, reads amounts and
// addresses strictly, and turns an unusable answer into chain_unavailable.
func TestUSDCChainReader(t *testing.T) {
	for _, bad := range []string{"", "http://rpc.example.org", "https://127.0.0.1/", "https://rpc.example.org:8545/", "https://user:pw@rpc.example.org/"} {
		if _, err := NewUSDCChain(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	c, err := NewUSDCChain("https://mainnet.base.org")
	if err != nil || c.Network != USDCBaseNetwork || c.Asset.String() != USDCBaseAsset || c.Confirmations != USDCConfirmationsDefault {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	if a, ok := topicAddress("0x000000000000000000000000" + "833589fcd6edb6e08f4c7c32d4f71b54bda02913"); !ok || a != c.Asset {
		t.Fatalf("topic address: %v %v", a, ok)
	}
	if _, ok := topicAddress("0x100000000000000000000000833589fcd6edb6e08f4c7c32d4f71b54bda02913"); ok {
		t.Fatal("a topic with high bytes set read as an address")
	}
	if v, ok := hexBig("0x" + "ff"); !ok || v.Int64() != 255 {
		t.Fatalf("hexBig: %v %v", v, ok)
	}
	for _, bad := range []string{"", "0x", "ff", "0xzz"} {
		if _, ok := hexBig(bad); ok {
			t.Errorf("hexBig %q accepted", bad)
		}
	}
	if _, err := c.Lookup(context.Background(), nil, "0xABC"); err == nil {
		t.Fatal("a malformed hash was looked up")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"rate limited"}}`))
	}))
	defer srv.Close()
	c.UseTestRPC(srv.Client(), srv.URL)
	var ce *ChainError
	if _, err := c.Lookup(context.Background(), nil, "0x0000000000000000000000000000000000000000000000000000000000000001"); !errors.As(err, &ce) || ce.Code != "chain_unavailable" {
		t.Fatalf("an RPC error: %v", err)
	}
}
