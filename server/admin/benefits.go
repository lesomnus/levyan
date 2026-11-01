package admin

import (
	"net/http"
	"strings"
	"time"
	"uuid"

	"github.com/lesomnus/payday/pdid"

	"github.com/lesomnus/levyan/internal/ent"
	entallowance "github.com/lesomnus/levyan/internal/ent/allowance"
	entcredit "github.com/lesomnus/levyan/internal/ent/credit"
	entoperator "github.com/lesomnus/levyan/internal/ent/operator"
	"github.com/lesomnus/levyan/internal/ent/predicate"
	entworkspace "github.com/lesomnus/levyan/internal/ent/workspace"
	"github.com/lesomnus/levyan/server/benefits"
	"github.com/lesomnus/levyan/server/invoices"
	"github.com/lesomnus/levyan/server/ledger"
)

// What an operator gives a product's workspaces beyond their prices (#43):
// allowances -- more of a meter each period for nothing -- and credit,
// money taken off what is billed, to a workspace or to a billing account,
// with an expiry. Each is written through the server with no wall, as the
// operator, with their reason.

// Allowance is more of a meter a workspace is given each period.
type Allowance struct {
	Id        string     `json:"id"`
	Workspace string     `json:"workspace"`
	Meter     string     `json:"meter"`
	Quantity  int64      `json:"quantity"`
	From      time.Time  `json:"from"`
	Until     *time.Time `json:"until,omitempty"`
	Reason    string     `json:"reason"`
	GrantedBy string     `json:"granted_by,omitempty"`
	Given     time.Time  `json:"given"`
	EndedBy   string     `json:"ended_by,omitempty"`
	EndReason string     `json:"end_reason,omitempty"`
}

// Credit is credit an operator granted, and what is left of it.
type Credit struct {
	Id             string    `json:"id"`
	Workspace      string    `json:"workspace,omitempty"`
	BillingAccount string    `json:"billing_account,omitempty"`
	Granted        int64     `json:"granted"`
	Left           int64     `json:"left"`
	Currency       string    `json:"currency"`
	Expires        time.Time `json:"expires"`
	Reason         string    `json:"reason"`
	GrantedBy      string    `json:"granted_by,omitempty"`
	Created        time.Time `json:"created"`
}

// operators is each operator's name, by their row.
func (a *Routes) operators(r *http.Request) (map[uuid.UUID]string, error) {
	vs, err := a.S.Ent.Operator.Query().Select(entoperator.FieldId, entoperator.FieldAlias).All(r.Context())
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID]string{}
	for _, v := range vs {
		out[v.Id] = v.Alias
	}

	return out, nil
}

func allowanceOf(v *ent.Allowance, who map[uuid.UUID]string) Allowance {
	out := Allowance{
		Id: pdid.Id(v.Id).String(), Quantity: v.Quantity, From: v.ValidFrom.UTC(), Reason: v.Reason,
		GrantedBy: who[v.GrantedById], Given: v.DateCreated.UTC(), EndedBy: who[v.EndedById], EndReason: v.EndReason,
	}
	if v.ValidUntil != nil {
		at := v.ValidUntil.UTC()
		out.Until = &at
	}
	if w := v.Edges.Workspace; w != nil {
		out.Workspace = text(w.Key)
	}
	if m := v.Edges.Meter; m != nil {
		out.Meter = m.Key
	}

	return out
}

func (a *Routes) creditOf(r *http.Request, l invoices.Lot, who map[uuid.UUID]string) (Credit, error) {
	out := Credit{
		Id: pdid.Id(l.Row.Id).String(), Granted: l.Row.Amount, Left: l.Left, Currency: l.Row.Currency,
		Expires: l.Expires().UTC(), Reason: l.Row.Reason, GrantedBy: who[l.Row.OperatorId], Created: l.Row.DateCreated.UTC(),
	}
	if ba := l.Row.BillingAccountId; ba != (uuid.UUID{}) {
		out.BillingAccount = pdid.Id(ba).String()
	}
	if ws := l.Workspace(); ws != (uuid.UUID{}) {
		w, err := a.S.Ent.Workspace.Get(r.Context(), ws)
		if err != nil {
			return Credit{}, err
		}
		out.Workspace = text(w.Key)
	}

	return out, nil
}

// workspaceOf is the workspace a query names, by the product's identifier of
// it, and false when it names none.
func workspaceOf(w http.ResponseWriter, r *http.Request) (pdid.Id, bool, bool) {
	v := strings.TrimSpace(r.URL.Query().Get("workspace"))
	if v == "" {
		return pdid.Nil, false, true
	}
	key, err := ledger.Workspace(v)
	if err != nil {
		http.Error(w, "workspace: "+err.Error(), http.StatusBadRequest)
		return pdid.Nil, false, false
	}

	return key, true, true
}

// giveAllowance gives a workspace of a product more of a meter each period,
// by the operator's hand.
func (a *Routes) giveAllowance(w http.ResponseWriter, r *http.Request) {
	t, ok := a.product(w, r)
	if !ok {
		return
	}
	var in struct {
		Workspace string     `json:"workspace"`
		Meter     string     `json:"meter"`
		Quantity  int64      `json:"quantity"`
		From      *time.Time `json:"from"`
		Until     *time.Time `json:"until"`
		Reason    string     `json:"reason"`
	}
	if !read(w, r, &in) {
		return
	}
	ws, err := ledger.Workspace(strings.TrimSpace(in.Workspace))
	if err != nil {
		http.Error(w, "workspace: "+err.Error(), http.StatusBadRequest)
		return
	}

	give := benefits.Give{Product: pdid.Id(t.Id), Workspace: ws, Meter: in.Meter, Quantity: in.Quantity, Reason: in.Reason}
	if in.From != nil {
		give.From = *in.From
	}
	if in.Until != nil {
		give.Until = *in.Until
	}
	v, err := a.S.Allowances.Give(r.Context(), give)
	if err != nil {
		fail(w, r, err)
		return
	}
	a.showAllowance(w, r, pdid.Id(v.Id), http.StatusCreated)
}

// endAllowance ends an allowance now, by the operator's hand: what it gave
// of the time before stays given.
func (a *Routes) endAllowance(w http.ResponseWriter, r *http.Request) {
	id, err := pdid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "an allowance, by the identifier `allowance ls` lists", http.StatusBadRequest)
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if !read(w, r, &in) {
		return
	}
	if _, err := a.S.Allowances.End(r.Context(), id, in.Reason); err != nil {
		fail(w, r, err)
		return
	}
	a.showAllowance(w, r, id, http.StatusOK)
}

func (a *Routes) showAllowance(w http.ResponseWriter, r *http.Request, id pdid.Id, code int) {
	v, err := a.S.Ent.Allowance.Query().Where(entallowance.Id(id.Uuid())).WithWorkspace().WithMeter().Only(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	who, err := a.operators(r)
	if err != nil {
		fail(w, r, err)
		return
	}
	answer(w, code, allowanceOf(v, who))
}

// allowances are a product's allowances, the latest given first: in force or
// to come, of one workspace when the query names one, and those ended too
// with `all`.
func (a *Routes) allowances(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, ok := a.product(w, r)
	if !ok {
		return
	}
	q := a.S.Ent.Allowance.Query().Where(entallowance.TenantId(t.Id))
	if r.URL.Query().Get("all") != "true" {
		q = q.Where(entallowance.Or(entallowance.ValidUntilIsNil(), entallowance.ValidUntilGT(a.S.Clock())))
	}
	key, named, ok := workspaceOf(w, r)
	if !ok {
		return
	}
	if named {
		q = q.Where(entallowance.HasWorkspaceWith(entworkspace.TenantId(t.Id), entworkspace.Key(key.Bytes())))
	}
	vs, err := q.WithWorkspace().WithMeter().Order(ent.Desc(entallowance.FieldDateCreated), ent.Desc(entallowance.FieldId)).Limit(500).All(ctx)
	if err != nil {
		fail(w, r, err)
		return
	}
	who, err := a.operators(r)
	if err != nil {
		fail(w, r, err)
		return
	}
	out := []Allowance{}
	for _, v := range vs {
		out = append(out, allowanceOf(v, who))
	}

	answer(w, http.StatusOK, out)
}

// grantCredit grants credit of a product to a workspace of it or to a
// billing account, by the operator's hand.
func (a *Routes) grantCredit(w http.ResponseWriter, r *http.Request) {
	t, ok := a.product(w, r)
	if !ok {
		return
	}
	var in struct {
		Workspace      string     `json:"workspace"`
		BillingAccount string     `json:"billing_account"`
		Amount         int64      `json:"amount"`
		Currency       string     `json:"currency"`
		Expires        *time.Time `json:"expires"`
		Reason         string     `json:"reason"`
	}
	if !read(w, r, &in) {
		return
	}

	g := invoices.Grant{Product: pdid.Id(t.Id), Amount: in.Amount, Currency: in.Currency, Reason: in.Reason}
	if v := strings.TrimSpace(in.Workspace); v != "" {
		ws, err := ledger.Workspace(v)
		if err != nil {
			http.Error(w, "workspace: "+err.Error(), http.StatusBadRequest)
			return
		}
		g.Workspace = ws
	}
	if v := strings.TrimSpace(in.BillingAccount); v != "" {
		ba, err := pdid.Parse(v)
		if err != nil {
			http.Error(w, "billing_account: "+v+" is not a billing account's identifier", http.StatusBadRequest)
			return
		}
		g.BillingAccount = ba
	}
	if in.Expires != nil {
		g.Expires = *in.Expires
	}

	l, err := a.S.Invoices.Grant(r.Context(), g)
	if err != nil {
		fail(w, r, err)
		return
	}
	a.showCredit(w, r, l, http.StatusCreated)
}

// expireCredit ends credit an operator granted now, by the operator's hand:
// what is left of it is taken off it.
func (a *Routes) expireCredit(w http.ResponseWriter, r *http.Request) {
	id, err := pdid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "a credit, by the identifier `credit ls` lists", http.StatusBadRequest)
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if !read(w, r, &in) {
		return
	}
	l, err := a.S.Invoices.Expire(r.Context(), id, in.Reason)
	if err != nil {
		fail(w, r, err)
		return
	}
	a.showCredit(w, r, l, http.StatusOK)
}

func (a *Routes) showCredit(w http.ResponseWriter, r *http.Request, l invoices.Lot, code int) {
	who, err := a.operators(r)
	if err != nil {
		fail(w, r, err)
		return
	}
	out, err := a.creditOf(r, l, who)
	if err != nil {
		fail(w, r, err)
		return
	}
	answer(w, code, out)
}

// credits are what operators granted of a product, the soonest to expire
// first: what is left to spend, of one workspace or one billing account when
// the query names one, and what was spent or expired too with `all`.
func (a *Routes) credits(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, ok := a.product(w, r)
	if !ok {
		return
	}
	where := []predicate.Credit{}
	key, named, ok := workspaceOf(w, r)
	if !ok {
		return
	}
	if named {
		where = append(where, entcredit.HasWorkspaceWith(entworkspace.TenantId(t.Id), entworkspace.Key(key.Bytes())))
	}
	if v := strings.TrimSpace(r.URL.Query().Get("billing_account")); v != "" {
		ba, err := pdid.Parse(v)
		if err != nil {
			http.Error(w, "billing_account: "+v+" is not a billing account's identifier", http.StatusBadRequest)
			return
		}
		where = append(where, entcredit.BillingAccountId(ba.Uuid()))
	}
	ls, err := invoices.Lots(ctx, a.S.Ent, t.Id, where...)
	if err != nil {
		fail(w, r, err)
		return
	}
	if r.URL.Query().Get("all") != "true" {
		ls = invoices.Live(ls, a.S.Clock())
	}
	who, err := a.operators(r)
	if err != nil {
		fail(w, r, err)
		return
	}
	out := []Credit{}
	for _, l := range ls {
		c, err := a.creditOf(r, l, who)
		if err != nil {
			fail(w, r, err)
			return
		}
		out = append(out, c)
	}

	answer(w, http.StatusOK, out)
}
