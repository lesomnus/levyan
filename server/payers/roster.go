package payers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"github.com/lesomnus/otx/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/spin"
	"github.com/lesomnus/roster/standing"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entbillingaccount "github.com/lesomnus/levyan/internal/ent/billingaccount"
	entdeparture "github.com/lesomnus/levyan/internal/ent/departure"
	entlink "github.com/lesomnus/levyan/internal/ent/link"
	entoffer "github.com/lesomnus/levyan/internal/ent/offer"
	entsubscription "github.com/lesomnus/levyan/internal/ent/subscription"
	"github.com/lesomnus/levyan/server/ledger"
)

// Standing is what roster says of an account, asked as a call comes: whether
// a call made as it now is served. roster's `standing.Accounts` is one --
// what roster says, believed for `standing.HeldTtl` -- and a test's is
// another.
type Standing interface {
	Held(ctx context.Context, account pdid.Id, began time.Time) error
}

// Roster is what levyan asks roster again, and of everybody now and then:
// roster's `standing.Accounts`, or a test's.
type Roster interface {
	Settle(ctx context.Context, marked []pdid.Id, gone func(ctx context.Context, account pdid.Id) error, back func(ctx context.Context, v standing.Standing) error) error
	Reconcile(ctx context.Context, accounts []pdid.Id, marked func(pdid.Id) bool) (*standing.Reconciled, error)
}

var (
	_ Standing = (*standing.Accounts)(nil)
	_ Roster   = (*standing.Accounts)(nil)
)

// The loops of roster's word: its stream, in every process (it drops what
// each process knows), and the marks settled, and everybody asked about, in
// one.
const (
	StreamLoop    = "levyan.roster-stream"
	SettleLoop    = "levyan.payers.settle"
	ReconcileLoop = "levyan.payers.reconcile"
)

// Heard is what roster's stream says of an account, which levyan writes
// down (Claude's, #4):
//
//   - **Erased or suspended** -- or its directory closed, which is suspended
//     here -- is a mark ([levyan.Departure]): each link the account pays by
//     ends at the end of its period, so that what the account was paying
//     for has no payer from then, and what was offered to it is withdrawn.
//     Nothing is destroyed yet: an erasure is acted on only once roster is
//     asked again and has nobody by them ([Payers.Settle]).
//   - **In good standing** takes a mark that stands back: the account goes on
//     paying as it did ([Payers.Back]).
//
// It is run in every process, as the stream is, so it is safe to do twice:
// what it writes is state.
func (p Payers) Heard(ctx context.Context, st standing.Standing) error {
	switch {
	case !st.Erased.IsZero():
		return p.Depart(ctx, st.Holder, levyan.DepartureReason_DEPARTURE_REASON_ERASED)
	case !st.Disabled.IsZero(), !st.Closed.IsZero():
		return p.Depart(ctx, st.Holder, levyan.DepartureReason_DEPARTURE_REASON_SUSPENDED)
	}

	return p.Back(ctx, st)
}

// Depart marks `account` as roster says it is gone, and ends every link it
// pays by at the end of its period.
func (p Payers) Depart(ctx context.Context, account pdid.Id, why levyan.DepartureReason) error {
	now := p.now()
	dep, err := p.db().Departure.Query().Where(entdeparture.Account(account.Uuid())).Only(ctx)
	switch {
	case ent.IsNotFound(err):
		_, err = p.S.Departure().Add(ctx, levyan.DepartureAddRequest_builder{
			Account:   account.Bytes(),
			Reason:    why,
			DateHeard: tsOf(now),
		}.Build())
		if status.Code(err) == codes.AlreadyExists {
			err = nil
		}
	case err != nil:
	case dep.DateSettled != nil:
		// Settled: nothing is left of them here to end.
		return nil
	case dep.DateBack != nil || dep.Reason != int32(why):
		_, err = p.S.Departure().Patch(ctx, levyan.DeparturePatchRequest_builder{
			Ref:              levyan.DepartureRef_builder{Id: dep.Id[:]}.Build(),
			Reason:           why.Enum(),
			DateHeard:        tsOf(now),
			DateBackNull:     proto.Bool(true),
			DateUpdatedForce: proto.Bool(true),
		}.Build())
	}
	if err != nil {
		return fmt.Errorf("departure: %w", err)
	}

	ended := levyan.LinkEnded_LINK_ENDED_SUSPENDED
	if why == levyan.DepartureReason_DEPARTURE_REASON_ERASED {
		ended = levyan.LinkEnded_LINK_ENDED_ERASED
	}

	ls, err := p.db().Link.Query().
		Where(entlink.Payer(account.Uuid()), entlink.Or(entlink.DateEndIsNil(), entlink.DateEndGT(now))).
		All(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, l := range ls {
		if empty(l) {
			continue
		}
		err := p.held(ctx, l.WorkspaceId, func(s levyan.Server, db *ent.Client) error {
			l, err := db.Link.Get(ctx, l.Id)
			if err != nil {
				return err
			}
			b, err := Boundary(ctx, db, l.TenantId, now)
			if err != nil {
				return err
			}
			_, err = end(ctx, s, l, b, ended)
			return err
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("link %s: %w", pdid.Id(l.Id), err))
		}
	}

	os, err := p.db().Offer.Query().
		Where(entoffer.Account(account.Uuid()), entoffer.Answer(int32(levyan.OfferAnswer_OFFER_ANSWER_UNSPECIFIED))).
		All(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, o := range os {
		if err := answer(ctx, p.S, o.Id, levyan.OfferAnswer_OFFER_ANSWER_WITHDRAWN, now); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// Back is roster having an account again that was marked: the mark is taken
// back, and each workspace it was paying for when it was marked -- that
// nobody else pays for since -- it pays for again, as it did, from where its
// link ended or the end of the period now open, whichever is later. An
// account whose mark was settled has nothing left here to come back to.
func (p Payers) Back(ctx context.Context, st standing.Standing) error {
	account := st.Holder
	dep, err := p.db().Departure.Query().Where(entdeparture.Account(account.Uuid())).Only(ctx)
	if ent.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if dep.DateBack != nil || dep.DateSettled != nil {
		return nil
	}

	now := p.now()
	if _, err := p.S.Departure().Patch(ctx, levyan.DeparturePatchRequest_builder{
		Ref:              levyan.DepartureRef_builder{Id: dep.Id[:]}.Build(),
		DateBack:         tsOf(now),
		DateUpdatedForce: proto.Bool(true),
	}.Build()); err != nil {
		return fmt.Errorf("departure: %w", err)
	}

	gone := []int32{int32(levyan.LinkEnded_LINK_ENDED_SUSPENDED), int32(levyan.LinkEnded_LINK_ENDED_ERASED)}
	ls, err := p.db().Link.Query().
		Where(entlink.Payer(account.Uuid()), entlink.EndedIn(gone...), entlink.DateEndGTE(dep.DateHeard)).
		All(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, l := range ls {
		if empty(l) {
			continue
		}
		err := p.held(ctx, l.WorkspaceId, func(s levyan.Server, db *ent.Client) error {
			// A workspace destroyed since is nobody's to pay for again.
			d, err := ledger.Destroyed(ctx, db, l.TenantId, l.WorkspaceId)
			if err != nil || len(d) > 0 {
				return err
			}
			b, err := Boundary(ctx, db, l.TenantId, now)
			if err != nil {
				return err
			}
			from := b
			if l.DateEnd.After(from) {
				from = *l.DateEnd
			}
			ws, err := links(ctx, db, l.TenantId, l.WorkspaceId)
			if err != nil {
				return err
			}
			if at(ws, from) != nil || later(ws, from) {
				// Somebody pays for it from then, or will.
				return nil
			}

			_, err = add(ctx, s, l.TenantId, l.WorkspaceId, account, l.BillingAccountId, from, levyan.LinkBegan_LINK_BEGAN_RESUMED, uuid.UUID{})
			return err
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("link %s: %w", pdid.Id(l.Id), err))
		}
	}

	return errors.Join(errs...)
}

// later says whether a link of `ls` begins after `t`.
func later(ls []*ent.Link, t time.Time) bool {
	for _, l := range ls {
		if l.DateStart.After(t) {
			return true
		}
	}

	return false
}

// Settle asks roster again about every account marked as erased and not
// settled: one it has nobody by any more is forgotten here ([Payers.Forget]),
// and one it has again is taken back ([Payers.Back]). What roster cannot
// answer for is asked again next time.
func (p Payers) Settle(ctx context.Context, r Roster) error {
	ds, err := p.db().Departure.Query().
		Where(
			entdeparture.Reason(int32(levyan.DepartureReason_DEPARTURE_REASON_ERASED)),
			entdeparture.DateSettledIsNil(),
			entdeparture.DateBackIsNil(),
		).
		All(ctx)
	if err != nil || len(ds) == 0 {
		return err
	}

	marked := make([]pdid.Id, 0, len(ds))
	for _, d := range ds {
		marked = append(marked, pdid.Id(d.Account))
	}

	return r.Settle(ctx, marked, p.Forget, p.Back)
}

// Forget is roster having nobody by an erased account any more: what levyan
// held that says who they were is replaced, and its mark is settled. The rows
// stay -- a billing account, with the business it billed, is the record tax
// law keeps of what it was billed; the links and statements name the account
// by an identifier that roster no longer answers to.
//
// A business's registration, name, kind and item are kept; who represents it
// and where its invoices went are not (implementation decision, and a
// question for legal review).
func (p Payers) Forget(ctx context.Context, account pdid.Id) error {
	now := p.now()
	bs, err := p.db().BillingAccount.Query().
		Where(entbillingaccount.Owner(account.Uuid()), entbillingaccount.DateForgottenIsNil()).
		All(ctx)
	if err != nil {
		return err
	}
	for _, b := range bs {
		req := levyan.BillingAccountPatchRequest_builder{
			Ref:              levyan.BillingAccountRef_builder{Id: b.Id[:]}.Build(),
			Name:             proto.String(""),
			Recipient:        proto.String(""),
			Email:            proto.String(""),
			Address:          proto.String(""),
			DateForgotten:    tsOf(now),
			DateUpdatedForce: proto.Bool(true),
		}.Build()
		if v := b.Business; v != nil {
			req.SetBusiness(levyan.BillingBusiness_builder{
				Registration: v.GetRegistration(),
				Name:         v.GetName(),
				Kind:         v.GetKind(),
				Item:         v.GetItem(),
			}.Build())
		}
		if _, err := p.S.BillingAccount().Patch(ctx, req); err != nil {
			return fmt.Errorf("billing account %s: %w", pdid.Id(b.Id), err)
		}

		// And of its subscriptions, the last four digits of the card each was
		// last paid with (#12); the kind and the brand say nobody.
		ms, err := p.db().Subscription.Query().
			Where(entsubscription.BillingAccountId(b.Id), entsubscription.MethodLast4NEQ("")).
			All(ctx)
		if err != nil {
			return err
		}
		for _, m := range ms {
			if _, err := p.S.Subscription().Patch(ctx, levyan.SubscriptionPatchRequest_builder{
				Ref:              levyan.SubscriptionRef_builder{Id: m.Id[:]}.Build(),
				MethodLast4:      proto.String(""),
				DateUpdatedForce: proto.Bool(true),
			}.Build()); err != nil {
				return fmt.Errorf("subscription %s: %w", pdid.Id(m.Id), err)
			}
		}
	}

	dep, err := p.db().Departure.Query().Where(entdeparture.Account(account.Uuid())).Only(ctx)
	if ent.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = p.S.Departure().Patch(ctx, levyan.DeparturePatchRequest_builder{
		Ref:              levyan.DepartureRef_builder{Id: dep.Id[:]}.Build(),
		DateSettled:      tsOf(now),
		DateUpdatedForce: proto.Bool(true),
	}.Build())

	return err
}

// Reconcile asks roster about every account levyan bills -- each that pays
// for a workspace or holds a billing account not forgotten -- for what its
// stream missed: one roster has nobody by that is not marked is marked as
// erased, and one marked that roster has again is taken back. A roster that
// has none of them is not believed (`standing.NobodyError`).
func (p Payers) Reconcile(ctx context.Context, r Roster) error {
	seen := map[pdid.Id]bool{}
	var accounts []pdid.Id
	keep := func(id uuid.UUID) {
		if v := pdid.Id(id); !seen[v] {
			seen[v] = true
			accounts = append(accounts, v)
		}
	}

	ls, err := p.db().Link.Query().Where(entlink.Or(entlink.DateEndIsNil(), entlink.DateEndGT(p.now()))).All(ctx)
	if err != nil {
		return err
	}
	for _, l := range ls {
		keep(l.Payer)
	}
	bs, err := p.db().BillingAccount.Query().Where(entbillingaccount.DateForgottenIsNil()).All(ctx)
	if err != nil {
		return err
	}
	for _, b := range bs {
		keep(b.Owner)
	}
	if len(accounts) == 0 {
		return nil
	}

	ds, err := p.db().Departure.Query().Where(entdeparture.DateBackIsNil()).All(ctx)
	if err != nil {
		return err
	}
	marked := map[pdid.Id]bool{}
	for _, d := range ds {
		marked[pdid.Id(d.Account)] = true
	}

	found, err := r.Reconcile(ctx, accounts, func(v pdid.Id) bool { return marked[v] })
	if err != nil {
		return err
	}
	var errs []error
	for _, v := range found.Gone {
		if err := p.Depart(ctx, v, levyan.DepartureReason_DEPARTURE_REASON_ERASED); err != nil {
			errs = append(errs, fmt.Errorf("account %s: %w", v, err))
		}
	}
	for _, st := range found.Back {
		if err := p.Back(ctx, st); err != nil {
			errs = append(errs, fmt.Errorf("account %s: %w", st.Holder, err))
		}
	}

	return errors.Join(errs...)
}

// Settles is the loop [SettleLoop]: [Payers.Settle] every `every`, in one
// process. A pass that fails is said in the log, and the next one asks again.
func Settles(p *Payers, r Roster, every time.Duration) spin.Spinner {
	return spin.Called(SettleLoop, spin.Every(every, func(ctx context.Context) error {
		if err := p.Settle(ctx, r); err != nil {
			log.From(ctx).WarnContext(ctx, "payers: settling roster's marks", slog.Any("err", err))
		}
		return nil
	}))
}

// Reconciles is the loop [ReconcileLoop]: [Payers.Reconcile] every `every`,
// in one process.
func Reconciles(p *Payers, r Roster, every time.Duration) spin.Spinner {
	return spin.Called(ReconcileLoop, spin.Every(every, func(ctx context.Context) error {
		if err := p.Reconcile(ctx, r); err != nil {
			log.From(ctx).WarnContext(ctx, "payers: asking roster about everybody", slog.Any("err", err))
		}
		return nil
	}))
}
