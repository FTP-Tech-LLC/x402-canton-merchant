package canton

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsCantonNetwork(t *testing.T) {
	for _, network := range []string{CantonMainnetCAIP2, CantonTestnetCAIP2, CantonDevnetCAIP2} {
		assert.True(t, IsCantonNetwork(network), network)
	}
	for _, network := range []string{"canton:*", "canton:", "eip155:1", ""} {
		assert.False(t, IsCantonNetwork(network), network)
	}
}

func TestFindDefaultAsset(t *testing.T) {
	found := FindDefaultAsset("CC", CantonMainnetCAIP2)
	if assert.NotNil(t, found) {
		assert.Equal(t, CantonCoinDecimals, found.Decimals)
	}
	assert.NotNil(t, FindDefaultAsset("canton-coin", CantonDevnetCAIP2))
	assert.Nil(t, FindDefaultAsset("cc", CantonMainnetCAIP2))
	assert.Nil(t, FindDefaultAsset("CC", "eip155:1"))
}

func TestInstrumentIDFromExtraForms(t *testing.T) {
	want := InstrumentID{Admin: "a", ID: "b"}
	forms := []interface{}{want, &want, map[string]interface{}{"admin": "a", "id": "b"}, map[string]string{"admin": "a", "id": "b"}}
	for _, form := range forms {
		got, ok := InstrumentIDFromExtra(map[string]interface{}{"instrumentId": form})
		assert.True(t, ok)
		assert.Equal(t, want, got)
	}
	_, ok := InstrumentIDFromExtra(map[string]interface{}{})
	assert.False(t, ok)
	_, ok = InstrumentIDFromExtra(map[string]interface{}{"instrumentId": map[string]interface{}{"admin": 1}})
	assert.False(t, ok)
	assert.Equal(t, "a::b", want.String())
}
