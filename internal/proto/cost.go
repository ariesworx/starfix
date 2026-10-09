package proto

import (
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
)

// Prices and cost reports (protocol 4, design §12.1). Rates are integer
// micro-dollars (10⁻⁶ USD) per million tokens, so a cost is exact: the
// server sums rate × tokens in picodollars (10⁻¹² USD) and sends the sum
// as a decimal string of US dollars, which no JSON reader rounds through
// a float. Only display rounds, to the cent (Dollars).
const (
	OpPriceSet = "price.set" // PriceSetArgs → PriceSetResult; admins only
	OpPrices   = "prices"    // PricesArgs → PricesResult
	OpCost     = "cost"      // CostArgs → CostResult
)

// Cost report group keys that no account, issue, principal or model can
// be, since none of those holds parentheses: the tokens no issue was held
// for, the issues under no epic, and the groups past a report's limit,
// summed (OtherModels, "(other)").
const (
	CostUnattributed = "(unattributed)"
	CostNoEpic       = "(no epic)"
)

// MaxRate bounds a rate: a million US dollars per million tokens.
const MaxRate = 1_000_000_000_000

// Rates are a model's list rates in micro-dollars per million tokens.
// CacheWrite is a five-minute cache write and CacheWrite1h a one-hour
// one.
type Rates struct {
	Input        int64 `json:"input"`
	Output       int64 `json:"output"`
	CacheWrite   int64 `json:"cache_write"`
	CacheWrite1h int64 `json:"cache_write_1h"`
	CacheRead    int64 `json:"cache_read"`
}

// PriceSetArgs sets Model's rates from From on: a date (2006-01-02,
// midnight UTC) or an RFC 3339 time. Model is spelled as the harness
// reports it.
type PriceSetArgs struct {
	Model string `json:"model"`
	From  string `json:"from"`
	Rates
}

// PriceSetResult says whether the price was added, replaced (same model
// and from) or already so (unchanged).
type PriceSetResult struct {
	Change string `json:"change"`
}

// PricesArgs lists every price; it takes nothing.
type PricesArgs struct{}

// Price is a model's rates from From on, and who last set them.
type Price struct {
	Model string    `json:"model"`
	From  time.Time `json:"from"`
	Rates
	SetBy string    `json:"set_by"`
	SetAt time.Time `json:"set_at"`
}

// PricesResult lists prices by model, then from.
type PricesResult struct {
	Prices []Price `json:"prices"`
}

// CostArgs selects a cost report: the records whose time is in [Since,
// Until), grouped By account, issue, epic, person or model. Since is as
// DigestArgs' (a time, a date or a duration back from now); Until is a
// time or a date, empty for now. Limit groups are listed (0 takes the
// server's default, 50; at most 500), and the rest summed into one
// OtherModels group.
type CostArgs struct {
	By    string `json:"by"`
	Since string `json:"since"`
	Until string `json:"until,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

// CostGroup is one group of a cost report: its tokens summed over models
// and their list-price equivalent, CostUSD, an exact decimal of the
// priced tokens. Title is an issue's or epic's. Unpriced: some tokens
// had no price at their time. Split: some came from records shared by
// time with another group, so that part is an estimate.
type CostGroup struct {
	Key   string `json:"key"`
	Title string `json:"title,omitempty"`
	Tokens
	CostUSD  string `json:"cost_usd"`
	Unpriced bool   `json:"unpriced,omitempty"`
	Split    bool   `json:"split,omitempty"`
}

// CostResult is a cost report. Groups are by cost, largest first; Total
// covers every record read. Unpriced names the models with tokens and no
// price at their time, at most MaxUsageModels, and UnpricedMore counts
// the rest. Truncated: the window had more records than the server
// reads, so every figure is a lower bound.
type CostResult struct {
	By           string      `json:"by"`
	Since        time.Time   `json:"since"`
	Until        time.Time   `json:"until"`
	Groups       []CostGroup `json:"groups"`
	Total        CostGroup   `json:"total"`
	Unpriced     []string    `json:"unpriced,omitempty"`
	UnpricedMore int         `json:"unpriced_more,omitempty"`
	Truncated    bool        `json:"truncated,omitempty"`
}

// picoPerUSD is picodollars in a dollar.
var picoPerUSD = big.NewInt(1_000_000_000_000)

// USD is picodollars as an exact decimal amount of US dollars, without
// trailing zeros: "12.5", "0.000003", "0". Nil is "0".
func USD(pico *big.Int) string {
	if pico == nil {
		return "0"
	}
	s := new(big.Rat).SetFrac(pico, picoPerUSD).FloatString(12)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

// amountPattern is an amount of money that is not negative, as USD
// writes one.
var amountPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

// ParseUSD reads an amount USD wrote back into picodollars. It refuses
// text that is not an amount, a negative one, and one finer than a
// picodollar.
func ParseUSD(usd string) (*big.Int, error) {
	r, ok := new(big.Rat).SetString(usd)
	if !amountPattern.MatchString(usd) || !ok {
		return nil, fmt.Errorf("%q is not an amount of US dollars", usd)
	}
	if r.Mul(r, new(big.Rat).SetInt(picoPerUSD)); !r.IsInt() {
		return nil, fmt.Errorf("%q is finer than a picodollar", usd)
	}
	return r.Num(), nil
}

// Dollars renders an exact amount of US dollars for people, rounded to
// the cent with halves up and thousands grouped: "$1,234.57". An amount
// above zero that rounds to nothing is "<$0.01", so a small cost never
// reads as free. A cost is never negative, so text that is not an
// amount, a negative one included, is returned as it is: a fault shows
// plainly rather than rounded into something that looks like a cost.
func Dollars(usd string) string {
	if !amountPattern.MatchString(usd) {
		return usd
	}
	r, _ := new(big.Rat).SetString(usd) // a decimal, by the pattern
	cents := r.FloatString(2)
	if cents == "0.00" && r.Sign() > 0 {
		return "<$0.01"
	}
	whole, frac, _ := strings.Cut(cents, ".")
	var b strings.Builder
	for i, c := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return "$" + b.String() + "." + frac
}

// ratePattern is a rate in US dollars per million tokens: digits, and
// at most six decimals.
var ratePattern = regexp.MustCompile(`^[0-9]{1,7}(\.[0-9]{1,6})?$`)

// ParseRate reads a rate in US dollars per million tokens, such as 3 or
// 3.75, into micro-dollars per million tokens. It refuses a negative
// rate, one finer than a micro-dollar, one past MaxRate, and anything
// but plain decimal digits.
func ParseRate(s string) (int64, error) {
	bad := errors.New("not a rate in US dollars per million tokens such as 3 or 0.375, with at most 6 decimals, up to 1000000")
	if !ratePattern.MatchString(s) {
		return 0, bad
	}
	whole, frac, _ := strings.Cut(s, ".")
	n := int64(0)
	for _, c := range whole + (frac + "000000")[:6] {
		n = n*10 + int64(c-'0')
	}
	if n > MaxRate {
		return 0, bad
	}
	return n, nil
}

// FormatRate renders micro-dollars per million tokens as US dollars per
// million tokens, exactly and without trailing zeros: "3.75".
func FormatRate(micros int64) string {
	return USD(new(big.Int).Mul(big.NewInt(micros), big.NewInt(1_000_000)))
}
