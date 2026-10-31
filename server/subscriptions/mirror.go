package subscriptions

import (
	"context"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"google.golang.org/protobuf/proto"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entsubscription "github.com/lesomnus/levyan/internal/ent/subscription"
	"github.com/lesomnus/levyan/internal/mor"
	"github.com/lesomnus/levyan/server/ledger"
)

// Observe writes down what the MoR answered of subscription `sub`, which is
// billing account `ba`'s: its mirror made, or made again of the answer when
// the MoR changed it after the answer held -- by the MoR's own clock -- and
// nothing but when levyan checked when it did not; an older answer changes
// nothing. `co` is the checkout that made it when the answer is of that
// checkout's completion, which is recorded completed with it. The billing
// account's collection is written beside it, in the same transaction.
//
// A second live subscription of a billing account that has one is canceled
// at the MoR as soon as it is written down, and written down again canceled.
func (s Subscriptions) Observe(ctx context.Context, ba uuid.UUID, sub mor.Subscription, co *ent.Checkout) (*ent.Subscription, error) {
	m, _, err := s.observe(ctx, ba, sub, co)
	return m, err
}

// observe is [Subscriptions.Observe], and whether it wrote anything but when
// levyan checked.
func (s Subscriptions) observe(ctx context.Context, ba uuid.UUID, sub mor.Subscription, co *ent.Checkout) (*ent.Subscription, bool, error) {
	var out *ent.Subscription
	second, moved := false, false
	err := s.tx(ctx, ba, func(srv levyan.Server, db *ent.Client) error {
		now := s.now()
		m, err := db.Subscription.Query().
			Where(entsubscription.Mor(s.Channel.Name()), entsubscription.MorSubscription(sub.ID)).
			Only(ctx)
		switch {
		case ent.IsNotFound(err):
			req := levyan.SubscriptionAddRequest_builder{
				BillingAccount:  levyan.BillingAccountRef_builder{Id: ba[:]}.Build(),
				Mor:             s.Channel.Name(),
				MorSubscription: sub.ID,
				DateChecked:     tsOf(now),
			}.Build()
			fill(req, sub)
			if co != nil && sub.Method != nil {
				// What the checkout was paid with: the first payment of it.
				req.SetLastTransaction(co.MorTransaction)
				req.SetLastProgress(int32(mor.Transaction{Status: mor.Paid}.Progress()))
				method(req, sub.Method)
			}
			v, err := srv.Subscription().Add(ctx, req)
			if err != nil {
				return fmt.Errorf("subscription %s: %w", sub.ID, err)
			}
			id, err := pdid.From(v.GetId())
			if err != nil {
				return err
			}
			if m, err = db.Subscription.Get(ctx, id.Uuid()); err != nil {
				return err
			}
			moved = true
		case err != nil:
			return err
		case m.BillingAccountId != ba:
			return fmt.Errorf("subscription %s: mirrored for billing account %s, and answered for %s", sub.ID, pdid.Id(m.BillingAccountId), pdid.Id(ba))
		default:
			held, got := ledger.Moment(m.DateMorUpdated), ledger.Moment(sub.Updated)
			if got.Before(held) {
				// Older than what is held, whatever order the answers came
				// back in: it says nothing that is so now.
				break
			}
			req := levyan.SubscriptionPatchRequest_builder{
				Ref:              levyan.SubscriptionRef_builder{Id: m.Id[:]}.Build(),
				DateChecked:      tsOf(now),
				DateUpdatedForce: proto.Bool(true),
			}.Build()
			if got.After(held) {
				patch(req, sub)
				moved = true
			}
			if co != nil && sub.Method != nil && later(co.MorTransaction, int32(mor.Transaction{Status: mor.Paid}.Progress()), m) {
				moved = true
				req.SetLastTransaction(co.MorTransaction)
				req.SetLastProgress(int32(mor.Transaction{Status: mor.Paid}.Progress()))
				req.SetMethodType(sub.Method.Type)
				req.SetMethodBrand(sub.Method.Brand)
				req.SetMethodLast4(sub.Method.Last4)
			}
			if _, err := srv.Subscription().Patch(ctx, req); err != nil {
				return fmt.Errorf("subscription %s: %w", sub.ID, err)
			}
			if m, err = db.Subscription.Get(ctx, m.Id); err != nil {
				return err
			}
		}
		out = m

		if co != nil && levyan.CheckoutState(co.State) == levyan.CheckoutState_CHECKOUT_STATE_OPEN {
			if _, err := srv.Checkout().Patch(ctx, levyan.CheckoutPatchRequest_builder{
				Ref:              levyan.CheckoutRef_builder{Id: co.Id[:]}.Build(),
				State:            levyan.CheckoutState_CHECKOUT_STATE_COMPLETED.Enum(),
				Subscription:     levyan.SubscriptionRef_builder{Id: m.Id[:]}.Build(),
				DateDone:         tsOf(now),
				DateUpdatedForce: proto.Bool(true),
			}.Build()); err != nil {
				return fmt.Errorf("checkout %s: %w", pdid.Id(co.Id), err)
			}
			moved = true
		}

		ms, err := mirrors(ctx, db, ba)
		if err != nil {
			return err
		}
		if l := live(ms); l != nil && l.Id != m.Id && levyan.SubscriptionStatus(m.Status) != levyan.SubscriptionStatus_SUBSCRIPTION_STATUS_CANCELED {
			second = true
		}

		return s.collect(ctx, srv, db, ba, now)
	})
	if err != nil {
		return nil, false, err
	}

	if second {
		// One subscription is collected through at a time: the one levyan
		// learned of first. This one -- two checkouts begun at once, both
		// finished -- is canceled now, before it bills anything of its own.
		logged(ctx, ba).WarnContext(ctx, "subscriptions: a second subscription of the billing account, canceled at the merchant of record", slog.String("subscription", sub.ID))
		got, err := s.Channel.Cancel(ctx, mor.Cancel{Subscription: sub.ID, Now: true})
		if err != nil {
			// Live still: canceled the next time it is heard of.
			logged(ctx, ba).ErrorContext(ctx, "subscriptions: the second subscription could not be canceled", slog.String("subscription", sub.ID), slog.Any("err", err))
			return out, moved, nil
		}
		m, _, err := s.observe(ctx, ba, got, nil)
		return m, true, err
	}

	return out, moved, nil
}

// ObserveTransaction writes down what the MoR answered of transaction `t` on
// the mirror of the subscription it is of: how the payer pays, and whether a
// payment waits for them -- when it is the newest transaction of the
// subscription levyan was told of, or that one further along, or as far
// along and asked later. Nothing for a transaction of no subscription
// mirrored; false when nothing was written.
func (s Subscriptions) ObserveTransaction(ctx context.Context, t mor.Transaction) (*ent.Subscription, bool, error) {
	if t.Subscription == "" || t.ID == "" {
		return nil, false, nil
	}
	m, err := s.db().Subscription.Query().
		Where(entsubscription.Mor(s.Channel.Name()), entsubscription.MorSubscription(t.Subscription)).
		Only(ctx)
	if ent.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}

	moved := false
	err = s.tx(ctx, m.BillingAccountId, func(srv levyan.Server, db *ent.Client) error {
		if m, err = db.Subscription.Get(ctx, m.Id); err != nil {
			return err
		}
		p := int32(t.Progress())
		if !later(t.ID, p, m) {
			return nil
		}
		same := t.ID == m.LastTransaction && p == m.LastProgress && t.Awaiting == m.AwaitingPayer &&
			(t.Method == nil || (t.Method.Type == m.MethodType && t.Method.Brand == m.MethodBrand && t.Method.Last4 == m.MethodLast4))
		if same {
			return nil
		}
		req := levyan.SubscriptionPatchRequest_builder{
			Ref:              levyan.SubscriptionRef_builder{Id: m.Id[:]}.Build(),
			LastTransaction:  proto.String(t.ID),
			LastProgress:     proto.Int32(p),
			AwaitingPayer:    proto.Bool(t.Awaiting),
			DateUpdatedForce: proto.Bool(true),
		}.Build()
		if t.Method != nil {
			req.SetMethodType(t.Method.Type)
			req.SetMethodBrand(t.Method.Brand)
			req.SetMethodLast4(t.Method.Last4)
		}
		if _, err := srv.Subscription().Patch(ctx, req); err != nil {
			return fmt.Errorf("subscription %s: %w", t.Subscription, err)
		}
		moved = true
		if m, err = db.Subscription.Get(ctx, m.Id); err != nil {
			return err
		}

		return s.collect(ctx, srv, db, m.BillingAccountId, s.now())
	})

	return m, moved, err
}

// later is whether an observation of transaction `id`, `p` along its life,
// is no older than what mirror `m` holds of its newest transaction: of a
// newer transaction -- Paddle's ids sort as their entities were made -- or of
// the same one, as far along at least.
func later(id string, p int32, m *ent.Subscription) bool {
	switch {
	case id > m.LastTransaction:
		return true
	case id == m.LastTransaction:
		return p >= m.LastProgress
	default:
		return false
	}
}

// collect writes billing account `ba`'s collection beside its mirrors, when
// they make it another than it holds: what it is now, since now.
func (s Subscriptions) collect(ctx context.Context, srv levyan.Server, db *ent.Client, ba uuid.UUID, now time.Time) error {
	b, err := db.BillingAccount.Get(ctx, ba)
	if err != nil {
		return err
	}
	ms, err := mirrors(ctx, db, ba)
	if err != nil {
		return err
	}
	st := state(b, current(ms))
	if levyan.CollectionState(b.Collection) == st {
		return nil
	}
	if _, err := srv.BillingAccount().Patch(ctx, levyan.BillingAccountPatchRequest_builder{
		Ref:              levyan.BillingAccountRef_builder{Id: ba[:]}.Build(),
		Collection:       st.Enum(),
		DateCollection:   tsOf(now),
		DateUpdatedForce: proto.Bool(true),
	}.Build()); err != nil {
		return fmt.Errorf("billing account %s: %w", pdid.Id(ba), err)
	}
	logged(ctx, ba).InfoContext(ctx, "subscriptions: collection",
		slog.String("from", levyan.CollectionState(b.Collection).String()),
		slog.String("to", st.String()),
	)

	return nil
}

// state is what billing account `b` collected through mirror `m` -- its live
// one, or the last it had -- is (implementation decision, #12):
//
//   - on a channel other than the merchant of record's, nothing is said;
//   - with no mirror, there is no subscription;
//   - canceled and paused are what they are;
//   - past due, or active, with a payment that waits for the payer, needs the
//     payer's action: approving a Korean card's payment in its app, passing
//     a bank's check. Without one, past due is past due, and active and in
//     a trial is collectable.
//
// A change the MoR has scheduled -- a cancellation or a pause at the end of
// the period -- leaves it collectable until it takes effect ([Collection]
// says when).
func state(b *ent.BillingAccount, m *ent.Subscription) levyan.CollectionState {
	if b.Channel != levyan.Channel_CHANNEL_MOR.String() {
		return levyan.CollectionState_COLLECTION_STATE_UNSPECIFIED
	}
	if m == nil {
		return levyan.CollectionState_COLLECTION_STATE_NO_SUBSCRIPTION
	}
	switch levyan.SubscriptionStatus(m.Status) {
	case levyan.SubscriptionStatus_SUBSCRIPTION_STATUS_CANCELED:
		return levyan.CollectionState_COLLECTION_STATE_CANCELED
	case levyan.SubscriptionStatus_SUBSCRIPTION_STATUS_PAUSED:
		return levyan.CollectionState_COLLECTION_STATE_PAUSED
	case levyan.SubscriptionStatus_SUBSCRIPTION_STATUS_PAST_DUE:
		if m.AwaitingPayer {
			return levyan.CollectionState_COLLECTION_STATE_NEEDS_ACTION
		}
		return levyan.CollectionState_COLLECTION_STATE_PAST_DUE
	case levyan.SubscriptionStatus_SUBSCRIPTION_STATUS_ACTIVE, levyan.SubscriptionStatus_SUBSCRIPTION_STATUS_TRIALING:
		if m.AwaitingPayer {
			return levyan.CollectionState_COLLECTION_STATE_NEEDS_ACTION
		}
		return levyan.CollectionState_COLLECTION_STATE_COLLECTABLE
	default:
		return levyan.CollectionState_COLLECTION_STATE_UNSPECIFIED
	}
}

// Collection is whether what a billing account is billed can be collected
// now: what dunning (#11) reads, and a product and a payer are told.
type Collection struct {
	State levyan.CollectionState

	// Since is when it came to be so, as levyan learned it: the billing
	// account's making, for one on the MoR's channel no subscription was
	// ever mirrored of.
	Since time.Time

	// Ends is when the MoR will cancel, or pause, the live subscription,
	// as somebody scheduled it there: zero for nothing scheduled.
	Ends time.Time

	// Mirror is the subscription it is collected through: its live one, or
	// the last it had; nil for none.
	Mirror *ent.Subscription
}

// CollectionOf is billing account `b`'s collection, as written beside its
// mirrors.
func CollectionOf(ctx context.Context, db *ent.Client, b *ent.BillingAccount) (Collection, error) {
	out := Collection{State: levyan.CollectionState(b.Collection)}
	if b.DateCollection != nil {
		out.Since = *b.DateCollection
	}
	if b.Channel != levyan.Channel_CHANNEL_MOR.String() {
		return Collection{}, nil
	}
	ms, err := mirrors(ctx, db, b.Id)
	if err != nil {
		return out, err
	}
	out.Mirror = current(ms)
	if out.State == levyan.CollectionState_COLLECTION_STATE_UNSPECIFIED {
		// None mirrored yet, ever.
		out.State, out.Since = levyan.CollectionState_COLLECTION_STATE_NO_SUBSCRIPTION, b.DateCreated
	}
	if m := live(ms); m != nil && m.DateChange != nil {
		switch levyan.SubscriptionChange(m.Change) {
		case levyan.SubscriptionChange_SUBSCRIPTION_CHANGE_CANCEL, levyan.SubscriptionChange_SUBSCRIPTION_CHANGE_PAUSE:
			out.Ends = *m.DateChange
		}
	}

	return out, nil
}

// fill is what an answer says of a subscription, on the mirror to be made of
// it.
func fill(req *levyan.SubscriptionAddRequest, sub mor.Subscription) {
	req.SetMorCustomer(sub.Customer)
	req.SetStatus(statusOf(sub.Status))
	req.SetCurrency(sub.Currency)
	if !sub.Period.Start.IsZero() {
		req.SetDatePeriodStart(tsOf(sub.Period.Start))
		req.SetDatePeriodEnd(tsOf(sub.Period.End))
	}
	if !sub.Next.IsZero() {
		req.SetDateNext(tsOf(sub.Next))
	}
	if c := sub.Change; c != nil {
		req.SetChange(changeOf(c.Kind))
		req.SetDateChange(tsOf(c.At))
	}
	if !sub.Canceled.IsZero() {
		req.SetDateCanceled(tsOf(sub.Canceled))
	}
	req.SetDateMorUpdated(tsOf(sub.Updated))
}

// patch is what an answer says of a subscription, over all its mirror held:
// what it no longer says is cleared.
func patch(req *levyan.SubscriptionPatchRequest, sub mor.Subscription) {
	req.SetMorCustomer(sub.Customer)
	req.SetStatus(statusOf(sub.Status))
	req.SetCurrency(sub.Currency)
	if sub.Period.Start.IsZero() {
		req.SetDatePeriodStartNull(true)
		req.SetDatePeriodEndNull(true)
	} else {
		req.SetDatePeriodStart(tsOf(sub.Period.Start))
		req.SetDatePeriodEnd(tsOf(sub.Period.End))
	}
	if sub.Next.IsZero() {
		req.SetDateNextNull(true)
	} else {
		req.SetDateNext(tsOf(sub.Next))
	}
	if c := sub.Change; c != nil {
		req.SetChange(changeOf(c.Kind))
		req.SetDateChange(tsOf(c.At))
	} else {
		req.SetChange(levyan.SubscriptionChange_SUBSCRIPTION_CHANGE_UNSPECIFIED)
		req.SetDateChangeNull(true)
	}
	if sub.Canceled.IsZero() {
		req.SetDateCanceledNull(true)
	} else {
		req.SetDateCanceled(tsOf(sub.Canceled))
	}
	req.SetDateMorUpdated(tsOf(sub.Updated))
}

func method(req *levyan.SubscriptionAddRequest, m *mor.Method) {
	req.SetMethodType(m.Type)
	req.SetMethodBrand(m.Brand)
	req.SetMethodLast4(m.Last4)
}

// statusOf is a status of package mor's as the schema writes it, value for
// value.
func statusOf(s mor.SubscriptionStatus) levyan.SubscriptionStatus {
	switch s {
	case mor.Active:
		return levyan.SubscriptionStatus_SUBSCRIPTION_STATUS_ACTIVE
	case mor.Trialing:
		return levyan.SubscriptionStatus_SUBSCRIPTION_STATUS_TRIALING
	case mor.PastDue:
		return levyan.SubscriptionStatus_SUBSCRIPTION_STATUS_PAST_DUE
	case mor.Paused:
		return levyan.SubscriptionStatus_SUBSCRIPTION_STATUS_PAUSED
	case mor.Canceled:
		return levyan.SubscriptionStatus_SUBSCRIPTION_STATUS_CANCELED
	default:
		return levyan.SubscriptionStatus_SUBSCRIPTION_STATUS_UNSPECIFIED
	}
}

// changeOf is a scheduled change of package mor's as the schema writes it.
func changeOf(k mor.ChangeKind) levyan.SubscriptionChange {
	switch k {
	case mor.CancelAtEnd:
		return levyan.SubscriptionChange_SUBSCRIPTION_CHANGE_CANCEL
	case mor.PauseAtEnd:
		return levyan.SubscriptionChange_SUBSCRIPTION_CHANGE_PAUSE
	case mor.Resume:
		return levyan.SubscriptionChange_SUBSCRIPTION_CHANGE_RESUME
	default:
		return levyan.SubscriptionChange_SUBSCRIPTION_CHANGE_UNSPECIFIED
	}
}
