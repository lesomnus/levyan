package admin

import (
	"net/http"
	"strings"
	"time"

	"github.com/lesomnus/payday/entitle"
	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entgrant "github.com/lesomnus/levyan/internal/ent/grant"
	entworkspace "github.com/lesomnus/levyan/internal/ent/workspace"
	"github.com/lesomnus/levyan/server/grants"
	"github.com/lesomnus/levyan/server/ledger"
)

// Grant is one thing a workspace of a product was given, for a window of
// time.
type Grant struct {
	Id        string `json:"id"`
	Workspace string `json:"workspace"`
	Key       string `json:"key"`

	// Value is on, limit or unlimited, and Limit how much for a limit.
	Value string `json:"value"`
	Limit *int64 `json:"limit,omitempty"`

	From  time.Time `json:"from"`
	Until time.Time `json:"until"`

	// Renews is a grant whose end is moved on while it lasts.
	Renews bool `json:"renews,omitempty"`

	Version uint64     `json:"version"`
	Source  string     `json:"source"`
	Rule    string     `json:"rule,omitempty"`
	Desc    string     `json:"desc,omitempty"`
	Given   time.Time  `json:"given"`
	Erased  *time.Time `json:"erased,omitempty"`
}

func grantOf(v *ent.Grant) (Grant, error) {
	out := Grant{
		Id:    pdid.Id(v.Id).String(),
		Key:   v.Key,
		Value: named(levyan.GrantValue(v.Value).String(), "GRANT_VALUE_"),
		From:  v.ValidFrom.UTC(), Until: v.ValidUntil.UTC(), Renews: v.Renews,
		Version: v.Version,
		Source:  named(levyan.GrantSource(v.Source).String(), "GRANT_SOURCE_"),
		Rule:    v.Rule, Desc: v.Desc, Given: v.DateCreated.UTC(), Erased: v.DateErased,
	}
	if v.Value == int32(levyan.GrantValue_GRANT_VALUE_LIMIT) {
		out.Limit = &v.Limit
	}
	if w := v.Edges.Workspace; w != nil {
		ws, err := pdid.From(w.Key)
		if err != nil {
			return Grant{}, err
		}
		out.Workspace = ws.String()
	}

	return out, nil
}

// grant is the grant `id` names as an operator is told of it.
func (a *Routes) grant(r *http.Request, id []byte) (Grant, error) {
	k, err := pdid.From(id)
	if err != nil {
		return Grant{}, err
	}
	v, err := a.S.Ent.Grant.Query().Where(entgrant.Id(k.Uuid())).WithWorkspace().Only(r.Context())
	if err != nil {
		return Grant{}, err
	}

	return grantOf(v)
}

// give gives a workspace of a product a grant, by the operator's hand: a
// feature on, a limit or no bound, of a key, from now or a moment to come,
// until a moment of its own or renewed while it lasts. It is the operator's,
// a contract's or a trial's, and replaces what the operator gave the
// workspace of the key only when it says so.
func (a *Routes) give(w http.ResponseWriter, r *http.Request) {
	t, ok := a.product(w, r)
	if !ok {
		return
	}
	var in struct {
		Workspace string     `json:"workspace"`
		Key       string     `json:"key"`
		Value     string     `json:"value"`
		Limit     *int64     `json:"limit"`
		From      *time.Time `json:"from"`
		Until     *time.Time `json:"until"`
		Source    string     `json:"source"`
		Desc      string     `json:"desc"`
		Replace   bool       `json:"replace"`
	}
	if !read(w, r, &in) {
		return
	}

	ws, err := ledger.Workspace(strings.TrimSpace(in.Workspace))
	if err != nil {
		http.Error(w, "workspace: "+err.Error(), http.StatusBadRequest)
		return
	}

	var gives entitle.Value
	switch strings.TrimSpace(in.Value) {
	case "on":
		gives = entitle.On
	case "unlimited":
		gives = entitle.Unlimited
	case "limit":
		if in.Limit == nil {
			http.Error(w, "limit: a limit says how much", http.StatusBadRequest)
			return
		}
		gives = entitle.Limited(*in.Limit)
		if *in.Limit < 0 {
			http.Error(w, "limit: zero or more", http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, "value: on, limit or unlimited", http.StatusBadRequest)
		return
	}
	if in.Limit != nil && gives.Kind() != entitle.KindLimit {
		http.Error(w, "limit: only a limit says how much", http.StatusBadRequest)
		return
	}

	source := levyan.GrantSource_GRANT_SOURCE_OPERATOR
	if v := strings.TrimSpace(in.Source); v != "" {
		n, ok := value(v, "GRANT_SOURCE_", levyan.GrantSource_value)
		if !ok || !grants.ByHand(levyan.GrantSource(n)) {
			http.Error(w, "source: operator, contract or trial; a rule's grants are the rule's", http.StatusBadRequest)
			return
		}
		source = levyan.GrantSource(n)
	}

	give := grants.Give{
		Product:   pdid.Id(t.Id),
		Workspace: ws,
		Key:       strings.TrimSpace(in.Key),
		Value:     gives,
		Source:    source,
		Desc:      strings.TrimSpace(in.Desc),
		Replace:   in.Replace,
	}
	if in.From != nil {
		give.From = *in.From
	}
	if in.Until != nil {
		give.Until = *in.Until
	}

	v, err := a.S.Grants.Give(r.Context(), give)
	if err != nil {
		fail(w, r, err)
		return
	}
	out, err := a.grant(r, v.GetId())
	if err != nil {
		fail(w, r, err)
		return
	}

	answer(w, http.StatusCreated, out)
}

// endGrant ends a grant now, by the operator's hand: in force no longer, and
// never again. A rule's is the rule's, and is refused.
func (a *Routes) endGrant(w http.ResponseWriter, r *http.Request) {
	id, err := pdid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "a grant, by the identifier `grant ls` lists", http.StatusBadRequest)
		return
	}
	v, err := a.S.Grants.End(r.Context(), id)
	if err != nil {
		fail(w, r, err)
		return
	}
	out, err := a.grant(r, v.GetId())
	if err != nil {
		fail(w, r, err)
		return
	}

	answer(w, http.StatusOK, out)
}

// grants are a product's grants, the latest given first: of one workspace
// when the query names one, and those erased out of every list too with
// `all`.
func (a *Routes) grants(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, ok := a.product(w, r)
	if !ok {
		return
	}

	q := a.S.Ent.Grant.Query().Where(entgrant.TenantId(t.Id))
	if r.URL.Query().Get("all") != "true" {
		q = q.Where(entgrant.DateErasedIsNil())
	}
	if v := strings.TrimSpace(r.URL.Query().Get("workspace")); v != "" {
		key, err := ledger.Workspace(v)
		if err != nil {
			http.Error(w, "workspace: "+err.Error(), http.StatusBadRequest)
			return
		}
		q = q.Where(entgrant.HasWorkspaceWith(entworkspace.TenantId(t.Id), entworkspace.Key(key.Bytes())))
	}

	vs, err := q.WithWorkspace().Order(ent.Desc(entgrant.FieldDateCreated), ent.Desc(entgrant.FieldId)).Limit(500).All(ctx)
	if err != nil {
		fail(w, r, err)
		return
	}
	out := []Grant{}
	for _, v := range vs {
		g, err := grantOf(v)
		if err != nil {
			fail(w, r, err)
			return
		}
		out = append(out, g)
	}

	answer(w, http.StatusOK, out)
}
