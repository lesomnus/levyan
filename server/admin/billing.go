package admin

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entperiod "github.com/lesomnus/levyan/internal/ent/period"
	entstatement "github.com/lesomnus/levyan/internal/ent/statement"
	entworkspace "github.com/lesomnus/levyan/internal/ent/workspace"
	"github.com/lesomnus/levyan/server/billing"
	"github.com/lesomnus/levyan/server/ledger"
)

// Period is one period of a product's billing, closed or being.
type Period struct {
	Id     string     `json:"id"`
	Start  time.Time  `json:"start"`
	End    time.Time  `json:"end"`
	Zone   string     `json:"zone"`
	Closed *time.Time `json:"closed,omitempty"`
}

// Statement is what one workspace came to over one closed period. Its
// amounts are fractions, exactly as the statement keeps them, and its total
// is what they come to rounded once, down.
type Statement struct {
	Id        string    `json:"id"`
	Workspace string    `json:"workspace"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	Channel   string    `json:"channel"`
	Currency  string    `json:"currency"`
	Total     int64     `json:"total"`
	Exact     string    `json:"exact"`
	Rating    uint32    `json:"rating"`
	Closed    time.Time `json:"closed"`
	Lines     []Line    `json:"lines,omitempty"`
}

// Line is one line of a statement.
type Line struct {
	Meter    string     `json:"meter"`
	Unit     string     `json:"unit"`
	Kind     string     `json:"kind"`
	Corrects *time.Time `json:"corrects,omitempty"`
	Usage    string     `json:"usage"`
	Included string     `json:"included"`
	Billable string     `json:"billable"`
	Amount   string     `json:"amount"`

	// What of included the workspace's allowances gave, and what of billable
	// its pools took (#43): empty for none.
	Allowance string   `json:"allowance,omitempty"`
	Pooled    string   `json:"pooled,omitempty"`
	Billed    string   `json:"billed,omitempty"`
	Prices    []string `json:"prices,omitempty"`
}

func periodOf(v *ent.Period) Period {
	return Period{Id: pdid.Id(v.Id).String(), Start: v.DateStart.UTC(), End: v.DateEnd.UTC(), Zone: v.Zone, Closed: v.DateClosed}
}

func statementOf(v *ent.Statement, ws pdid.Id, lines bool) Statement {
	out := Statement{
		Id: pdid.Id(v.Id).String(), Workspace: ws.String(),
		Start: v.DateStart.UTC(), End: v.DateEnd.UTC(),
		Channel: named(v.Channel, "CHANNEL_"), Currency: v.Currency,
		Total: v.Total, Exact: v.Exact, Rating: v.Rating, Closed: v.DateCreated,
	}
	if !lines {
		return out
	}
	for _, l := range v.Lines.GetLines() {
		line := Line{
			Meter: l.GetKey(), Unit: l.GetUnit(), Kind: named(l.GetKind().String(), "LINE_KIND_"),
			Usage: l.GetUsage(), Included: l.GetIncluded(), Billable: l.GetBillable(), Amount: l.GetAmount(),
			Allowance: l.GetAllowance(), Pooled: l.GetPooled(),
		}
		if l.GetKind() == levyan.LineKind_LINE_KIND_CORRECTION {
			at := l.GetCorrectsStart().AsTime()
			line.Corrects = &at
			line.Billed = l.GetBilled()
		}
		for _, p := range l.GetParts() {
			line.Prices = append(line.Prices, text(p.GetPrice()))
		}
		out.Lines = append(out.Lines, line)
	}

	return out
}

// terms are a product's terms said: its zone, and the channel and currency
// its statements are priced on and in. What is left out stays as it is;
// empty is UTC, or none.
func (a *Routes) terms(w http.ResponseWriter, r *http.Request) {
	t, ok := a.product(w, r)
	if !ok {
		return
	}
	var in struct {
		Zone     *string `json:"zone"`
		Channel  *string `json:"channel"`
		Currency *string `json:"currency"`
	}
	if !read(w, r, &in) {
		return
	}

	req := levyan.TenantPatchRequest_builder{
		Ref:              levyan.TenantRef_builder{Id: t.Id[:]}.Build(),
		DateUpdatedForce: proto.Bool(true),
	}.Build()
	if in.Zone != nil {
		req.SetZone(strings.TrimSpace(*in.Zone))
	}
	if in.Channel != nil {
		c := strings.TrimSpace(*in.Channel)
		if c != "" {
			v, ok := value(c, "CHANNEL_", levyan.Channel_value)
			if !ok {
				http.Error(w, "channel: mor or pg_kr", http.StatusBadRequest)
				return
			}
			c = levyan.Channel(v).String()
		}
		req.SetChannel(c)
	}
	if in.Currency != nil {
		req.SetCurrency(strings.TrimSpace(*in.Currency))
	}

	if _, err := a.S.Ungated.Tenant().Patch(r.Context(), req); err != nil {
		fail(w, r, err)
		return
	}
	a.show(w, r)
}

func (a *Routes) periods(w http.ResponseWriter, r *http.Request) {
	t, ok := a.product(w, r)
	if !ok {
		return
	}
	vs, err := a.S.Ent.Period.Query().Where(entperiod.TenantId(t.Id)).Order(ent.Desc(entperiod.FieldDateStart)).All(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	out := []Period{}
	for _, v := range vs {
		out = append(out, periodOf(v))
	}
	answer(w, http.StatusOK, out)
}

// closePeriods closes, now, every period of a product that has ended by
// `through` -- now, unless the request says an earlier moment -- without
// waiting out the grace: an operator's hand, on the trail as theirs.
func (a *Routes) closePeriods(w http.ResponseWriter, r *http.Request) {
	t, ok := a.product(w, r)
	if !ok {
		return
	}
	var in struct {
		Through *time.Time `json:"through"`
	}
	if !read(w, r, &in) {
		return
	}

	now := ledger.Moment(a.S.Clock())
	through := now
	if in.Through != nil {
		if in.Through.After(now) {
			http.Error(w, "through: a period is closed once it has ended, and "+in.Through.UTC().Format(time.RFC3339)+" is ahead of now", http.StatusBadRequest)
			return
		}
		through = *in.Through
	}

	vs, err := a.S.Closer.Close(r.Context(), t, through)
	if errors.Is(err, billing.ErrUnpriced) {
		http.Error(w, "the product names no channel or no currency (`product configure`), so nothing of it is priced and no period of it closes", http.StatusConflict)
		return
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	out := []Period{}
	for _, v := range vs {
		out = append(out, periodOf(v))
	}
	answer(w, http.StatusOK, out)
}

// statements are a product's statements, the latest period first: of one
// workspace, when the query names one.
func (a *Routes) statements(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, ok := a.product(w, r)
	if !ok {
		return
	}

	q := a.S.Ent.Statement.Query().Where(entstatement.TenantId(t.Id))
	if v := strings.TrimSpace(r.URL.Query().Get("workspace")); v != "" {
		key, err := ledger.Workspace(v)
		if err != nil {
			http.Error(w, "workspace: "+err.Error(), http.StatusBadRequest)
			return
		}
		ws, err := a.S.Ent.Workspace.Query().Where(entworkspace.TenantId(t.Id), entworkspace.Key(key.Bytes())).Only(ctx)
		if ent.IsNotFound(err) {
			answer(w, http.StatusOK, []Statement{})
			return
		}
		if err != nil {
			fail(w, r, err)
			return
		}
		q = q.Where(entstatement.WorkspaceId(ws.Id))
	}

	vs, err := q.WithWorkspace().Order(ent.Desc(entstatement.FieldDateStart), ent.Asc(entstatement.FieldId)).Limit(500).All(ctx)
	if err != nil {
		fail(w, r, err)
		return
	}
	out := []Statement{}
	for _, v := range vs {
		ws, err := pdid.From(v.Edges.Workspace.Key)
		if err != nil {
			fail(w, r, err)
			return
		}
		out = append(out, statementOf(v, ws, false))
	}
	answer(w, http.StatusOK, out)
}

func (a *Routes) statement(w http.ResponseWriter, r *http.Request) {
	id, err := pdid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "a statement, by the identifier `statement ls` lists", http.StatusBadRequest)
		return
	}
	v, err := a.S.Ent.Statement.Query().Where(entstatement.Id(id.Uuid())).WithWorkspace().Only(r.Context())
	if ent.IsNotFound(err) {
		http.Error(w, "no statement is "+id.String(), http.StatusNotFound)
		return
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	ws, err := pdid.From(v.Edges.Workspace.Key)
	if err != nil {
		fail(w, r, err)
		return
	}

	answer(w, http.StatusOK, statementOf(v, ws, true))
}
