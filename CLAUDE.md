# Working on levyan

A [payday](https://github.com/lesomnus/payday) app that bills other payday
apps' tenants for what they hold and use. Most of it is **generated from
`proto/`**, so the usual shape of a change is: edit the schema, regenerate,
then write the part no schema can state.

`README.md` is how to run it (in Korean), `docs/design.md` why it is shaped
this way (in Korean, condensed from levyan#1, which says whose each decision
is). Code comments, commits and this file are English; issues, PRs and the
docs are Korean.

## Regenerate after touching the schema

```sh
scripts/gen.sh            # go tool pd gen ., and then scripts/api.sh
go tool pd gen --check .  # what CI runs: fails if anything moved
go tool pd doctor .       # what would go wrong, before it does
```

A generated file that was not regenerated **compiles perfectly and is wrong**.
If you edited anything under `proto/`, you are not done until `pd gen --check`
exits 0. `pd gen` fetches the buf it pins; there is no page and no `--ts`.

**`pd gen` deletes the API's generated Go** (`api/levyan/v1/*.pb.go`), and so
does `pd gen --check`: payday sweeps away generated Go it did not write
anywhere in the tree, and does not stop at the api module's go.mod. So the API
is generated after it, always -- `scripts/gen.sh` does both, and CI runs
`scripts/api.sh --check` after `pd gen --check`. A build after `pd gen` alone
fails to compile `server/api`, which is how it is noticed; the fix belongs in
payday's sweep.

## The API apps call (`api/`)

`levyan.v1` is what a product calls, and it is **written by hand**: it is not
generated from `proto/`, names nothing of payday's, and lives in a Go module of
its own (`github.com/lesomnus/levyan/api`) that requires gRPC and protobuf and
nothing else. That is so that an app calling levyan is never held to the payday
levyan is built with -- the way loft is held to roster's (docs/design.md §7).
The root module uses it through `replace ./api`.

```sh
scripts/api.sh            # after editing api/levyan/v1/*.proto, and after every pd gen
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

- **product** — a `ServiceKey` frame, which sees its own tenant and nothing else;
- **payer** — not yet: an account, in no tenant, once payday says how an account
  calls (payday#184);
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

## Background work

What `cmd.Build` appends to `s.Spin` runs under `spin.lease` in `levyan serve`:
every loop in **one** process of the deployment, but the ones marked
`spin.InEveryProcess` (payday's access writer). So a loop of levyan's has a name
of its own, `levyan.…` (`spin.Called`), and `spin.Run` refuses an unnamed one.
A lease is at most one at a time but for a moment around a failover, not exactly
once: a loop is still safe run twice. `levyan-admin serve` runs only its own
process's access writer.

## Configuration and secrets

`cmd.Config` is payday's blocks plus `admin`. A file, then `LEVYAN_*` over it;
`levyan config env` lists every variable. A secret is a `cfg.Secret` --
`${file:/run/…}` or `${env:…}`, never the value in a file somebody reads --
and is printed by `config` as its reference. Never log a secret: not a service
key, not an operator key (the admin log says the operator's name), not a payment
key when those come.

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
