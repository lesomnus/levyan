# Working on levyan

A [payday](https://github.com/lesomnus/payday) app that bills other payday
apps' tenants for what they hold and use. Most of it is **generated from
`proto/`**, so the usual shape of a change is: edit the schema, regenerate,
then write the part no schema can state.

`README.md` is how to run it (in Korean), `docs/design.md` why it is shaped
this way (in Korean, condensed from levyan#1). Code comments, commits and this
file are English; issues, PRs and the docs are Korean.

Whose a decision is, is said wherever one is written down -- docs, PRs, code
comments -- with one of three labels, and never more than it was:

- **대표**, the owner: the owner's decisions -- levyan#1's "정한 것
  (2026-10-31, 대표)" table (docs/design.md §2) and the owner's later
  answers, each recorded with its date (25–27 on 2026-10-31, §11).
- **Claude**, the lead: what Claude decided carrying out the owner's plan
  ("남은 작업 모두 진행해줘") -- what a sub-issue was handed with. The owner
  may overturn it, and it is never written as the owner's.
- **구현에서 정한 것**: what the agent implementing a sub-issue chose.

## Regenerate after touching the schema

```sh
scripts/gen.sh            # go tool pd gen ., and then scripts/api.sh
go tool pd gen --check .  # what CI runs: fails if anything moved
go tool pd doctor .       # what would go wrong, before it does
```

A generated file that was not regenerated **compiles perfectly and is wrong**.
If you edited anything under `proto/`, you are not done until `pd gen --check`
exits 0. `pd gen` fetches the buf it pins; there is no page and no `--ts`.

The API (`api/`) is a module of its own, which `pd gen` and `pd gen --check`
leave alone (payday#189): `scripts/api.sh` is its generation, and runs the buf
`pd gen` fetched.

## The API apps call (`api/`)

`levyan.v1` is what a product calls, and it is **written by hand**: it is not
generated from `proto/`, names nothing of payday's, and lives in a Go module of
its own (`github.com/lesomnus/levyan/api`) that requires gRPC and protobuf and
nothing else. That is so that an app calling levyan is never held to the payday
levyan is built with -- the way loft is held to roster's (docs/design.md §8).
The root module uses it through `replace ./api`.

```sh
scripts/api.sh            # after editing api/levyan/v1/*.proto
scripts/api.sh --check    # what CI runs: generated Go current, nothing of payday's required
scripts/api.sh --breaking origin/main
```

- Add fields; never renumber, rename or retype one. CI runs `buf breaking`
  against the base of every pull request.
- `server/api` maps between the API's messages and the schema's, and reads
  through the **walled** stack with the caller's frame. Never hand it
  `Ungated`.
- An enum the API shares with the schema has the same values under the same
  names (`server/api/api_test.go`).
- Never import `github.com/lesomnus/payday/...` or the root module from
  `api/`; `scripts/api.sh --check` refuses it.

## Upgrading payday

```sh
go tool pd upgrade --payday <commit>   # go get, tidy, its floors, and its pd gen
go tool pd gen --check .
```

A commit, not `latest`: payday tags no versions. `pd upgrade` commits nothing:
read `git diff`. The generators are pinned at the versions payday's own go.mod
carries; Renovate leaves payday and protobuf-orm alone (payday's
`renovate/app` preset).

## Do not edit — regenerate

| | |
| --- | --- |
| `*.g.go`, `*.pb.go` | wherever `go_package` puts them |
| `server/bare/`, `server/pd/`, `internal/ent/` | in whole |
| `proto/levyan/payday/` | in whole — payday's entities, **copied** in |
| `proto/**/*_svc.g.proto` | the generated contract of an entity |
| `api/levyan/v1/*.pb.go` | `scripts/api.sh` |

**`.g` means a generator wrote it.** Everything else is yours — including
`proto/levyan/*.proto`, `api/levyan/v1/*.proto`, `cmd/`, `cli/` and `server/`
outside `bare` and `pd`.

## Adding an entity

```sh
go tool pd entity add --tenanted Widget .
```

Use this rather than writing the file. It picks a free domain number and writes
the tenancy out — the two things that are cheap to get wrong and expensive to
find later.

Field numbers are read by name across every entity:

- **1** key · **2** tenant · **3** *yours, a set smaller than a tenant* ·
  **4** `alias` · **5** `name` · **6** `desc` · **7** `labels` ·
  **13/14/15** the timestamps
- **8–12 and 16+** are yours. An entity that does not want 4–7 **leaves those
  numbers empty** rather than spending them on something else.

A tenant here is a **product** (loft is the first). Field 3 is free for a set
smaller than a product -- a product's own tenant, a workspace, is the obvious
one when payers come (#4).

## Adding a field to an entity that has shipped

A field added to an entity that is on `main` already has a **default** the
database fills the rows it has with -- `(orm.field) = {default: ""}`, the
field's zero -- or is **nullable**. A column that may
not be NULL and has no default cannot be added to a table with rows in it:
PostgreSQL refuses it (`column "label" of relation "workspace" contains null
values`), and SQLite too. This is Claude's rule (2026-10-31), after
`Workspace.label` (#25) stopped staging's migration; `Statement.first_unit`
(#22) was the same. A default gives the generated Add request's field
explicit presence, so a builder sets it with `proto.Bool`, `proto.String`.

No test says so -- every one starts from an empty database. `scripts/upgrade.sh
BEFORE NOW` does, and CI runs it (`upgrade`): the build before, and `OLDEST`,
the build staging runs (pinned in the workflow; move it when staging moves),
write what a product and an operator write, and this build migrates it and
reads it back. A feature that writes rows reachable without a roster or a
merchant of record is written there too, and read back.

```sh
go build -o /tmp/now/ ./cmd/levyan ./cmd/levyan-admin
git worktree add --detach /tmp/before-src origin/main
(cd /tmp/before-src && go build -o /tmp/before/ ./cmd/levyan ./cmd/levyan-admin)
scripts/upgrade.sh /tmp/before /tmp/now   # and UPGRADE_POSTGRES=postgres://…/<empty db>
```

## Writing a layer

A layer embeds `Overlay` — and must also write `WithDriver`, which nothing
inherits:

```go
func (s Book) WithDriver(drv dialect.Driver) (levyan.Server, error) {
	next, err := enttx.Rebind(s.Next(), drv)
	if err != nil {
		return nil, err
	}

	return New(next, s.now), nil
}

var (
	_ levyan.Server               = Book{}
	_ enttx.Binder[levyan.Server] = Book{}
)
```

Leave it out and **nothing fails until a transaction is opened** — a batch, or a
multi-write RPC — because `enttx.Rebind` asks at run time. `pd doctor` finds it.

A rule about the row an `Add` writes goes on the server behind with
`bare.<E>WithAddRule` rather than in an `Add` you override (`server/book` is
the example). **Do not edit the generated `Gate`.**

## Two servers, and one of them has no wall

`cmd.Build` makes `Walled` and `Ungated`, both with the price book's rules
(`server/book`) on them. `Ungated` is not a privilege — it is an instance the
wall was never installed on, for work that cannot be done from inside a tenant:
resolving who is calling, and what an operator asks of the operator's entry
point. **Never hand `Ungated` to anything a product can reach.**

## Two binaries, two doors

`cmd` is the wiring both share. Each binary puts its own door on it:

- `levyan` (`cli`): products, by service key (`server/keys`), held to the
  policy table (`server/policy`), calling `levyan.v1` (`server/api`). That door
  is `cli.Front`, and `cmd.Server.Grpc` refuses to serve without an `Auth` and a
  `Policy` -- there is no `auth.Plain` fallback.
- `levyan-admin` (`cli/admin`, `server/admin`): operators, by the keys in
  `admin.keys`, over HTTP on `admin.addr`. What they write goes through
  `Ungated` with their `Operator` row as the frame's actor, so the trail says
  who published a price.

Neither binary links the other's door: `go test ./cmd/levyan` asks `go list
-deps` of both, and also that both link the same drivers and witnesses (they
read one configuration). A new package that only one binary may link goes into
that test.

## What may be called

`server/policy` is one table of method → kind of caller, and what is not in it
is refused. A new RPC is closed until it is added there. The kinds:

- **product** — a `ServiceKey` frame, which sees its own tenant and nothing else:
  `levyan.v1`, the receiver of its flows
  (`opentelemetry.proto.collector.metrics.v1.MetricsService/Export`), and
  payday's `payday.GrantService` (`Watch`, `List`), what its workspaces are
  granted;
- **payer** — an account of roster's calling as itself, in no tenant (payday's
  account frame), relayed by a product with its key and `levyan-account`
  (`server/relay`): `levyan.v1.PayerService` and nothing else;
- operators never call here.

The generated entity services are nobody's. A product reads what it needs
through `levyan.v1`; give it a method there rather than a row of the table for
a generated one.

## The price book

- A **meter** is never erased (`erase: {disabled: true}`): its key would come
  free, and a key taken again to count something else bills one name two ways.
  What it is -- key, unit, kind, aggregation, `unbounded_zero` -- is
  immutable.
- A **price version** is never changed: every field immutable, no Patch, no
  Erase. A change is a new version; a mistake is another version of the same
  dates, published after it. Which is in force is `internal/pricing`
  (`Book.InForce`, `Book.Timeline`), and the rule is in `proto/levyan/price.proto`.
- A version is never effective before it is published, and its dates are kept to
  the microsecond, as PostgreSQL keeps them (`server/book`).
- **Money** is an int64 of the currency's minor unit (`internal/money`). Two
  currencies are never added (`Amount.Add` refuses). A currency is priced in
  only once `internal/money` lists it.
- **Rounding** is once, at the end of a version's amount, and down. Everything
  before it is exact (`math/big`). Do not round a share, a band or a quantity on
  the way.

## The ledger

What products send of their workspaces (`server/ledger`, docs/design.md §9):
a stock's **readings** (`levyan.v1.MeteringService/Record`) and a flow's
**points** (OpenTelemetry's `MetricsService/Export`, `server/otlp`). Both are
rows that are never changed: every field immutable, no Patch, no Erase.

- **Kept once.** A reading is (product, workspace, meter, measured_at) and a
  point (series, start, time), each a unique index. The same again is
  `Already`; the same key with another value is refused (`Conflict`) and said
  in the log -- never one of the two values picked. A point that counts no
  more than one before it under the same start adds nothing to any period and
  is not written (`Nothing`).
- **Refused one at a time.** A call answers each reading or point on its own
  (OTLP's partial success, for the receiver), so a meter the price book does
  not have yet does not cost the workspace its other readings.
- **Written through the servers, as the caller.** `ledger.Ledger.S` is the
  walled stack for a product's calls, so the trail names the key that sent
  each row. What the ledger reads to decide -- meters, workspaces, rows already
  kept, periods -- it reads with ent on the transaction it writes in, and only
  of the product the frame names, never one a request names.
- **A workspace's row is held by every write of its ledger** (`ledger.Lock`:
  `FOR UPDATE` on PostgreSQL; SQLite has one writer, `_txlock=immediate`), and
  by the close of a period for it (#6). Do not write the ledger without it:
  it is what makes "late" exact.
- **Late** is a row that speaks for a period whose close has begun -- a
  `Period` row exists: a reading measured in it, or within `usage.Hold` before
  it; a point counted in it, or whose next point is. It is kept and marked, and
  a later statement corrects the period.
- **`usage.Hold` is the rating's**, not configuration: how long a reading
  speaks for its value when no later one comes. `readings.gap` is only the
  warning (`levyan.readings.gaps`); a bill must not move because somebody tuned
  an alert.
- `Workspace` is the set (field 3) of every ledger row. `Workspace` and
  `Series` have a Patch only because protobuf-orm writes `<Entity>GetKey`,
  which an edge to them needs, beside a Patch; every field is immutable and no
  caller is given it.
- **An aggregate read through ent** scans a time with `ledger.instant` (SQLite
  answers one as nanoseconds) and an identifier with `ledger.key` (the
  standard library's `uuid.UUID` is no `sql.Scanner`). A struct field ent
  cannot scan is read as JSON, and the panic inside `Rows.Scan` then hangs the
  call rather than failing it.

The receiver is gRPC on purpose: it is the same door, behind the same chain,
as `levyan.v1`, and the operator's binary does not link it (`cmd/levyan`'s
test).

## Rating and closing periods

What a workspace's period comes to (`server/billing`, docs/design.md §10). The
arithmetic is pure and has tests of its own, by hand: `internal/usage` (what a
stretch held or did) and `internal/rating` (what it comes to under the versions
in force). Keep it there; `server/billing` only reads the ledger into it.

- **A period is a calendar month in the product's zone** (`Tenant.zone`, the
  overlay), and periods follow one another from where the last one ended. A
  product with no `channel` or `currency` is priced nothing and closed never.
  Both are names (`CHANNEL_PG_KR`), not the enum: protobuf-orm's service
  contracts do not import an enum declared in another file than their
  entity's, and the price's is in a file that imports the tenant's.
- **The close writes the `Period` row first**, then each workspace's
  statement in a transaction of its own that holds the workspace's row
  (`ledger.Lock`), then stamps the period closed. Twice is once: a statement
  is unique on (product, workspace, period), and a close that stopped goes on
  where it stopped. Never read the ledger for a close before the row is
  written -- it is what makes a row late.
- **A statement is never changed**: every field immutable, no Erase. Its
  Patch exists only because an invoice's line has an edge to it, and
  protobuf-orm writes `<Entity>GetKey` beside a Patch alone; it changes
  nothing and nobody is given it. A correction is a line on a later
  statement: the corrected period rated again from the ledger as it is, less
  what was billed for it (its own line and every correction since), for each
  period a late row speaks for.
  A correction already made comes to nothing by the same arithmetic, so
  nothing records which late rows were handled.
- **Exact until the total.** Amounts are `big.Rat` and are stored as
  fractions (`110/3`); a statement's total is the exact sum rounded once,
  down (`rating.Total`, which floors a negative one too). Never round a line.
- **The estimate is the close.** `BillingService.GetUsage` is
  `billing.Estimate`: what the period's statement would say now, each stock's
  latest reading held to the period's end. Do not give the API a second way
  to work out an amount.
- A statement says the rating's version (`rating.Version`). A change to what
  any usage comes to -- `usage.Hold` included -- is a new version.
- **Unpriced is said, not silent.** A stretch of a line's period with no
  version in force on the statement's channel and currency, while the meter
  has one on another, is `Line.Unpriced` (`rating.Rate`), kept on the
  statement's line and in the API's; the close logs each (product, meter,
  channel, currency) once a period, and `levyan-admin price check` lists them
  before a period closes. A meter priced nowhere is not sold: it comes to
  nothing and says nothing. Marking moves no amount, so no rating version.
- Tests move the deployment's time with `levyantest.World.At`: every server
  `cmd.Build` makes reads `cmd.Server.Clock`, so a price can be effective in
  the past of the readings a test records.

## The PG channel (`internal/pg`)

What levyan asks of a PG it sells through (docs/design.md §11): the
interface, `pg.Channel`, and one candidate's adapter, `internal/pg/toss`.
**Which PG is the owner's decision and is not made**; nothing but the wiring
names `toss`, and the fake (`internal/pg/toss/tosstest`) is what tests use.

- **Card data never passes through levyan.** A card is registered in the PG's
  window, and levyan gets a one-time authorization back for the billing key.
- **A billing key is a secret** (`pg.BillingKey`): it prints and logs as
  nothing, refuses to marshal, and `Reveal` is the one way to it. Store it
  sealed, never in a log, a span, an error or the trail. A charge's URL names
  it, so the `http.Client` an adapter is given must not record URLs.
- **A declined charge is a result, not an error**: a `Failed` payment. An
  error is the request failing, and `pg.KindOf` says what is known -- only
  `Unknown` may have moved money.
- **An unknown charge is asked about before it is sent again**: query the
  order; send it again only on `NotFound`, under the same order and a new
  idempotency key (Toss answers a key with its first answer, an error too).
- **A webhook is a hint**: `Webhook` reads it without the network for the
  inbox, `Recheck` asks the PG, and only that answer is believed. Two answers
  are ordered by `Payment.Progress`, never by time.

## The MoR channel (`internal/mor`)

What levyan asks of a merchant of record it sells through (docs/design.md
§12): the interface, `mor.Channel`, and its adapter, `internal/mor/paddle`.
**The MoR is Paddle** -- the owner's decision, 2026-10-31 (#12) -- and the
owner has made no Paddle account yet, so there are no keys: the fake
(`internal/mor/paddle/paddletest`) is what tests use, and `TestLive` waits.
Nothing but the wiring (`cmd`) names `paddle`.

- **levyan computes the amount.** A closed period's amount goes to the MoR as
  a one-time charge of the payer's subscription (`Charge`): an amount, never a
  quantity for a usage feature of the MoR's to price.
- **The subscription is the MoR's.** levyan keeps a mirror made of what the
  MoR's API answers (`Subscription`, `Recheck`), never of a webhook's body; the
  payer can cancel at the MoR without asking levyan.
- **A charge is kept once by its key**, and the MoR keeps no key of levyan's:
  the adapter writes the key into the charge and looks for it before every
  send. Write each send down before making it, and hand the adapter the last
  one before (`Charge.Sent`): within `mor.Settle` of it nothing is sent again,
  and an absence is `ErrTooSoon`, not `NotFound`. `ErrTwice` is a key charged
  twice; the rest are to give back.
- **A declined charge is a result, not an error**: a `Failed` transaction.
  Only `Unknown` may have moved money. Its `Failure.Decline` is `pg.Decline`'s
  four kinds by the same names and values (`mor.Decline`), read off Paddle's
  codes by their own words (`paddle/codes.go`); a charge made now that
  `prevent_change` declines says no more of why, and is `DeclineOther`.
- **What the MoR charges at least is its own** (`Channel.Minimum`: 980 KRW,
  70 USD cents …): the collector's minimum, never a constant of levyan's. A
  currency levyan prices in has one, or the adapter's test fails.
- **No charge for a next period that will not come.** A subscription
  scheduled to cancel or to pause has none, and Paddle forgives what was to
  be billed in it: `NextPeriod` of one is `ErrNoNextPeriod`. Charge `Now`,
  which is how a period's amount is handed over unless there is a reason not
  to.
- **A refund is an adjustment the MoR may take days to approve**, and one
  pending blocks the next (`RateLimited`).
- **A webhook is a hint**: `Webhook` checks the signature and that it is
  fresh, and reads only what it names; `Recheck` asks the API. Keep it once by
  `Notice.ID`; order a transaction's answers by `Transaction.Progress`, a
  subscription's by `Subscription.Updated`. The MoR's own list of what it
  noticed (`Notices`, Paddle's `GET /events`) is read into the same inbox, by
  the same ids.
- **Plans are by currency** (`paddle.Config.Plans`): a checkout is in its
  billing account's currency, in the plan of it.

## Subscriptions (`server/subscriptions`)

A billing account on the MoR's channel is collected through a subscription
the payer starts in the MoR's checkout (docs/design.md §12, "연결"):

- **A checkout is completed only as levyan recorded it** (`Checkout`), for
  the billing account it was begun for: `CompleteCheckout` takes levyan's id
  of it, never the MoR's. A billing account has one open at a time -- begun
  again, it is the same one while the MoR takes it (`mor.Start.Again`) -- and
  one live subscription: a checkout of one that has a live one is refused
  once the MoR is asked again, and a second subscription made all the same
  is canceled at the MoR as levyan learns of it.
- **The mirror** (`Subscription`) is written through the servers, of what
  the MoR's API answers, and an answer older than the one it holds -- by the
  MoR's `updated_at` -- changes nothing. How the payer pays, and whether a
  payment waits for them, come of the newest transaction of it levyan was
  told of (Paddle ids sort as made, then `Progress`). Of a card, keep its
  brand and its last four digits, and nothing more; `method_last4` is
  `personal`.
- **A billing account's collection** (`BillingAccount.collection`,
  `CollectionState`) is written beside its mirror, in the same transaction
  holding the billing account's row, and only when it changes -- so each
  change is on the trail. What a state that is not collectable leads to is
  #11's; read it with `subscriptions.CollectionOf`.
- **The inbox** (`Notice`) and its cursor (`NoticeCursor`) are written with
  ent alone and are `trail: off`: a notice is a hint, and what it leads to is
  asked of the MoR and written through the servers. The webhook endpoint
  (`POST /webhooks/mor/<adapter>` on `server.http`) holds the inbox and the
  channel's signature check and **never a server stack** -- it is the
  public's to reach. It answers within `subscriptions.Answer`, with no
  request of its own to the MoR; with no `mor.webhook_secret` it is not
  served at all.
- **The pull** reads past nothing newer than `subscriptions.Overlap`, so a
  notice the MoR lists late is not passed over; the inbox keeps each once.
- **Collecting an invoice is not done here** but by the MoR's
  `invoices.Collector` (`server/invoices/merchant`): it charges the
  subscription of the billing account's live mirror
  (`subscriptions.CollectionOf`), asks of every subscription it has had
  (`Collection.Mirrors`), and is the inbox's `Taker.Transaction`
  (`cmd.Server.Taker`) for what the MoR answers of each transaction a
  notice names.
- **The API key and the webhook secret are `cfg.Secret`s**, sent in a header
  and never in a URL; no error of the adapter's carries either.

## Grants

What a product's workspaces are granted (`server/grants`, docs/design.md §13,
#7): a key, a value and a window, which the product keeps a copy of with
payday's `entitle.Follower` and checks in the transaction of the write each
decides (payday#185). levyan is the source; the contract is payday's
`payday.GrantService`, served in `server/api/grants.go`.

- **State, never a delta.** Every change of a grant is all of it again at a
  greater `version`, which the layer (`grants.Layer`, on both stacks) says --
  1 on Add, the row's plus one on Patch -- and no request does. What changes
  is its end and whether it renews, nothing else: another value is another
  grant. A grant is taken away by ending it (`valid_until` now), never by
  deleting it; one over for `grants.Listed` is soft-erased out of every list.
- **Every write of a grant holds its row first** (`FOR UPDATE` on PostgreSQL),
  and a grant given holds its workspace's (`ledger.Lock`): the layer reads the
  row it patches, and two changes read at once would be one version. Write a
  grant through `grants.Grants` (Give, End, Renew, Retire, Follow), never a
  Patch of your own.
- **Every grant ends.** One with no end of its own renews: `grants.horizon`
  (a week) from its last renewal, renewed once less than half is left. One
  given with `--until` ends then.
- **Positive grants only.** The copy reads a missing grant as not entitled, so
  a restriction is a grant that restricts when present (`loft.restricted`
  ON), and an app's core writes are never held to a grant's presence.
- **Its changes are always in the outbox**, whatever `watch.outbox` says of
  the rest (`cmd.Build`'s second resource recorder), and the drain
  (`payday.outbox`) publishes them; `Watch` subscribes before `ready`, reads
  each changed grant through the walled stack, and is cut off
  (ResourceExhausted) when the broker cuts its subscription for falling
  behind. So `watch.broker: none` is refused, and two processes need the
  `postgres` broker.
- **Sources.** An operator's (`levyan-admin grant add|end|ls`: operator,
  contract or trial, one giver -- a second grant of a key is refused unless
  `--replace`) and a rule's (`grants.Rule`, the hook billing state plugs
  into: `cmd.Server.Grants.Rules`). A rule's grants are its own by its name:
  kept while wanted, given again when the value changes, ended when no longer
  wanted, and never an operator's to end.

## Payers

Who pays for products' workspaces (`server/payers`, docs/design.md §14, #4).
A payer is an account of roster's, paying with a `BillingAccount` (global,
the account's own); a `Link` says who pays for a workspace from when to when.

- **One payer a period.** A workspace's first payer is its creator (product's
  `WorkspaceService/Arrive`). Every other change -- an offer accepted, `Use`,
  `Stop` -- takes effect at the end of the period it is made in; only `Leave`
  and roster's word end a link before then. So a period has one payer, and
  is billed on its billing account's channel and currency
  (`billing.Rater.PayerOf`, `TermsFor`).
- **The free count is per product and per period**: of the workspaces a payer
  paid for over the period, the first `payers.free` by when the payer first
  linked each have their allowances; the rest are rated from the first unit
  (`Terms.FirstUnit`, kept on the statement as `first_unit`). A workspace with
  no payer keeps its allowances.
- **An account's call** is a product's key plus `levyan-account`; it is held
  to roster's word now (`payers.Standing`, roster's `standing.Accounts.Held`)
  and refused while no roster is configured -- never served on nobody's
  word. Its methods are the API's `PayerService`, each served as the schema's
  declaration of the same name (`proto/levyan/payer.proto`, `batch.AsOp`),
  which is what payday holds an account's call to; a test keeps the two
  lists one. A new payer method is a method in both, and a call of
  `pd.AccountReads` naming the schema's constant in its handler.
- **What an account writes**: its billing accounts as itself (the trail names
  it); in a product's tenant -- a link, an offer's answer -- as the
  deployment within that product, through the walled stack, after reading
  with ent that the account may.
- **roster's word** (`levyan.roster-stream`, in every process): erased or
  suspended ends the account's links at the period's end and is a mark
  (`Departure`); good again takes the mark back and resumes. Personal data is
  replaced only once roster, asked again, has nobody by the account
  (`levyan.payers.settle`, roster's rule that a mark is acted on only after
  asking again); `levyan.payers.reconcile` finds what the stream missed.
- **A workspace with no payer** is restricted after `payers.grace` by the
  grants' rule `payers.grace` (`payers.Payerless`): `<product>.restricted` ON.
- **What a billing account is billed through** -- channel and currency -- is
  the deployment's map from its country (`payers.channels`, `payers.Channels`,
  each key over the defaults: the merchant of record, KRW in KR and USD
  elsewhere). It is asked once, as the account is made (`payers.Layer`), and
  kept on the row: a map changed later moves no account, and moving one is a
  channel switch at a period's end, not built yet. A product's prices must
  be on every channel and currency the map bills in, or what is billed there
  comes to nothing.

## Invoices

What billing accounts are asked to pay, and the money it moves
(`server/invoices`, docs/design.md §15, #9).

- **One invoice of a billing account a period, in a product.** Every
  statement with a billing account is a line (`InvoiceLine`, unique by its
  statement) of exactly one invoice of that billing account -- the invariant
  reconciliation (#13) relies on; a statement with none is on no invoice.
  `Invoices.Account` gathers, holding the billing account's row: lines onto
  the (product, billing account)'s one draft, in period order, and a draft
  decided once its period is closed -- finalized, kept for the next period
  when its due is more than nothing and less than its channel's `Minimum`
  (the owner's #25), or written off when no link of its billing account goes
  on past its end (Claude's proposal, unconfirmed: say so wherever it is
  written). A later period's statement waits until its draft is decided.
- **Finalized is final.** The layer (`invoices.Layer`, both stacks) refuses
  any change to a finalized invoice's amounts, number, bill-to or due date;
  what moves is its settlement (state along `next`, `paid`, `reduced`,
  `date_settled`) and the channel's references. A correction is a credit
  note, or a late row's correction line on a later statement -- never the row.
- **Amounts.** `subtotal` is its lines' totals; `tax` only on a seller's
  channel (`invoices.Seller`: every channel but the MoR), `invoices.tax` of
  the subtotal toward nothing; `total`; `credited`, what the credit balance
  paid, taken off the total before the channel sees it; `due`. Remaining is
  `due - paid - reduced`. What is owed back is never paid out: it is a
  `Credit` row (the balance is their sum, per product and billing account),
  and money goes back only through a credit note against a paid invoice.
- **A seller's number** is the deployment's one series (invoices', and credit
  notes' apart), the greatest plus one, given in the finalizing transaction
  under a PostgreSQL advisory lock; its legal document is asked for after the
  commit, by that number, which keeps it once at the issuer.
- **Attempts** (`PaymentAttempt`) are the payment ledger, charges and refunds
  alike, each with its own key (`invoices.KeyOf`). **A send is written down
  and committed before it is made** -- sends counted, stamped, state UNKNOWN
  -- and what came of an UNKNOWN one is found out by `Collector.Ask`, never by
  sending it blind; `NotFound` from an ask is REQUESTED again, a send of its
  own (`SendKey`). SUCCEEDED and FAILED are final. The loop makes an
  invoice's first attempt only: after a decline, the invoice stays open and
  past due for dunning (#11).
- **A channel is a `Collector`** in `cmd.Server.Invoices.Collectors`: its
  minimum, whether it pushes, its due date, its seller's documents, a send and
  an ask. A channel with none has its drafts gathered and never finalized --
  so nothing is asked of a channel nothing can ask. Tests use
  `invoicestest.Channel`. The adapters' errors are read as they are
  (`invoices.KindOf` knows `pg.Error` and `mor.Error`).
- **Credit notes and voids** are an operator's (`levyan-admin invoice
  credit|void`), refused while a charge is out. A credit note takes back of
  the subtotal with its tax, off what is due first, then gives back (a refund,
  as far as money paid) or credits; a void takes back what is left as a
  CANCELLATION credit note, and refuses an invoice paid in any part.
- **Kept five years**: invoices, lines, credit notes, attempts and credit have
  no Erase, and a finalized invoice keeps its bill-to as it was.
- **A payer reads** its billing accounts' finalized invoices of the relaying
  product (`PayerService` `ListInvoices`, `GetInvoice`, `ListCreditNotes`,
  each declaring what it reads with `pd.AccountReads`).
- A workspace's `label` is what the product calls it (`Arrive`'s `label`); a
  line keeps the label it was written with.
- **The merchant of record's channel** (`CHANNEL_MOR`,
  `server/invoices/merchant`, docs/design.md §15, "MoR 수금기") charges an
  invoice to the billing account's subscription now, under the attempt's
  key, with the send before as `mor.Charge.Sent`, and refunds a credit note
  from the charge's line. A billing account that is not collectable is sent
  nothing: the attempt fails as a decline of the kind closest to what its
  payer has to do, by a code naming its collection (`merchant.Failure`). An
  ask looks in **every** subscription the billing account has had, since a
  lost answer may have charged one canceled since; never only the live one.
- **Shadow drafts are never charged** (`Invoices.LiveFrom`,
  `invoices.mor.live_from`, Claude's answer to open question 63): a draft
  begun for a period that ended before collection on its channel began is
  closed as `INVOICE_STATE_SHADOW` -- nothing asked of the channel, no credit
  taken -- and a draft of shadow periods alone gathers no later statement. A
  line never moves, so a draft gathered across the date before it was set is
  a shadow whole, and what it held after is said in the log. `mor` with no
  `live_from` is refused at load and by `cmd.Build`.
- **What a channel says outside a send or an ask** (`Invoices.Heard`, a
  rechecked notice's transaction) is written as an ask's answer, and only
  when further along than the attempt: twice is once, older is nothing, and
  "never had it" is an ask's alone, after `mor.Settle`. A payment of a key the
  ledger does not account for -- twice, or after its attempt failed -- marks
  the attempt (`date_twice`) for reconciliation (#13): the one thing, with an
  empty channel number, the layer lets be written of an attempt that ended.
- **A payer reads an invoice's finalized states, by name** (`server/api`'s
  `finalized`): a state is shown to nobody until it is named there.
- **The bank-transfer channel** (`CHANNEL_BANK_KR`, `server/invoices/bank`)
  pushes nothing: an invoice is finalized with its due date and its tax
  invoice, and an operator records each transfer (`invoice paid`) and each
  refund made back by hand (`invoice credit --reason refund --reference`).
  Its billing accounts are an operator's alone (`billing-account add
  --channel bank_kr`): `payers.Layer` refuses one from a frame that is no
  operator's, and `payers.channels` refuses it as a country's default.
- **Tax invoices** go through `internal/taxinvoice` (`Issuer`, once by its
  key, which is the invoice's number), whose services register their kind;
  `invoices.bank_kr.issuer` names one. **None is linked**: which service is
  the owner's open question, so do not name one as chosen, and a deployment
  that configures the channel does not come up yet. `taxinvoicetest` (kind
  `test`) is the fake, and no binary links it (cmd/levyan's test).

## Background work

What `cmd.Build` appends to `s.Spin` runs under `spin.lease` in `levyan serve`:
every loop in **one** process of the deployment, but the ones marked
`spin.InEveryProcess` (payday's access writer). So a loop of levyan's has a name
of its own, `levyan.…` (`spin.Called`), and `spin.Run` refuses an unnamed one.
A lease is at most one at a time but for a moment around a failover, not exactly
once: a loop is still safe run twice. `levyan-admin serve` runs only its own
process's access writer.

levyan's own:

- `levyan.roster-stream` (roster's `standing.Accounts.Follower`, in every
  process), `levyan.payers.settle` and `levyan.payers.reconcile`: roster's
  word on the accounts that pay, when `auth.roster` names a roster.
- `levyan.grants.renew` (`grants.Renewals`) gives what the rules want and ends
  what they no longer want, renews the grants that are due, and erases those
  over for `grants.Listed`, through `Ungated` as the deployment.
- `levyan.periods.close` (`billing.ClosePeriods`) closes every period that has
  ended and waited out `periods.grace`, through `Ungated` as the deployment;
  an operator's `period close` is the same through their frame.
- `levyan.readings.gaps` (`ledger.LookForGaps`) warns of workspaces that have
  stopped sending a stock meter's readings, in the log and in the gauge
  `levyan.readings.gaps`.
- `levyan.invoices.collect` (`invoices.Collects`, every `invoices.every`, ten
  minutes, which is `mor.Settle`) invoices what the close wrote, decides
  drafts, issues a seller's documents and sends and asks about attempts,
  through `Ungated` as the deployment.
- `levyan.mor.inbox` (`subscriptions.Takes`, `cmd.Server.Taker`) takes the
  MoR's notices -- the mirror, and what each transaction says of the
  attempts whose keys it carries -- every five seconds, and at once when
  this process keeps a webhook; and
  `levyan.mor.events` (`subscriptions.Pulls`, `mor.pull`) reads the MoR's
  list of its events into the inbox. Neither runs when `mor` names no MoR.

A pass of any of them that fails is logged and the next tries again. And
payday's `payday.outbox` always runs: it is what streams a grant's change.

## Configuration and secrets

`cmd.Config` is payday's blocks plus `admin`. A file, then `LEVYAN_*` over it;
`levyan config env` lists every variable. A secret is a `cfg.Secret` --
`${file:/run/…}` or `${env:…}`, never the value in a file somebody reads --
and is printed by `config` as its reference. Never log a secret: not a service
key, not an operator key (the admin log says the operator's name), not a payment
key (`mor.api_key`, `mor.webhook_secret`).

A block is validated as it is loaded, and what is wrong of a part is said
where it is written: give the part a type of its own with a `Validate`
(`ChannelsConfig`, `MorKind`, `MorPlans`), since `cfg` places an error by its
leaf. What is missing, or what the parts say of one another, the block's own
`Validate` says -- and what one block says against another, the root's
(`Config.Validate`: `mor` with no `invoices.mor.live_from`), which `cmd.Build`
asks again of a configuration made in code. With no `mor` block there is no
MoR, and nothing else changes.

## Running

```sh
go run ./cmd/levyan migrate
LEVYAN_ADMIN_KEYS="dev=\${file:$HOME/.config/levyan-dev.key}" go run ./cmd/levyan-admin serve
go run ./cmd/levyan serve
```

`README.md` has the operator's commands. Try a binary from a scratch
directory, with a configuration that names its own database.

## Testing

```sh
go test ./...                                    # SQLite, a database per test
PDTEST_POSTGRES='postgres://…' go test ./...     # PostgreSQL, a schema per test
```

`internal/levyantest` is a levyan built as `levyan serve` builds one and
migrated: arrange through `Ungated`, as an operator would, and ask through
`World.Serve()` as a product does, with `levyantest.As(ctx, key)`. Money and the
versions in force are `internal/pricing`'s own tests, which need no database.

## Logging an error

```go
log.From(ctx).ErrorContext(ctx, "what failed", slog.Any("err", err))
```

The error itself, never `err.Error()`, and one to a record (payday's
`docs/guide/server.md`, "Logging an error").

## Reference

- `README.md` — running it, in Korean
- `docs/design.md` — the design, in Korean; levyan#1 and its sub-issues
- <https://github.com/lesomnus/payday/tree/main/docs> — the guides and the
  references behind them
- lesomnus/loft and lesomnus/roster — the two other payday apps; loft is the
  first product levyan bills
