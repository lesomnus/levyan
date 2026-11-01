// Package payers is who pays for products' workspaces (#4): the billing
// accounts roster's accounts pay with, the links that say which account pays
// for which workspace from when to when, the offers that make somebody a
// payer, and what levyan does when roster says an account is gone.
//
// # One payer at a time, changed at a period's end
//
// A workspace's first payer is its creator, from when the product says it was
// made ([Payers.Arrive]). Every change after that -- another account
// accepting an offer, the payer paying with another billing account, the
// payer stopping -- takes effect at the end of the period it is made in, so
// that a period has one payer at most and is billed on one channel. Only an
// account leaving the workspace ([Payers.Leave]) ends its link at once.
//
// # No payer
//
// A workspace whose links have all ended, and that nobody will pay for from
// the next period either, has no payer. Its owners are told so by the product
// (`levyan.v1.WorkspaceService/GetPayer`), and once that has lasted the grace
// of the policy table's row of it (`dunning.kinds.payerless`, #11) -- or at
// once, as the row says -- the workspace is restricted: [Payerless] is the
// rule that gives it `<product>.restricted` ON, through the grants' rules
// hook.
//
// # Written through the servers
//
// Every write goes through the stack it is handed: the walled one, as the
// product, for what a product says; the walled one again, as the deployment
// within the product's tenant, for what an account's own call makes there
// (payday's accounts guide); the deployment's own, for what roster's word
// does. What decides a write is read with ent on the transaction it is
// written in, of the product the call names and no other, and the
// workspace's row is held first (`ledger.Lock`), as a period's close holds it
// -- so a close never reads a payer half changed.
//
// # A workspace's life
//
// What a product says became of a workspace -- closed, restored, destroyed
// ([Payers.Close], [Payers.Restore], [Payers.Destroy]) -- is when it is
// billed and when not (#14). A destroyed workspace's links end then, and
// nothing begins to be paid for, or offered, of it after.
package payers

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"github.com/protobuf-orm/ent/dialect"
	"github.com/protobuf-orm/protoc-gen-orm-ent/runtime/enttx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/lesomnus/payday/config"
	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/dunning"
	"github.com/lesomnus/levyan/internal/ent"
	entlink "github.com/lesomnus/levyan/internal/ent/link"
	entoffer "github.com/lesomnus/levyan/internal/ent/offer"
	entworkspace "github.com/lesomnus/levyan/internal/ent/workspace"
	"github.com/lesomnus/levyan/server/billing"
	"github.com/lesomnus/levyan/server/ledger"
	"github.com/lesomnus/levyan/server/pd"
)

// Payers keeps who pays for products' workspaces, through S.
type Payers struct {
	// S is the stack a row is written through, as whoever the context is
	// framed as.
	S levyan.Server

	// Drv is what a transaction is begun on, and Dialect what it speaks.
	Drv     dialect.Driver
	Dialect string

	// Now is the deployment's clock.
	Now func() time.Time

	// Payerless is the policy table's row of a workspace with no payer
	// (`dunning.kinds.payerless`, #11): how long it may go with none before
	// it is restricted -- thirty days by default, as `payers.grace` was
	// (Claude's, #4; the owner's default since 2026-11-01) -- or that it is
	// restricted at once, and how long before it is read-only: sixty days by
	// default.
	Payerless dunning.Row
}

func (p Payers) now() time.Time {
	if p.Now == nil {
		return ledger.Moment(time.Now())
	}

	return ledger.Moment(p.Now())
}

// On is these payers writing through `s`: the same, with another stack.
func (p Payers) On(s levyan.Server) Payers {
	p.S = s
	return p
}

func (p Payers) postgres() bool { return p.Dialect == config.DialectPostgres }

func (p Payers) db() *ent.Client { return ent.NewClient(ent.Driver(p.Drv)) }

// tx runs `f` in one transaction: the stack and an ent client on it.
func (p Payers) tx(ctx context.Context, f func(s levyan.Server, db *ent.Client) error) error {
	tdrv, tx, err := dialect.BeginTx(ctx, p.Drv)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	s, err := enttx.Rebind(p.S, tdrv)
	if err != nil {
		return err
	}
	if err := f(s, ent.NewClient(ent.Driver(tdrv))); err != nil {
		return err
	}

	return tx.Commit()
}

// held is `f` in a transaction that holds workspace `ws`'s row first.
func (p Payers) held(ctx context.Context, ws uuid.UUID, f func(s levyan.Server, db *ent.Client) error) error {
	return p.tx(ctx, func(s levyan.Server, db *ent.Client) error {
		if err := ledger.Lock(ctx, db, p.postgres(), ws); err != nil {
			return err
		}

		return f(s, db)
	})
}

// Boundary is when the period of `product` that `now` is in ends: where every
// change of who pays takes effect.
func Boundary(ctx context.Context, db *ent.Client, product uuid.UUID, now time.Time) (time.Time, error) {
	t, err := db.Tenant.Get(ctx, product)
	if err != nil {
		return time.Time{}, err
	}
	terms, err := billing.TermsOf(t)
	if err != nil {
		return time.Time{}, err
	}
	cur, err := billing.Current(ctx, db, product, terms, now)
	if err != nil {
		return time.Time{}, err
	}

	return cur.End, nil
}

// workspace is the row of `key`, a workspace of `product`, written through
// the stack if levyan has heard nothing of it yet.
func (p Payers) workspace(ctx context.Context, product, key pdid.Id) (uuid.UUID, error) {
	if key.IsZero() || key.Domain() != pd.TenantDomain {
		return uuid.UUID{}, status.Errorf(codes.InvalidArgument, "workspace: %s is not the identifier of a payday tenant", key)
	}

	id, ok, err := Workspace(ctx, p.db(), product.Uuid(), key)
	if err != nil || ok {
		return id, err
	}
	_, err = p.S.Workspace().Add(ctx, levyan.WorkspaceAddRequest_builder{
		Tenant: levyan.TenantRef_builder{Id: product.Bytes()}.Build(),
		Key:    key.Bytes(),
	}.Build())
	if err != nil && status.Code(err) != codes.AlreadyExists {
		return uuid.UUID{}, err
	}

	id, _, err = Workspace(ctx, p.db(), product.Uuid(), key)
	return id, err
}

// Label says what `product` calls its workspace `key`, for a bill to name
// it by (#9): written through the stack when it is not what the row says
// already. A workspace levyan has heard nothing of is written first, as one
// that arrives is.
func (p Payers) Label(ctx context.Context, product, key pdid.Id, label string) error {
	label = strings.TrimSpace(label)
	if label == "" {
		return nil
	}
	if n := utf8.RuneCountInString(label); n > MaxLabel {
		return status.Errorf(codes.InvalidArgument, "label: %d characters, and a workspace's label is at most %d", n, MaxLabel)
	}
	ws, err := p.workspace(ctx, product, key)
	if err != nil {
		return err
	}
	w, err := p.db().Workspace.Get(ctx, ws)
	if err != nil || w.Label == label {
		return err
	}

	_, err = p.S.Workspace().Patch(ctx, levyan.WorkspacePatchRequest_builder{
		Ref:   levyan.WorkspaceRef_builder{Id: ws[:]}.Build(),
		Label: proto.String(label),
	}.Build())
	return err
}

// MaxLabel is the most characters a workspace's label has: what an invoice's
// line names it by.
const MaxLabel = 100

// WorkspaceOf is the workspace whose row is `id`.
func (p Payers) WorkspaceOf(ctx context.Context, id pdid.Id) (*ent.Workspace, error) {
	return p.db().Workspace.Get(ctx, id.Uuid())
}

// Workspace is the row of `key`, a workspace of `product`, and false when
// levyan has heard nothing of it.
func Workspace(ctx context.Context, db *ent.Client, product uuid.UUID, key pdid.Id) (uuid.UUID, bool, error) {
	v, err := db.Workspace.Query().Where(entworkspace.TenantId(product), entworkspace.Key(key.Bytes())).Only(ctx)
	if ent.IsNotFound(err) {
		return uuid.UUID{}, false, nil
	}
	if err != nil {
		return uuid.UUID{}, false, err
	}

	return v.Id, true, nil
}

// State is who pays for a workspace.
type State struct {
	// Workspace is the workspace as its product names it.
	Workspace pdid.Id

	// Current is the link in force now, and Next the one in force from the
	// end of the period when it is another: nil for none.
	Current *ent.Link
	Next    *ent.Link

	// Offer is what is offered and not answered.
	Offer *ent.Offer

	// Since is when the workspace last had a payer, when it has none now and
	// none from the end of the period; zero otherwise, and for a workspace
	// that never had one. Restricted is when it is restricted for that, and
	// Readonly when it is read-only for it, the second stage.
	Since      time.Time
	Restricted time.Time
	Readonly   time.Time
}

// Payerless is whether the workspace has no payer, now or next.
func (s State) Payerless() bool { return !s.Since.IsZero() }

// State is who pays for workspace `key` of `product`.
func (p Payers) State(ctx context.Context, product, key pdid.Id) (State, error) {
	out := State{Workspace: key}
	ws, ok, err := Workspace(ctx, p.db(), product.Uuid(), key)
	if err != nil || !ok {
		return out, err
	}

	return p.state(ctx, p.db(), product.Uuid(), key, ws)
}

func (p Payers) state(ctx context.Context, db *ent.Client, product uuid.UUID, key pdid.Id, ws uuid.UUID) (State, error) {
	out := State{Workspace: key}
	now := p.now()
	b, err := Boundary(ctx, db, product, now)
	if err != nil {
		return out, err
	}

	ls, err := links(ctx, db, product, ws)
	if err != nil {
		return out, err
	}
	out.Current = at(ls, now)
	if n := at(ls, b); n != nil && (out.Current == nil || n.Id != out.Current.Id) {
		out.Next = n
	}
	if out.Current == nil && out.Next == nil {
		// Nobody pays for it: since when, and so from when it is restricted
		// -- but for a workspace destroyed, which is nobody's to pay for and
		// nothing to restrict ([Payerless]).
		d, err := ledger.Destroyed(ctx, db, product, ws)
		if err != nil {
			return out, err
		}
		if _, ok := d[ws]; !ok {
			for _, l := range ls {
				if l.DateEnd != nil && l.DateEnd.After(out.Since) {
					out.Since = ledger.Moment(*l.DateEnd)
				}
			}
		}
		if !out.Since.IsZero() {
			out.Restricted = p.Payerless.Restricts(out.Since)
			out.Readonly = p.Payerless.Readonly(out.Since)
		}
	}

	o, err := db.Offer.Query().
		Where(entoffer.TenantId(product), entoffer.WorkspaceId(ws), entoffer.Answer(int32(levyan.OfferAnswer_OFFER_ANSWER_UNSPECIFIED))).
		Order(ent.Desc(entoffer.FieldDateCreated)).
		First(ctx)
	switch {
	case err == nil:
		out.Offer = o
	case !ent.IsNotFound(err):
		return out, err
	}

	return out, nil
}

// links is every link of workspace `ws` that was ever in force, the earliest
// first.
func links(ctx context.Context, db *ent.Client, product, ws uuid.UUID) ([]*ent.Link, error) {
	vs, err := db.Link.Query().
		Where(entlink.TenantId(product), entlink.WorkspaceId(ws)).
		Order(ent.Asc(entlink.FieldDateStart), ent.Asc(entlink.FieldId)).
		All(ctx)
	if err != nil {
		return nil, err
	}

	out := vs[:0]
	for _, v := range vs {
		if !empty(v) {
			out = append(out, v)
		}
	}

	return out, nil
}

// at is the link of `ls` in force at `t`, the latest begun of them, or nil.
func at(ls []*ent.Link, t time.Time) *ent.Link {
	var out *ent.Link
	for _, l := range ls {
		if l.DateStart.After(t) || (l.DateEnd != nil && !l.DateEnd.After(t)) {
			continue
		}
		if out == nil || !l.DateStart.Before(out.DateStart) {
			out = l
		}
	}

	return out
}

// empty is a link taken back before it began.
func empty(l *ent.Link) bool { return l.DateEnd != nil && !l.DateEnd.After(l.DateStart) }

// end ends link `l` at `at` for `why`, through `s`: nothing if it ends by
// then already. One that would only have begun at `at` or after is taken back
// before it begins.
func end(ctx context.Context, s levyan.Server, l *ent.Link, at time.Time, why levyan.LinkEnded) (*levyan.Link, error) {
	if l.DateEnd != nil && !l.DateEnd.After(at) {
		return nil, nil
	}
	if l.DateStart.After(at) {
		at = l.DateStart
	}

	return s.Link().Patch(ctx, levyan.LinkPatchRequest_builder{
		Ref:              levyan.LinkRef_builder{Id: l.Id[:]}.Build(),
		DateEnd:          tsOf(at),
		Ended:            why.Enum(),
		DateUpdatedForce: boolOf(true),
	}.Build())
}

// add is a link of `account` paying for workspace `ws` of `product` from
// `from`, written through `s`.
func add(ctx context.Context, s levyan.Server, product, ws uuid.UUID, account pdid.Id, billing uuid.UUID, from time.Time, began levyan.LinkBegan, offer uuid.UUID) (*levyan.Link, error) {
	req := levyan.LinkAddRequest_builder{
		Tenant:    levyan.TenantRef_builder{Id: product[:]}.Build(),
		Workspace: levyan.WorkspaceRef_builder{Id: ws[:]}.Build(),
		Payer:     account.Bytes(),
		DateStart: tsOf(from),
		Began:     began,
	}.Build()
	if billing != (uuid.UUID{}) {
		req.SetBillingAccount(levyan.BillingAccountRef_builder{Id: billing[:]}.Build())
	}
	if offer != (uuid.UUID{}) {
		req.SetOffer(levyan.OfferRef_builder{Id: offer[:]}.Build())
	}

	return s.Link().Add(ctx, req)
}

// Arrive is what a product says of a workspace it made: its creator pays for
// it from when it was made -- `created`, or now when zero -- with the first
// billing account they made, if any. A workspace levyan has a payer of
// already, now or ever, is answered as it is.
func (p Payers) Arrive(ctx context.Context, product, key, creator pdid.Id, created time.Time) (State, error) {
	if creator.IsZero() {
		return State{}, status.Error(codes.InvalidArgument, "creator: the account that made the workspace, at roster")
	}
	now := p.now()
	if created.IsZero() {
		created = now
	}
	created = ledger.Moment(created)
	if created.After(now.Add(ledger.Skew)) {
		return State{}, status.Errorf(codes.InvalidArgument, "created: %s is ahead of now", created.Format(time.RFC3339))
	}

	ws, err := p.workspace(ctx, product, key)
	if err != nil {
		return State{}, err
	}

	var out State
	err = p.held(ctx, ws, func(s levyan.Server, db *ent.Client) error {
		had, err := db.Link.Query().Where(entlink.TenantId(product.Uuid()), entlink.WorkspaceId(ws)).Exist(ctx)
		if err != nil {
			return err
		}
		if !had {
			if err := gone(ctx, db, product.Uuid(), ws, key); err != nil {
				return err
			}
			ba, err := First(ctx, db, creator)
			if err != nil {
				return err
			}
			if _, err := add(ctx, s, product.Uuid(), ws, creator, ba, created, levyan.LinkBegan_LINK_BEGAN_ARRIVED, uuid.UUID{}); err != nil {
				return err
			}
		}

		out, err = p.state(ctx, db, product.Uuid(), key, ws)
		return err
	})

	return out, err
}

// Leave is what a product says of an account no longer in a workspace: if it
// pays for it, it pays no longer, from now; if it was to from the end of the
// period, it is not; and what was offered to it is withdrawn.
func (p Payers) Leave(ctx context.Context, product, key, account pdid.Id) (State, error) {
	if account.IsZero() {
		return State{}, status.Error(codes.InvalidArgument, "account: the account that left, at roster")
	}
	ws, ok, err := Workspace(ctx, p.db(), product.Uuid(), key)
	if err != nil {
		return State{}, err
	}
	if !ok {
		return State{Workspace: key}, nil
	}

	var out State
	err = p.held(ctx, ws, func(s levyan.Server, db *ent.Client) error {
		now := p.now()
		ls, err := links(ctx, db, product.Uuid(), ws)
		if err != nil {
			return err
		}
		for _, l := range ls {
			if l.Payer != account.Uuid() {
				continue
			}
			if _, err := end(ctx, s, l, now, levyan.LinkEnded_LINK_ENDED_LEFT); err != nil {
				return err
			}
		}
		if err := withdraw(ctx, s, db, product.Uuid(), ws, account, now); err != nil {
			return err
		}

		out, err = p.state(ctx, db, product.Uuid(), key, ws)
		return err
	})

	return out, err
}

// Offer is a product asking `account` to pay for workspace `key`, which it
// answers as itself: what was offered before is withdrawn. An account that
// pays for it already, or will from the end of the period, is not asked.
func (p Payers) Offer(ctx context.Context, product, key, account pdid.Id) (*levyan.Offer, error) {
	if account.IsZero() {
		return nil, status.Error(codes.InvalidArgument, "account: the account asked to pay, at roster")
	}
	ws, err := p.workspace(ctx, product, key)
	if err != nil {
		return nil, err
	}

	var out *levyan.Offer
	err = p.held(ctx, ws, func(s levyan.Server, db *ent.Client) error {
		if err := gone(ctx, db, product.Uuid(), ws, key); err != nil {
			return err
		}
		st, err := p.state(ctx, db, product.Uuid(), key, ws)
		if err != nil {
			return err
		}
		for _, l := range []*ent.Link{st.Current, st.Next} {
			if l != nil && l.Payer == account.Uuid() {
				return status.Error(codes.FailedPrecondition, "the account pays for the workspace already")
			}
		}
		if err := withdraw(ctx, s, db, product.Uuid(), ws, pdid.Nil, p.now()); err != nil {
			return err
		}

		out, err = s.Offer().Add(ctx, levyan.OfferAddRequest_builder{
			Tenant:    levyan.TenantRef_builder{Id: product.Bytes()}.Build(),
			Workspace: levyan.WorkspaceRef_builder{Id: ws[:]}.Build(),
			Account:   account.Bytes(),
		}.Build())
		return err
	})

	return out, err
}

// withdraw withdraws what is offered of workspace `ws` and not answered: to
// `account`, or to anybody when it is Nil.
func withdraw(ctx context.Context, s levyan.Server, db *ent.Client, product, ws uuid.UUID, account pdid.Id, now time.Time) error {
	q := db.Offer.Query().Where(entoffer.TenantId(product), entoffer.WorkspaceId(ws), entoffer.Answer(int32(levyan.OfferAnswer_OFFER_ANSWER_UNSPECIFIED)))
	if !account.IsZero() {
		q.Where(entoffer.Account(account.Uuid()))
	}
	vs, err := q.All(ctx)
	if err != nil {
		return err
	}
	for _, v := range vs {
		if err := answer(ctx, s, v.Id, levyan.OfferAnswer_OFFER_ANSWER_WITHDRAWN, now); err != nil {
			return err
		}
	}

	return nil
}

func answer(ctx context.Context, s levyan.Server, offer uuid.UUID, a levyan.OfferAnswer, now time.Time) error {
	_, err := s.Offer().Patch(ctx, levyan.OfferPatchRequest_builder{
		Ref:              levyan.OfferRef_builder{Id: offer[:]}.Build(),
		Answer:           a.Enum(),
		DateAnswered:     tsOf(now),
		DateUpdatedForce: boolOf(true),
	}.Build())
	if err != nil {
		return fmt.Errorf("offer %s: %w", pdid.Id(offer), err)
	}

	return nil
}
