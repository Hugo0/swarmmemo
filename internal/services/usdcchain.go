package services

// The USDC chain reader (RFC 0016): work rewards in USDC are paid off the
// board, by the payer's own wallet, and work.settle names the transaction.
// This reads that transaction back from an EVM JSON-RPC endpoint the
// operator configures: its receipt (status, block, logs), the block's time
// and the chain head, and returns the token's Transfer logs it carried. It
// is read only: no key, no signing, no broadcast.

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Base mainnet and its native USDC, the defaults a WORK_USDC_RPC_URL reads.
const (
	USDCBaseNetwork = "eip155:8453"
	USDCBaseAsset   = "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"
	// USDCConfirmationsDefault is how many blocks deep (the transaction's
	// own block counting as one) a settlement must be.
	USDCConfirmationsDefault = 3
	usdcRPCTimeout           = 15 * time.Second
	usdcRPCBytes             = 256 << 10
	// USDCLogsMax bounds the logs of one receipt we read.
	USDCLogsMax = 256
)

// erc20TransferTopic is keccak256("Transfer(address,address,uint256)").
const erc20TransferTopic = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"

// USDCChain is the configured chain reader.
type USDCChain struct {
	Network       string // CAIP-2
	Asset         EVMAddress
	RPCURL        string
	Confirmations int64
	client        *http.Client
}

// NewUSDCChain is a reader of Base USDC through rpcURL, a public HTTPS
// JSON-RPC endpoint.
func NewUSDCChain(rpcURL string) (*USDCChain, error) {
	if err := checkX402URL(rpcURL); err != nil {
		return nil, fmt.Errorf("work usdc: WORK_USDC_RPC_URL (%s)", err.Error())
	}
	asset, _ := ParseEVMAddress(USDCBaseAsset)
	return &USDCChain{Network: USDCBaseNetwork, Asset: asset, RPCURL: rpcURL, Confirmations: USDCConfirmationsDefault}, nil
}

// UseTestRPC sends every call to url through client: an httptest server
// stands in for the chain. Tests only.
func (c *USDCChain) UseTestRPC(client *http.Client, url string) { c.client, c.RPCURL = client, url }

// USDCTransfer is one Transfer log of the configured token.
type USDCTransfer struct {
	From, To EVMAddress
	Value    *big.Int
}

// USDCTx is a mined transaction as settlement reads it.
type USDCTx struct {
	Hash          string
	Success       bool
	Block         int64
	BlockTime     int64
	Confirmations int64
	Transfers     []USDCTransfer
}

// ChainError is a lookup that did not produce a transaction: Code is
// usdc_tx_not_found (unknown or not mined yet) or chain_unavailable.
type ChainError struct{ Code, Reason string }

func (e *ChainError) Error() string { return "work usdc: " + e.Code + " (" + e.Reason + ")" }

var usdcTxHashRE = regexp.MustCompile(`^0x[0-9a-f]{64}$`)

// ValidTxHash says whether s is a lowercase 0x-prefixed 32-byte hash.
func ValidTxHash(s string) bool { return usdcTxHashRE.MatchString(s) }

// Lookup reads txHash: its receipt, its block's timestamp and the head.
func (c *USDCChain) Lookup(ctx context.Context, dial func(ctx context.Context, network, addr string) (net.Conn, error), txHash string) (USDCTx, error) {
	if !ValidTxHash(txHash) {
		return USDCTx{}, &ChainError{Code: "usdc_tx_not_found", Reason: "malformed hash"}
	}
	client := c.httpClient(dial)
	var receipt *struct {
		Status      string `json:"status"`
		BlockNumber string `json:"blockNumber"`
		Logs        []struct {
			Address string   `json:"address"`
			Topics  []string `json:"topics"`
			Data    string   `json:"data"`
			Removed bool     `json:"removed"`
		} `json:"logs"`
	}
	if err := c.call(ctx, client, "eth_getTransactionReceipt", []any{txHash}, &receipt); err != nil {
		return USDCTx{}, err
	}
	if receipt == nil {
		return USDCTx{}, &ChainError{Code: "usdc_tx_not_found", Reason: "no receipt"}
	}
	out := USDCTx{Hash: txHash, Success: receipt.Status == "0x1"}
	var ok bool
	if out.Block, ok = hexInt(receipt.BlockNumber); !ok {
		return USDCTx{}, &ChainError{Code: "chain_unavailable", Reason: "malformed receipt"}
	}
	if len(receipt.Logs) > USDCLogsMax {
		return USDCTx{}, &ChainError{Code: "chain_unavailable", Reason: "too many logs"}
	}
	for _, l := range receipt.Logs {
		addr, ok := ParseEVMAddress(strings.ToLower(l.Address))
		if !ok || addr != c.Asset || l.Removed || len(l.Topics) != 3 || strings.ToLower(l.Topics[0]) != erc20TransferTopic {
			continue
		}
		from, ok1 := topicAddress(l.Topics[1])
		to, ok2 := topicAddress(l.Topics[2])
		value, ok3 := hexBig(l.Data)
		if !ok1 || !ok2 || !ok3 {
			continue
		}
		out.Transfers = append(out.Transfers, USDCTransfer{From: from, To: to, Value: value})
	}
	var head string
	if err := c.call(ctx, client, "eth_blockNumber", []any{}, &head); err != nil {
		return USDCTx{}, err
	}
	h, ok := hexInt(head)
	if !ok {
		return USDCTx{}, &ChainError{Code: "chain_unavailable", Reason: "malformed head"}
	}
	out.Confirmations = max(h-out.Block+1, 0)
	var block *struct {
		Timestamp string `json:"timestamp"`
	}
	if err := c.call(ctx, client, "eth_getBlockByNumber", []any{"0x" + strconv.FormatInt(out.Block, 16), false}, &block); err != nil {
		return USDCTx{}, err
	}
	if block == nil {
		return USDCTx{}, &ChainError{Code: "chain_unavailable", Reason: "no block"}
	}
	if out.BlockTime, ok = hexInt(block.Timestamp); !ok {
		return USDCTx{}, &ChainError{Code: "chain_unavailable", Reason: "malformed block"}
	}
	return out, nil
}

func (c *USDCChain) call(ctx context.Context, client *http.Client, method string, params []any, dest any) error {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	ctx, cancel := context.WithTimeout(ctx, usdcRPCTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.RPCURL, bytes.NewReader(body))
	if err != nil {
		return &ChainError{Code: "chain_unavailable", Reason: "request"}
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("user-agent", "swarmmemo-work-settle/1")
	resp, err := client.Do(req)
	if err != nil {
		return &ChainError{Code: "chain_unavailable", Reason: "unreachable"}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, usdcRPCBytes+1))
	if err != nil || len(raw) > usdcRPCBytes || resp.StatusCode != http.StatusOK {
		return &ChainError{Code: "chain_unavailable", Reason: "status " + strconv.Itoa(resp.StatusCode)}
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct{}       `json:"error"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Error != nil {
		return &ChainError{Code: "chain_unavailable", Reason: "rpc error"}
	}
	if len(env.Result) == 0 {
		env.Result = json.RawMessage("null")
	}
	if json.Unmarshal(env.Result, dest) != nil {
		return &ChainError{Code: "chain_unavailable", Reason: "malformed result"}
	}
	return nil
}

// httpClient is the RPC client: through dial (the board's SSRF-safe
// dialer), no proxy, no redirect, bounded in time.
func (c *USDCChain) httpClient(dial func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Client {
	if c.client != nil {
		return c.client
	}
	t := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 10 * time.Second,
		ResponseHeaderTimeout: usdcRPCTimeout, MaxResponseHeaderBytes: 64 << 10, DisableKeepAlives: true}
	if dial != nil {
		t.DialContext = dial
	}
	return &http.Client{Transport: t, Timeout: usdcRPCTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

var hexQuantityRE = regexp.MustCompile(`^0x[0-9a-fA-F]{1,15}$`)

func hexInt(s string) (int64, bool) {
	if !hexQuantityRE.MatchString(s) {
		return 0, false
	}
	n, err := strconv.ParseInt(s[2:], 16, 64)
	return n, err == nil
}

func hexBig(s string) (*big.Int, bool) {
	if len(s) < 3 || len(s) > 66 || s[:2] != "0x" {
		return nil, false
	}
	n, ok := new(big.Int).SetString(s[2:], 16)
	return n, ok && n.Sign() >= 0
}

func topicAddress(topic string) (EVMAddress, bool) {
	t := strings.ToLower(topic)
	if len(t) != 66 || t[:2] != "0x" || strings.Trim(t[2:26], "0") != "" {
		return EVMAddress{}, false
	}
	return ParseEVMAddress("0x" + t[26:])
}

// ParseUSDC reads a decimal USDC amount ("0.10") into micro-USDC.
func ParseUSDC(s string) (int64, bool) { return parseUnits(s, 6) }
