// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"

// LedgerKind is the kind of a ledger entry and the source of a credit grant,
// as the database stores it. The values never change.
type LedgerKind string

const (
	LedgerSubscriptionCredit LedgerKind = "subscription_credit"
	LedgerManualTopUp        LedgerKind = "manual_top_up"
	LedgerAutoTopUp          LedgerKind = "auto_top_up"
	LedgerUsage              LedgerKind = "usage"
	LedgerExpiry             LedgerKind = "expiry"
	LedgerRefund             LedgerKind = "refund"
)

// Proto returns the protobuf value.
func (k LedgerKind) Proto() accountsv1.LedgerEntryKind {
	switch k {
	case LedgerSubscriptionCredit:
		return accountsv1.LedgerEntryKind_LEDGER_ENTRY_KIND_SUBSCRIPTION_CREDIT
	case LedgerManualTopUp:
		return accountsv1.LedgerEntryKind_LEDGER_ENTRY_KIND_MANUAL_TOP_UP
	case LedgerAutoTopUp:
		return accountsv1.LedgerEntryKind_LEDGER_ENTRY_KIND_AUTO_TOP_UP
	case LedgerUsage:
		return accountsv1.LedgerEntryKind_LEDGER_ENTRY_KIND_USAGE
	case LedgerExpiry:
		return accountsv1.LedgerEntryKind_LEDGER_ENTRY_KIND_EXPIRY
	case LedgerRefund:
		return accountsv1.LedgerEntryKind_LEDGER_ENTRY_KIND_REFUND
	default:
		return accountsv1.LedgerEntryKind_LEDGER_ENTRY_KIND_UNSPECIFIED
	}
}

// SubscriptionStatus is the stored subscription status. A billing account
// without a subscription row has SubscriptionNone.
type SubscriptionStatus string

const (
	SubscriptionNone     SubscriptionStatus = "none"
	SubscriptionPending  SubscriptionStatus = "pending"
	SubscriptionActive   SubscriptionStatus = "active"
	SubscriptionPastDue  SubscriptionStatus = "past_due"
	SubscriptionCanceled SubscriptionStatus = "canceled"
)

// Proto returns the protobuf value.
func (s SubscriptionStatus) Proto() accountsv1.SubscriptionStatus {
	switch s {
	case SubscriptionNone:
		return accountsv1.SubscriptionStatus_SUBSCRIPTION_STATUS_NONE
	case SubscriptionPending:
		return accountsv1.SubscriptionStatus_SUBSCRIPTION_STATUS_PENDING
	case SubscriptionActive:
		return accountsv1.SubscriptionStatus_SUBSCRIPTION_STATUS_ACTIVE
	case SubscriptionPastDue:
		return accountsv1.SubscriptionStatus_SUBSCRIPTION_STATUS_PAST_DUE
	case SubscriptionCanceled:
		return accountsv1.SubscriptionStatus_SUBSCRIPTION_STATUS_CANCELED
	default:
		return accountsv1.SubscriptionStatus_SUBSCRIPTION_STATUS_UNSPECIFIED
	}
}

// TopUpKind is a manual or an automatic top-up.
type TopUpKind string

const (
	TopUpManual TopUpKind = "manual"
	TopUpAuto   TopUpKind = "auto"
)

// LedgerKind returns the ledger kind of the credit of the top-up.
func (k TopUpKind) LedgerKind() LedgerKind {
	if k == TopUpAuto {
		return LedgerAutoTopUp
	}
	return LedgerManualTopUp
}

// TopUpStatus is the state of a top-up; see the top_ups table.
type TopUpStatus string

const (
	TopUpPending   TopUpStatus = "pending"
	TopUpCharging  TopUpStatus = "charging"
	TopUpCharged   TopUpStatus = "charged"
	TopUpUnknown   TopUpStatus = "unknown"
	TopUpCompleted TopUpStatus = "completed"
	TopUpFailed    TopUpStatus = "failed"
)

// FailureReason says why a top-up failed.
type FailureReason string

const (
	// FailureDeclined: Paddle declined the payment.
	FailureDeclined FailureReason = "payment_declined"
	// FailurePastDue: the subscription is past_due.
	FailurePastDue FailureReason = "subscription_past_due"
	// FailureTooSoon: the next billing period starts within 30 minutes.
	FailureTooSoon FailureReason = "next_billing_too_soon"
	// FailureRejected: Paddle rejected the charge for another reason.
	FailureRejected FailureReason = "rejected"
	// FailurePaymentFailed: the payment webhook reported a failed payment.
	FailurePaymentFailed FailureReason = "payment_failed"
	// FailureTimedOut: no verified payment webhook arrived in time.
	FailureTimedOut FailureReason = "timed_out"
	// FailureNoSubscription: the billing account has no subscription that
	// Paddle can charge.
	FailureNoSubscription FailureReason = "no_subscription"
	// FailureNotApplied: Paddle did not process the call (for example an
	// invalid API key or rate limiting). It does not count as a failed
	// automatic top-up, because nothing was charged or declined.
	FailureNotApplied FailureReason = "not_applied"
	// FailureSettingsChanged: the automatic top-up settings changed after
	// the top-up was queued, and the current settings no longer allow it
	// (automatic top-up is off, the amount is above the current amount or
	// the monthly cap leaves no room). Nothing was charged, so it does not
	// count as a failed automatic top-up.
	FailureSettingsChanged FailureReason = "settings_changed"
)

// CountsAsFailure reports whether the reason counts toward the 2-failure
// shutoff of automatic top-up. A reason that left the payment provider
// uncalled does not count.
func (r FailureReason) CountsAsFailure() bool {
	return r != FailureNotApplied && r != FailureSettingsChanged
}

// NotificationKind is the kind of a billing notification email.
type NotificationKind string

const (
	NotifyPaymentFailed     NotificationKind = "payment_failed"
	NotifyAutoTopUpDisabled NotificationKind = "auto_top_up_disabled"
	NotifyLowBalance        NotificationKind = "low_balance"
	NotifyCreditExpiry      NotificationKind = "credit_expiry"
	// NotifyOrganizationBlocked and NotifyOrganizationSuspended report that
	// a linked organization became BLOCKED or SUSPENDED.
	NotifyOrganizationBlocked    NotificationKind = "organization_blocked"
	NotifyOrganizationSuspended  NotificationKind = "organization_suspended"
	notifyDetailTopUpKind                         = "top_up_kind"
	notifyDetailAmount                            = "amount_usd_micros"
	notifyDetailReason                            = "reason"
	notifyDetailThreshold                         = "threshold_usd_micros"
	notifyDetailBalance                           = "balance_usd_micros"
	notifyDetailExpireTime                        = "expire_time"
	notifyDetailPaymentScope                      = "scope"
	notifyScopeSubscription                       = "subscription"
	notifyScopeTopUp                              = "top_up"
	notifyDetailService                           = "service"
	notifyDetailOrganizationID                    = "organization_id"
	notifyDetailOrganizationName                  = "organization_name"
	notifyDetailSuspendTime                       = "suspend_time"
)
