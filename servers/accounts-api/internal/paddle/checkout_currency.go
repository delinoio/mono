// SPDX-License-Identifier: FSL-1.1-ALv2

package paddle

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// CurrencyUSD is the only currency accepted for subscription checkout.
const CurrencyUSD = "USD"

// ErrUnsafeCheckoutCurrency marks a verified price or conversion setting that
// allows a subscription checkout to use another currency.
var ErrUnsafeCheckoutCurrency = errors.New("paddle checkout is not configured for USD only")

// Paddle says this endpoint returns every supported currency, including
// those that were never configured. Require each documented entry so a
// partial response cannot hide an enabled conversion. Also check new entries.
// https://developer.paddle.com/api-reference/settings/get-currency-conversions/
const documentedConversionCurrencies = "ARS AUD BRL CAD CHF CLP CNY COP CZK DKK EUR GBP HKD HUF ILS INR JPY KRW MXN NOK NZD PEN PLN RUB SEK SGD THB TRY TWD UAH USD VND ZAR"

type checkoutUnitPrice struct {
	CurrencyCode string `json:"currency_code"`
}

// VerifyUSDCheckoutCurrency checks the configured catalog price and the
// account's automatic conversion settings in the client's environment.
func (c *Client) VerifyUSDCheckoutCurrency(ctx context.Context, priceID string) error {
	var price struct {
		ID                 string            `json:"id"`
		UnitPrice          checkoutUnitPrice `json:"unit_price"`
		UnitPriceOverrides []struct {
			UnitPrice checkoutUnitPrice `json:"unit_price"`
		} `json:"unit_price_overrides"`
	}
	if err := c.do(ctx, http.MethodGet, "/prices/"+url.PathEscape(priceID), nil, &price); err != nil {
		return fmt.Errorf("read subscription price: %w", err)
	}
	if price.ID == "" || price.ID != priceID || !validCurrencyCode(price.UnitPrice.CurrencyCode) || price.UnitPriceOverrides == nil {
		return errors.New("paddle subscription price currency data is incomplete or invalid")
	}
	if price.UnitPrice.CurrencyCode != CurrencyUSD {
		return fmt.Errorf("%w: subscription base price must use USD", ErrUnsafeCheckoutCurrency)
	}
	for _, override := range price.UnitPriceOverrides {
		if !validCurrencyCode(override.UnitPrice.CurrencyCode) {
			return errors.New("paddle subscription price override currency is missing or invalid")
		}
		if override.UnitPrice.CurrencyCode != CurrencyUSD {
			return fmt.Errorf("%w: subscription country price overrides must use USD", ErrUnsafeCheckoutCurrency)
		}
	}

	var conversions map[string]struct {
		Enabled *bool `json:"enabled"`
	}
	if err := c.do(ctx, http.MethodGet, "/settings/currency-conversions", nil, &conversions); err != nil {
		return fmt.Errorf("read checkout currency conversions: %w", err)
	}
	for code := range strings.FieldsSeq(documentedConversionCurrencies) {
		if conversions[code].Enabled == nil {
			return errors.New("paddle checkout currency conversion settings are incomplete")
		}
	}
	nonUSDConversionEnabled := false
	for code, conversion := range conversions {
		if !validCurrencyCode(code) || conversion.Enabled == nil {
			return errors.New("paddle checkout currency conversion settings are invalid")
		}
		nonUSDConversionEnabled = nonUSDConversionEnabled || (code != CurrencyUSD && *conversion.Enabled)
	}
	if nonUSDConversionEnabled {
		return fmt.Errorf("%w: disable automatic conversion for all non-USD currencies", ErrUnsafeCheckoutCurrency)
	}
	return nil
}

func validCurrencyCode(code string) bool {
	return len(code) == 3 && strings.IndexFunc(code, func(letter rune) bool {
		return letter < 'A' || letter > 'Z'
	}) == -1
}
