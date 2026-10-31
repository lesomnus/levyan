package admin

import (
	"net/http"
	"strings"
	"time"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entfinding "github.com/lesomnus/levyan/internal/ent/finding"
	enttenant "github.com/lesomnus/levyan/internal/ent/tenant"
	"github.com/lesomnus/levyan/server/reconcile"
)

// Finding is one thing reconciliation found drifted, as an operator finds
// it: what it says, what it is about, and what they do about it.
type Finding struct {
	Id       string `json:"id"`
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	State    string `json:"state"`
	Subject  string `json:"subject"`
	Product  string `json:"product,omitempty"`
	Summary  string `json:"summary"`

	Rows     []FindingRow `json:"rows,omitempty"`
	Amount   int64        `json:"amount,omitempty"`
	Currency string       `json:"currency,omitempty"`

	// Correction is what an operator does about it, and Command the command
	// that does it.
	Correction string `json:"correction"`
	Command    string `json:"command,omitempty"`

	Found      time.Time  `json:"found"`
	Updated    time.Time  `json:"updated"`
	Resolved   *time.Time `json:"resolved,omitempty"`
	Resolution string     `json:"resolution,omitempty"`
	Accepted   *time.Time `json:"accepted,omitempty"`
	AcceptedBy string     `json:"accepted_by,omitempty"`
	Reason     string     `json:"reason,omitempty"`
}

// FindingRow is a row a finding is about.
type FindingRow struct {
	Entity string `json:"entity"`
	Ref    string `json:"ref"`
}

// Pass is what a pass of reconciliation found, and did of it.
type Pass struct {
	At         time.Time  `json:"at"`
	Checks     []Check    `json:"checks"`
	Found      int        `json:"found"`
	Open       int        `json:"open"`
	New        int        `json:"new"`
	Changed    int        `json:"changed"`
	Resolved   int        `json:"resolved"`
	Statements Statements `json:"statements"`

	// Findings is every finding that is open as it ends.
	Findings []Finding `json:"findings"`
}

// Check is one check of a pass.
type Check struct {
	Name    string `json:"name"`
	Skipped bool   `json:"skipped,omitempty"`
	Found   int    `json:"found"`
	Err     string `json:"err,omitempty"`
}

// Statements is how many statements with a billing account are on one
// invoice, on none, and on more than one.
type Statements struct {
	WithAccount int `json:"with_account"`
	OnOne       int `json:"on_one"`
	OnNone      int `json:"on_none"`
	OnMore      int `json:"on_more"`
}

func findingOf(v *ent.Finding) Finding {
	k, _ := reconcile.KindOf(v.Kind)
	out := Finding{
		Id:       pdid.Id(v.Id).String(),
		Kind:     named(v.Kind, "FINDING_KIND_"),
		Severity: named(levyan.FindingSeverity(v.Severity).String(), "FINDING_SEVERITY_"),
		State:    named(levyan.FindingState(v.State).String(), "FINDING_STATE_"),
		Subject:  v.Subject, Summary: v.Summary,
		Amount: v.Amount, Currency: v.Currency,
		Correction: named(levyan.FindingCorrection(v.Correction).String(), "FINDING_CORRECTION_"),
		Found:      v.DateCreated.UTC(), Updated: v.DateUpdated.UTC(),
		Resolved: v.DateResolved, Resolution: v.Resolution,
		Accepted: v.DateAccepted, Reason: v.Reason,
	}
	if t := v.Edges.Product; t != nil {
		out.Product = t.Alias
	}
	if o := v.Edges.AcceptedBy; o != nil {
		out.AcceptedBy = o.Alias
	}
	for _, r := range v.Rows.GetRows() {
		out.Rows = append(out.Rows, FindingRow{Entity: r.GetEntity(), Ref: r.GetRef()})
	}
	out.Command = command(k, v, out)

	return out
}

// command is the command that does what finding `v` asks for, as an
// operator runs it.
func command(k levyan.FindingKind, v *ent.Finding, f Finding) string {
	of := func(entity string) string {
		for _, r := range f.Rows {
			if r.Entity == entity {
				return r.Ref
			}
		}
		return ""
	}
	if levyan.FindingState(v.State) != levyan.FindingState_FINDING_STATE_OPEN {
		return ""
	}
	switch reconcile.CorrectionOf(k) {
	case levyan.FindingCorrection_FINDING_CORRECTION_REFUND:
		return "levyan-admin reconcile refund --yes " + of("PaymentAttempt")
	case levyan.FindingCorrection_FINDING_CORRECTION_SETTLE:
		return "levyan-admin reconcile settle --yes " + of("PaymentAttempt")
	case levyan.FindingCorrection_FINDING_CORRECTION_RULE:
		return "levyan-admin reconcile rule --yes " + f.Product
	case levyan.FindingCorrection_FINDING_CORRECTION_CLOSE:
		return "levyan-admin period close --yes " + f.Product
	case levyan.FindingCorrection_FINDING_CORRECTION_REASK:
		return ""
	}

	return "levyan-admin reconcile accept --reason '…' --yes " + f.Id
}

// reconcile is a pass of reconciliation made now, as the operator: what it
// writes is on the trail as theirs.
func (a *Routes) reconcile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rep, err := a.S.Reconciler.Run(ctx)
	if err != nil {
		fail(w, r, err)
		return
	}
	out := Pass{
		At: rep.At, Found: rep.Found, Open: rep.Open, New: rep.New, Changed: rep.Changed, Resolved: rep.Resolved,
		Statements: Statements{WithAccount: rep.Statements.WithAccount, OnOne: rep.Statements.OnOne, OnNone: rep.Statements.OnNone, OnMore: rep.Statements.OnMore},
		Findings:   []Finding{},
	}
	for _, c := range rep.Checks {
		out.Checks = append(out.Checks, Check{Name: c.Name, Skipped: c.Skipped, Found: c.Found, Err: c.Err})
	}
	vs, err := a.S.Ent.Finding.Query().
		Where(entfinding.State(int32(levyan.FindingState_FINDING_STATE_OPEN))).
		WithProduct().WithAcceptedBy().
		Order(ent.Asc(entfinding.FieldDateCreated), ent.Asc(entfinding.FieldId)).
		All(ctx)
	if err != nil {
		fail(w, r, err)
		return
	}
	for _, v := range vs {
		out.Findings = append(out.Findings, findingOf(v))
	}
	answer(w, http.StatusOK, out)
}

// findings are what reconciliation found, the oldest first: those that are
// so -- open or accepted -- unless the query names a state, or `all`; of one
// kind, and of one product, when it names them.
func (a *Routes) findings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := a.S.Ent.Finding.Query()
	switch v := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("state"))); v {
	case "":
		q = q.Where(entfinding.StateIn(int32(levyan.FindingState_FINDING_STATE_OPEN), int32(levyan.FindingState_FINDING_STATE_ACCEPTED)))
	case "all":
	default:
		s, ok := value(v, "FINDING_STATE_", levyan.FindingState_value)
		if !ok {
			http.Error(w, "state: open, accepted, resolved or all", http.StatusBadRequest)
			return
		}
		q = q.Where(entfinding.State(s))
	}
	if v := strings.TrimSpace(r.URL.Query().Get("kind")); v != "" {
		name := "FINDING_KIND_" + strings.ToUpper(strings.TrimPrefix(strings.ToUpper(v), "FINDING_KIND_"))
		if _, ok := reconcile.KindOf(name); !ok {
			http.Error(w, "kind: a kind of finding, as `reconcile ls` says it: twice", http.StatusBadRequest)
			return
		}
		q = q.Where(entfinding.Kind(name))
	}
	if v := strings.TrimSpace(r.URL.Query().Get("product")); v != "" {
		t, err := a.S.Ent.Tenant.Query().Where(enttenant.Alias(v)).Only(ctx)
		if ent.IsNotFound(err) {
			http.Error(w, "no product is "+v, http.StatusNotFound)
			return
		}
		if err != nil {
			fail(w, r, err)
			return
		}
		q = q.Where(entfinding.ProductId(t.Id))
	}

	vs, err := q.WithProduct().WithAcceptedBy().
		Order(ent.Asc(entfinding.FieldDateCreated), ent.Asc(entfinding.FieldId)).
		Limit(1000).
		All(ctx)
	if err != nil {
		fail(w, r, err)
		return
	}
	out := []Finding{}
	for _, v := range vs {
		out = append(out, findingOf(v))
	}
	answer(w, http.StatusOK, out)
}

// findingBy is the finding the path names, or false once it has answered.
func (a *Routes) findingBy(w http.ResponseWriter, r *http.Request) (pdid.Id, bool) {
	id, err := pdid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "a finding, by the identifier `reconcile ls` lists", http.StatusBadRequest)
		return pdid.Nil, false
	}

	return id, true
}

func (a *Routes) finding(w http.ResponseWriter, r *http.Request) {
	id, ok := a.findingBy(w, r)
	if !ok {
		return
	}
	a.showFinding(w, r, id, http.StatusOK)
}

func (a *Routes) showFinding(w http.ResponseWriter, r *http.Request, id pdid.Id, code int) {
	v, err := a.S.Ent.Finding.Query().Where(entfinding.Id(id.Uuid())).WithProduct().WithAcceptedBy().Only(r.Context())
	if ent.IsNotFound(err) {
		http.Error(w, "no finding is "+id.String(), http.StatusNotFound)
		return
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	answer(w, code, findingOf(v))
}

// accept accepts a finding as it is, with the operator's reason.
func (a *Routes) accept(w http.ResponseWriter, r *http.Request) {
	id, ok := a.findingBy(w, r)
	if !ok {
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if !read(w, r, &in) {
		return
	}
	if _, err := a.S.Reconciler.Accept(r.Context(), id, in.Reason); err != nil {
		fail(w, r, err)
		return
	}
	a.showFinding(w, r, id, http.StatusOK)
}

// refundExtra gives back what an attempt's key was charged past what the
// ledger accounts for.
func (a *Routes) refundExtra(w http.ResponseWriter, r *http.Request) {
	vs, err := a.S.Reconciler.Refund(r.Context(), r.PathValue("attempt"))
	if err != nil {
		fail(w, r, err)
		return
	}
	out := []Attempt{}
	for _, v := range vs {
		out = append(out, attemptOf(v))
	}
	answer(w, http.StatusCreated, out)
}

// settle settles an attempt from what the channel answers of it.
func (a *Routes) settle(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Transaction string `json:"transaction"`
	}
	if !read(w, r, &in) {
		return
	}
	v, err := a.S.Reconciler.Settle(r.Context(), r.PathValue("attempt"), in.Transaction)
	if err != nil {
		fail(w, r, err)
		return
	}
	answer(w, http.StatusOK, attemptOf(v))
}

// rule follows a product's rules again: one, or every one.
func (a *Routes) rule(w http.ResponseWriter, r *http.Request) {
	t, ok := a.product(w, r)
	if !ok {
		return
	}
	var in struct {
		Rule string `json:"rule"`
	}
	if !read(w, r, &in) {
		return
	}
	if err := a.S.Reconciler.Rule(r.Context(), pdid.Id(t.Id), in.Rule); err != nil {
		fail(w, r, err)
		return
	}
	a.grants(w, r)
}
