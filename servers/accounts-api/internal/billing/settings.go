// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/servers/accounts-api/internal/db"
)

// Settings are the automatic top-up and low-balance alert settings of a
// billing account.
type Settings struct {
	AutoEnabled         bool
	AutoThreshold       int64
	AutoAmount          int64
	AutoCap             int64
	ConsecutiveFailures int32
	LowBalanceThreshold int64
	LowBalanceAlerted   bool
}

// defaultSettings are the settings of a billing account without a row.
var defaultSettings = Settings{LowBalanceThreshold: DefaultLowBalanceThresholdUSDMicros}

// loadSettings reads the settings, or the defaults.
func loadSettings(ctx context.Context, q db.Querier, accountID string) (Settings, error) {
	var st Settings
	err := q.QueryRow(ctx, `SELECT auto_top_up_enabled, auto_top_up_threshold_usd_micros, auto_top_up_amount_usd_micros,
			auto_top_up_monthly_cap_usd_micros, auto_top_up_consecutive_failures, low_balance_threshold_usd_micros,
			low_balance_alerted
		FROM credit_settings WHERE billing_account_id = $1`, accountID).Scan(&st.AutoEnabled, &st.AutoThreshold,
		&st.AutoAmount, &st.AutoCap, &st.ConsecutiveFailures, &st.LowBalanceThreshold, &st.LowBalanceAlerted)
	if errors.Is(err, pgx.ErrNoRows) {
		return defaultSettings, nil
	}
	return st, err
}

// LoadSettings reads the settings of a billing account for the API.
func LoadSettings(ctx context.Context, q db.Querier, accountID string) (Settings, error) {
	return loadSettings(ctx, q, accountID)
}

// ensureSettings creates the settings row with the defaults when it does not
// exist.
func ensureSettings(ctx context.Context, tx pgx.Tx, accountID string, now time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO credit_settings (billing_account_id, low_balance_threshold_usd_micros, update_time)
		VALUES ($1, $2, $3) ON CONFLICT (billing_account_id) DO NOTHING`,
		accountID, DefaultLowBalanceThresholdUSDMicros, now)
	return err
}

// AutoTopUpFailureLimit is the number of failed automatic top-ups in a row
// that turns automatic top-up off.
const AutoTopUpFailureLimit = 2

// nextFailureState returns the failure count after one more failed
// automatic top-up and whether the setting turns off.
func nextFailureState(consecutive int32, enabled bool) (int32, bool) {
	n := consecutive + 1
	return n, enabled && n >= AutoTopUpFailureLimit
}

// capAllows reports whether an automatic top-up of amount fits under the
// monthly cap after monthToDate.
func capAllows(monthToDate, amount, monthlyCap int64) bool {
	return amount <= monthlyCap-monthToDate
}

// MonthToDate returns the sum of the automatic top-ups of the UTC calendar
// month of now that were not failed: completed ones and those in flight. A
// top-up belongs to the month of its cap time, which is the queue time of a
// pending top-up and the claim time of a claimed one; a row without cap time
// belongs to the month of its create_time.
func MonthToDate(ctx context.Context, q db.Querier, accountID string, now time.Time) (int64, error) {
	return monthToDateExcept(ctx, q, accountID, now, "")
}

// monthToDateExcept is MonthToDate without the top-up exceptID, so that a
// pending top-up can be checked against the monthly cap without counting
// itself. An empty exceptID excludes nothing.
func monthToDateExcept(ctx context.Context, q db.Querier, accountID string, now time.Time, exceptID string) (int64, error) {
	start := MonthStart(now)
	var sum int64
	err := q.QueryRow(ctx, `SELECT COALESCE(SUM(amount_usd_micros), 0) FROM top_ups
		WHERE billing_account_id = $1 AND kind = 'auto' AND status <> 'failed'
			AND COALESCE(cap_time, create_time) >= $2 AND COALESCE(cap_time, create_time) < $3
			AND ($4::text = '' OR top_up_id::text <> $4::text)`,
		accountID, start, start.AddDate(0, 1, 0), exceptID).Scan(&sum)
	return sum, err
}

// AutoTopUpProto returns the automatic top-up settings message.
func (st Settings) AutoTopUpProto(monthToDate int64) *accountsv1.AutoTopUpSettings {
	return &accountsv1.AutoTopUpSettings{
		Enabled:              st.AutoEnabled,
		ThresholdUsdMicros:   st.AutoThreshold,
		AmountUsdMicros:      st.AutoAmount,
		MonthlyCapUsdMicros:  st.AutoCap,
		ConsecutiveFailures:  st.ConsecutiveFailures,
		MonthToDateUsdMicros: monthToDate,
	}
}

// AlertProto returns the alert settings message.
func (st Settings) AlertProto() *accountsv1.AlertSettings {
	return &accountsv1.AlertSettings{LowBalanceThresholdUsdMicros: st.LowBalanceThreshold}
}

// ValidateAutoTopUp applies the rules of UpdateAutoTopUpSettings: with
// enabled true, a threshold of at least $10, an amount of at least $25 and
// at most $10,000 and a monthly cap of at least the amount, all whole cents; with enabled false,
// zero or non-negative whole cents.
func ValidateAutoTopUp(in *accountsv1.AutoTopUpSettings) error {
	if in == nil {
		return invalidArgument("settings is required")
	}
	values := []int64{in.GetThresholdUsdMicros(), in.GetAmountUsdMicros(), in.GetMonthlyCapUsdMicros()}
	for _, v := range values {
		if v < 0 || !WholeCents(v) {
			return invalidArgument("automatic top-up amounts must be non-negative whole cents")
		}
	}
	if !in.GetEnabled() {
		return nil
	}
	switch {
	case in.GetThresholdUsdMicros() < MinAutoTopUpThresholdUSDMicros:
		return invalidArgument("threshold_usd_micros must be at least 10 USD")
	case in.GetAmountUsdMicros() < MinTopUpUSDMicros:
		return invalidArgument("amount_usd_micros must be at least 25 USD")
	case in.GetAmountUsdMicros() > MaxTopUpUSDMicros:
		return invalidArgument("amount_usd_micros must be at most 10,000 USD")
	case in.GetMonthlyCapUsdMicros() < in.GetAmountUsdMicros():
		return invalidArgument("monthly_cap_usd_micros must be at least amount_usd_micros")
	}
	return nil
}

// UpdateAutoTopUp replaces the automatic top-up settings in tx, which holds
// the billing account lock. Enabling the setting resets the failure count.
func UpdateAutoTopUp(ctx context.Context, tx pgx.Tx, accountID string, in *accountsv1.AutoTopUpSettings, now time.Time) (Settings, error) {
	if err := ValidateAutoTopUp(in); err != nil {
		return Settings{}, err
	}
	if err := ensureSettings(ctx, tx, accountID, now); err != nil {
		return Settings{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE credit_settings SET
			auto_top_up_consecutive_failures = CASE WHEN $2 AND NOT auto_top_up_enabled THEN 0
				ELSE auto_top_up_consecutive_failures END,
			auto_top_up_enabled = $2, auto_top_up_threshold_usd_micros = $3, auto_top_up_amount_usd_micros = $4,
			auto_top_up_monthly_cap_usd_micros = $5, update_time = $6
		WHERE billing_account_id = $1`, accountID, in.GetEnabled(), in.GetThresholdUsdMicros(),
		in.GetAmountUsdMicros(), in.GetMonthlyCapUsdMicros(), now); err != nil {
		return Settings{}, err
	}
	return loadSettings(ctx, tx, accountID)
}

// UpdateAlert replaces the low-balance alert threshold in tx, which holds the
// billing account lock. A new threshold re-arms the alert.
func UpdateAlert(ctx context.Context, tx pgx.Tx, accountID string, in *accountsv1.AlertSettings, now time.Time) (Settings, error) {
	if in == nil {
		return Settings{}, invalidArgument("settings is required")
	}
	threshold := in.GetLowBalanceThresholdUsdMicros()
	if threshold < 0 || !WholeCents(threshold) {
		return Settings{}, invalidArgument("low_balance_threshold_usd_micros must be non-negative whole cents")
	}
	if err := ensureSettings(ctx, tx, accountID, now); err != nil {
		return Settings{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE credit_settings SET low_balance_threshold_usd_micros = $2,
			low_balance_alerted = CASE WHEN low_balance_threshold_usd_micros = $2 THEN low_balance_alerted ELSE false END,
			update_time = $3
		WHERE billing_account_id = $1`, accountID, threshold, now); err != nil {
		return Settings{}, err
	}
	return loadSettings(ctx, tx, accountID)
}
