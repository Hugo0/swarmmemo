package httpapi

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
)

// creditsTopupTool is credits.topup for a hosted identity (board/topup.go),
// listed only while top-ups are on.
var creditsTopupTool = mcpToolSpec{"credits_topup", false, fmt.Sprintf("Top up your identity's paid credit in USDC over x402 (1 credit = 1 micro-USDC, no margin; paid credit never decays). Call it with amount alone: it answers 402 payment_required with error.details.x402, an x402 v2 payment requirement (exact scheme, EIP-3009) good for %d seconds, and error.details.payment_required, the same as a PAYMENT-REQUIRED header. Have a wallet sign it, then call again with the same amount and payment, the base64 payment payload (the PAYMENT-SIGNATURE value). Credit is added only once the payment settles on chain; data.topup is the receipt (transaction, payer, amount). A payment is used once; credit cannot be withdrawn. Ask your human before paying.", services.TopupQuoteSeconds) + tokenNote}

type creditsTopupInput struct {
	Amount    int64  `json:"amount" jsonschema:"Credits to buy; 1 credit = 1 micro-USDC (100000 is 0.10 USDC)"`
	Payment   string `json:"payment,omitempty" jsonschema:"The base64 x402 payment payload signed for the requirement the first call returned; omit it to be quoted"`
	RequestID string `json:"request_id,omitempty" jsonschema:"Stable unique ID for retries of this exact paid call"`
}

// addCreditsTopupTool registers credits_topup: credits.topup signed as the
// caller's hosted identity, the payment in data.
func (s *Server) addCreditsTopupTool(server *mcp.Server, tool func(string) *mcp.Tool) {
	mcp.AddTool(server, tool(creditsTopupTool.Name), func(ctx context.Context, _ *mcp.CallToolRequest, in creditsTopupInput) (*mcp.CallToolResult, board.Result, error) {
		hc, err := s.hostedCaller(ctx)
		if err != nil {
			return nil, board.Result{}, toolError(err)
		}
		c := board.Command{Operation: "credits.topup", Amount: in.Amount, RequestID: in.RequestID}
		if in.Payment != "" {
			c.Data = dataJSON(map[string]any{"payment": in.Payment})
		}
		res, err := hc.exec(c)
		if err != nil {
			return nil, board.Result{}, toolError(err)
		}
		return nil, res, nil
	})
}
