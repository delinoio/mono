# Repository Billing Contract

## Scope

This contract defines the currency and monetary precision for every user-facing charge, price, balance and credit in the repository, and the shared billing model that every service uses. Project and domain contracts own their pricing, products and enforcement, but they must follow these rules.

## Currency

- All charges to users use USD. Do not price, charge or keep balances in another currency.
- Displaying an approximate amount in another currency is allowed only as an informational label. The charged and stored amount stays in USD.

## Precision

- The smallest monetary unit is 0.000001 USD (one micro-USD). Store and compute all prices, usage costs, credit amounts and balances with this precision.
- Represent amounts as signed 64-bit integers that count micro-USD (1 USD = 1,000,000 micro-USD). Never use binary floating-point types (`float`, `double`, JavaScript `number` arithmetic) for monetary values.
- Name integer amount fields and columns with a `_usd_micros` suffix, for example `balance_usd_micros` or `unit_price_usd_micros`.
- In TypeScript, handle amounts as `bigint`, which is the default protobuf-es mapping for `int64`. In Go, use `int64`. In SQL, use a 64-bit integer column such as `BIGINT`.
- Payment providers can charge at a coarser precision, such as USD cents. Convert at the payment boundary only, and document the conversion and its rounding direction in the owning domain contract.
- Any calculation that can produce a fraction smaller than one micro-USD, such as per-unit usage pricing, must document its rounding direction in the owning domain contract. Do not round implicitly.
- Round only for display. A rounded display value must never be stored or used in further calculations.

## Billing ownership

Every service uses [`delino-accounts`](project-delino-accounts.md) for billing.

- `delino-accounts` owns payment methods, billing contacts, the payment-provider integration, balances, credits, charges and invoices. It is the only component that calls a payment provider.
- Other services must not integrate a payment provider directly, store payment methods or keep their own balances or credits.
- Other services report billable usage or charges to the `delino-accounts` API at `https://accounts.api.delino.io`. Each service owns the pricing of its own products and documents it in its domain contract. Record each consuming service in the [API server contract](servers-accounts-api-foundation.md) before it calls the API.
- User-facing billing management lives in the `delino-accounts` web app at `https://accounts.delino.io`. Other services link to it. They must not build their own payment or invoice pages.

## Shared billing accounts and prepaid credits

The [`delino-accounts` API server contract](servers-accounts-api-foundation.md) and [protos contract](protos-delino-accounts-api-contract.md) own the details. Billing accounts, prepaid credits, the Paddle integration, the billing emails, usage reports, organization links and billing statuses are implemented in `accounts-api`.

- Billing accounts: `delino-accounts` keeps shared billing accounts with the roles `owner`, `admin` and `viewer`. Any Logto user can create one. One billing account can pay for organizations of several services. Each organization of a service links to at most one billing account. A user links an organization in the consuming service and must manage both the organization and the billing account.
- Prepaid credits: services charge from prepaid USD credits in the billing account, never by a direct payment per charge. Usage is always deducted, also below zero; a negative balance is debt.
- Payment provider (recorded decision): [Paddle](https://www.paddle.com/) is the payment provider and merchant of record. Charges are tax-exclusive: Paddle adds the tax on top, and the balance receives the full purchased amount. Paddle amounts are whole USD cents; `delino-accounts` converts them to micro-USD by multiplying by 10,000, only at the payment boundary, which is exact and needs no rounding.
- Credits come from a required $10/month subscription (a $10 credit grant with each payment), manual top-ups of $25 to $10,000 and optional automatic top-ups. Credit is added only by verified and deduplicated Paddle webhooks. Credit grants expire 12 months after purchase and are consumed first-expiring first (FIFO); a new grant pays off debt first. Credits are non-refundable except by a manual refund in the Paddle dashboard.
- Usage reports: a service reports each usage aggregate with the amount that it computed from its own prices, with its documented rounding, and with the idempotency key (service, organization, meter, window start). A repeated key deducts nothing again. Services call the usage-report API with a Logto machine-to-machine access token and the scope `accounts:report-usage`, according to the [authentication contract](repository-auth-contract.md#service-to-service-calls).
- Billing status: `delino-accounts` returns the status `ALLOWED`, `BLOCKED` or `SUSPENDED` of each linked organization. Each service documents in its own contract how it enforces these statuses and how it treats organizations without a link.
- Retention: usage records, the ledger, credit grants and billing audit events are kept for 7 years. Deleting a billing account or an organization keeps these records.
- Consuming services: [`ownsand`](project-ownsand.md#billing).

## References

- [Repository defaults](repository-defaults.md)
- [delino-accounts project index](project-delino-accounts.md)
- [delino-accounts API server contract](servers-accounts-api-foundation.md)
- [delino-accounts protos contract](protos-delino-accounts-api-contract.md)
