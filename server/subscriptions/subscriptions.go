// Package subscriptions is a billing account's subscription with the
// merchant of record, as levyan keeps it (#12, docs/design.md §12): the
// checkout that begins it, the mirror of what the MoR's API answers of it,
// the billing account's collection read off that mirror, and the inbox of the
// MoR's notices that keeps the mirror current -- its webhooks, and its own
// list of them, pulled.
//
// # A checkout, and one subscription
//
// A payer subscribes in the MoR's checkout, which levyan begins for a billing
// account of theirs on the MoR's channel, in the plan of its currency
// ([Subscriptions.Start]), and records ([levyan.Checkout]): a checkout is
// completed only as levyan recorded it, for the billing account it was begun
// for ([Subscriptions.Complete]), whether the payer comes back from it or the
// MoR's notice says it was finished first. Done twice, it is done once.
//
// A billing account has one live subscription. A checkout begun while one
// stands is refused, once the MoR is asked again whether it stands; one begun
// while another is open is that one again, while the MoR still takes it, so
// that a payer who begins twice subscribes once. Two the MoR made all the
// same -- two checkouts begun at the same moment, and both finished -- are
// one too many, and the one levyan learned of second is canceled at the MoR
// as it learns of it.
//
// # The mirror
//
// The subscription is the MoR's ([levyan.Subscription]): what levyan keeps is
// made of what the MoR's API answers, never of a webhook's body, and an
// answer older than the one it holds -- by the MoR's own time of its last
// change -- changes nothing. How the payer pays, and whether a payment waits
// for them, comes of the newest transaction of it levyan was told of. What
// the billing account's collection is ([levyan.CollectionState]) is read off
// its live mirror and written on the billing account beside it, in the same
// transaction, so that each change of it is on the trail; what a state that
// is not collectable leads to is dunning's (#11).
//
// # Written through the servers
//
// Every write of a checkout, a mirror or a billing account goes through the
// stack this is handed: the walled one as the account, for what its own call
// makes (`server/api`), and the deployment's own for what a notice leads to.
// What decides a write is read with ent on the transaction it is written in,
// which holds the billing account's row first. The inbox ([Inbox]) is the one
// table written with ent alone, and kept off the trail (its schema says why).
package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"time"
	"uuid"

	"github.com/lesomnus/otx/log"
	"github.com/protobuf-orm/ent/dialect"
	"github.com/protobuf-orm/ent/dialect/sql"
	"github.com/protobuf-orm/protoc-gen-orm-ent/runtime/enttx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lesomnus/payday/config"
	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entbillingaccount "github.com/lesomnus/levyan/internal/ent/billingaccount"
	entcheckout "github.com/lesomnus/levyan/internal/ent/checkout"
	entsubscription "github.com/lesomnus/levyan/internal/ent/subscription"
	"github.com/lesomnus/levyan/internal/mor"
	"github.com/lesomnus/levyan/server/ledger"
)

// Subscriptions keeps billing accounts' subscriptions with the merchant of
// record, through S.
type Subscriptions struct {
	// S is the stack a row is written through, as whoever the context is
	// framed as.
	S levyan.Server

	// Drv is what a transaction is begun on, and Dialect what it speaks.
	Drv     dialect.Driver
	Dialect string

	// Now is the deployment's clock: when levyan checked, learned, recorded.
	Now func() time.Time

	// Channel is the merchant of record: nil when the deployment names none
	// (`mor`), when no checkout is begun or completed.
	Channel mor.Channel
}

// On is these subscriptions writing through `s`: the same, with another
// stack.
func (s Subscriptions) On(srv levyan.Server) Subscriptions {
	s.S = srv
	return s
}

// Configured says whether a merchant of record is named.
func (s Subscriptions) Configured() bool { return s.Channel != nil }

func (s Subscriptions) now() time.Time {
	if s.Now == nil {
		return ledger.Moment(time.Now())
	}

	return ledger.Moment(s.Now())
}

func (s Subscriptions) postgres() bool { return s.Dialect == config.DialectPostgres }

func (s Subscriptions) db() *ent.Client { return ent.NewClient(ent.Driver(s.Drv)) }

// tx runs `f` in one transaction that holds billing account `ba`'s row first:
// the stack and an ent client on it.
func (s Subscriptions) tx(ctx context.Context, ba uuid.UUID, f func(srv levyan.Server, db *ent.Client) error) error {
	tdrv, tx, err := dialect.BeginTx(ctx, s.Drv)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	srv, err := enttx.Rebind(s.S, tdrv)
	if err != nil {
		return err
	}
	db := ent.NewClient(ent.Driver(tdrv))
	if err := lock(ctx, db, s.postgres(), ba); err != nil {
		return err
	}
	if err := f(srv, db); err != nil {
		return err
	}

	return tx.Commit()
}

// lock holds billing account `id`'s row until the transaction `db` is on
// ends: every write of its checkouts, its mirrors and its collection holds
// it, so that two that read it at once are one after the other. SQLite runs
// one writer at a time (`_txlock=immediate`), so there it asks nothing.
func lock(ctx context.Context, db *ent.Client, postgres bool, id uuid.UUID) error {
	if !postgres {
		return nil
	}
	q := db.BillingAccount.Query().Where(entbillingaccount.Id(id))
	q.Modify(func(s *sql.Selector) { s.ForUpdate() })
	_, err := q.Ids(ctx)

	return err
}

// ErrNoMerchant is a checkout asked of a deployment that names no merchant of
// record.
var ErrNoMerchant = status.Error(codes.FailedPrecondition, "no merchant of record is configured here (`mor`), so no checkout is begun or completed")

// own is billing account `id` when `account` holds it and it is not
// forgotten, and NotFound when not: somebody else's is no billing account of
// theirs.
func own(ctx context.Context, db *ent.Client, account, id pdid.Id) (*ent.BillingAccount, error) {
	v, err := db.BillingAccount.Query().
		Where(entbillingaccount.Id(id.Uuid()), entbillingaccount.Owner(account.Uuid()), entbillingaccount.DateForgottenIsNil()).
		Only(ctx)
	if ent.IsNotFound(err) {
		return nil, status.Errorf(codes.NotFound, "no billing account of the account's is %s", id)
	}

	return v, err
}

// onTheMerchant refuses a billing account on another channel than the
// merchant of record's: nothing of it is collected through a subscription.
func onTheMerchant(ba *ent.BillingAccount) error {
	if ba.Channel != levyan.Channel_CHANNEL_MOR.String() {
		return status.Errorf(codes.FailedPrecondition, "the billing account is billed through %s, which takes no checkout of the merchant of record's", ba.Channel)
	}

	return nil
}

// Start begins a checkout for billing account `billing` of `account`'s, which
// sends the payer back to `success` once they have finished it: the checkout,
// as levyan recorded it, and what a page opens it with.
//
// A billing account with a live subscription is refused, once the MoR is
// asked again whether it is live; one with an open checkout is answered with
// that one again while the MoR still takes it, and with another, the first
// recorded as canceled, once it does not.
func (s Subscriptions) Start(ctx context.Context, account, billing pdid.Id, success string) (*ent.Checkout, mor.Window, error) {
	if !s.Configured() {
		return nil, mor.Window{}, ErrNoMerchant
	}
	if u, err := url.Parse(success); err != nil || !u.IsAbs() || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, mor.Window{}, status.Error(codes.InvalidArgument, "success_url: where the checkout sends the payer back, an absolute URL")
	}
	db := s.db()
	ba, err := own(ctx, db, account, billing)
	if err != nil {
		return nil, mor.Window{}, err
	}
	if err := onTheMerchant(ba); err != nil {
		return nil, mor.Window{}, err
	}

	ms, err := mirrors(ctx, db, ba.Id)
	if err != nil {
		return nil, mor.Window{}, err
	}
	if m := live(ms); m != nil {
		// Asked again: the payer may have canceled it at the MoR since
		// levyan last heard, which is no reason to keep them from another.
		got, err := s.Channel.Subscription(ctx, m.MorSubscription)
		if err != nil {
			return nil, mor.Window{}, merchant("whether its subscription stands", err)
		}
		if _, err := s.Observe(ctx, ba.Id, got, nil); err != nil {
			return nil, mor.Window{}, err
		}
		if got.Status != mor.Canceled {
			return nil, mor.Window{}, status.Errorf(codes.FailedPrecondition, "the billing account is subscribed already (%s): one subscription is collected through at a time", got.Status)
		}
	}

	start := mor.Start{Account: pdid.Id(ba.Id).String(), Currency: ba.Currency, Success: success}
	if m := latest(ms); m != nil && m.MorCustomer != "" {
		// The MoR knows the payer from the subscription before.
		start.Customer = m.MorCustomer
	}
	open, err := db.Checkout.Query().
		Where(entcheckout.BillingAccountId(ba.Id), entcheckout.Mor(s.Channel.Name()), entcheckout.State(int32(levyan.CheckoutState_CHECKOUT_STATE_OPEN))).
		Order(ent.Desc(entcheckout.FieldDateCreated), ent.Desc(entcheckout.FieldId)).
		First(ctx)
	switch {
	case ent.IsNotFound(err):
		open = nil
	case err != nil:
		return nil, mor.Window{}, err
	default:
		start.Again = &mor.Checkout{ID: open.MorTransaction, Account: start.Account}
	}

	co, err := s.Channel.StartCheckout(ctx, start)
	switch {
	case errors.Is(err, mor.ErrFinished):
		// Finished meanwhile, in another window: completed here, and no
		// other begun.
		if _, _, err := s.finish(ctx, open); err != nil {
			return nil, mor.Window{}, err
		}
		return nil, mor.Window{}, status.Error(codes.FailedPrecondition, "the billing account is subscribed already: the checkout begun before was finished")
	case err != nil:
		return nil, mor.Window{}, merchant("to begin a checkout", err)
	}
	if open != nil && co.ID == open.MorTransaction {
		return open, co.Window, nil
	}

	var out *ent.Checkout
	err = s.tx(ctx, ba.Id, func(srv levyan.Server, db *ent.Client) error {
		now := s.now()
		if open != nil {
			// The MoR takes it no longer: begun anew, above.
			if _, err := srv.Checkout().Patch(ctx, levyan.CheckoutPatchRequest_builder{
				Ref:              levyan.CheckoutRef_builder{Id: open.Id[:]}.Build(),
				State:            levyan.CheckoutState_CHECKOUT_STATE_CANCELED.Enum(),
				DateDone:         tsOf(now),
				DateUpdatedForce: proto.Bool(true),
			}.Build()); err != nil {
				return fmt.Errorf("checkout %s: %w", pdid.Id(open.Id), err)
			}
		}
		v, err := srv.Checkout().Add(ctx, levyan.CheckoutAddRequest_builder{
			Account:        account.Bytes(),
			BillingAccount: levyan.BillingAccountRef_builder{Id: ba.Id[:]}.Build(),
			Mor:            s.Channel.Name(),
			MorTransaction: co.ID,
			State:          levyan.CheckoutState_CHECKOUT_STATE_OPEN,
		}.Build())
		if err != nil {
			return err
		}
		id, err := pdid.From(v.GetId())
		if err != nil {
			return err
		}
		out, err = db.Checkout.Get(ctx, id.Uuid())
		return err
	})
	if err != nil {
		return nil, mor.Window{}, err
	}

	return out, co.Window, nil
}

// Complete asks the MoR about checkout `id` of `account`'s: the checkout as it
// is then -- completed, with the mirror of the subscription it started; still
// open; or canceled -- which a checkout completed or canceled already is
// answered with, and nothing is asked.
func (s Subscriptions) Complete(ctx context.Context, account, id pdid.Id) (*ent.Checkout, *ent.Subscription, error) {
	if !s.Configured() {
		return nil, nil, ErrNoMerchant
	}
	co, err := s.db().Checkout.Query().
		Where(entcheckout.Id(id.Uuid()), entcheckout.Account(account.Uuid())).
		Only(ctx)
	if ent.IsNotFound(err) {
		return nil, nil, status.Errorf(codes.NotFound, "no checkout of the account's is %s", id)
	}
	if err != nil {
		return nil, nil, err
	}
	if co.Mor != s.Channel.Name() {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "the checkout was begun with %s, which this deployment no longer sells through", co.Mor)
	}

	return s.finish(ctx, co)
}

// finish is checkout `co` as the MoR has it now, written down: completed into
// the mirror of the subscription it made, or canceled when the MoR says it can
// no longer be finished. One still open is answered as it is. One done
// already is answered as it was, and the MoR is not asked.
func (s Subscriptions) finish(ctx context.Context, co *ent.Checkout) (*ent.Checkout, *ent.Subscription, error) {
	db := s.db()
	switch levyan.CheckoutState(co.State) {
	case levyan.CheckoutState_CHECKOUT_STATE_COMPLETED:
		m, err := db.Subscription.Get(ctx, co.SubscriptionId)
		return co, m, err
	case levyan.CheckoutState_CHECKOUT_STATE_CANCELED:
		return co, nil, nil
	}

	sub, err := s.Channel.CompleteCheckout(ctx, mor.Checkout{ID: co.MorTransaction, Account: pdid.Id(co.BillingAccountId).String()})
	switch {
	case errors.Is(err, mor.ErrOpen):
		return co, nil, nil
	case err != nil && mor.KindOf(err) == mor.Declined:
		err := s.tx(ctx, co.BillingAccountId, func(srv levyan.Server, db *ent.Client) error {
			v, err := db.Checkout.Get(ctx, co.Id)
			if err != nil || levyan.CheckoutState(v.State) != levyan.CheckoutState_CHECKOUT_STATE_OPEN {
				return err
			}
			_, err = srv.Checkout().Patch(ctx, levyan.CheckoutPatchRequest_builder{
				Ref:              levyan.CheckoutRef_builder{Id: co.Id[:]}.Build(),
				State:            levyan.CheckoutState_CHECKOUT_STATE_CANCELED.Enum(),
				DateDone:         tsOf(s.now()),
				DateUpdatedForce: proto.Bool(true),
			}.Build())
			return err
		})
		if err != nil {
			return nil, nil, err
		}
		v, err := db.Checkout.Get(ctx, co.Id)
		return v, nil, err
	case err != nil:
		return nil, nil, merchant("about the checkout", err)
	}

	m, err := s.Observe(ctx, co.BillingAccountId, sub, co)
	if err != nil {
		return nil, nil, err
	}
	v, err := db.Checkout.Get(ctx, co.Id)

	return v, m, err
}

// merchant is an error of the MoR's, asked `what`, as a caller is told it:
// to try again later, whatever it was, but for a request the MoR says was
// wrong. What the MoR's error says is in the log, with its request's id.
func merchant(what string, err error) error {
	switch mor.KindOf(err) {
	case mor.Invalid, mor.NotFound:
		return status.Errorf(codes.FailedPrecondition, "the merchant of record was asked %s, and refused: %v", what, err)
	default:
		return status.Errorf(codes.Unavailable, "the merchant of record could not be asked %s: %v", what, err)
	}
}

// mirrors is every mirror of billing account `ba`'s, in the order levyan
// learned of them.
func mirrors(ctx context.Context, db *ent.Client, ba uuid.UUID) ([]*ent.Subscription, error) {
	return db.Subscription.Query().
		Where(entsubscription.BillingAccountId(ba)).
		Order(ent.Asc(entsubscription.FieldDateCreated), ent.Asc(entsubscription.FieldId)).
		All(ctx)
}

// live is the first of `ms` that is not canceled: the one a billing account
// is collected through. Nil for none.
func live(ms []*ent.Subscription) *ent.Subscription {
	i := slices.IndexFunc(ms, func(m *ent.Subscription) bool {
		return levyan.SubscriptionStatus(m.Status) != levyan.SubscriptionStatus_SUBSCRIPTION_STATUS_CANCELED
	})
	if i < 0 {
		return nil
	}

	return ms[i]
}

// latest is the last of `ms`. Nil for none.
func latest(ms []*ent.Subscription) *ent.Subscription {
	if len(ms) == 0 {
		return nil
	}

	return ms[len(ms)-1]
}

// current is the mirror a billing account's collection is read off: its live
// one, or the last it had.
func current(ms []*ent.Subscription) *ent.Subscription {
	if m := live(ms); m != nil {
		return m
	}

	return latest(ms)
}

func tsOf(t time.Time) *timestamppb.Timestamp { return timestamppb.New(ledger.Moment(t)) }

// logged is the log of `ctx`, said of billing account `ba`.
func logged(ctx context.Context, ba uuid.UUID) *slog.Logger {
	return log.From(ctx).With(slog.String("billing_account", pdid.Id(ba).String()))
}
