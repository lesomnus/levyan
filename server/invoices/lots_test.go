package invoices_test

import (
	"context"
	"testing"
	"time"
	"uuid"

	"github.com/lesomnus/payday/pdid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	"github.com/lesomnus/levyan/cmd"
	"github.com/lesomnus/levyan/internal/ent"
	entcredit "github.com/lesomnus/levyan/internal/ent/credit"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/server/invoices"
)

// What an operator grants (#43): money taken off what is billed, of a
// workspace's lines alone or of a billing account's whole invoice, the
// soonest to expire first and the balance last, never once it has expired --
// and then taken off it, by a row of its own -- never by a shadow's draft,
// never in another product, and back to it when what it paid is taken back.

// grant is credit of `amount` granted by an operator: to workspace `ws`, or
// to billing account `ba` when `ws` is none, expiring at `expires` -- a year
// from now when zero.
func (w world) grant(ws, ba pdid.Id, amount int64, expires time.Time) pdid.Id {
	w.t.Helper()
	l, err := w.S.Invoices.Grant(w.Operator(w.ctx, "alice"), invoices.Grant{
		Product: w.product, Workspace: ws, BillingAccount: ba, Amount: amount, Expires: expires, Reason: "2027 봄 프로모션",
	})
	w.X.NoError(err)
	return pdid.Id(l.Row.Id)
}

// left is what is left of lot `id`.
func (w world) left(id pdid.Id) int64 {
	w.t.Helper()
	ls, err := invoices.Lots(w.ctx, w.S.Ent, w.product.Uuid(), entcredit.Id(id.Uuid()))
	w.X.NoError(err)
	w.X.Len(ls, 1)
	return ls[0].Left
}

// spent is what each lot paid of invoice `inv`, and the balance under none.
func (w world) spent(inv *ent.Invoice) map[pdid.Id]int64 {
	w.t.Helper()
	vs, err := w.S.Ent.Credit.Query().Where(entcredit.InvoiceId(inv.Id), entcredit.Kind(int32(levyan.CreditKind_CREDIT_KIND_APPLIED))).All(w.ctx)
	w.X.NoError(err)
	out := map[pdid.Id]int64{}
	for _, v := range vs {
		k := pdid.Nil
		if v.Lot != nil {
			k = pdid.Id(*v.Lot)
		}
		out[k] -= v.Amount
	}
	return out
}

// asPayer is a context whose calls the product relays for `account`.
func (w world) asPayer(account pdid.Id) context.Context {
	return levyantest.AsAccount(w.ctx, w.key, account)
}

// clean is a pass of reconciliation that makes every check, and finds
// nothing wrong of credit nor of what an invoice says it was paid with.
func (w world) clean() {
	w.t.Helper()
	w.S.Reconciler.Pace = 0
	rep, err := w.S.Reconciler.Run(w.ctx)
	w.X.NoError(err)
	for _, c := range rep.Checks {
		w.X.Empty(c.Err, "check %s", c.Name)
	}
	fs, err := w.S.Ent.Finding.Query().All(w.ctx)
	w.X.NoError(err)
	for _, f := range fs {
		switch f.Kind {
		case levyan.FindingKind_FINDING_KIND_CREDIT_DIFFERS.String(),
			levyan.FindingKind_FINDING_KIND_INVOICE_PAID_DIFFERS.String(),
			levyan.FindingKind_FINDING_KIND_CHECK_FAILED.String():
			w.X.Failf("reconciliation found something", "%s %s: %s", f.Kind, f.Subject, f.Summary)
		}
	}
}

// A workspace's credit pays its own lines alone, on whichever invoice they
// are: of January's 10,000 -- 5,000 of each workspace -- the 7,000 granted to
// one pays its 5,000, and what is left of it waits for its lines to come to
// something. The channel is asked for what is left.
func TestAWorkspacesCreditPaysItsOwnLinesAlone(t *testing.T) {
	w := setup(t, mor)
	x := w.X

	a, b := levyantest.Tenant(), levyantest.Tenant()
	_, ba := w.payer(a, b)
	lot := w.grant(a, pdid.Nil, 7000, time.Time{})

	w.At(on(2, 10))
	w.daily(a, on(1, 1), on(2, 1), 200)
	w.daily(b, on(1, 1), on(2, 1), 200)
	w.close(on(2, 10))
	w.run()
	jan := w.only(ba)
	x.Equal(int64(10000), jan.Total)
	x.Equal(int64(5000), jan.Credited, "a's lines")
	x.Equal(int64(5000), jan.Due)
	x.Equal(map[pdid.Id]int64{lot: 5000}, w.spent(jan))
	x.Equal(int64(2000), w.left(lot))
	sends := w.ch.Sends()
	x.Equal(int64(5000), sends[len(sends)-1].Attempt.Amount.Minor, "the channel sees what is left")

	// February: a uses nothing past what is included, so its credit pays
	// nothing of b's.
	w.At(on(3, 10))
	w.daily(a, on(2, 1), on(3, 1), 50)
	w.daily(b, on(2, 1), on(3, 1), 200)
	w.close(on(3, 10))
	w.run()
	feb := w.invoices(ba)[1]
	x.Zero(feb.Credited)
	x.Equal(int64(5000), feb.Due)
	x.Equal(int64(2000), w.left(lot))
	x.Zero(w.balance(ba), "a grant is none of the balance")

	w.clean()
}

// A billing account's credit pays its whole invoice, in its product.
func TestABillingAccountsCreditPaysItsWholeInvoice(t *testing.T) {
	w := setup(t, mor)
	x := w.X

	a, b := levyantest.Tenant(), levyantest.Tenant()
	account, ba := w.payer(a, b)
	lot := w.grant(pdid.Nil, ba, 7000, time.Time{})

	w.At(on(2, 10))
	w.daily(a, on(1, 1), on(2, 1), 200)
	w.daily(b, on(1, 1), on(2, 1), 200)
	w.close(on(2, 10))
	w.run()
	jan := w.only(ba)
	x.Equal(int64(7000), jan.Credited)
	x.Equal(int64(3000), jan.Due)
	x.Zero(w.left(lot))

	// What the payer reads of it.
	res, err := levyanv1.NewPayerServiceClient(w.conn).GetInvoice(w.asPayer(account), levyanv1.GetInvoiceRequest_builder{Id: id(jan).String()}.Build())
	x.NoError(err)
	x.Len(res.GetInvoice().GetCredits(), 1)
	c := res.GetInvoice().GetCredits()[0]
	x.Equal(lot.String(), c.GetCredit())
	x.Equal(int64(7000), c.GetAmount())
	x.Empty(c.GetWorkspace())

	w.clean()
}

// Of what may pay an invoice, what expires soonest is spent first -- a
// workspace's or a billing account's -- and the balance, which never
// expires, last.
func TestTheCreditThatExpiresSoonestIsSpentFirst(t *testing.T) {
	w := setup(t, mor)
	x := w.X

	a := levyantest.Tenant()
	_, ba := w.payer(a)
	late := w.grant(pdid.Nil, ba, 3000, on(12, 1))
	soon := w.grant(pdid.Nil, ba, 1000, on(6, 1))
	sooner := w.grant(a, pdid.Nil, 1500, on(4, 1))

	w.At(on(2, 10))
	w.daily(a, on(1, 1), on(2, 1), 200)
	w.close(on(2, 10))
	w.run()
	jan := w.only(ba)
	x.Equal(int64(5000), jan.Credited)
	x.Zero(jan.Due)
	x.Equal(paid, jan.State)
	x.Equal(map[pdid.Id]int64{sooner: 1500, soon: 1000, late: 2500}, w.spent(jan))
	x.Equal(int64(500), w.left(late))

	// What a credit note takes back of what they paid goes back to them,
	// the latest to expire first, so that it lasts as long as it may: 2,000
	// of January, all of it the latest's.
	_, err := w.S.Invoices.Credit(w.ctx, id(jan), invoices.Credit{Reason: levyan.CreditNoteReason_CREDIT_NOTE_REASON_CORRECTION, Amount: 2000})
	x.NoError(err)
	x.Equal(int64(2500), w.left(late))
	x.Zero(w.left(soon))
	x.Zero(w.left(sooner))
	x.Zero(w.balance(ba))

	w.At(on(3, 10))
	w.daily(a, on(2, 1), on(3, 1), 200)
	w.close(on(3, 10))
	w.run()
	feb := w.invoices(ba)[1]
	x.Equal(map[pdid.Id]int64{late: 2500}, w.spent(feb))
	x.Equal(int64(2500), feb.Due)

	w.clean()
}

// The balance -- what a billing account is owed, which never expires -- is
// spent after what was granted: of February's 5,000, the 2,000 granted, and
// then the 1,000 January's transfer paid past what was due.
func TestTheBalanceIsSpentAfterWhatWasGranted(t *testing.T) {
	w := setup(t, mor)
	x := w.X
	w.ch.Push = false

	a := levyantest.Tenant()
	_, ba := w.payer(a)
	w.At(on(2, 10))
	w.daily(a, on(1, 1), on(2, 1), 200)
	w.close(on(2, 10))
	w.run()
	jan := w.only(ba)
	_, err := w.S.Invoices.Record(w.ctx, id(jan), 6000, on(2, 10), "수민")
	x.NoError(err)
	x.Equal(int64(1000), w.balance(ba))
	lot := w.grant(pdid.Nil, ba, 2000, time.Time{})

	w.At(on(3, 10))
	w.daily(a, on(2, 1), on(3, 1), 200)
	w.close(on(3, 10))
	w.run()
	feb := w.invoices(ba)[1]
	x.Equal(int64(3000), feb.Credited)
	x.Equal(int64(2000), feb.Due)
	x.Equal(map[pdid.Id]int64{lot: 2000, pdid.Nil: 1000}, w.spent(feb))
	x.Zero(w.balance(ba))

	w.clean()
}

// Credit that expired is never spent, and what was left of it is taken off
// it by a row of its own, so that it comes to nothing and says where it
// went: granted until the 5th of February, it pays nothing of the January
// invoice decided on the 10th.
func TestCreditThatExpiredIsNotSpentAndSaysSo(t *testing.T) {
	w := setup(t, mor)
	x := w.X

	a := levyantest.Tenant()
	_, ba := w.payer(a)
	lot := w.grant(a, pdid.Nil, 7000, on(2, 5))

	w.At(on(2, 10))
	w.daily(a, on(1, 1), on(2, 1), 200)
	w.close(on(2, 10))

	// Decided before anything took what was left of it off, the invoice is
	// paid nothing of it all the same.
	x.NoError(w.S.Invoices.Account(w.ctx, w.product.Uuid(), ba.Uuid()))
	jan := w.only(ba)
	x.Zero(jan.Credited)
	x.Equal(int64(5000), jan.Due)
	x.Equal(int64(7000), w.left(lot), "not taken off yet")

	w.run()
	x.Zero(w.left(lot))

	vs, err := w.S.Ent.Credit.Query().Where(entcredit.Lot(lot.Uuid())).Order(ent.Asc(entcredit.FieldDateCreated)).All(w.ctx)
	x.NoError(err)
	x.Len(vs, 2)
	x.Equal(int32(levyan.CreditKind_CREDIT_KIND_GRANTED), vs[0].Kind)
	x.Equal(int32(levyan.CreditKind_CREDIT_KIND_EXPIRED), vs[1].Kind)
	x.Equal(int64(-7000), vs[1].Amount)
	x.Equal(uuid.UUID{}, vs[1].OperatorId, "the deployment's, as it expired")

	// Another pass takes nothing more.
	w.run()
	n, err := w.S.Ent.Credit.Query().Where(entcredit.Lot(lot.Uuid())).Count(w.ctx)
	x.NoError(err)
	x.Equal(2, n)

	// An operator ends one before it expires, with why; one spent or
	// expired is refused.
	other := w.grant(a, pdid.Nil, 1000, time.Time{})
	_, err = w.S.Invoices.Expire(w.ctx, other, "잘못 줌")
	x.Equal(codes.PermissionDenied, status.Code(err), "an operator's")
	_, err = w.S.Invoices.Expire(w.Operator(w.ctx, "bob"), other, "")
	x.Equal(codes.InvalidArgument, status.Code(err), "with why")
	got, err := w.S.Invoices.Expire(w.Operator(w.ctx, "bob"), other, "잘못 줌")
	x.NoError(err)
	x.Zero(got.Left)
	_, err = w.S.Invoices.Expire(w.Operator(w.ctx, "bob"), other, "again")
	x.Equal(codes.FailedPrecondition, status.Code(err))
	_, err = w.S.Invoices.Expire(w.Operator(w.ctx, "bob"), lot, "again")
	x.Equal(codes.FailedPrecondition, status.Code(err))

	w.clean()
}

// A shadow's draft spends nothing of what was granted: it is never
// collected, and what was granted waits for the first invoice that is.
func TestAShadowSpendsNoCredit(t *testing.T) {
	w := setup(t, mor)
	x := w.X
	w.S.Invoices.LiveFrom[mor] = on(3, 1)

	a := levyantest.Tenant()
	_, ba := w.payer(a)
	lot := w.grant(a, pdid.Nil, 7000, time.Time{})

	w.At(on(2, 10))
	w.daily(a, on(1, 1), on(2, 1), 200)
	w.close(on(2, 10))
	w.run()
	jan := w.only(ba)
	x.Equal(int32(levyan.InvoiceState_INVOICE_STATE_SHADOW), jan.State)
	x.Zero(jan.Credited)
	x.Equal(int64(7000), w.left(lot))

	w.At(on(3, 10))
	w.daily(a, on(2, 1), on(3, 1), 200)
	w.close(on(3, 10))
	w.run()
	vs := w.invoices(ba)
	x.Len(vs, 2)
	x.Equal(int64(5000), vs[1].Credited, "the first collected")
	x.Equal(int64(2000), w.left(lot))

	w.clean()
}

// Credit is the product's it was granted in: a billing account that pays in
// two products spends what one granted on that one's invoices alone, and
// is shown it by that one alone.
func TestCreditIsTheProductsItWasGrantedIn(t *testing.T) {
	w := setup(t, mor)
	x := w.X

	a := levyantest.Tenant()
	account, ba := w.payer(a)
	w.grant(pdid.Nil, ba, 7000, time.Time{})

	other := w.Product("other")
	other = w.Terms(other, "UTC", mor, "KRW")
	okey, _ := w.Key(other, "other-prod", nil)
	things := w.Meter(other, "other.things", "{thing}", stock, twa)
	w.Price(other, things, mor, "KRW", 0, 10, on(1, 1))
	op, err := pdid.From(other.GetId())
	x.NoError(err)
	ows := levyantest.Tenant()
	_, err = w.S.Payers.Arrive(w.ctx, op, ows, account, on(1, 1))
	x.NoError(err)

	w.At(on(2, 10))
	w.daily(a, on(1, 1), on(2, 1), 200)
	res, err := levyanv1.NewMeteringServiceClient(w.conn).Record(levyantest.As(w.ctx, okey), levyanv1.RecordRequest_builder{Readings: []*levyanv1.Reading{
		levyanv1.Reading_builder{Workspace: ows.String(), Meter: "other.things", MeasuredAt: timestamppb.New(on(1, 1)), Value: 300, Unit: "{thing}"}.Build(),
	}}.Build())
	x.NoError(err)
	x.Equal(levyanv1.Outcome_OUTCOME_KEPT, res.GetResults()[0].GetOutcome())
	w.close(on(2, 10))
	p, err := w.S.Ent.Tenant.Get(w.ctx, op.Uuid())
	x.NoError(err)
	_, err = w.S.Closer.Close(w.ctx, p, on(2, 10))
	x.NoError(err)
	w.run()

	for _, inv := range w.invoices(ba) {
		if inv.TenantId == op.Uuid() {
			x.Zero(inv.Credited, "the other product's invoice")
			x.Greater(inv.Due, int64(0))
		} else {
			x.Equal(int64(5000), inv.Credited)
		}
	}

	// The payer is shown what is left of it by the product it was granted
	// in, and not by the other.
	got, err := levyanv1.NewPayerServiceClient(w.conn).GetBillingAccount(w.asPayer(account), levyanv1.GetBillingAccountRequest_builder{Id: ba.String()}.Build())
	x.NoError(err)
	x.Len(got.GetCredits(), 1)
	x.Equal(int64(2000), got.GetCredits()[0].GetLeft())
	x.Equal(int64(7000), got.GetCredits()[0].GetGranted())
	x.Equal(ba.String(), got.GetCredits()[0].GetBillingAccount())
	got, err = levyanv1.NewPayerServiceClient(w.conn).GetBillingAccount(levyantest.AsAccount(w.ctx, okey, account), levyanv1.GetBillingAccountRequest_builder{Id: ba.String()}.Build())
	x.NoError(err)
	x.Empty(got.GetCredits())

	w.clean()
}

// A workspace's credit follows the workspace: once another billing account
// pays for it, it pays its lines there. And the product reads what is left
// of it with its payer.
func TestAWorkspacesCreditFollowsItToAnotherPayer(t *testing.T) {
	w := setup(t, mor)
	x := w.X

	a := levyantest.Tenant()
	_, first := w.payer(a)
	lot := w.grant(a, pdid.Nil, 7000, time.Time{})
	other, second := w.payer()

	w.At(on(1, 15))
	o, err := w.S.Payers.Offer(w.ctx, w.product, a, other)
	x.NoError(err)
	oid, err := pdid.From(o.GetId())
	x.NoError(err)
	_, err = w.S.Payers.Accept(w.ctx, w.product, other, oid, second)
	x.NoError(err)

	w.At(on(3, 10))
	w.daily(a, on(1, 1), on(3, 1), 200)
	w.close(on(3, 10))
	w.run()
	x.Equal(int64(5000), w.only(first).Credited, "January, the first payer's")
	x.Equal(int64(2000), w.only(second).Credited, "February, the second's: what was left")
	x.Zero(w.left(lot))

	got, err := levyanv1.NewWorkspaceServiceClient(w.conn).GetPayer(w.as, levyanv1.GetPayerRequest_builder{Workspace: a.String()}.Build())
	x.NoError(err)
	x.Empty(got.GetPayer().GetCredits(), "spent")

	more := w.grant(a, pdid.Nil, 3000, on(9, 1))
	got, err = levyanv1.NewWorkspaceServiceClient(w.conn).GetPayer(w.as, levyanv1.GetPayerRequest_builder{Workspace: a.String()}.Build())
	x.NoError(err)
	x.Len(got.GetPayer().GetCredits(), 1)
	c := got.GetPayer().GetCredits()[0]
	x.Equal(more.String(), c.GetId())
	x.Equal(a.String(), c.GetWorkspace())
	x.Equal(int64(3000), c.GetLeft())
	x.True(c.GetExpires().AsTime().Equal(on(9, 1)))

	w.clean()
}

// What a credit note takes back of what a lot paid goes back to the lot --
// never to the balance, which never expires: voided, the invoice gives the
// lot back what it paid, and its billing account's balance is as it was.
func TestWhatALotPaidOfAnInvoiceTakenBackIsTheLotsAgain(t *testing.T) {
	w := setup(t, mor)
	x := w.X
	w.ch.Push = false

	a := levyantest.Tenant()
	_, ba := w.payer(a)
	lot := w.grant(a, pdid.Nil, 3000, time.Time{})

	w.At(on(2, 10))
	w.daily(a, on(1, 1), on(2, 1), 200)
	w.close(on(2, 10))
	w.run()
	jan := w.only(ba)
	x.Equal(int64(3000), jan.Credited)
	x.Equal(open, jan.State)
	x.Zero(w.left(lot))

	_, err := w.S.Invoices.Void(w.ctx, id(jan), "계약 해지")
	x.NoError(err)
	x.Equal(int64(3000), w.left(lot), "back to the lot")
	x.Zero(w.balance(ba), "and none of it the balance's")

	w.clean()
}

// On a channel levyan sells on itself, a workspace's credit pays its lines
// with their tax, as the balance pays the total.
func TestAWorkspacesCreditPaysItsLinesWithTheirTax(t *testing.T) {
	w := setup(t, pgkr, func(c *cmd.Config) {
		c.Payers.Channels = cmd.ChannelsConfig{"KR": {Channel: "PG_KR", Currency: "KRW"}}
	})
	x := w.X

	a, b := levyantest.Tenant(), levyantest.Tenant()
	_, ba := w.payer(a, b)
	w.grant(a, pdid.Nil, 7000, time.Time{})

	w.At(on(2, 10))
	w.daily(a, on(1, 1), on(2, 1), 200)
	w.daily(b, on(1, 1), on(2, 1), 200)
	w.close(on(2, 10))
	w.run()
	jan := w.only(ba)
	x.Equal(int64(10000), jan.Subtotal)
	x.Equal(int64(1000), jan.Tax)
	x.Equal(int64(5500), jan.Credited, "a's 5,000 and its tax")
	x.Equal(int64(5500), jan.Due)
}

// A lot an operator ended stays ended: what comes back to it after -- of an
// invoice it paid, voided -- is never spent, and is taken off it again.
func TestALotEndedStaysEnded(t *testing.T) {
	w := setup(t, mor)
	x := w.X
	w.ch.Push = false

	a, b := levyantest.Tenant(), levyantest.Tenant()
	_, ba := w.payer(a, b)
	lot := w.grant(a, pdid.Nil, 7000, time.Time{})

	w.At(on(2, 10))
	w.daily(a, on(1, 1), on(2, 1), 200)
	w.daily(b, on(1, 1), on(2, 1), 200)
	w.close(on(2, 10))
	w.run()
	jan := w.only(ba)
	x.Equal(int64(5000), jan.Credited)
	x.Equal(open, jan.State)
	x.Equal(int64(2000), w.left(lot))

	got, err := w.S.Invoices.Expire(w.Operator(w.ctx, "bob"), lot, "프로모션 철회")
	x.NoError(err)
	x.Zero(got.Left)
	x.False(got.Ended.IsZero())

	_, err = w.S.Invoices.Void(w.ctx, id(jan), "계약 해지")
	x.NoError(err)
	x.Equal(int64(5000), w.left(lot), "what it paid came back to it")
	res, err := levyanv1.NewWorkspaceServiceClient(w.conn).GetPayer(w.as, levyanv1.GetPayerRequest_builder{Workspace: a.String()}.Build())
	x.NoError(err)
	x.Empty(res.GetPayer().GetCredits(), "and it is over")

	w.At(on(3, 10))
	w.daily(a, on(2, 1), on(3, 1), 200)
	w.daily(b, on(2, 1), on(3, 1), 200)
	w.close(on(3, 10))
	x.NoError(w.S.Invoices.Account(w.ctx, w.product.Uuid(), ba.Uuid()))
	feb := w.invoices(ba)[1]
	x.Zero(feb.Credited, "an ended lot pays nothing, though something came back to it")
	x.Equal(int64(5000), w.left(lot))
	w.run()
	x.Zero(w.left(lot), "and what came back to it is taken off it again")
	_, err = w.S.Invoices.Expire(w.Operator(w.ctx, "bob"), lot, "again")
	x.Equal(codes.FailedPrecondition, status.Code(err))

	w.clean()
}
