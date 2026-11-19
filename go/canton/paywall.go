package canton

import (
	"fmt"
	"html"
	"strings"

	x402http "github.com/x402-foundation/x402/go/v2/http"
	"github.com/x402-foundation/x402/go/v2/types"
)

// cantonPaywall is the page a browser gets for an unpaid request. Without it the
// SDK serves its EVM/USDC paywall: 2.5 MB, with the amount shown 10,000 times
// too high. This page is a few hundred bytes with the right Canton amount. The
// offer itself is in the PAYMENT-REQUIRED header, as for every other client.
type cantonPaywall struct{}

var _ x402http.PaywallProvider = cantonPaywall{}

func (cantonPaywall) GenerateHTML(required types.PaymentRequired, _ *x402http.PaywallConfig) string {
	amount := "the listed amount"
	if len(required.Accepts) > 0 {
		accepted := required.Accepts[0]
		amount = html.EscapeString(displayAmount(accepted.Amount) + " " + displayAsset(accepted.Asset))
	}
	return "<!doctype html><meta charset=utf-8><title>Payment required</title>" +
		"<h1>Payment required: " + amount + "</h1>" +
		"<p>This endpoint is paid with the x402 protocol. Use an x402 client that supports Canton.</p>"
}

func displayAsset(asset string) string {
	// A token asset is "<admin>::<id>" and the admin party itself contains "::":
	// the token is what follows the last separator.
	if i := strings.LastIndex(asset, "::"); i >= 0 {
		return asset[i+2:]
	}
	return asset
}

// displayAmount renders an atomic amount (1 unit = 1e-10) as a decimal.
func displayAmount(atomic string) string {
	if atomic == "" || strings.Trim(atomic, "0123456789") != "" {
		return atomic
	}
	atomic = strings.Repeat("0", max(0, wireDecimals+1-len(atomic))) + atomic
	whole, fraction := atomic[:len(atomic)-wireDecimals], strings.TrimRight(atomic[len(atomic)-wireDecimals:], "0")
	if fraction == "" {
		return whole
	}
	return fmt.Sprintf("%s.%s", whole, fraction)
}
