package admin_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lesomnus/levyan/internal/levyantest"
	"github.com/lesomnus/levyan/server/admin"
)

// An operator gives a workspace a grant by hand -- a pilot's feature, a
// contract's limit -- ends it, and lists what a product's workspaces have
// been given. What they wrote, the trail says they wrote: the grant given,
// and its end.
func TestAnOperatorGivesAndEndsGrants(t *testing.T) {
	x := require.New(t)
	d := open(t, map[string]string{"alice": aliceKey})
	d.as(aliceKey, http.MethodPost, "/admin/products", map[string]string{"alias": "loft", "name": "Loft"}, nil)
	ws := levyantest.Tenant().String()

	var pilot admin.Grant
	d.as(aliceKey, http.MethodPost, "/admin/products/loft/grants", map[string]any{
		"workspace": ws, "key": "loft.export", "value": "on", "source": "trial", "desc": "파일럿",
	}, &pilot)
	x.Equal(ws, pilot.Workspace)
	x.Equal("on", pilot.Value)
	x.Equal("trial", pilot.Source)
	x.True(pilot.Renews)
	x.Equal(uint64(1), pilot.Version)
	x.Equal(d.operator("alice"), d.actorOf(pilot.Id), "the trail names who gave it")

	until := time.Now().UTC().Add(90 * 24 * time.Hour).Truncate(time.Second)
	var seats admin.Grant
	d.as(aliceKey, http.MethodPost, "/admin/products/loft/grants", map[string]any{
		"workspace": ws, "key": "loft.seats", "value": "limit", "limit": 20, "until": until, "source": "contract",
	}, &seats)
	x.Equal(int64(20), *seats.Limit)
	x.False(seats.Renews, "an end of its own")
	x.Equal(until, seats.Until)

	code, b := d.ask(aliceKey, http.MethodPost, "/admin/products/loft/grants", map[string]any{
		"workspace": ws, "key": "loft.seats", "value": "limit", "limit": 30,
	})
	x.Equal(http.StatusConflict, code, "%s", b)
	x.Contains(string(b), "replace")

	var more admin.Grant
	d.as(aliceKey, http.MethodPost, "/admin/products/loft/grants", map[string]any{
		"workspace": ws, "key": "loft.seats", "value": "limit", "limit": 30, "replace": true, "source": "contract",
	}, &more)
	x.Equal(int64(30), *more.Limit)

	for _, body := range []map[string]any{
		{"workspace": ws, "key": "loft.export", "value": "off"},
		{"workspace": ws, "key": "loft.seats", "value": "limit"},
		{"workspace": ws, "key": "loft.seats", "value": "on", "limit": 3},
		{"workspace": ws, "key": "loft.seats", "value": "limit", "limit": -1},
		{"workspace": ws, "key": "loft.robots", "value": "on", "source": "rule"},
		{"workspace": "loft", "key": "loft.robots", "value": "on"},
		{"workspace": ws, "key": "robots", "value": "on"},
	} {
		code, b := d.ask(aliceKey, http.MethodPost, "/admin/products/loft/grants", body)
		x.Equal(http.StatusBadRequest, code, "%v: %s", body, b)
	}

	var ended admin.Grant
	d.as(aliceKey, http.MethodPost, "/admin/grants/"+pilot.Id+"/end", map[string]string{}, &ended)
	x.False(ended.Renews)
	x.Equal(uint64(2), ended.Version)
	x.False(ended.Until.After(time.Now()))

	var vs []admin.Grant
	d.as(aliceKey, http.MethodGet, "/admin/products/loft/grants", nil, &vs)
	x.Len(vs, 3)
	x.Equal(more.Id, vs[0].Id, "the latest given first")
	d.as(aliceKey, http.MethodGet, "/admin/products/loft/grants?workspace="+levyantest.Tenant().String(), nil, &vs)
	x.Empty(vs)

	code, _ = d.ask(aliceKey, http.MethodPost, "/admin/grants/"+levyantest.Tenant().String()+"/end", map[string]string{})
	x.Equal(http.StatusNotFound, code)
	code, _ = d.ask(aliceKey, http.MethodPost, "/admin/products/nobody/grants", map[string]any{"workspace": ws, "key": "loft.x", "value": "on"})
	x.Equal(http.StatusNotFound, code)
}
