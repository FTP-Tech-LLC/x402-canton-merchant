package server

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	x402 "github.com/x402-foundation/x402/go/v2"
	"github.com/x402-foundation/x402/go/v2/types"

	canton "github.com/FTP-Tech-LLC/x402-canton-merchant/go/mechanisms/canton"
)

const (
	network = x402.Network("canton:mainnet")
	dso     = "DSO::1220" + "aa" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	feePay  = "facilitator::1220" + "ff"
)

func baseReqs() types.PaymentRequirements {
	return types.PaymentRequirements{
		Scheme:            "exact",
		Network:           string(network),
		Amount:            "100000000",
		Asset:             "CC",
		PayTo:             "merchant::1220bb",
		MaxTimeoutSeconds: 60,
		Extra:             map[string]interface{}{"instrumentId": map[string]interface{}{"admin": dso, "id": "Amulet"}},
	}
}

func supportedKind() types.SupportedKind {
	return types.SupportedKind{
		X402Version: 2, Scheme: "exact", Network: string(network),
		Extra: map[string]interface{}{"feePayer": feePay, "synchronizerId": "global-domain::1220test"},
	}
}

func TestSchemeIdentity(t *testing.T) {
	s := NewExactCantonScheme()
	assert.Equal(t, "exact", s.Scheme())
	assert.Equal(t, "transfer-factory", s.DefaultAssetTransferMethod())
	assert.Equal(t, []string{"feePayer", "synchronizerId"}, s.DynamicExtraFields())
	var _ x402.SchemeNetworkServer = s
	var _ x402.AssetDecimalsProvider = s
	var _ x402.DynamicExtraFieldsProvider = s
}

func TestPaymentFlowsDeclareAuthorizationAndUpfront(t *testing.T) {
	flows := NewExactCantonScheme().PaymentFlows()
	cfg, ok := flows["transfer-factory"]
	require.True(t, ok)
	assert.Equal(t, x402.PaymentFlowAuthorization, cfg.Default)
	assert.Equal(t, []x402.PaymentFlowName{x402.PaymentFlowAuthorization, x402.PaymentFlowUpfront}, cfg.Supported)
}

func TestGetAssetDecimals(t *testing.T) {
	s := NewExactCantonScheme()
	decimals, ok := s.GetAssetDecimals("CC", network)
	assert.True(t, ok)
	assert.Equal(t, 10, decimals)
	_, ok = s.GetAssetDecimals("SOMETHING", network)
	assert.False(t, ok)
	_, ok = s.GetAssetDecimals("CC", "eip155:1")
	assert.False(t, ok)
}

func TestParsePriceAcceptsExplicitAssetAmount(t *testing.T) {
	s := NewExactCantonScheme()
	got, err := s.ParsePrice(x402.AssetAmount{Amount: "100000000", Asset: "CC"}, network)
	require.NoError(t, err)
	assert.Equal(t, x402.AssetAmount{Amount: "100000000", Asset: "CC", Extra: map[string]interface{}{}}, got)

	got, err = s.ParsePrice(map[string]interface{}{"amount": "5", "asset": "CC"}, network)
	require.NoError(t, err)
	assert.Equal(t, "5", got.Amount)
}

func TestParsePriceRejectsMoneyStrings(t *testing.T) {
	s := NewExactCantonScheme()
	for _, price := range []x402.Price{"$0.10", 0.1, 10, nil} {
		_, err := s.ParsePrice(price, network)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "explicit AssetAmount")
	}
}

func TestParsePriceRejectsMissingAssetAndBadAmountType(t *testing.T) {
	s := NewExactCantonScheme()
	_, err := s.ParsePrice(x402.AssetAmount{Amount: "1"}, network)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "asset must be specified")

	_, err = s.ParsePrice(map[string]interface{}{"amount": 5, "asset": "CC"}, network)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "amount must be a string")
}

func TestEnhanceMergesFeePayerSynchronizerAndMethod(t *testing.T) {
	out, err := NewExactCantonScheme().EnhancePaymentRequirements(context.Background(), baseReqs(), supportedKind(), nil)
	require.NoError(t, err)
	assert.Equal(t, feePay, out.Extra["feePayer"])
	assert.Equal(t, "global-domain::1220test", out.Extra["synchronizerId"])
	assert.Equal(t, "transfer-factory", out.Extra["assetTransferMethod"])
	assert.Contains(t, out.Extra, "instrumentId")
}

func TestEnhanceFeePayerAlwaysFromSupportedKind(t *testing.T) {
	req := baseReqs()
	req.Extra["feePayer"] = "attacker::1220"
	out, err := NewExactCantonScheme().EnhancePaymentRequirements(context.Background(), req, supportedKind(), nil)
	require.NoError(t, err)
	assert.Equal(t, feePay, out.Extra["feePayer"])
}

func TestEnhanceKeepsMerchantSynchronizerId(t *testing.T) {
	req := baseReqs()
	req.Extra["synchronizerId"] = "pinned::1220"
	out, err := NewExactCantonScheme().EnhancePaymentRequirements(context.Background(), req, supportedKind(), nil)
	require.NoError(t, err)
	assert.Equal(t, "pinned::1220", out.Extra["synchronizerId"])
}

func TestEnhanceAssetTransferMethodIsSetFirstSoMerchantExtraWins(t *testing.T) {
	req := baseReqs()
	req.Extra["assetTransferMethod"] = "something-else"
	out, err := NewExactCantonScheme().EnhancePaymentRequirements(context.Background(), req, supportedKind(), nil)
	require.NoError(t, err)
	assert.Equal(t, "something-else", out.Extra["assetTransferMethod"])
}

func TestEnhanceDoesNotMutateInput(t *testing.T) {
	req := baseReqs()
	_, err := NewExactCantonScheme().EnhancePaymentRequirements(context.Background(), req, supportedKind(), nil)
	require.NoError(t, err)
	assert.NotContains(t, req.Extra, "feePayer")
}

func TestEnhanceWithoutKindExtraLeavesFieldsAbsent(t *testing.T) {
	kind := supportedKind()
	kind.Extra = nil
	out, err := NewExactCantonScheme().EnhancePaymentRequirements(context.Background(), baseReqs(), kind, nil)
	require.NoError(t, err)
	assert.NotContains(t, out.Extra, "feePayer")
	assert.NotContains(t, out.Extra, "synchronizerId")
}

func TestEnhanceRejectsAssetThatDisagreesWithInstrument(t *testing.T) {
	req := baseReqs()
	req.Asset = "wrong::Amulet"
	_, err := NewExactCantonScheme().EnhancePaymentRequirements(context.Background(), req, supportedKind(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "disagrees")
}

func TestAssertConsistencyNoOpForSymbolicAsset(t *testing.T) {
	require.NoError(t, AssertAssetInstrumentConsistency(baseReqs()))
}

func TestAssertConsistencyNoOpWithoutInstrument(t *testing.T) {
	req := baseReqs()
	req.Asset = "wrong::Amulet"
	req.Extra = map[string]interface{}{}
	require.NoError(t, AssertAssetInstrumentConsistency(req))
}

func TestAssertConsistencyPassesWhenStructuredAssetMatches(t *testing.T) {
	req := baseReqs()
	req.Asset = dso + "::Amulet"
	require.NoError(t, AssertAssetInstrumentConsistency(req))
}

func TestAssertConsistencyAcceptsTypedInstrument(t *testing.T) {
	req := baseReqs()
	req.Extra = map[string]interface{}{"instrumentId": canton.InstrumentID{Admin: dso, ID: "Amulet"}}
	req.Asset = "wrong::Amulet"
	err := AssertAssetInstrumentConsistency(req)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "disagrees"))
}
