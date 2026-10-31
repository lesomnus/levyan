package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
	"uuid"

	"github.com/lesomnus/otx/log"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/spin"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entcheckout "github.com/lesomnus/levyan/internal/ent/checkout"
	entnotice "github.com/lesomnus/levyan/internal/ent/notice"
	entnoticecursor "github.com/lesomnus/levyan/internal/ent/noticecursor"
	entsubscription "github.com/lesomnus/levyan/internal/ent/subscription"
	"github.com/lesomnus/levyan/internal/mor"
	"github.com/lesomnus/levyan/server/ledger"
	"github.com/lesomnus/levyan/server/pd"
)

// The loops of the merchant of record's notices, each in one process: the
// inbox taken, and the MoR's list of its events read into it.
const (
	InboxLoop  = "levyan.mor.inbox"
	EventsLoop = "levyan.mor.events"
)

// MaxBody is the most of a webhook read. Paddle's carry an entity, a
// transaction with its lines among them, which is some kilobytes; one larger
// than this is refused before its signature is checked.
const MaxBody = 1 << 20

// Answer is the most a webhook is held for before it is answered: within the
// five seconds Paddle waits (https://developer.paddle.com/webhooks/about/respond-to-webhooks),
// so that one the database is slow to keep is answered as a failure, and
// sent again, rather than not at all.
const Answer = 4 * time.Second

// Overlap is how long after a notice the MoR's list of them is read past it
// no further: what was noticed since is read again by the next pull, which
// the inbox keeps once -- so that a notice the MoR lists a little after one
// it made later is not passed over.
const Overlap = mor.Settle

// WebhookPath is where the webhooks of the merchant of record `ch` come to
// on the public listener (`server.http`): /webhooks/mor/paddle.
func WebhookPath(ch mor.Channel) string { return "/webhooks/mor/" + ch.Name() }

// Inbox is the merchant of record's notices, each kept once by its id
// ([levyan.Notice]): written with ent alone, by the endpoint its webhooks
// come to -- which so holds this table and the channel's check of a
// signature, and no server -- and by the pull of its list of them.
type Inbox struct {
	// Db is the inbox's table.
	Db *ent.Client

	// Channel is the merchant of record the notices are of.
	Channel mor.Channel

	// Now is the deployment's clock: when a notice came.
	Now func() time.Time

	wake chan struct{}
}

// NewInbox is the inbox of `ch`'s notices, in `db`.
func NewInbox(db *ent.Client, ch mor.Channel, now func() time.Time) *Inbox {
	return &Inbox{Db: db, Channel: ch, Now: now, wake: make(chan struct{}, 1)}
}

func (in *Inbox) now() time.Time {
	if in.Now == nil {
		return ledger.Moment(time.Now())
	}

	return ledger.Moment(in.Now())
}

// Wake has the loop that takes the inbox look at it now, rather than at its
// next turn: a notice was kept.
func (in *Inbox) Wake() {
	select {
	case in.wake <- struct{}{}:
	default:
	}
}

// Keep keeps `n`, which came `via` a webhook or the MoR's list, once: false
// when the inbox has it already, by its id, however it came. A notice about
// nothing levyan acts on is kept taken.
func (in *Inbox) Keep(ctx context.Context, n mor.Notice, via levyan.NoticeVia) (bool, error) {
	db := in.Db
	ok, err := db.Notice.Query().Where(entnotice.Mor(in.Channel.Name()), entnotice.MorEvent(n.ID)).Exist(ctx)
	if err != nil || ok {
		return false, err
	}

	now := in.now()
	q := db.Notice.Create().
		SetId(pdid.New(pd.NoticeDomain).Uuid()).
		SetMor(in.Channel.Name()).
		SetMorEvent(n.ID).
		SetType(n.Event).
		SetKind(int32(kindOf(n.Type))).
		SetVia(int32(via)).
		SetMorTransaction(n.Transaction).
		SetMorSubscription(n.Subscription).
		SetMorAdjustment(n.Adjustment).
		SetOutcome(int32(levyan.NoticeOutcome_NOTICE_OUTCOME_UNSPECIFIED)).
		SetTries(0).
		SetError("").
		SetDateCreated(now)
	if !n.At.IsZero() {
		q.SetDateOccurred(ledger.Moment(n.At))
	}
	if n.Type == mor.NoticeOther {
		q.SetOutcome(int32(levyan.NoticeOutcome_NOTICE_OUTCOME_DROPPED)).SetDateDone(now)
	}
	if err := q.Exec(ctx); err != nil {
		if ent.IsConstraintError(err) {
			// Kept by another delivery of it, at the same moment.
			return false, nil
		}
		return false, err
	}

	return true, nil
}

// Handler is the endpoint the MoR's webhooks come to ([WebhookPath]): each
// read up to [MaxBody], its signature and its freshness checked
// ([mor.Channel.Webhook]), kept, and answered -- within [Answer], and with no
// request of its own to the MoR, which comes after, as the inbox is taken. A
// webhook that does not hold is refused (400); one levyan could not keep, or
// check for want of its secret, is answered as levyan's failure (503), which
// the MoR sends again. What `ctx` logs to is what the endpoint logs to.
func (in *Inbox) Handler(ctx context.Context) http.Handler {
	l := log.From(ctx).With(slog.String("mor", in.Channel.Name()))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "a webhook is a POST", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
		if err != nil {
			var big *http.MaxBytesError
			if errors.As(err, &big) {
				l.WarnContext(r.Context(), "mor: a webhook larger than levyan reads, refused", slog.Int64("limit", big.Limit))
				http.Error(w, "too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "the body could not be read", http.StatusBadRequest)
			return
		}

		n, err := in.Channel.Webhook(r.Header, body)
		if err != nil {
			if mor.KindOf(err) == mor.Unknown {
				l.ErrorContext(r.Context(), "mor: a webhook could not be checked", slog.Any("err", err))
				http.Error(w, "not now", http.StatusServiceUnavailable)
				return
			}
			l.WarnContext(r.Context(), "mor: a webhook refused", slog.Any("err", err))
			http.Error(w, "refused", http.StatusBadRequest)
			return
		}

		keep, cancel := context.WithTimeout(r.Context(), Answer)
		defer cancel()
		kept, err := in.Keep(keep, n, levyan.NoticeVia_NOTICE_VIA_WEBHOOK)
		if err != nil {
			l.ErrorContext(r.Context(), "mor: a webhook could not be kept", slog.String("event", n.ID), slog.Any("err", err))
			http.Error(w, "not now", http.StatusServiceUnavailable)
			return
		}
		if kept {
			in.Wake()
		}
		l.DebugContext(r.Context(), "mor: a webhook", slog.String("event", n.ID), slog.String("type", n.Event), slog.Bool("kept", kept))
		w.WriteHeader(http.StatusOK)
	})
}

// Taker takes the inbox: asks the MoR about what each notice names, and
// writes down what it answers through Subs -- the deployment's own stack.
type Taker struct {
	In   *Inbox
	Subs Subscriptions

	// Transaction is what else is done with a transaction a notice names, as
	// the MoR answers it now: what it brings the attempts whose keys it
	// carries, and their invoices, to -- the MoR's collector's
	// (`server/invoices/merchant`, `Collector.Heard`), which `cmd.Build`
	// wires. Nil does nothing more; an error leaves the notice to be taken
	// again.
	Transaction func(ctx context.Context, n *ent.Notice, t mor.Transaction) error

	// Batch is the most notices one pass takes: 100 when zero.
	Batch int
}

// Take takes what waits in the inbox, oldest first, and answers how many it
// took for good. A notice the MoR could not be asked about is left to be
// taken again later -- longer after each try, an hour at most -- and said in
// the log.
func (t Taker) Take(ctx context.Context) (int, error) {
	batch := t.Batch
	if batch <= 0 {
		batch = 100
	}
	now := t.In.now()
	ns, err := t.In.Db.Notice.Query().
		Where(
			entnotice.Mor(t.In.Channel.Name()),
			entnotice.DateDoneIsNil(),
			entnotice.Or(entnotice.DateRetryIsNil(), entnotice.DateRetryLTE(now)),
		).
		Order(ent.Asc(entnotice.FieldDateCreated), ent.Asc(entnotice.FieldId)).
		Limit(batch).
		All(ctx)
	if err != nil {
		return 0, err
	}

	done := 0
	for _, n := range ns {
		if err := t.take(ctx, n); err != nil {
			tries := n.Tries + 1
			l := log.From(ctx).With(slog.String("event", n.MorEvent), slog.String("type", n.Type), slog.Int("tries", int(tries)))
			l.WarnContext(ctx, "mor: a notice could not be taken, and is taken again later", slog.Any("err", err))
			if err := t.In.Db.Notice.UpdateOneId(n.Id).
				SetTries(tries).
				SetError(err.Error()).
				SetDateRetry(now.Add(backoff(tries))).
				Exec(ctx); err != nil {
				return done, err
			}
			continue
		}
		done++
	}

	return done, nil
}

// backoff is how long after its `tries`th failed try a notice is taken again:
// ten seconds, doubled each time, an hour at most.
func backoff(tries int32) time.Duration {
	d := 10 * time.Second
	for range tries - 1 {
		d *= 2
		if d >= time.Hour {
			return time.Hour
		}
	}

	return d
}

// take is notice `n` taken: what it names asked of the MoR and written down,
// and the notice with what came of it. An error is a try that failed, to be
// tried again.
func (t Taker) take(ctx context.Context, n *ent.Notice) error {
	rec := t.In.Db.Notice.UpdateOneId(n.Id).SetDateDone(t.In.now()).ClearDateRetry()

	switch levyan.NoticeKind(n.Kind) {
	case levyan.NoticeKind_NOTICE_KIND_SUBSCRIPTION, levyan.NoticeKind_NOTICE_KIND_TRANSACTION:
	default:
		return rec.SetOutcome(int32(levyan.NoticeOutcome_NOTICE_OUTCOME_DROPPED)).Exec(ctx)
	}

	r, err := t.Subs.Channel.Recheck(ctx, noticeOf(n))
	if err != nil {
		if mor.KindOf(err) == mor.Invalid {
			// Never to be asked about: the MoR refuses what it names.
			return rec.SetOutcome(int32(levyan.NoticeOutcome_NOTICE_OUTCOME_REFUSED)).SetError(err.Error()).Exec(ctx)
		}
		return err
	}

	var heard Heard
	switch {
	case r.Subscription != nil:
		heard, err = t.Subs.Heard(ctx, *r.Subscription)
	case r.Transaction != nil:
		rec.SetTransaction(snapshot(*r.Transaction))
		heard, err = t.Subs.HeardTransaction(ctx, *r.Transaction)
		if err == nil && t.Transaction != nil {
			err = t.Transaction(ctx, n, *r.Transaction)
		}
	default:
		return errors.New("the merchant of record answered nothing of what the notice names")
	}
	if err != nil {
		return err
	}

	rec.SetOutcome(int32(heard.Outcome))
	if heard.BillingAccount != (uuid.UUID{}) {
		rec.SetBillingAccountId(heard.BillingAccount)
	}
	if heard.Mirror != (uuid.UUID{}) {
		rec.SetSubscriptionId(heard.Mirror)
	}
	if heard.Checkout != (uuid.UUID{}) {
		rec.SetCheckoutId(heard.Checkout)
	}

	return rec.Exec(ctx)
}

// Heard is what came of an answer of the MoR's a notice led to: what it was
// found to be about, and whether anything was written.
type Heard struct {
	Outcome        levyan.NoticeOutcome
	BillingAccount uuid.UUID
	Mirror         uuid.UUID
	Checkout       uuid.UUID
}

// Heard writes down what the MoR answered of subscription `sub`, which a
// notice named: on its mirror, or -- for one levyan has none of yet, which a
// checkout of levyan's made -- by completing that checkout. A subscription no
// checkout of levyan's made is foreign, and nothing is written of it.
func (s Subscriptions) Heard(ctx context.Context, sub mor.Subscription) (Heard, error) {
	db := s.db()
	m, err := db.Subscription.Query().
		Where(entsubscription.Mor(s.Channel.Name()), entsubscription.MorSubscription(sub.ID)).
		Only(ctx)
	switch {
	case err == nil:
		v, moved, err := s.observe(ctx, m.BillingAccountId, sub, nil)
		if err != nil {
			return Heard{}, err
		}
		out := Heard{Outcome: levyan.NoticeOutcome_NOTICE_OUTCOME_UNCHANGED, BillingAccount: v.BillingAccountId, Mirror: v.Id}
		if moved {
			out.Outcome = levyan.NoticeOutcome_NOTICE_OUTCOME_APPLIED
		}
		return out, nil
	case !ent.IsNotFound(err):
		return Heard{}, err
	}

	// A subscription levyan has no mirror of: one a checkout of levyan's
	// made, whose own notice has not been taken yet, or nobody's here.
	ba, err := pdid.Parse(sub.Account)
	if err != nil {
		return Heard{Outcome: levyan.NoticeOutcome_NOTICE_OUTCOME_FOREIGN}, nil
	}
	cos, err := db.Checkout.Query().
		Where(entcheckout.BillingAccountId(ba.Uuid()), entcheckout.Mor(s.Channel.Name()), entcheckout.State(int32(levyan.CheckoutState_CHECKOUT_STATE_OPEN))).
		Order(ent.Asc(entcheckout.FieldDateCreated)).
		All(ctx)
	if err != nil {
		return Heard{}, err
	}
	open := false
	for _, co := range cos {
		v, m, err := s.finish(ctx, co)
		if err != nil {
			return Heard{}, err
		}
		if m != nil && m.MorSubscription == sub.ID {
			return Heard{Outcome: levyan.NoticeOutcome_NOTICE_OUTCOME_APPLIED, BillingAccount: ba.Uuid(), Mirror: m.Id, Checkout: v.Id}, nil
		}
		open = open || levyan.CheckoutState(v.State) == levyan.CheckoutState_CHECKOUT_STATE_OPEN
	}
	if open {
		// Its checkout is not finished as the MoR answers it yet: asked
		// again later.
		return Heard{}, fmt.Errorf("subscription %s: its checkout is not finished yet, as the merchant of record answers it", sub.ID)
	}

	return Heard{Outcome: levyan.NoticeOutcome_NOTICE_OUTCOME_FOREIGN}, nil
}

// HeardTransaction writes down what the MoR answered of transaction `t`,
// which a notice named: the checkout it is completed, if it is one of
// levyan's, and on the mirror of the subscription it is of, how the payer
// pays and whether a payment waits for them. One of nothing of levyan's is
// foreign.
func (s Subscriptions) HeardTransaction(ctx context.Context, t mor.Transaction) (Heard, error) {
	out := Heard{Outcome: levyan.NoticeOutcome_NOTICE_OUTCOME_FOREIGN}
	co, err := s.db().Checkout.Query().
		Where(entcheckout.Mor(s.Channel.Name()), entcheckout.MorTransaction(t.ID)).
		Only(ctx)
	switch {
	case err == nil:
		out.Outcome, out.BillingAccount, out.Checkout = levyan.NoticeOutcome_NOTICE_OUTCOME_UNCHANGED, co.BillingAccountId, co.Id
		if levyan.CheckoutState(co.State) == levyan.CheckoutState_CHECKOUT_STATE_OPEN {
			v, m, err := s.finish(ctx, co)
			if err != nil {
				return Heard{}, err
			}
			if levyan.CheckoutState(v.State) != levyan.CheckoutState_CHECKOUT_STATE_OPEN {
				out.Outcome = levyan.NoticeOutcome_NOTICE_OUTCOME_APPLIED
			}
			if m != nil {
				out.Mirror = m.Id
			}
		}
	case !ent.IsNotFound(err):
		return Heard{}, err
	}

	m, moved, err := s.ObserveTransaction(ctx, t)
	if err != nil {
		return Heard{}, err
	}
	if m != nil {
		out.BillingAccount, out.Mirror = m.BillingAccountId, m.Id
		if moved {
			out.Outcome = levyan.NoticeOutcome_NOTICE_OUTCOME_APPLIED
		} else if out.Outcome == levyan.NoticeOutcome_NOTICE_OUTCOME_FOREIGN {
			out.Outcome = levyan.NoticeOutcome_NOTICE_OUTCOME_UNCHANGED
		}
	}

	return out, nil
}

// Takes is the loop [InboxLoop]: [Taker.Take] every `every`, and as soon as
// the inbox is woken ([Inbox.Wake]) -- a webhook kept, in this process. A
// pass that fails is said in the log, and the next tries again.
func Takes(t Taker, every time.Duration) spin.Spinner {
	batch := t.Batch
	if batch <= 0 {
		batch = 100
	}
	return spin.Called(InboxLoop, spin.Func(func(ctx context.Context) error {
		for {
			// A whole batch taken: there may be more waiting.
			for {
				n, err := t.Take(ctx)
				if err != nil {
					log.From(ctx).WarnContext(ctx, "mor: taking the inbox", slog.Any("err", err))
					break
				}
				if n < batch {
					break
				}
			}

			select {
			case <-ctx.Done():
				return nil
			case <-t.In.wake:
			case <-time.After(every):
			}
		}
	}))
}

// Puller reads the merchant of record's list of its notices into the inbox,
// from where the last read stopped ([levyan.NoticeCursor]).
type Puller struct {
	In *Inbox

	// Now is the MoR's channel's clock, which a notice's time is read
	// against: what is newer than [Overlap] by it is read again next time.
	Now func() time.Time

	// Pages is the most pages one pull reads: 10 when zero.
	Pages int
}

func (p Puller) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}

	return p.Now()
}

// Pull reads what the MoR noticed since the last pull into the inbox, kept
// once by its id beside the webhooks of it, and answers how many it kept
// that the inbox did not have. The first pull reads from the first checkout
// levyan began with the MoR, less [Overlap]; with none begun, the MoR has
// nothing of levyan's, and nothing is read.
//
// It stops reading past a notice that is less than [Overlap] old: the next
// pull reads it again, with whatever the MoR has listed before it since.
func (p Puller) Pull(ctx context.Context) (int, error) {
	db, name := p.In.Db, p.In.Channel.Name()
	cur, err := db.NoticeCursor.Query().Where(entnoticecursor.Mor(name)).Only(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return 0, err
	}

	r := mor.Read{}
	if cur != nil && cur.MorEvent != "" {
		r.After = cur.MorEvent
	} else {
		first, err := db.Checkout.Query().
			Where(entcheckout.Mor(name)).
			Order(ent.Asc(entcheckout.FieldDateCreated)).
			First(ctx)
		if ent.IsNotFound(err) {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		r.Since = first.DateCreated.Add(-Overlap)
	}

	pages := p.Pages
	if pages <= 0 {
		pages = 10
	}
	kept, past, settled := 0, "", true
	for range pages {
		got, err := p.In.Channel.Notices(ctx, r)
		if err != nil {
			return kept, err
		}
		recent := p.now().Add(-Overlap)
		for _, n := range got.Notices {
			ok, err := p.In.Keep(ctx, n, levyan.NoticeVia_NOTICE_VIA_EVENTS)
			if err != nil {
				return kept, err
			}
			if ok {
				kept++
			}
			if settled && n.At.After(recent) {
				settled = false
			}
			if settled {
				past = n.ID
			}
		}
		if settled && got.Last != "" {
			past = got.Last
		}
		if !got.More || got.Last == "" {
			break
		}
		r = mor.Read{After: got.Last}
	}

	if err := p.mark(ctx, cur, past); err != nil {
		return kept, err
	}
	if kept > 0 {
		p.In.Wake()
	}

	return kept, nil
}

// mark writes down that the list was read, and read past `past` when it is a
// notice's id.
func (p Puller) mark(ctx context.Context, cur *ent.NoticeCursor, past string) error {
	db, now := p.In.Db, p.In.now()
	if cur == nil {
		err := db.NoticeCursor.Create().
			SetId(pdid.New(pd.NoticeCursorDomain).Uuid()).
			SetMor(p.In.Channel.Name()).
			SetMorEvent(past).
			SetDateRead(now).
			SetDateCreated(now).
			Exec(ctx)
		if ent.IsConstraintError(err) {
			return nil
		}
		return err
	}
	u := db.NoticeCursor.UpdateOneId(cur.Id).SetDateRead(now)
	if past != "" {
		u.SetMorEvent(past)
	}

	return u.Exec(ctx)
}

// Pulls is the loop [EventsLoop]: [Puller.Pull] every `every`. A pass that
// fails is said in the log, and the next reads from where the last stopped.
func Pulls(p Puller, every time.Duration) spin.Spinner {
	return spin.Called(EventsLoop, spin.Every(every, func(ctx context.Context) error {
		if _, err := p.Pull(ctx); err != nil {
			log.From(ctx).WarnContext(ctx, "mor: reading the merchant of record's events", slog.Any("err", err))
		}
		return nil
	}))
}

// kindOf is what a notice of package mor's is about, as the schema writes it.
func kindOf(t mor.NoticeType) levyan.NoticeKind {
	switch t {
	case mor.NoticeTransaction:
		return levyan.NoticeKind_NOTICE_KIND_TRANSACTION
	case mor.NoticeSubscription:
		return levyan.NoticeKind_NOTICE_KIND_SUBSCRIPTION
	default:
		return levyan.NoticeKind_NOTICE_KIND_OTHER
	}
}

// noticeOf is notice `n` as package mor has one, to recheck.
func noticeOf(n *ent.Notice) mor.Notice {
	v := mor.Notice{
		ID:           n.MorEvent,
		Event:        n.Type,
		Transaction:  n.MorTransaction,
		Subscription: n.MorSubscription,
		Adjustment:   n.MorAdjustment,
	}
	switch levyan.NoticeKind(n.Kind) {
	case levyan.NoticeKind_NOTICE_KIND_TRANSACTION:
		v.Type = mor.NoticeTransaction
	case levyan.NoticeKind_NOTICE_KIND_SUBSCRIPTION:
		v.Type = mor.NoticeSubscription
	}
	if n.DateOccurred != nil {
		v.At = *n.DateOccurred
	}

	return v
}

// snapshot is what the MoR answered of transaction `t`, as a notice keeps it:
// nothing of who paid but the kind of method.
func snapshot(t mor.Transaction) *levyan.NoticeTransaction {
	v := levyan.NoticeTransaction_builder{
		Status:          t.Status.String(),
		Progress:        int32(t.Progress()),
		MorSubscription: t.Subscription,
		MorCustomer:     t.Customer,
		Total:           t.Total.Minor,
		Currency:        t.Total.Currency,
		AwaitingPayer:   t.Awaiting,
	}.Build()
	if !t.At.IsZero() {
		v.SetDatePaid(timestamppb.New(t.At))
	}
	if t.Method != nil {
		v.SetMethodType(t.Method.Type)
	}
	if f := t.Failure; f != nil {
		v.SetFailure(f.Kind.String())
		v.SetFailureCode(f.Code)
	}
	for _, l := range t.Lines {
		v.SetLines(append(v.GetLines(), levyan.NoticeLine_builder{
			MorLine: l.ID,
			Key:     l.Key,
			Amount:  l.Amount.Minor,
			Tax:     l.Tax.Minor,
			Total:   l.Total.Minor,
		}.Build()))
	}
	for _, a := range t.Adjustments {
		v.SetAdjustments(append(v.GetAdjustments(), levyan.NoticeAdjustment_builder{
			MorAdjustment: a.ID,
			Kind:          a.Kind.String(),
			Status:        a.Status.String(),
			Amount:        a.Amount.Minor,
			Key:           a.Key,
			DateCreated:   timestamppb.New(a.At),
		}.Build()))
	}

	return v
}
