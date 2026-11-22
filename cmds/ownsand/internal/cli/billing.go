// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"

	"connectrpc.com/connect"

	ownsand "github.com/delinoio/mono/go/ownsand"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// BillingURL is where users pay, top up and manage billing accounts.
const BillingURL = "https://accounts.delino.io"

// ConsoleURL is the OwnSand web console, where the organization owner links
// a billing account on the organization settings page.
const ConsoleURL = "https://ownsand.delino.io"

// Display units of the billing meters.
var (
	// bytesPerGiB is 2^30 bytes.
	bytesPerGiB = big.NewInt(1 << 30)
	// byteSecondsPerGiBMonth is 1 GiB-month (730 GiB-hours) in byte-seconds.
	byteSecondsPerGiBMonth = new(big.Int).Mul(big.NewInt(1<<30), big.NewInt(730*3600))
	microsPerUSD           = big.NewInt(1_000_000)
)

// billingErrorText returns the text that the CLI prints for a billing error:
// the server message, which contains the billing link, and the link when a
// server left it out. ok is false when err is not a billing error.
func billingErrorText(err error) (string, bool) {
	if !ownsand.IsBillingError(err) {
		return "", false
	}
	var ce *connect.Error
	msg := err.Error()
	if errors.As(err, &ce) {
		msg = ce.Message()
	}
	if !strings.Contains(msg, BillingURL) {
		msg += "; manage billing at " + BillingURL
	}
	return "billing: " + msg, true
}

// billingStatusJSON is the --format json output of `billing status`: the
// protobuf JSON of both responses. usage is absent when GetBillingUsage was
// not called or the caller may not read it.
type billingStatusJSON struct {
	Status map[string]any `json:"status"`
	Usage  map[string]any `json:"usage,omitempty"`
}

func (a *app) billingStatus(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	st, err := a.client.Billing().GetStatus(ctx, &ownsandv1.GetBillingStatusRequest{OrganizationId: org})
	if err != nil {
		return err
	}
	billing := st.GetBilling()
	apiKey := a.getenv(ownsand.EnvAPIKey) != ""
	var usage *ownsandv1.GetBillingUsageResponse
	usageDenied := false
	switch {
	case !billing.GetLinked():
		// GetBillingUsage fails with failed_precondition without a link.
	case apiKey:
		// API keys get permission_denied from GetBillingUsage.
		usageDenied = true
	default:
		// The CLI does not read the caller's role first: GetBillingUsage
		// itself accepts only an owner or admin, and a member gets
		// permission_denied, which shows the status only.
		usage, err = a.client.Billing().GetUsage(ctx, &ownsandv1.GetBillingUsageRequest{OrganizationId: org})
		switch {
		case err == nil:
		case connect.CodeOf(err) == connect.CodePermissionDenied:
			a.logger.Debug("ownsand billing usage not readable for this caller", "organization_id", org)
			usage, usageDenied = nil, true
		case connect.CodeOf(err) == connect.CodeFailedPrecondition:
			// The link was removed between the two calls.
			a.logger.Debug("ownsand billing usage without a link", "organization_id", org)
			usage = nil
		default:
			return err
		}
	}
	a.logger.Debug("ownsand billing status", "organization_id", org, "phase", billing.GetPhase().String(), "linked", billing.GetLinked(), "usage", usage != nil)
	if a.fmt == FormatJSON {
		out := billingStatusJSON{}
		if out.Status, err = protoValue(st); err != nil {
			return err
		}
		if usage != nil {
			if out.Usage, err = protoValue(usage); err != nil {
				return err
			}
		}
		if err := writeJSON(a.env.Stdout, out); err != nil {
			return err
		}
		// The JSON result on standard output is complete at this point; the
		// notes on standard error are advice, so a failed write of them does
		// not change the exit status.
		a.billingNotes(a.env.Stderr, org, billing, usageDenied)
		return nil
	}
	w := &stickyWriter{w: a.env.Stdout}
	a.printBillingText(w, org, billing, usage)
	a.billingNotes(w, org, billing, usageDenied)
	return w.err
}

// printBillingText writes the text result to w, a stickyWriter that holds the
// first write error for the caller.
func (a *app) printBillingText(w *stickyWriter, org string, billing *ownsandv1.OrganizationBilling, usage *ownsandv1.GetBillingUsageResponse) {
	line := func(k, v string) { fmt.Fprintf(w, "%s: %s\n", a.paint(ansiCyan, safeText(k)), safeText(v)) }
	line("organization", org)
	line("status", enumName(billing.GetPhase()))
	if r := billing.GetReason(); r != ownsandv1.BillingReason_BILLING_REASON_UNSPECIFIED {
		line("reason", enumName(r))
	}
	if billing.GetLinked() {
		if usage != nil {
			line("billing account", fmt.Sprintf("%s (%s)", usage.GetBillingAccountName(), usage.GetBillingAccountId()))
		} else {
			line("billing account", "linked")
		}
	} else {
		line("billing account", "not linked")
	}
	if billing.GetBlockedSince() != nil {
		line("blocked since", ts(billing.GetBlockedSince()))
	}
	if billing.GetSuspendTime() != nil {
		label := "suspension time"
		if billing.GetPhase() == ownsandv1.BillingPhase_BILLING_PHASE_SUSPENDED {
			label = "suspended since"
		}
		line(label, ts(billing.GetSuspendTime()))
	}
	if usage == nil {
		return
	}
	line("balance", formatUSD(usage.GetBalanceUsdMicros()))
	line("month start", ts(usage.GetMonthStart()))
	fmt.Fprintln(w)
	headers := []string{"meter", "usage", "cost"}
	if a.color {
		for i := range headers {
			headers[i] = strings.ToUpper(headers[i])
		}
	}
	rows := make([][]string, 0, len(usage.GetMeterUsages()))
	for _, m := range usage.GetMeterUsages() {
		rows = append(rows, []string{meterLabel(m.GetMeter()), meterQuantity(m.GetMeter(), m.GetQuantity()), formatUsageCost(m.GetAmountUsdMicros())})
	}
	_ = a.printTable(w, headers, rows) // w keeps the error
	fmt.Fprintln(w, "Usage of the last 5 to 10 minutes may not be included yet.")
}

// billingNotes writes what the user can do next. Write errors are the
// caller's to read from w, as for printBillingText.
func (a *app) billingNotes(w io.Writer, org string, billing *ownsandv1.OrganizationBilling, usageDenied bool) {
	if !billing.GetLinked() {
		fmt.Fprintf(w, "No billing account is linked. The organization owner links one on the organization settings page of the console: %s/orgs/%s/settings\n", ConsoleURL, org)
		fmt.Fprintf(w, "Create or manage billing accounts at %s\n", BillingURL)
	}
	switch billing.GetPhase() {
	case ownsandv1.BillingPhase_BILLING_PHASE_BLOCKED:
		fmt.Fprintf(w, "The organization is blocked: the control plane rejects new sandboxes, resumes, snapshots, templates and device enrollment. It becomes suspended at the suspension time unless billing recovers. Manage billing at %s\n", BillingURL)
	case ownsandv1.BillingPhase_BILLING_PHASE_SUSPENDED:
		fmt.Fprintf(w, "The organization is suspended: running sandboxes are paused, and exec and file operations are rejected too. Manage billing at %s\n", BillingURL)
	}
	if billing.GetLinked() && usageDenied {
		fmt.Fprintln(w, "Only an organization owner or admin signed in as a user can see the balance and the usage.")
	}
}

func meterLabel(m ownsandv1.BillingMeter) string {
	switch m {
	case ownsandv1.BillingMeter_BILLING_METER_RELAY_EGRESS:
		return "relay egress"
	case ownsandv1.BillingMeter_BILLING_METER_R2_STORAGE:
		return "R2 storage"
	case ownsandv1.BillingMeter_BILLING_METER_CLASS_A_RPC:
		return "Class A RPCs"
	case ownsandv1.BillingMeter_BILLING_METER_CLASS_B_RPC:
		return "Class B RPCs"
	}
	return enumName(m)
}

// meterQuantity shows a meter quantity in its display unit: GiB for relay
// egress bytes, GiB-months for R2 storage byte-seconds and calls for RPCs.
func meterQuantity(m ownsandv1.BillingMeter, q int64) string {
	switch m {
	case ownsandv1.BillingMeter_BILLING_METER_RELAY_EGRESS:
		return formatDecimal(new(big.Rat).SetFrac(big.NewInt(q), bytesPerGiB), true) + " GiB"
	case ownsandv1.BillingMeter_BILLING_METER_R2_STORAGE:
		return formatDecimal(new(big.Rat).SetFrac(big.NewInt(q), byteSecondsPerGiBMonth), true) + " GiB-months"
	case ownsandv1.BillingMeter_BILLING_METER_CLASS_A_RPC, ownsandv1.BillingMeter_BILLING_METER_CLASS_B_RPC:
		return fmt.Sprintf("%d calls", q)
	}
	return fmt.Sprint(q)
}

// formatUSD shows a micro-USD amount in USD with 2 decimals, for a balance
// or a total. Rounding is for display only.
func formatUSD(micros int64) string {
	return usd(formatDecimal(new(big.Rat).SetFrac(big.NewInt(micros), microsPerUSD), false))
}

// formatUsageCost shows a usage cost: 2 decimals, and up to 6 decimals for a
// cost below $0.01 that is not zero.
func formatUsageCost(micros int64) string {
	return usd(formatDecimal(new(big.Rat).SetFrac(big.NewInt(micros), microsPerUSD), true))
}

func usd(s string) string {
	if rest, ok := strings.CutPrefix(s, "-"); ok {
		return "-$" + rest
	}
	return "$" + s
}

// formatDecimal rounds v to 2 decimals, to nearest with halves away from
// zero. With small, a value that is not zero and whose absolute value is
// below 0.01 shows with up to 6 decimals instead, without trailing zeros. A
// value that rounds to zero shows without a sign.
func formatDecimal(v *big.Rat, small bool) string {
	abs := new(big.Rat).Abs(v)
	var s string
	if small && v.Sign() != 0 && abs.Cmp(big.NewRat(1, 100)) < 0 {
		s = strings.TrimRight(v.FloatString(6), "0")
		if i := strings.IndexByte(s, '.'); i >= 0 && len(s)-i-1 < 2 {
			s += strings.Repeat("0", 2-(len(s)-i-1))
		}
	} else {
		s = v.FloatString(2)
	}
	if strings.Trim(s, "-0.") == "" {
		return strings.TrimPrefix(s, "-")
	}
	return s
}
