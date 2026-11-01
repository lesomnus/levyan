package admin_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/server/admin"
)

// An operator gives a workspace more of a meter each period, ends it, and
// lists what a product's workspaces are given; and grants credit to a
// workspace or a billing account, ends it, and lists what is left of it.
// Each says why, and the trail says who wrote it (#43).
func TestAnOperatorGivesAllowancesAndGrantsCredit(t *testing.T) {
	x := require.New(t)
	d := open(t, map[string]string{"alice": aliceKey, "bob": bobKey})
	loft := d.w.Product("loft")
	d.w.Meter(loft, "loft.assets.active", "{asset}", levyan.MeterKind_METER_KIND_STOCK, levyan.Aggregation_AGGREGATION_TIME_WEIGHTED_AVERAGE)
	ws := levyantest.Tenant().String()

	var a admin.Allowance
	d.as(aliceKey, http.MethodPost, "/admin/products/loft/allowances", map[string]any{
		"workspace": ws, "meter": "loft.assets.active", "quantity": 500, "reason": "2027 봄 프로모션",
	}, &a)
	x.Equal(ws, a.Workspace)
	x.Equal("loft.assets.active", a.Meter)
	x.Equal(int64(500), a.Quantity)
	x.Nil(a.Until)
	x.Equal("alice", a.GrantedBy)
	x.Equal("2027 봄 프로모션", a.Reason)
	x.Equal(d.operator("alice"), d.actorOf(a.Id), "the trail names who gave it")

	for _, body := range []map[string]any{
		{"workspace": ws, "meter": "loft.assets.active", "quantity": 500},
		{"workspace": ws, "meter": "loft.assets.active", "quantity": 0, "reason": "x"},
		{"workspace": "loft", "meter": "loft.assets.active", "quantity": 5, "reason": "x"},
	} {
		code, b := d.ask(aliceKey, http.MethodPost, "/admin/products/loft/allowances", body)
		x.Equal(http.StatusBadRequest, code, "%v: %s", body, b)
	}
	code, _ := d.ask(aliceKey, http.MethodPost, "/admin/products/loft/allowances", map[string]any{"workspace": ws, "meter": "loft.nothing", "quantity": 5, "reason": "x"})
	x.Equal(http.StatusNotFound, code)

	var vs []admin.Allowance
	d.as(aliceKey, http.MethodGet, "/admin/products/loft/allowances?workspace="+ws, nil, &vs)
	x.Len(vs, 1)

	code, _ = d.ask(bobKey, http.MethodPost, "/admin/allowances/"+a.Id+"/end", map[string]string{})
	x.Equal(http.StatusBadRequest, code, "with why")
	var ended admin.Allowance
	d.as(bobKey, http.MethodPost, "/admin/allowances/"+a.Id+"/end", map[string]string{"reason": "프로모션 종료"}, &ended)
	x.NotNil(ended.Until)
	x.False(ended.Until.After(time.Now()))
	x.Equal("bob", ended.EndedBy)
	x.Equal("프로모션 종료", ended.EndReason)
	code, _ = d.ask(bobKey, http.MethodPost, "/admin/allowances/"+a.Id+"/end", map[string]string{"reason": "again"})
	x.Equal(http.StatusConflict, code, "over already")
	d.as(aliceKey, http.MethodGet, "/admin/products/loft/allowances", nil, &vs)
	x.Empty(vs, "nothing in force, nor to come")
	d.as(aliceKey, http.MethodGet, "/admin/products/loft/allowances?all=true", nil, &vs)
	x.Len(vs, 1)

	// Credit of the workspace, in what it is billed in -- which nothing says
	// yet, so the operator does.
	code, b := d.ask(aliceKey, http.MethodPost, "/admin/products/loft/credits", map[string]any{"workspace": ws, "amount": 100000, "reason": "장애 보상"})
	x.Equal(http.StatusBadRequest, code, "%s", b)
	var c admin.Credit
	d.as(aliceKey, http.MethodPost, "/admin/products/loft/credits", map[string]any{"workspace": ws, "amount": 100000, "currency": "KRW", "reason": "장애 보상"}, &c)
	x.Equal(ws, c.Workspace)
	x.Empty(c.BillingAccount)
	x.Equal(int64(100000), c.Granted)
	x.Equal(int64(100000), c.Left)
	x.Equal("KRW", c.Currency)
	x.WithinDuration(time.Now().AddDate(1, 0, 0), c.Expires, time.Minute, "a year, unless said")
	x.Equal("alice", c.GrantedBy)
	x.Equal(d.operator("alice"), d.actorOf(c.Id))

	// And of a billing account, in what it is billed in.
	owner := levyantest.Account()
	v, err := d.w.S.Payers.Create(context.Background(), owner, levyan.BillingAccount_builder{Country: "KR", Recipient: "수민"}.Build())
	x.NoError(err)
	ba, err := pdid.From(v.GetId())
	x.NoError(err)
	expires := time.Now().UTC().AddDate(0, 3, 0).Truncate(time.Second)
	var cb admin.Credit
	d.as(aliceKey, http.MethodPost, "/admin/products/loft/credits", map[string]any{"billing_account": ba.String(), "amount": 5000, "expires": expires, "reason": "계약"}, &cb)
	x.Equal(ba.String(), cb.BillingAccount)
	x.Equal("KRW", cb.Currency)
	x.Equal(expires, cb.Expires)

	for _, body := range []map[string]any{
		{"billing_account": ba.String(), "amount": 5000, "currency": "USD", "reason": "x"},
		{"billing_account": ba.String(), "workspace": ws, "amount": 5000, "reason": "x"},
		{"amount": 5000, "reason": "x"},
		{"billing_account": ba.String(), "amount": 5000},
		{"billing_account": ba.String(), "amount": 0, "reason": "x"},
		{"billing_account": ba.String(), "amount": 5000, "reason": "x", "expires": time.Now().Add(-time.Hour)},
	} {
		code, b := d.ask(aliceKey, http.MethodPost, "/admin/products/loft/credits", body)
		x.Equal(http.StatusBadRequest, code, "%v: %s", body, b)
	}

	var cs []admin.Credit
	d.as(aliceKey, http.MethodGet, "/admin/products/loft/credits", nil, &cs)
	x.Len(cs, 2)
	x.Equal(cb.Id, cs[0].Id, "the soonest to expire first")
	d.as(aliceKey, http.MethodGet, "/admin/products/loft/credits?billing_account="+ba.String(), nil, &cs)
	x.Len(cs, 1)
	d.as(aliceKey, http.MethodGet, "/admin/products/loft/credits?workspace="+ws, nil, &cs)
	x.Len(cs, 1)
	x.Equal(c.Id, cs[0].Id)

	var gone admin.Credit
	d.as(bobKey, http.MethodPost, "/admin/credits/"+cb.Id+"/expire", map[string]string{"reason": "계약 해지"}, &gone)
	x.Zero(gone.Left)
	code, _ = d.ask(bobKey, http.MethodPost, "/admin/credits/"+cb.Id+"/expire", map[string]string{"reason": "again"})
	x.Equal(http.StatusConflict, code, "nothing is left of it")
	d.as(aliceKey, http.MethodGet, "/admin/products/loft/credits", nil, &cs)
	x.Len(cs, 1, "what is left to spend")
	d.as(aliceKey, http.MethodGet, "/admin/products/loft/credits?all=true", nil, &cs)
	x.Len(cs, 2)

	code, _ = d.ask(bobKey, http.MethodPost, "/admin/credits/"+levyantest.Tenant().String()+"/expire", map[string]string{"reason": "x"})
	x.Equal(http.StatusNotFound, code)
	code, _ = d.ask("", http.MethodGet, "/admin/products/loft/credits", nil)
	x.Equal(http.StatusUnauthorized, code)
}
