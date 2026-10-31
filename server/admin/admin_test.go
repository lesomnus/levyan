package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/xli/cfg"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/levyan/internal/ent/audit"
	"github.com/lesomnus/levyan/internal/ent/operator"
	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/server/admin"
	"github.com/lesomnus/levyan/server/keys"
	"github.com/lesomnus/levyan/server/ledger"
)

const (
	aliceKey = "alice-has-a-key-of-thirty-two-characters-or-more"
	bobKey   = "bob-has-a-key-of-thirty-two-characters-or-more-too"
)

func secret(t *testing.T, v string) cfg.Secret {
	t.Helper()
	var s cfg.Secret
	require.NoError(t, s.UnmarshalText([]byte(v)))
	return s
}

type door struct {
	w   *levyantest.World
	r   *admin.Routes
	url string
}

func open(t *testing.T, operators map[string]string) *door {
	t.Helper()
	w := levyantest.New(t)

	ks := map[string]cfg.Secret{}
	for n, k := range operators {
		ks[n] = secret(t, k)
	}
	ops, err := admin.Operators(ks)
	w.X.NoError(err)

	r := &admin.Routes{S: w.S, Operators: ops, Expect: ledger.Expect{Gap: 36 * time.Hour, Recent: 7 * 24 * time.Hour}}
	w.X.NoError(r.Sync(t.Context()))
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)

	return &door{w: w, r: r, url: srv.URL}
}

// ask is a request to the entry point with `key`, answering its status and
// its body.
func (d *door) ask(key, method, path string, body any) (int, []byte) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		d.w.X.NoError(err)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, d.url+path, rd)
	d.w.X.NoError(err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	res, err := http.DefaultClient.Do(req)
	d.w.X.NoError(err)
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	d.w.X.NoError(err)

	return res.StatusCode, b
}

// as is `ask` that has to succeed, decoding the answer into out.
func (d *door) as(key, method, path string, body, out any) {
	code, b := d.ask(key, method, path, body)
	d.w.X.Less(code, 300, "%s %s: %d %s", method, path, code, b)
	if out != nil {
		d.w.X.NoError(json.Unmarshal(b, out))
	}
}

// actorOf is who the trail says wrote the row `id` names: its first record's
// actor.
func (d *door) actorOf(id string) pdid.Id {
	k, err := pdid.Parse(id)
	d.w.X.NoError(err)
	v, err := d.w.S.Ent.Audit.Query().Where(audit.ObjectId(k.Uuid())).First(context.Background())
	d.w.X.NoError(err)
	return pdid.Id(v.ActorId)
}

func (d *door) operator(name string) pdid.Id {
	v, err := d.w.S.Ent.Operator.Query().Where(operator.Alias(name), operator.DateErasedIsNil()).Only(context.Background())
	d.w.X.NoError(err)
	return pdid.Id(v.Id)
}

func TestTheDoorOpensToAnOperatorsKeyAlone(t *testing.T) {
	x := require.New(t)

	_, err := admin.Operators(map[string]cfg.Secret{"alice": secret(t, "short")})
	x.Error(err, "a key that opens every product is not short")
	_, err = admin.Operators(map[string]cfg.Secret{"Alice Smith": secret(t, aliceKey)})
	x.Error(err, "a name is what an alias is")

	d := open(t, map[string]string{"alice": aliceKey})

	code, _ := d.ask("", http.MethodGet, "/admin/products", nil)
	x.Equal(http.StatusUnauthorized, code)
	code, _ = d.ask(bobKey, http.MethodGet, "/admin/products", nil)
	x.Equal(http.StatusUnauthorized, code, "a key nobody listed")
	code, _ = d.ask(aliceKey, http.MethodGet, "/admin/products", nil)
	x.Equal(http.StatusOK, code)
}

// The operators are what the configuration says: a row for each name it
// lists, which the trail names, and the row of a name it no longer lists
// erased, so that its key opens nothing.
func TestTheOperatorsAreWhatTheConfigurationSays(t *testing.T) {
	x := require.New(t)
	d := open(t, map[string]string{"alice": aliceKey, "bob": bobKey})

	alice, bob := d.operator("alice"), d.operator("bob")
	x.NotEqual(alice, bob)

	// Listed again, an operator is the row they were.
	x.NoError(d.r.Sync(t.Context()))
	x.Equal(alice, d.operator("alice"))

	ops, err := admin.Operators(map[string]cfg.Secret{"alice": secret(t, aliceKey)})
	x.NoError(err)
	d.r.Operators = ops
	x.NoError(d.r.Sync(t.Context()))
	n, err := d.w.S.Ent.Operator.Query().Where(operator.Alias("bob"), operator.DateErasedIsNil()).Count(t.Context())
	x.NoError(err)
	x.Zero(n, "bob is not listed any more")

	code, _ := d.ask(bobKey, http.MethodGet, "/admin/products", nil)
	x.Equal(http.StatusUnauthorized, code)
}

// An operator puts a product up and issues it a key, which is said once: the
// product calls with it, nothing shows it again, and once revoked it opens
// nothing. What the operator wrote, the trail says they wrote.
func TestAnOperatorPutsAProductUpAndIssuesItsKeys(t *testing.T) {
	x := require.New(t)
	d := open(t, map[string]string{"alice": aliceKey})
	store := keys.Store(d.w.S.Ungated, nil)

	var p admin.Detail
	d.as(aliceKey, http.MethodPost, "/admin/products", map[string]string{"alias": "loft", "name": "Loft"}, &p)
	x.Equal("loft", p.Alias)
	x.Equal("Loft", p.Name)
	x.Equal(d.operator("alice"), d.actorOf(p.Id), "the trail names who put it up")

	code, _ := d.ask(aliceKey, http.MethodPost, "/admin/products", map[string]string{"alias": "loft"})
	x.Equal(http.StatusConflict, code, "a product's alias is its own")

	var k admin.Issued
	d.as(aliceKey, http.MethodPost, "/admin/products/loft/keys", map[string]string{"alias": "loft-prod"}, &k)
	x.True(strings.HasPrefix(k.Token, keys.Prefix))
	x.Equal("loft-prod", k.Alias)
	x.Equal(d.operator("alice"), d.actorOf(k.Id))

	id, err := store.Lookup(t.Context(), k.Token)
	x.NoError(err)
	x.Equal(k.Id, id.Id, "the key is the actor")
	x.Equal(p.Id, id.TenantId, "in its product")

	code, b := d.ask(aliceKey, http.MethodGet, "/admin/products/loft", nil)
	x.Equal(http.StatusOK, code)
	x.NotContains(string(b), k.Token, "said once")
	x.NoError(json.Unmarshal(b, &p))
	x.Len(p.Keys, 1)
	x.Nil(p.Keys[0].Revoked)

	var revoked admin.Key
	d.as(aliceKey, http.MethodPost, "/admin/keys/"+k.Id+"/revoke", map[string]string{}, &revoked)
	x.NotNil(revoked.Revoked)
	_, err = store.Lookup(t.Context(), k.Token)
	x.Error(err, "a key revoked opens nothing")

	code, _ = d.ask(aliceKey, http.MethodPost, "/admin/products/nobody/keys", map[string]string{"alias": "x"})
	x.Equal(http.StatusNotFound, code)
}

// An operator writes a product's meters and publishes its prices, and asks
// which version was in force for a customer when -- the versions before a
// change answered as they were.
func TestAnOperatorPublishesPricesAndAsksWhichWasInForce(t *testing.T) {
	x := require.New(t)
	d := open(t, map[string]string{"alice": aliceKey})
	d.as(aliceKey, http.MethodPost, "/admin/products", map[string]string{"alias": "loft", "name": "Loft"}, nil)

	var m admin.Meter
	d.as(aliceKey, http.MethodPost, "/admin/products/loft/meters", map[string]any{
		"key": "loft.assets.active", "unit": "{asset}", "kind": "stock", "aggregation": "time_weighted_average", "name": "활성 자산",
	}, &m)
	x.Equal("stock", m.Kind)
	x.Equal("time_weighted_average", m.Aggregation)

	code, b := d.ask(aliceKey, http.MethodPost, "/admin/products/loft/meters", map[string]any{
		"key": "loft.transfer", "unit": "By", "kind": "flow", "aggregation": "max",
	})
	x.Equal(http.StatusBadRequest, code)
	x.Contains(string(b), "aggregation", "the refusal says which field")

	// Prices from a day that is still to come, so that publishing them is
	// not reaching back: from 1 March for everybody, and a rise from 1 May
	// for new customers and 1 July for the ones already there.
	year := time.Now().UTC().Year() + 1
	day := func(m time.Month, d int) time.Time { return time.Date(year, m, d, 0, 0, 0, 0, time.UTC) }

	var v1, v2 admin.Price
	d.as(aliceKey, http.MethodPost, "/admin/products/loft/prices", map[string]any{
		"meter": "loft.assets.active", "channel": "pg_kr", "currency": "KRW",
		"included": 100, "unit_price": 50, "effective": day(3, 1), "desc": "처음 가격",
	}, &v1)
	d.as(aliceKey, http.MethodPost, "/admin/products/loft/prices", map[string]any{
		"meter": "loft.assets.active", "channel": "pg_kr", "currency": "KRW",
		"included": 100, "unit_price": 70, "effective": day(5, 1), "existing": day(7, 1), "desc": "인상, 두 달 고지",
	}, &v2)
	x.Equal(int64(1), v1.Per)
	x.Equal(d.operator("alice"), d.actorOf(v2.Id), "the trail says who published it")

	code, b = d.ask(aliceKey, http.MethodPost, "/admin/products/loft/prices", map[string]any{
		"meter": "loft.assets.active", "channel": "pg_kr", "currency": "KRW", "unit_price": 1,
		"effective": time.Now().Add(-24 * time.Hour),
	})
	x.Equal(http.StatusBadRequest, code, "%s", b)
	x.Contains(string(b), "date_effective")

	var all []admin.Price
	d.as(aliceKey, http.MethodGet, "/admin/products/loft/prices", nil, &all)
	x.Len(all, 2)
	x.Equal(int64(50), all[0].UnitPrice, "the first as it was published")
	x.Equal(int64(70), all[1].UnitPrice)

	timeline := func(since time.Time) [][3]any {
		var vs []admin.Segment
		d.as(aliceKey, http.MethodGet, "/admin/products/loft/timeline?meter=loft.assets.active&channel=pg_kr&currency=KRW"+
			"&since="+since.Format(time.RFC3339)+"&from="+day(2, 1).Format(time.RFC3339)+"&to="+day(9, 1).Format(time.RFC3339), nil, &vs)
		out := [][3]any{}
		for _, v := range vs {
			price := int64(-1)
			if v.Price != nil {
				price = v.Price.UnitPrice
			}
			out = append(out, [3]any{v.From.UTC(), v.To.UTC(), price})
		}
		return out
	}

	// A customer since February: nothing before March, the first price, and
	// the rise once their notice is over.
	x.Equal([][3]any{
		{day(2, 1), day(3, 1), int64(-1)},
		{day(3, 1), day(7, 1), int64(50)},
		{day(7, 1), day(9, 1), int64(70)},
	}, timeline(day(2, 1)))

	// One who starts in June pays the rise from the start.
	x.Equal([][3]any{
		{day(2, 1), day(6, 1), int64(-1)},
		{day(6, 1), day(9, 1), int64(70)},
	}, timeline(day(6, 1)))

	code, _ = d.ask(aliceKey, http.MethodGet, "/admin/products/loft/timeline?meter=loft.nothing&channel=pg_kr&currency=KRW"+
		"&from="+day(2, 1).Format(time.RFC3339)+"&to="+day(9, 1).Format(time.RFC3339), nil)
	x.Equal(http.StatusNotFound, code)
}
