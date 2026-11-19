// Package server is the resource-server (merchant) side of the Canton "exact"
// scheme for the official x402 Go SDK. It is a port of the TypeScript
// ExactCantonScheme (typescript/packages/mechanisms/canton/src/exact/server).
package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	x402 "github.com/x402-foundation/x402/go/v2"
	"github.com/x402-foundation/x402/go/v2/types"

	canton "github.com/FTP-Tech-LLC/x402-canton-merchant/go/mechanisms/canton"
)

// ExactCantonScheme implements x402.SchemeNetworkServer for Canton networks.
type ExactCantonScheme struct{}

// NewExactCantonScheme returns the server scheme.
func NewExactCantonScheme() *ExactCantonScheme {
	return &ExactCantonScheme{}
}

// Scheme returns "exact".
func (s *ExactCantonScheme) Scheme() string {
	return canton.SchemeExact
}

// DefaultAssetTransferMethod returns "transfer-factory".
func (s *ExactCantonScheme) DefaultAssetTransferMethod() string {
	return canton.AssetTransferMethodTransferFactory
}

// PaymentFlows declares authorization (default) and upfront for transfer-factory.
// Settle verifies the payload in full, so it can run before the handler.
func (s *ExactCantonScheme) PaymentFlows() map[string]x402.PaymentFlowConfig {
	return map[string]x402.PaymentFlowConfig{
		canton.AssetTransferMethodTransferFactory: {
			Supported: []x402.PaymentFlowName{x402.PaymentFlowAuthorization, x402.PaymentFlowUpfront},
			Default:   x402.PaymentFlowAuthorization,
		},
	}
}

// DynamicExtraFields lists the extra keys the facilitator injects into the 402
// (see EnhancePaymentRequirements), so they are left out of the
// requirements-vs-accepted match.
func (s *ExactCantonScheme) DynamicExtraFields() []string {
	return []string{"feePayer", "synchronizerId"}
}

// GetAssetDecimals returns the decimals of a known default asset (Canton Coin
// is 10). ok is false for an unknown asset.
func (s *ExactCantonScheme) GetAssetDecimals(asset string, network x402.Network) (int, bool) {
	found := canton.FindDefaultAsset(asset, string(network))
	if found == nil {
		return 0, false
	}
	return found.Decimals, true
}

// ParsePrice accepts only an explicit AssetAmount ({amount: "<atomic>", asset}).
// A money string such as "$0.10" has no Canton Coin peg and is refused.
func (s *ExactCantonScheme) ParsePrice(price x402.Price, network x402.Network) (x402.AssetAmount, error) {
	switch p := price.(type) {
	case x402.AssetAmount:
		return assetAmountOf(p, network)
	case *x402.AssetAmount:
		if p != nil {
			return assetAmountOf(*p, network)
		}
	case map[string]interface{}:
		if _, hasAmount := p["amount"]; hasAmount {
			return assetAmountFromMap(p, network)
		}
	}
	return x402.AssetAmount{}, errors.New(
		"the Canton exact scheme requires an explicit AssetAmount price " +
			`({ amount: "<atomic units>", asset: "CC" | "<registry symbol>" }); ` +
			"dollar-string pricing has no Canton Coin peg")
}

func assetAmountOf(a x402.AssetAmount, network x402.Network) (x402.AssetAmount, error) {
	if a.Asset == "" {
		return x402.AssetAmount{}, fmt.Errorf("asset must be specified for an AssetAmount on network %s", network)
	}
	if a.Extra == nil {
		a.Extra = map[string]interface{}{}
	}
	return a, nil
}

func assetAmountFromMap(p map[string]interface{}, network x402.Network) (x402.AssetAmount, error) {
	amount, amountOK := p["amount"].(string)
	if !amountOK {
		return x402.AssetAmount{}, fmt.Errorf("amount must be a string of atomic units on network %s", network)
	}
	asset, _ := p["asset"].(string)
	extra, _ := p["extra"].(map[string]interface{})
	return assetAmountOf(x402.AssetAmount{Amount: amount, Asset: asset, Extra: extra}, network)
}

// EnhancePaymentRequirements merges the facilitator's feePayer and
// synchronizerId (from /supported) into the 402 extra and checks that a
// structured asset agrees with extra.instrumentId.
//
//   - assetTransferMethod is set first, so a merchant extra can still carry it.
//   - feePayer always comes from the supported kind when the kind has one.
//   - synchronizerId comes from the kind only when the merchant did not set it.
func (s *ExactCantonScheme) EnhancePaymentRequirements(
	_ context.Context,
	req types.PaymentRequirements,
	kind types.SupportedKind,
	_ []string,
) (types.PaymentRequirements, error) {
	extra := map[string]interface{}{
		"assetTransferMethod": canton.AssetTransferMethodTransferFactory,
	}
	for key, value := range req.Extra {
		extra[key] = value
	}
	if feePayer, ok := kind.Extra["feePayer"]; ok {
		extra["feePayer"] = feePayer
	}
	if _, merchantSet := extra["synchronizerId"]; !merchantSet {
		if synchronizerID, ok := kind.Extra["synchronizerId"]; ok {
			extra["synchronizerId"] = synchronizerID
		}
	}
	enhanced := req
	enhanced.Extra = extra
	if err := AssertAssetInstrumentConsistency(enhanced); err != nil {
		return types.PaymentRequirements{}, err
	}
	return enhanced, nil
}

// AssertAssetInstrumentConsistency checks that a structured asset of the form
// "<admin>::<id>" equals extra.instrumentId. The facilitator validates against
// instrumentId and ignores asset, so a silent disagreement would settle the
// wrong instrument. A symbolic asset such as "CC" is not cross-checked.
func AssertAssetInstrumentConsistency(req types.PaymentRequirements) error {
	instrument, ok := canton.InstrumentIDFromExtra(req.Extra)
	if !ok {
		return nil
	}
	if !strings.Contains(req.Asset, "::") {
		return nil
	}
	if expected := instrument.String(); req.Asset != expected {
		return fmt.Errorf(
			"payment requirements asset %q disagrees with extra.instrumentId (%q). "+
				"On Canton the instrumentId is authoritative: make them consistent",
			req.Asset, expected)
	}
	return nil
}
