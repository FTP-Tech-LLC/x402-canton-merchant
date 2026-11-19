// Package canton holds the constants and helpers of the Canton "exact" x402
// scheme. It mirrors the TypeScript package @x402/canton so that a later move
// into the official SDK only changes the import path. Nothing in it is specific
// to one facilitator or merchant library.
package canton

import (
	"encoding/json"
	"fmt"
)

const (
	// SchemeExact is the x402 scheme name.
	SchemeExact = "exact"

	// CantonMainnetCAIP2 is the CAIP-2 style identifier of Canton MainNet.
	CantonMainnetCAIP2 = "canton:mainnet"
	// CantonTestnetCAIP2 is the identifier of Canton TestNet.
	CantonTestnetCAIP2 = "canton:testnet"
	// CantonDevnetCAIP2 is the identifier of Canton DevNet.
	CantonDevnetCAIP2 = "canton:devnet"

	// CaipFamily is the CAIP family pattern used to group signers in a
	// supported response. It is not a valid network for scheme registration.
	CaipFamily = "canton:*"

	// AssetTransferMethodTransferFactory is the only on-ledger settlement
	// method: a token-standard TransferFactory_Transfer carried inline.
	AssetTransferMethodTransferFactory = "transfer-factory"

	// CantonCoinSymbol is the symbol Canton Coin carries in `asset`.
	CantonCoinSymbol = "CC"
	// CantonCoinDecimals is the ledger precision of Canton Coin: 1 CC = 1e10.
	CantonCoinDecimals = 10
)

// Error codes of the exact scheme, as in the TypeScript types.ts. They appear
// in VerifyResponse.invalidReason and SettleResponse.errorReason.
const (
	ErrAmountMismatch        = "invalid_exact_canton_amount_mismatch"
	ErrExecuteBeforeTooFar   = "invalid_exact_canton_execute_before_too_far"
	ErrExecuteFailed         = "invalid_exact_canton_execute_failed"
	ErrExpired               = "invalid_exact_canton_expired"
	ErrFeePayerMismatch      = "invalid_exact_canton_fee_payer_mismatch"
	ErrInstrumentIDMismatch  = "invalid_exact_canton_instrument_id_mismatch"
	ErrInsufficientInputs    = "invalid_exact_canton_insufficient_inputs"
	ErrMalformedPayload      = "invalid_exact_canton_malformed_payload"
	ErrMemoMismatch          = "invalid_exact_canton_memo_mismatch"
	ErrMerchantMismatch      = "invalid_exact_canton_merchant_mismatch"
	ErrMerchantNotRegistered = "invalid_exact_canton_merchant_not_registered"
	ErrMissingProof          = "invalid_exact_canton_missing_proof"
	ErrPreapprovalMissing    = "invalid_exact_canton_preapproval_missing"
	ErrSelfPayment           = "invalid_exact_canton_self_payment"
	ErrSignatureInvalid      = "invalid_exact_canton_signature_invalid"
	ErrUnexpectedLedgerError = "unexpected_canton_ledger_error"
	ErrInvalidNetwork        = "invalid_network"
	ErrSettlementPending     = "settlement_pending"
	ErrUnsupportedScheme     = "unsupported_scheme"
)

const canonicalInstrumentIDFormat = "%s::%s"

// InstrumentID names a Canton instrument: the registrar (admin) party and the
// instrument id. Canton Coin is {DSO party, "Amulet"}.
type InstrumentID struct {
	Admin string `json:"admin"`
	ID    string `json:"id"`
}

// String renders the structured asset form "<admin>::<id>".
func (i InstrumentID) String() string {
	return fmt.Sprintf(canonicalInstrumentIDFormat, i.Admin, i.ID)
}

// InstrumentIDFromExtra reads extra["instrumentId"], accepting both the typed
// struct and the map form that appears after a JSON round trip. The second
// result is false when the key is absent or not an object with string fields.
func InstrumentIDFromExtra(extra map[string]interface{}) (InstrumentID, bool) {
	raw, present := extra["instrumentId"]
	if !present {
		return InstrumentID{}, false
	}
	switch v := raw.(type) {
	case InstrumentID:
		return v, true
	case *InstrumentID:
		if v == nil {
			return InstrumentID{}, false
		}
		return *v, true
	case map[string]interface{}:
		admin, adminOK := v["admin"].(string)
		id, idOK := v["id"].(string)
		return InstrumentID{Admin: admin, ID: id}, adminOK && idOK
	default:
		encoded, err := json.Marshal(raw)
		if err != nil {
			return InstrumentID{}, false
		}
		var out InstrumentID
		if json.Unmarshal(encoded, &out) != nil {
			return InstrumentID{}, false
		}
		return out, true
	}
}

// IsCantonNetwork reports whether the string is one of the three Canton
// network identifiers.
func IsCantonNetwork(network string) bool {
	switch network {
	case CantonMainnetCAIP2, CantonTestnetCAIP2, CantonDevnetCAIP2:
		return true
	default:
		return false
	}
}

// DefaultAsset is an asset the scheme knows without configuration.
type DefaultAsset struct {
	Asset    string
	Symbol   string
	Decimals int
}

// CantonCoinAsset is the Canton Coin default asset. Its identity is symbolic.
var CantonCoinAsset = DefaultAsset{Asset: CantonCoinSymbol, Symbol: CantonCoinSymbol, Decimals: CantonCoinDecimals}

// FindDefaultAsset returns the default asset for a symbol on a Canton network,
// or nil when the network is not Canton or the asset is not a known default.
// Canton Coin has no USD peg, so only "CC" and "canton-coin" are known.
func FindDefaultAsset(asset string, network string) *DefaultAsset {
	if !IsCantonNetwork(network) {
		return nil
	}
	if asset == CantonCoinSymbol || asset == "canton-coin" {
		found := CantonCoinAsset
		return &found
	}
	return nil
}
