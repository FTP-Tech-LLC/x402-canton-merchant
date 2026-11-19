package canton

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	cantonmech "github.com/FTP-Tech-LLC/x402-canton-merchant/go/mechanisms/canton"
)

const (
	// DSOMainnetParty is the Canton Coin registrar (DSO) party on MainNet.
	DSOMainnetParty = "DSO::1220b1431ef217342db44d516bb9befde802be7d8899637d290895fa58880f19accc"
	// CantonCoinInstrumentID is the Canton Coin instrument id.
	CantonCoinInstrumentID = "Amulet"

	// maxAtomicDigits bounds the atomic amount. A Daml Numeric(38, 10) holds at
	// most 38 digits in total, so 38 atomic digits is the largest valid amount.
	maxAtomicDigits = 38
	// wireDecimals is the scale of every Canton instrument on the wire.
	wireDecimals = cantonmech.CantonCoinDecimals
)

var (
	atomicAmountPattern  = regexp.MustCompile(`^[1-9][0-9]*$`)
	decimalAmountPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]{1,10})?$`)
)

// InstrumentID names a Canton instrument (admin party and id). It is the type of
// the mirror package, available here so that an import of one package suffices.
type InstrumentID = cantonmech.InstrumentID

// Price is what one call of a paid route costs: an amount in atomic units and
// the instrument it is paid in. Build it with CC, Token or Atomic. A Price that
// failed to parse is reported by NewMiddleware, never by a panic.
type Price struct {
	atomic     string
	instrument cantonmech.InstrumentID
	err        error
}

// CC prices a route in Canton Coin, for example CC("0.01"). The decimal has at
// most 10 fractional digits and must be positive.
func CC(decimal string) Price {
	return Token(DSOMainnetParty, CantonCoinInstrumentID, decimal)
}

// Token prices a route in a registry token, for example
// Token(admin, "USDCx", "1.5"). The admin must be a party the facilitator
// supports; NewMiddleware checks it against /supported.
func Token(admin, id, decimal string) Price {
	atomic, err := decimalToAtomic(decimal)
	return Price{atomic: atomic, instrument: cantonmech.InstrumentID{Admin: admin, ID: id}, err: err}
}

// Atomic prices a route with an exact atomic amount (1 unit = 1e-10 of the
// instrument), matching ^[1-9][0-9]*$.
func Atomic(amount string, instrument cantonmech.InstrumentID) Price {
	var err error
	if !atomicAmountPattern.MatchString(amount) || len(amount) > maxAtomicDigits {
		err = fmt.Errorf("atomic amount %q must match ^[1-9][0-9]*$ and have at most %d digits", amount, maxAtomicDigits)
	}
	return Price{atomic: amount, instrument: instrument, err: err}
}

func (p Price) validate() error {
	if p.err != nil {
		return p.err
	}
	if p.atomic == "" {
		return errors.New("price is not set; use CC, Token or Atomic")
	}
	if p.instrument.Admin == "" || p.instrument.ID == "" {
		return errors.New("price instrument needs admin and id")
	}
	return nil
}

func (p Price) isCantonCoin() bool {
	return p.instrument == cantonmech.InstrumentID{Admin: DSOMainnetParty, ID: CantonCoinInstrumentID}
}

// asset is the 402 asset field: "CC" for Canton Coin, "<admin>::<id>" otherwise.
func (p Price) asset() string {
	if p.isCantonCoin() {
		return cantonmech.CantonCoinSymbol
	}
	return p.instrument.String()
}

func decimalToAtomic(decimal string) (string, error) {
	if !decimalAmountPattern.MatchString(decimal) {
		return "", fmt.Errorf("price %q must be a positive decimal with at most %d fractional digits", decimal, wireDecimals)
	}
	whole, fraction, _ := strings.Cut(decimal, ".")
	fraction += strings.Repeat("0", wireDecimals-len(fraction))
	atomic := strings.TrimLeft(whole+fraction, "0")
	if atomic == "" {
		return "", fmt.Errorf("price %q must be greater than zero", decimal)
	}
	if len(atomic) > maxAtomicDigits {
		return "", fmt.Errorf("price %q is too large: at most %d atomic digits", decimal, maxAtomicDigits)
	}
	return atomic, nil
}
