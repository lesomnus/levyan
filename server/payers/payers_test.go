package payers_test

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/pdtest"
	"github.com/lesomnus/roster/standing"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/cmd"
	"github.com/lesomnus/levyan/internal/dunning"
	"github.com/lesomnus/levyan/internal/ent"
	entgrant "github.com/lesomnus/levyan/internal/ent/grant"
	entlink "github.com/lesomnus/levyan/internal/ent/link"
	entworkspace "github.com/lesomnus/levyan/internal/ent/workspace"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/server/grants"
	"github.com/lesomnus/levyan/server/payers"
)

// grace is how long a workspace may go with no payer before it is
// restricted, as the policy table's row of it says by default: thirty days,
// what `payers.grace` was.
var grace = dunning.Defaults().Payerless.Grace

// on is a day of 2027 at midnight UTC.
func on(month, d int) time.Time { return time.Date(2027, time.Month(month), d, 0, 0, 0, 0, time.UTC) }

type world struct {
	*levyantest.World
	t    *testing.T
	ctx  context.Context
	loft *levyan.Tenant
	p    pdid.Id
}

func setup(t *testing.T) world {
	w := levyantest.New(t)
	w.At(on(1, 15))
	loft := w.Product("loft")
	p, err := pdid.From(loft.GetId())
	w.X.NoError(err)

	return world{World: w, t: t, ctx: context.Background(), loft: loft, p: p}
}

func (w world) payers() payers.Payers { return w.S.Payers }

// account is a billing account of `owner`'s in `country`.
func (w world) account(owner pdid.Id, country string) pdid.Id {
	w.t.Helper()
	v, err := w.payers().Create(w.ctx, owner, levyan.BillingAccount_builder{
		Name: "회사용", Country: country, Recipient: "김수민", Email: "billing@example.com", Address: "서울",
		Business: levyan.BillingBusiness_builder{
			Registration: "123-45-67890", Name: "주식회사 예시", Representative: "김수민",
			Address: "서울", Kind: "서비스", Item: "소프트웨어", Email: "tax@example.com",
		}.Build(),
	}.Build())
	w.X.NoError(err)
	id, err := pdid.From(v.GetId())
	w.X.NoError(err)
	return id
}

func (w world) state(ws pdid.Id) payers.State {
	w.t.Helper()
	st, err := w.payers().State(w.ctx, w.p, ws)
	w.X.NoError(err)
	return st
}

func payer(l *ent.Link) pdid.Id {
	if l == nil {
		return pdid.Nil
	}
	return pdid.Id(l.Payer)
}

// A workspace is paid for by its creator from when the product says it was
// made, with the first billing account they made; said again, nothing
// changes. A workspace made in the future is no workspace made.
func TestAWorkspaceIsPaidForByItsCreator(t *testing.T) {
	w := setup(t)
	x := pdtest.NewX(t)

	a := levyantest.Account()
	first := w.account(a, "KR")
	w.account(a, "US")
	ws := levyantest.Tenant()

	st, err := w.payers().Arrive(w.ctx, w.p, ws, a, on(1, 3))
	x.NoError(err)
	x.NotNil(st.Current)
	x.Equal(a, payer(st.Current))
	x.Equal(first.Uuid(), st.Current.BillingAccountId, "the first they made")
	x.Equal(on(1, 3), st.Current.DateStart.UTC())
	x.Equal(int32(levyan.LinkBegan_LINK_BEGAN_ARRIVED), st.Current.Began)
	x.Nil(st.Next)
	x.False(st.Payerless())

	again, err := w.payers().Arrive(w.ctx, w.p, ws, levyantest.Account(), time.Time{})
	x.NoError(err)
	x.Equal(st.Current.Id, again.Current.Id, "said again, it changes nothing")

	none := levyantest.Account()
	st, err = w.payers().Arrive(w.ctx, w.p, levyantest.Tenant(), none, time.Time{})
	x.NoError(err)
	x.Equal(none, payer(st.Current))
	x.Equal(uuid.UUID{}, st.Current.BillingAccountId, "none made yet")
	x.Equal(on(1, 15), st.Current.DateStart.UTC(), "made now")

	_, err = w.payers().Arrive(w.ctx, w.p, levyantest.Tenant(), a, on(2, 1))
	x.Equal(codes.InvalidArgument, status.Code(err), "made ahead of now")
}

// Another account pays once it accepts, and from the end of the period it
// accepted in: the payer before it pays until then. What is offered is one
// at a time, answered once, and only by the account it was made to.
func TestANewPayerPaysFromTheEndOfThePeriodItAccepted(t *testing.T) {
	w := setup(t)
	x := pdtest.NewX(t)

	a, b, c := levyantest.Account(), levyantest.Account(), levyantest.Account()
	ws := levyantest.Tenant()
	_, err := w.payers().Arrive(w.ctx, w.p, ws, a, on(1, 3))
	x.NoError(err)

	_, err = w.payers().Offer(w.ctx, w.p, ws, a)
	x.Equal(codes.FailedPrecondition, status.Code(err), "it pays already")

	toC, err := w.payers().Offer(w.ctx, w.p, ws, c)
	x.NoError(err)
	toB, err := w.payers().Offer(w.ctx, w.p, ws, b)
	x.NoError(err)
	cId, _ := pdid.From(toC.GetId())
	bId, _ := pdid.From(toB.GetId())

	_, err = w.payers().Accept(w.ctx, w.p, c, cId, pdid.Nil)
	x.Equal(codes.FailedPrecondition, status.Code(err), "withdrawn by the offer after it")
	_, err = w.payers().Accept(w.ctx, w.p, c, bId, pdid.Nil)
	x.Equal(codes.NotFound, status.Code(err), "another's offer is no offer of theirs")
	_, err = w.payers().Accept(w.ctx, w.p, b, bId, w.account(c, "KR"))
	x.Equal(codes.NotFound, status.Code(err), "nor another's billing account theirs")

	ba := w.account(b, "US")
	l, err := w.payers().Accept(w.ctx, w.p, b, bId, ba)
	x.NoError(err)
	x.Equal(on(2, 1), l.GetDateStart().AsTime(), "from the end of the period")
	x.Equal(levyan.LinkBegan_LINK_BEGAN_ACCEPTED, l.GetBegan())

	st := w.state(ws)
	x.Equal(a, payer(st.Current), "who paid pays until then")
	x.Equal(on(2, 1), st.Current.DateEnd.UTC())
	x.Equal(int32(levyan.LinkEnded_LINK_ENDED_REPLACED), st.Current.Ended)
	x.Equal(b, payer(st.Next))
	x.Nil(st.Offer, "answered")

	_, err = w.payers().Accept(w.ctx, w.p, b, bId, ba)
	x.Equal(codes.FailedPrecondition, status.Code(err), "answered once")

	w.At(on(2, 2))
	st = w.state(ws)
	x.Equal(b, payer(st.Current))
	x.Nil(st.Next)

	// Declined, and nothing changes.
	toA, err := w.payers().Offer(w.ctx, w.p, ws, a)
	x.NoError(err)
	aId, _ := pdid.From(toA.GetId())
	x.NoError(w.payers().Decline(w.ctx, w.p, a, aId))
	x.Equal(b, payer(w.state(ws).Current))
}

// A payer pays with another of its billing accounts, or stops paying, from
// the end of the period; once it has stopped, the workspace has no payer.
func TestAChangeOfAPayersIsAtTheEndOfThePeriod(t *testing.T) {
	w := setup(t)
	x := pdtest.NewX(t)

	a := levyantest.Account()
	kr := w.account(a, "KR")
	ws := levyantest.Tenant()
	_, err := w.payers().Arrive(w.ctx, w.p, ws, a, on(1, 3))
	x.NoError(err)

	us := w.account(a, "US")
	l, err := w.payers().Use(w.ctx, w.p, a, ws, us)
	x.NoError(err)
	x.Equal(on(2, 1), l.GetDateStart().AsTime())
	st := w.state(ws)
	x.Equal(kr.Uuid(), st.Current.BillingAccountId, "until the end of the period")
	x.Equal(us.Uuid(), st.Next.BillingAccountId)

	_, err = w.payers().Stop(w.ctx, w.p, levyantest.Account(), ws)
	x.Equal(codes.FailedPrecondition, status.Code(err), "nobody else's to stop")

	_, err = w.payers().Stop(w.ctx, w.p, a, ws)
	x.NoError(err)
	st = w.state(ws)
	x.Equal(a, payer(st.Current), "it pays until the end of the period")
	x.Nil(st.Next, "and not after")
	x.False(st.Payerless())

	w.At(on(2, 3))
	st = w.state(ws)
	x.Nil(st.Current)
	x.True(st.Payerless())
	x.Equal(on(2, 1), st.Since)
	x.Equal(on(2, 1).Add(grace), st.Restricted)
}

// An account that leaves a workspace pays for it no longer, from now, and
// what was offered to it is withdrawn.
func TestAnAccountThatLeavesPaysNoLonger(t *testing.T) {
	w := setup(t)
	x := pdtest.NewX(t)

	a, b := levyantest.Account(), levyantest.Account()
	ws := levyantest.Tenant()
	_, err := w.payers().Arrive(w.ctx, w.p, ws, a, on(1, 3))
	x.NoError(err)
	o, err := w.payers().Offer(w.ctx, w.p, ws, b)
	x.NoError(err)
	oId, _ := pdid.From(o.GetId())

	st, err := w.payers().Leave(w.ctx, w.p, ws, b)
	x.NoError(err)
	x.Nil(st.Offer, "what was offered to it is withdrawn")
	_, err = w.payers().Accept(w.ctx, w.p, b, oId, pdid.Nil)
	x.Equal(codes.FailedPrecondition, status.Code(err))

	w.At(on(1, 20))
	st, err = w.payers().Leave(w.ctx, w.p, ws, a)
	x.NoError(err)
	x.Nil(st.Current)
	x.True(st.Payerless())
	x.Equal(on(1, 20), st.Since)

	again, err := w.payers().Leave(w.ctx, w.p, ws, a)
	x.NoError(err)
	x.Equal(st.Since, again.Since, "said again, it changes nothing")
}

// A workspace nobody pays for is restricted once that has lasted the grace
// -- a grant of the rule's, `loft.restricted` ON -- and the restriction ends
// once somebody accepts to pay, from when they accept. One that never had a
// payer is not restricted.
func TestAWorkspaceWithNoPayerIsRestrictedAfterItsGrace(t *testing.T) {
	w := setup(t)
	x := pdtest.NewX(t)

	a, b := levyantest.Account(), levyantest.Account()
	ws := levyantest.Tenant()
	_, err := w.payers().Arrive(w.ctx, w.p, ws, a, on(1, 3))
	x.NoError(err)
	_, err = w.payers().Leave(w.ctx, w.p, ws, a)
	x.NoError(err)

	restricted := func() []*ent.Grant {
		vs, err := w.S.Ent.Grant.Query().Where(entgrant.Key("loft.restricted")).All(w.ctx)
		x.NoError(err)
		return vs
	}

	w.At(on(1, 15).Add(grace - time.Hour))
	x.NoError(w.S.Grants.Pass(w.ctx))
	x.Empty(restricted(), "within its grace")

	w.At(on(1, 15).Add(grace + time.Hour))
	x.NoError(w.S.Grants.Pass(w.ctx))
	vs := restricted()
	x.Len(vs, 1)
	x.Equal(payers.RuleName, vs[0].Rule)
	x.Equal(int32(levyan.GrantValue_GRANT_VALUE_ON), vs[0].Value)
	x.True(vs[0].Renews)

	o, err := w.payers().Offer(w.ctx, w.p, ws, b)
	x.NoError(err)
	oId, _ := pdid.From(o.GetId())
	_, err = w.payers().Accept(w.ctx, w.p, b, oId, pdid.Nil)
	x.NoError(err)
	x.NoError(w.S.Grants.Pass(w.ctx))
	vs = restricted()
	x.Len(vs, 1)
	x.False(vs[0].Renews, "ended: somebody will pay")
	x.False(vs[0].ValidUntil.After(w.S.Clock()))

	// A workspace levyan never heard of a payer of is not restricted.
	key, err := grants.Restricted("loft")
	x.NoError(err)
	x.Equal("loft.restricted", key)
	_, err = grants.Restricted("9lives")
	x.Error(err)
}

// A workspace nobody pays for is restricted as the policy table's row of it
// says (`dunning.kinds.payerless`, #11): its grace what the deployment says,
// or no grace at all -- restricted as its last payer goes -- and what it is
// told it is restricted from is that.
func TestAWorkspaceWithNoPayerIsRestrictedAsItsRowSays(t *testing.T) {
	for _, tc := range []struct {
		name  string
		with  func(*cmd.Config)
		after time.Duration
	}{
		{"at once", func(c *cmd.Config) { c.Dunning.Kinds.Payerless.Restrict = "at_once" }, 0},
		{"after ten days", func(c *cmd.Config) {
			ten := cmd.Span(10 * 24 * time.Hour)
			c.Dunning.Kinds.Payerless.Grace = &ten
		}, 10 * 24 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lw := levyantest.New(t, tc.with)
			lw.At(on(1, 15))
			loft := lw.Product("loft")
			p, err := pdid.From(loft.GetId())
			lw.X.NoError(err)
			w := world{World: lw, t: t, ctx: context.Background(), loft: loft, p: p}
			x := w.X

			a, ws := levyantest.Account(), levyantest.Tenant()
			_, err = w.payers().Arrive(w.ctx, w.p, ws, a, on(1, 3))
			x.NoError(err)
			_, err = w.payers().Leave(w.ctx, w.p, ws, a)
			x.NoError(err)
			st := w.state(ws)
			x.Equal(on(1, 15), st.Since)
			x.Equal(on(1, 15).Add(tc.after), st.Restricted)

			restricted := func() bool {
				vs, err := w.S.Ent.Grant.Query().Where(entgrant.Key("loft.restricted"), entgrant.Renews(true)).All(w.ctx)
				x.NoError(err)
				return len(vs) == 1
			}
			if tc.after > 0 {
				w.At(on(1, 15).Add(tc.after - time.Hour))
				x.NoError(w.S.Grants.Pass(w.ctx))
				x.False(restricted(), "within its grace")
			}
			w.At(on(1, 15).Add(tc.after))
			x.NoError(w.S.Grants.Pass(w.ctx))
			x.True(restricted())
		})
	}
}

// A billing account is in a country, which says the channel it is sold
// through and the currency it is billed in -- as the deployment sells that
// country when the account is made, whatever a request says. By default
// that is the merchant of record, in won in Korea and in US dollars
// elsewhere.
func TestABillingAccountIsBilledAsItsCountryIs(t *testing.T) {
	w := setup(t)
	x := pdtest.NewX(t)

	a := levyantest.Account()
	for country, want := range map[string]payers.Billed{
		"KR": {Channel: levyan.Channel_CHANNEL_MOR, Currency: "KRW"},
		"US": {Channel: levyan.Channel_CHANNEL_MOR, Currency: "USD"},
	} {
		v, err := w.S.Ent.BillingAccount.Get(w.ctx, w.account(a, country).Uuid())
		x.NoError(err)
		x.Equal(want.Channel.String(), v.Channel, country)
		x.Equal(want.Currency, v.Currency, country)
	}

	v, err := w.S.Ungated.BillingAccount().Add(w.ctx, levyan.BillingAccountAddRequest_builder{
		Owner: a.Bytes(), Country: "KR", Channel: levyan.Channel_CHANNEL_PG_KR.String(), Currency: "USD",
	}.Build())
	x.NoError(err)
	x.Equal(levyan.Channel_CHANNEL_MOR.String(), v.GetChannel(), "whatever the request says")
	x.Equal("KRW", v.GetCurrency())
	for _, c := range []string{"kr", "KOR", ""} {
		_, err := w.payers().Create(w.ctx, a, levyan.BillingAccount_builder{Country: c}.Build())
		x.Equal(codes.InvalidArgument, status.Code(err), "%q", c)
	}
	_, err = w.S.Ungated.BillingAccount().Add(w.ctx, levyan.BillingAccountAddRequest_builder{Country: "KR"}.Build())
	x.Equal(codes.InvalidArgument, status.Code(err), "a billing account is an account's")
}

// What a deployment sells each country through is its own to say
// (`payers.channels`), over the defaults key by key; and a billing account
// keeps what was said as it was made, whatever the deployment comes to say.
func TestABillingAccountKeepsWhatItWasMadeOn(t *testing.T) {
	w := setup(t)
	x := pdtest.NewX(t)

	a := levyantest.Account()
	before := w.account(a, "KR")

	// The deployment comes up again selling Korea through the gateway, and
	// Japan in yen.
	s := w.Again(func(c *cmd.Config) {
		c.Payers.Channels = cmd.ChannelsConfig{
			"KR": {Channel: "PG_KR", Currency: "KRW"},
			"JP": {Channel: "mor", Currency: "JPY"},
		}
	})
	for country, want := range map[string]payers.Billed{
		"KR": {Channel: levyan.Channel_CHANNEL_PG_KR, Currency: "KRW"},
		"JP": {Channel: levyan.Channel_CHANNEL_MOR, Currency: "JPY"},
		"US": {Channel: levyan.Channel_CHANNEL_MOR, Currency: "USD"},
	} {
		v, err := s.Payers.Create(w.ctx, a, levyan.BillingAccount_builder{Country: country, Recipient: "김수민"}.Build())
		x.NoError(err)
		x.Equal(want.Channel.String(), v.GetChannel(), country)
		x.Equal(want.Currency, v.GetCurrency(), country)
	}

	v, err := s.Ent.BillingAccount.Get(w.ctx, before.Uuid())
	x.NoError(err)
	x.Equal(levyan.Channel_CHANNEL_MOR.String(), v.Channel, "made before, it stays on what it was made on")
	x.Equal("KRW", v.Currency)

	// A map is what it says of every country it names, or of `default`.
	cs := payers.Channels{"KR": {Channel: levyan.Channel_CHANNEL_PG_KR, Currency: "KRW"}}
	_, err = cs.Of("US")
	x.Equal(codes.FailedPrecondition, status.Code(err), "nothing sells there")
	for name, bad := range map[string]payers.Channels{
		"a country in lower case": {"kr": {Channel: levyan.Channel_CHANNEL_MOR, Currency: "KRW"}},
		"no channel":              {"KR": {Currency: "KRW"}},
		"a currency mistyped":     {"KR": {Channel: levyan.Channel_CHANNEL_MOR, Currency: "KWR"}},
	} {
		x.Error(payers.DefaultChannels().With(bad).Check(), name)
	}
	x.NoError(payers.DefaultChannels().Check())
}

// roster says an account is gone: each link it pays by ends at the end of
// its period, and what was offered to it is withdrawn. Said back before then,
// it goes on paying from where it would have stopped. Asked again once
// roster has nobody by an erased one, what says who they were is replaced --
// the business it billed is kept -- and their mark is settled.
func TestRosterSaysAnAccountIsGone(t *testing.T) {
	w := setup(t)
	x := pdtest.NewX(t)

	a, b := levyantest.Account(), levyantest.Account()
	ba := w.account(a, "KR")
	ws, other := levyantest.Tenant(), levyantest.Tenant()
	_, err := w.payers().Arrive(w.ctx, w.p, ws, a, on(1, 3))
	x.NoError(err)
	_, err = w.payers().Arrive(w.ctx, w.p, other, b, on(1, 3))
	x.NoError(err)
	_, err = w.payers().Offer(w.ctx, w.p, other, a)
	x.NoError(err)

	x.NoError(w.payers().Heard(w.ctx, standing.Standing{Holder: a, Erased: on(1, 15)}))
	st := w.state(ws)
	x.Equal(a, payer(st.Current), "until the end of the period")
	x.Equal(on(2, 1), st.Current.DateEnd.UTC())
	x.Equal(int32(levyan.LinkEnded_LINK_ENDED_ERASED), st.Current.Ended)
	x.Nil(w.state(other).Offer, "withdrawn")

	// Said twice, as every process says it.
	x.NoError(w.payers().Heard(w.ctx, standing.Standing{Holder: a, Erased: on(1, 15)}))
	n, err := w.S.Ent.Link.Query().Where(entlink.Payer(a.Uuid())).Count(w.ctx)
	x.NoError(err)
	x.Equal(1, n)

	// Back, before the period ends: it pays on from where it stopped.
	w.At(on(1, 20))
	x.NoError(w.payers().Heard(w.ctx, standing.Standing{Holder: a}))
	st = w.state(ws)
	x.Equal(a, payer(st.Next))
	x.Equal(on(2, 1), st.Next.DateStart.UTC())
	x.Equal(int32(levyan.LinkBegan_LINK_BEGAN_RESUMED), st.Next.Began)
	x.Equal(ba.Uuid(), st.Next.BillingAccountId)

	// Suspended now, and erased: settled once roster has nobody by them.
	w.At(on(2, 10))
	x.NoError(w.payers().Heard(w.ctx, standing.Standing{Holder: a, Disabled: on(2, 10)}))
	x.Equal(on(3, 1), w.state(ws).Current.DateEnd.UTC())
	x.NoError(w.payers().Heard(w.ctx, standing.Standing{Holder: a, Erased: on(2, 11)}))

	r := &roster{gone: map[pdid.Id]bool{}}
	x.NoError(w.payers().Settle(w.ctx, r))
	v, err := w.S.Ent.BillingAccount.Get(w.ctx, ba.Uuid())
	x.NoError(err)
	x.Nil(v.DateForgotten, "roster still has them")
	x.Equal("김수민", v.Recipient)

	r.gone[a] = true
	x.NoError(w.payers().Settle(w.ctx, r))
	v, err = w.S.Ent.BillingAccount.Get(w.ctx, ba.Uuid())
	x.NoError(err)
	x.NotNil(v.DateForgotten)
	x.Empty(v.Name)
	x.Empty(v.Recipient)
	x.Empty(v.Email)
	x.Empty(v.Address)
	x.Equal("123-45-67890", v.Business.GetRegistration(), "the business it billed is kept")
	x.Equal("주식회사 예시", v.Business.GetName())
	x.Empty(v.Business.GetRepresentative())
	x.Empty(v.Business.GetEmail())

	// Settled, and nothing brings it back.
	x.NoError(w.payers().Heard(w.ctx, standing.Standing{Holder: a}))
	x.Nil(w.state(ws).Next)
}

// What the stream missed is found by asking about everybody: an account
// roster has nobody by is marked as erased.
func TestWhatTheStreamMissedIsFound(t *testing.T) {
	w := setup(t)
	x := pdtest.NewX(t)

	a, b := levyantest.Account(), levyantest.Account()
	ws := levyantest.Tenant()
	_, err := w.payers().Arrive(w.ctx, w.p, ws, a, on(1, 3))
	x.NoError(err)
	w.account(b, "KR")

	r := &roster{gone: map[pdid.Id]bool{a: true}}
	x.NoError(w.payers().Reconcile(w.ctx, r))
	x.ElementsMatch([]pdid.Id{a, b}, r.asked)
	st := w.state(ws)
	x.Equal(int32(levyan.LinkEnded_LINK_ENDED_ERASED), st.Current.Ended)
	x.Equal(on(2, 1), st.Current.DateEnd.UTC())

	r = &roster{gone: map[pdid.Id]bool{}, fail: errors.New("roster is away")}
	x.Error(w.payers().Reconcile(w.ctx, r))
}

// roster is a roster that has nobody by the accounts in `gone`.
type roster struct {
	gone  map[pdid.Id]bool
	fail  error
	asked []pdid.Id
}

func (r *roster) Settle(ctx context.Context, marked []pdid.Id, gone func(context.Context, pdid.Id) error, back func(context.Context, standing.Standing) error) error {
	for _, v := range marked {
		if r.gone[v] {
			if err := gone(ctx, v); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *roster) Reconcile(ctx context.Context, accounts []pdid.Id, marked func(pdid.Id) bool) (*standing.Reconciled, error) {
	if r.fail != nil {
		return nil, r.fail
	}
	out := &standing.Reconciled{}
	for _, v := range accounts {
		r.asked = append(r.asked, v)
		if r.gone[v] && !marked(v) {
			out.Gone = append(out.Gone, v)
		}
	}
	return out, nil
}

// A workspace destroyed is nobody's to pay for (#14): every link that would
// pay for it past its destruction ends there -- one to begin at the end of
// the period is taken back before it begins -- what was offered of it is
// withdrawn, and once destroyed nothing begins to be paid for or offered of
// it: an account roster has back pays for it no more.
func TestADestroyedWorkspaceIsNobodysToPayFor(t *testing.T) {
	w := setup(t)
	x := pdtest.NewX(t)

	a, b := levyantest.Account(), levyantest.Account()
	ws, gone := levyantest.Tenant(), levyantest.Tenant()
	_, err := w.payers().Arrive(w.ctx, w.p, ws, a, on(1, 3))
	x.NoError(err)
	_, err = w.payers().Arrive(w.ctx, w.p, gone, a, on(1, 3))
	x.NoError(err)

	// Another account pays for it from February, as it accepted; and a
	// third is asked.
	o, err := w.payers().Offer(w.ctx, w.p, ws, b)
	x.NoError(err)
	oid, err := pdid.From(o.GetId())
	x.NoError(err)
	_, err = w.payers().Accept(w.ctx, w.p, b, oid, pdid.Nil)
	x.NoError(err)
	_, err = w.payers().Offer(w.ctx, w.p, ws, levyantest.Account())
	x.NoError(err)

	said, err := w.payers().Destroy(w.ctx, w.p, ws, on(1, 14))
	x.NoError(err)
	x.Equal(on(1, 14), said.Life.Destroyed)
	x.False(said.Late)

	ls, err := w.S.Ent.Link.Query().Where(entlink.HasWorkspaceWith(entworkspace.Key(ws.Bytes()))).All(w.ctx)
	x.NoError(err)
	x.Len(ls, 2)
	for _, l := range ls {
		x.Equal(int32(levyan.LinkEnded_LINK_ENDED_DESTROYED), l.Ended)
		switch payer(l) {
		case a:
			x.Equal(on(1, 14), l.DateEnd.UTC(), "when it was destroyed")
		case b:
			x.Equal(l.DateStart.UTC(), l.DateEnd.UTC(), "taken back before it began")
		}
	}
	st := w.state(ws)
	x.Nil(st.Current)
	x.Nil(st.Next)
	x.Nil(st.Offer, "withdrawn")
	x.False(st.Payerless(), "nobody's to pay for, and nothing to restrict")

	_, err = w.payers().Offer(w.ctx, w.p, ws, levyantest.Account())
	x.Equal(codes.FailedPrecondition, status.Code(err))
	again, err := w.payers().Arrive(w.ctx, w.p, ws, a, on(1, 3))
	x.NoError(err, "it arrived before, and is answered as it is")
	x.Nil(again.Current)

	// Suspended in January, the account's other link ends as February
	// begins; that workspace is destroyed in February, and the account is
	// back after it.
	x.NoError(w.payers().Heard(w.ctx, standing.Standing{Holder: a, Disabled: on(1, 15)}))
	x.Equal(on(2, 1), w.state(gone).Current.DateEnd.UTC())
	w.At(on(2, 10))
	_, err = w.payers().Destroy(w.ctx, w.p, gone, on(2, 5))
	x.NoError(err)
	x.NoError(w.payers().Heard(w.ctx, standing.Standing{Holder: a}))
	st = w.state(gone)
	x.Nil(st.Current)
	x.Nil(st.Next, "it pays for what is destroyed no more")
}
