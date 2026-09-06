package spend

import (
	"fmt"
	"strconv"
	"strings"
)

// The units a price may be quoted in. A model that reports its tokens is priced
// per million of them; anything else can only be priced per billed call.
const (
	UnitInputTokens  = "input_mtok"
	UnitOutputTokens = "output_mtok"
	UnitCall         = "call"
)

// AnyModel is the model wildcard in a price key: one price for every model a
// provider serves.
const AnyModel = "*"

// Prices maps provider:model:unit to the price of one unit. There are no
// defaults: a price this service was not told is a price it does not know, and
// guessing one would put a number nobody approved into a spending decision.
type Prices map[string]Micros

// ParsePrices reads the COST_GATE_PRICES form:
//
//	gemini:gemini-3.5-flash-lite:input_mtok=0.075,apify:instagram:call=0.004
//
// Amounts are US dollars. The model may be * to cover every model a provider
// serves.
func ParsePrices(raw string) (Prices, error) {
	prices := Prices{}
	for _, field := range strings.Split(raw, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		key, amount, found := strings.Cut(field, "=")
		if !found {
			return nil, fmt.Errorf("price %q is not provider:model:unit=amount", field)
		}
		provider, model, unit, err := splitPriceKey(strings.TrimSpace(key))
		if err != nil {
			return nil, err
		}
		value, err := ParseUSD(strings.TrimSpace(amount))
		if err != nil {
			return nil, fmt.Errorf("price %q: %w", field, err)
		}
		full := priceKey(provider, model, unit)
		if _, duplicate := prices[full]; duplicate {
			return nil, fmt.Errorf("price %s is set twice", full)
		}
		prices[full] = value
	}
	if len(prices) == 0 {
		return nil, fmt.Errorf("no prices are configured")
	}
	return prices, nil
}

func splitPriceKey(key string) (provider, model, unit string, err error) {
	parts := strings.Split(key, ":")
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("price key %q is not provider:model:unit", key)
	}
	provider, model, unit = parts[0], parts[1], parts[2]
	if provider == "" || model == "" {
		return "", "", "", fmt.Errorf("price key %q has an empty provider or model", key)
	}
	switch unit {
	case UnitInputTokens, UnitOutputTokens, UnitCall:
	default:
		return "", "", "", fmt.Errorf("price key %q: unit must be one of %s, %s, %s",
			key, UnitInputTokens, UnitOutputTokens, UnitCall)
	}
	return provider, model, unit, nil
}

func priceKey(provider, model, unit string) string {
	return provider + ":" + model + ":" + unit
}

// ParsePricesOrNone treats an empty list as no prices rather than an error.
// Calls are still counted and stored under it, valued at zero and reported as
// unpriced, which is how a month's real usage gets on record before anyone has
// approved an amount.
func ParsePricesOrNone(raw string) (Prices, error) {
	if strings.TrimSpace(raw) == "" {
		return Prices{}, nil
	}
	return ParsePrices(raw)
}

// Cost prices one call. It reports false when nothing configured covers it: an
// unpriced call is counted and surfaced, never silently valued at zero.
func (p Prices) Cost(usage Usage) (Micros, bool) {
	if usage.Measured {
		input, hasInput := p.lookup(usage.Provider, usage.Model, UnitInputTokens)
		output, hasOutput := p.lookup(usage.Provider, usage.Model, UnitOutputTokens)
		if hasInput || hasOutput {
			return perMillion(input, usage.InputTokens) + perMillion(output, usage.OutputTokens), true
		}
	}
	if perCall, ok := p.lookup(usage.Provider, usage.Model, UnitCall); ok {
		return perCall * Micros(usage.Calls), true
	}
	return 0, false
}

// lookup prefers the exact model, then the provider's wildcard.
func (p Prices) lookup(provider, model, unit string) (Micros, bool) {
	if price, ok := p[priceKey(provider, model, unit)]; ok {
		return price, true
	}
	price, ok := p[priceKey(provider, AnyModel, unit)]
	return price, ok
}

// perMillion prices tokens quoted per million, rounded to the nearest micro.
func perMillion(price Micros, tokens int64) Micros {
	if price == 0 || tokens == 0 {
		return 0
	}
	return Micros((int64(price)*tokens + 500_000) / 1_000_000)
}

// ParseUSD reads a plain decimal dollar amount into whole micros. It is not
// strconv.ParseFloat: 0.075 has no exact binary form, and a spending limit that
// lands a micro either side of what was approved is a limit nobody approved.
func ParseUSD(raw string) (Micros, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return 0, fmt.Errorf("an amount is required")
	}
	if strings.HasPrefix(text, "-") {
		return 0, fmt.Errorf("%q is negative", raw)
	}
	text = strings.TrimPrefix(text, "$")

	whole, fraction, _ := strings.Cut(text, ".")
	if whole == "" {
		whole = "0"
	}
	if len(fraction) > 6 {
		return 0, fmt.Errorf("%q is finer than one micro-dollar", raw)
	}
	fraction += strings.Repeat("0", 6-len(fraction))

	dollars, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a dollar amount", raw)
	}
	micros, err := strconv.ParseInt(fraction, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a dollar amount", raw)
	}
	if dollars > (1<<62)/1_000_000 {
		return 0, fmt.Errorf("%q is too large", raw)
	}
	return Micros(dollars*1_000_000 + micros), nil
}
