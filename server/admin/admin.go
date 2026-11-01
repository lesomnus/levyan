// Package admin is the operator's entry point: what an operator does to the
// price book from outside every product -- puts a product up, issues and
// revokes its keys, writes its meters, publishes its prices, asks which was in
// force when and which are missing -- what it asks of the ledger -- which
// workspaces have stopped sending readings -- what it grants a workspace by
// hand, what it gives them beyond their prices -- allowances and credit
// (#43) -- what it does to invoices -- voids one, writes a credit note
// against one, records a transfer for one -- and what it does of what reconciliation
// found (#13) -- makes a pass now, accepts a finding, gives back what a key
// was charged twice, settles an attempt from what its channel answers,
// follows a product's rules again -- over HTTP, on a listener of its own.
//
// It is served by `levyan-admin serve`, and `levyan serve` links none of it:
// the public binary has no path that writes as the deployment. Every request
// carries an operator's key (`admin.keys`). What one writes goes through the
// server with no wall (`cmd.Server.Ungated`) -- a product is put up by whoever
// runs the deployment, never from inside one -- with the operator's row as its
// actor, so that the trail says who published a price and not only that one
// was.
package admin

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"mime"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lesomnus/otx/log"
	"github.com/lesomnus/xli/cfg"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lesomnus/payday/frame"
	"github.com/lesomnus/payday/pderr"
	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/cmd"
	"github.com/lesomnus/levyan/internal/ent"
	entmeter "github.com/lesomnus/levyan/internal/ent/meter"
	"github.com/lesomnus/levyan/internal/ent/operator"
	"github.com/lesomnus/levyan/internal/ent/servicekey"
	enttenant "github.com/lesomnus/levyan/internal/ent/tenant"
	"github.com/lesomnus/levyan/internal/pricing"
	"github.com/lesomnus/levyan/server/keys"
	"github.com/lesomnus/levyan/server/ledger"
)

// Operator is somebody who may use this entry point: their name, and the
// digest of the key they hold.
type Operator struct {
	Name string
	sum  [32]byte
}

// MinKey is the shortest key an operator may hold: this door opens every
// product there is.
const MinKey = 32

// name is the grammar of an operator's name, which is an alias's: what the
// trail's row of them is named by.
var name = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)

// Operators reads `admin.keys`: each operator's name and key.
func Operators(keys map[string]cfg.Secret) ([]Operator, error) {
	out := []Operator{}
	for _, n := range slices.Sorted(maps.Keys(keys)) {
		if !name.MatchString(n) {
			return nil, fmt.Errorf("admin.keys.%s: an operator's name is lower case letters, digits and single hyphens", n)
		}
		v, err := keys[n].Value()
		if err != nil {
			return nil, fmt.Errorf("admin.keys.%s: %w", n, err)
		}
		if len(v) < MinKey {
			return nil, fmt.Errorf("admin.keys.%s: a key that opens every product is not %d characters long; give it %d or more", n, len(v), MinKey)
		}
		out = append(out, Operator{Name: n, sum: sha256.Sum256([]byte(v))})
	}

	return out, nil
}

// Routes is the operator's entry point on a deployment.
type Routes struct {
	S         *cmd.Server
	Operators []Operator

	// Expect is what is expected of readings, which says what a gap is
	// (`readings` of the configuration).
	Expect ledger.Expect

	// rows is each operator's row, by name, which [Routes.Sync] makes -- and
	// may make again while the door is open.
	rows atomic.Pointer[map[string]*levyan.Operator]
}

// Sync makes the deployment's operators what `admin.keys` says: a row for each
// name it lists that has none, and the rows of names it no longer lists
// erased -- their key opens nothing anyway, and what the trail names them on
// still names somebody. Written through the server with no wall, as the
// deployment's own work.
func (a *Routes) Sync(ctx context.Context) error {
	have, err := a.S.Ent.Operator.Query().Where(operator.DateErasedIsNil()).All(ctx)
	if err != nil {
		return err
	}

	rows := map[string]*levyan.Operator{}
	for _, v := range have {
		rows[v.Alias] = levyan.Operator_builder{Id: v.Id[:], Alias: v.Alias}.Build()
	}

	listed := map[string]bool{}
	for _, o := range a.Operators {
		listed[o.Name] = true
		if _, ok := rows[o.Name]; ok {
			continue
		}
		v, err := a.S.Ungated.Operator().Add(ctx, levyan.OperatorAddRequest_builder{Alias: o.Name}.Build())
		if status.Code(err) == codes.AlreadyExists {
			// Another entry point coming up beside this one wrote it first.
			var u *ent.Operator
			if u, err = a.S.Ent.Operator.Query().Where(operator.Alias(o.Name), operator.DateErasedIsNil()).Only(ctx); err == nil {
				v = levyan.Operator_builder{Id: u.Id[:], Alias: u.Alias}.Build()
			}
		}
		if err != nil {
			return fmt.Errorf("operator %s: %w", o.Name, err)
		}
		rows[o.Name] = v
	}
	for n, v := range rows {
		if listed[n] {
			continue
		}
		if _, err := a.S.Ungated.Operator().Erase(ctx, levyan.OperatorRef_builder{Id: v.GetId()}.Build()); err != nil {
			return fmt.Errorf("operator %s: %w", n, err)
		}
		delete(rows, n)
	}

	a.rows.Store(&rows)
	return nil
}

type operatorKey struct{}

// Who is the operator a request was made by.
func Who(ctx context.Context) string {
	v, _ := ctx.Value(operatorKey{}).(string)
	return v
}

// Handler serves the entry point. [Routes.Sync] has to have run.
func (a *Routes) Handler() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /admin/products", a.products)
	api.HandleFunc("POST /admin/products", a.addProduct)
	api.HandleFunc("GET /admin/products/{product}", a.show)
	api.HandleFunc("POST /admin/products/{product}/keys", a.issue)
	api.HandleFunc("POST /admin/keys/{id}/revoke", a.revoke)
	api.HandleFunc("POST /admin/products/{product}/meters", a.addMeter)
	api.HandleFunc("GET /admin/products/{product}/prices", a.prices)
	api.HandleFunc("POST /admin/products/{product}/prices", a.publish)
	api.HandleFunc("GET /admin/products/{product}/timeline", a.timeline)
	api.HandleFunc("GET /admin/gaps", a.gaps)
	api.HandleFunc("POST /admin/products/{product}/terms", a.terms)
	api.HandleFunc("GET /admin/products/{product}/periods", a.periods)
	api.HandleFunc("POST /admin/products/{product}/periods/close", a.closePeriods)
	api.HandleFunc("GET /admin/products/{product}/statements", a.statements)
	api.HandleFunc("GET /admin/statements/{id}", a.statement)
	api.HandleFunc("GET /admin/products/{product}/grants", a.grants)
	api.HandleFunc("POST /admin/products/{product}/grants", a.give)
	api.HandleFunc("POST /admin/grants/{id}/end", a.endGrant)
	api.HandleFunc("GET /admin/products/{product}/allowances", a.allowances)
	api.HandleFunc("POST /admin/products/{product}/allowances", a.giveAllowance)
	api.HandleFunc("POST /admin/allowances/{id}/end", a.endAllowance)
	api.HandleFunc("GET /admin/products/{product}/credits", a.credits)
	api.HandleFunc("POST /admin/products/{product}/credits", a.grantCredit)
	api.HandleFunc("POST /admin/credits/{id}/expire", a.expireCredit)
	api.HandleFunc("GET /admin/products/{product}/invoices", a.invoices)
	api.HandleFunc("GET /admin/invoices/{id}", a.invoice)
	api.HandleFunc("POST /admin/invoices/{id}/void", a.voidInvoice)
	api.HandleFunc("POST /admin/invoices/{id}/credit", a.creditInvoice)
	api.HandleFunc("POST /admin/invoices/{id}/paid", a.paidInvoice)
	api.HandleFunc("GET /admin/products/{product}/unpriced", a.unpriced)
	api.HandleFunc("POST /admin/billing-accounts", a.openBillingAccount)
	api.HandleFunc("POST /admin/reconcile", a.reconcile)
	api.HandleFunc("GET /admin/findings", a.findings)
	api.HandleFunc("GET /admin/findings/{id}", a.finding)
	api.HandleFunc("POST /admin/findings/{id}/accept", a.accept)
	api.HandleFunc("POST /admin/attempts/{attempt}/refund", a.refundExtra)
	api.HandleFunc("POST /admin/attempts/{attempt}/settle", a.settle)
	api.HandleFunc("POST /admin/products/{product}/rules", a.rule)

	mux := http.NewServeMux()
	mux.Handle("/admin/", a.authorized(api))
	return mux
}

type recorder struct {
	http.ResponseWriter
	code int
}

func (r *recorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

// authorized lets through a request that carries an operator's key, as that
// operator -- their row in the frame, which is what the trail names as the
// actor of what the request writes -- and says in the log who made it and what
// came of it. Never the key, which is in no log.
func (a *Routes) authorized(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		who := ""
		if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			sum := sha256.Sum256([]byte(strings.TrimSpace(tok)))
			// Every operator is compared, so that how long it takes does not
			// say how far down the list a key would have been.
			for _, o := range a.Operators {
				if subtle.ConstantTimeCompare(sum[:], o.sum[:]) == 1 {
					who = o.Name
				}
			}
		}
		var row *levyan.Operator
		ok := false
		if rows := a.rows.Load(); rows != nil {
			row, ok = (*rows)[who]
		}
		id, err := pdid.From(row.GetId())
		if who == "" || !ok || err != nil {
			log.From(ctx).WarnContext(ctx, "admin: a request without an operator's key", "addr", r.RemoteAddr, "method", r.Method, "path", r.URL.Path)
			http.Error(w, "who is asking?", http.StatusUnauthorized)
			return
		}

		f := frame.New(id, pdid.Nil, frame.Whole()).WithRow(row)
		f.Name = who
		ctx = context.WithValue(frame.Into(ctx, f), operatorKey{}, who)

		rec := &recorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r.WithContext(ctx))
		log.From(ctx).InfoContext(ctx, "admin", "operator", who, "method", r.Method, "path", r.URL.Path, "status", rec.code)
	})
}

// Product is a product as an operator finds it.
type Product struct {
	Id      string    `json:"id"`
	Alias   string    `json:"alias"`
	Name    string    `json:"name"`
	Desc    string    `json:"desc,omitempty"`
	Created time.Time `json:"created"`

	// Its terms: the zone its periods are months in, and the channel and
	// currency its statements are priced on and in.
	Zone     string `json:"zone,omitempty"`
	Channel  string `json:"channel,omitempty"`
	Currency string `json:"currency,omitempty"`
}

// Detail is one product, with its keys and its meters.
type Detail struct {
	Product
	Keys   []Key   `json:"keys"`
	Meters []Meter `json:"meters"`
}

// Key is one of a product's keys, and never the key itself.
type Key struct {
	Id      string     `json:"id"`
	Alias   string     `json:"alias"`
	Desc    string     `json:"desc,omitempty"`
	Expires *time.Time `json:"expires,omitempty"`
	Created time.Time  `json:"created"`
	Revoked *time.Time `json:"revoked,omitempty"`
}

// Issued is a key just issued, and the one time it is said.
type Issued struct {
	Key
	Token string `json:"token"`
}

// Meter is one of a product's meters.
type Meter struct {
	Id            string    `json:"id"`
	Key           string    `json:"key"`
	Name          string    `json:"name,omitempty"`
	Desc          string    `json:"desc,omitempty"`
	Unit          string    `json:"unit"`
	Kind          string    `json:"kind"`
	Aggregation   string    `json:"aggregation"`
	UnboundedZero bool      `json:"unbounded_zero,omitempty"`
	Created       time.Time `json:"created"`
}

// Price is one version of what a meter costs.
type Price struct {
	Id        string    `json:"id"`
	Meter     string    `json:"meter"`
	Channel   string    `json:"channel"`
	Currency  string    `json:"currency"`
	Included  int64     `json:"included,omitempty"`
	UnitPrice int64     `json:"unit_price,omitempty"`
	Per       int64     `json:"per,omitempty"`
	Bands     []Band    `json:"bands,omitempty"`
	Effective time.Time `json:"effective"`
	Existing  time.Time `json:"existing"`
	Published time.Time `json:"published"`
	Desc      string    `json:"desc,omitempty"`
}

// Band is one band of a banded price; the last has no bound.
type Band struct {
	UpTo *int64 `json:"up_to,omitempty"`
	Fee  int64  `json:"fee"`
}

// Segment is a stretch of time, and the version in force through it -- or
// none.
type Segment struct {
	From  time.Time `json:"from"`
	To    time.Time `json:"to"`
	Price *Price    `json:"price,omitempty"`
}

func productOf(t *ent.Tenant) Product {
	return Product{
		Id: pdid.Id(t.Id).String(), Alias: t.Alias, Name: t.Name, Desc: t.Desc, Created: t.DateCreated,
		Zone: t.Zone, Channel: named(t.Channel, "CHANNEL_"), Currency: t.Currency,
	}
}

func keyOf(v *ent.ServiceKey) Key {
	return Key{
		Id: pdid.Id(v.Id).String(), Alias: v.Alias, Desc: v.Desc,
		Expires: v.DateExpires, Created: v.DateCreated, Revoked: v.DateErased,
	}
}

func meterOf(v *ent.Meter) Meter {
	return Meter{
		Id: pdid.Id(v.Id).String(), Key: v.Key, Name: v.Name, Desc: v.Desc, Unit: v.Unit,
		Kind:          named(levyan.MeterKind(v.Kind).String(), "METER_KIND_"),
		Aggregation:   named(levyan.Aggregation(v.Aggregation).String(), "AGGREGATION_"),
		UnboundedZero: v.UnboundedZero,
		Created:       v.DateCreated,
	}
}

func priceOf(v *levyan.Price, meter string) Price {
	p := Price{
		Id:        text(v.GetId()),
		Meter:     meter,
		Channel:   named(v.GetChannel().String(), "CHANNEL_"),
		Currency:  v.GetCurrency(),
		Included:  v.GetIncluded(),
		UnitPrice: v.GetUnitPrice(),
		Per:       v.GetPer(),
		Effective: v.GetDateEffective().AsTime(),
		Existing:  v.GetDateExisting().AsTime(),
		Published: v.GetDateCreated().AsTime(),
		Desc:      v.GetDesc(),
	}
	for _, b := range v.GetBands().GetBands() {
		band := Band{Fee: b.GetFee()}
		if b.HasUpTo() {
			band.UpTo = proto.Int64(b.GetUpTo())
		}
		p.Bands = append(p.Bands, band)
	}

	return p
}

// named is an enum's value as a request writes it: "stock" for
// METER_KIND_STOCK, "pg_kr" for CHANNEL_PG_KR.
func named(v, prefix string) string { return strings.ToLower(strings.TrimPrefix(v, prefix)) }

// value is the enum value a request names, or false.
func value(name, prefix string, values map[string]int32) (int32, bool) {
	v, ok := values[prefix+strings.ToUpper(strings.TrimSpace(name))]
	return v, ok && v != 0
}

func text(b []byte) string {
	id, err := pdid.From(b)
	if err != nil {
		return ""
	}

	return id.String()
}

func (a *Routes) products(w http.ResponseWriter, r *http.Request) {
	ts, err := a.S.Ent.Tenant.Query().Order(ent.Asc(enttenant.FieldAlias)).All(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	out := []Product{}
	for _, t := range ts {
		out = append(out, productOf(t))
	}
	answer(w, http.StatusOK, out)
}

func (a *Routes) product(w http.ResponseWriter, r *http.Request) (*ent.Tenant, bool) {
	t, err := a.S.Ent.Tenant.Query().Where(enttenant.Alias(r.PathValue("product"))).Only(r.Context())
	if ent.IsNotFound(err) {
		http.Error(w, "no product is "+r.PathValue("product"), http.StatusNotFound)
		return nil, false
	}
	if err != nil {
		fail(w, r, err)
		return nil, false
	}

	return t, true
}

func (a *Routes) addProduct(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Alias string `json:"alias"`
		Name  string `json:"name"`
		Desc  string `json:"desc"`
	}
	if !read(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Alias) == "" {
		http.Error(w, "a product is put up under an alias: loft", http.StatusBadRequest)
		return
	}
	v, err := a.S.Ungated.Tenant().Add(r.Context(), levyan.TenantAddRequest_builder{
		Alias: strings.TrimSpace(in.Alias),
		Name:  strings.TrimSpace(in.Name),
		Desc:  strings.TrimSpace(in.Desc),
	}.Build())
	if err != nil {
		fail(w, r, err)
		return
	}
	r.SetPathValue("product", v.GetAlias())
	a.show(w, r)
}

func (a *Routes) show(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, ok := a.product(w, r)
	if !ok {
		return
	}
	out := Detail{Product: productOf(t), Keys: []Key{}, Meters: []Meter{}}

	ks, err := a.S.Ent.ServiceKey.Query().Where(servicekey.TenantId(t.Id)).Order(ent.Asc(servicekey.FieldDateCreated, servicekey.FieldId)).All(ctx)
	if err != nil {
		fail(w, r, err)
		return
	}
	for _, k := range ks {
		out.Keys = append(out.Keys, keyOf(k))
	}

	ms, err := a.S.Ent.Meter.Query().Where(entmeter.TenantId(t.Id)).Order(ent.Asc(entmeter.FieldDateCreated, entmeter.FieldId)).All(ctx)
	if err != nil {
		fail(w, r, err)
		return
	}
	for _, m := range ms {
		out.Meters = append(out.Meters, meterOf(m))
	}

	answer(w, http.StatusOK, out)
}

func (a *Routes) issue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, ok := a.product(w, r)
	if !ok {
		return
	}
	var in struct {
		Alias   string     `json:"alias"`
		Desc    string     `json:"desc"`
		Expires *time.Time `json:"expires"`
	}
	if !read(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Alias) == "" {
		http.Error(w, "a key is issued under a name an operator knows it by: loft-prod", http.StatusBadRequest)
		return
	}

	token, v, err := keys.Issue(ctx, a.S.Ungated, levyan.TenantRef_builder{Id: t.Id[:]}.Build(), strings.TrimSpace(in.Alias), strings.TrimSpace(in.Desc), in.Expires)
	if err != nil {
		fail(w, r, err)
		return
	}
	id, err := pdid.From(v.GetId())
	if err != nil {
		fail(w, r, err)
		return
	}
	k, err := a.S.Ent.ServiceKey.Get(ctx, id.Uuid())
	if err != nil {
		fail(w, r, err)
		return
	}

	answer(w, http.StatusCreated, Issued{Key: keyOf(k), Token: token})
}

func (a *Routes) revoke(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := pdid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "a key, by the identifier `product show` lists", http.StatusBadRequest)
		return
	}
	if _, err := a.S.Ungated.ServiceKey().Erase(ctx, levyan.ServiceKeyRef_builder{Id: id.Bytes()}.Build()); err != nil {
		fail(w, r, err)
		return
	}
	k, err := a.S.Ent.ServiceKey.Get(ctx, id.Uuid())
	if err != nil {
		fail(w, r, err)
		return
	}

	answer(w, http.StatusOK, keyOf(k))
}

func (a *Routes) addMeter(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, ok := a.product(w, r)
	if !ok {
		return
	}
	var in struct {
		Key           string `json:"key"`
		Name          string `json:"name"`
		Desc          string `json:"desc"`
		Unit          string `json:"unit"`
		Kind          string `json:"kind"`
		Aggregation   string `json:"aggregation"`
		UnboundedZero bool   `json:"unbounded_zero"`
	}
	if !read(w, r, &in) {
		return
	}
	kind, ok := value(in.Kind, "METER_KIND_", levyan.MeterKind_value)
	if !ok {
		http.Error(w, "kind: stock or flow", http.StatusBadRequest)
		return
	}
	agg, ok := value(in.Aggregation, "AGGREGATION_", levyan.Aggregation_value)
	if !ok {
		http.Error(w, "aggregation: time_weighted_average, max, last or sum", http.StatusBadRequest)
		return
	}

	v, err := a.S.Ungated.Meter().Add(ctx, levyan.MeterAddRequest_builder{
		Tenant:        levyan.TenantRef_builder{Id: t.Id[:]}.Build(),
		Key:           strings.TrimSpace(in.Key),
		Name:          strings.TrimSpace(in.Name),
		Desc:          strings.TrimSpace(in.Desc),
		Unit:          strings.TrimSpace(in.Unit),
		Kind:          levyan.MeterKind(kind),
		Aggregation:   levyan.Aggregation(agg),
		UnboundedZero: in.UnboundedZero,
	}.Build())
	if err != nil {
		fail(w, r, err)
		return
	}
	id, err := pdid.From(v.GetId())
	if err != nil {
		fail(w, r, err)
		return
	}
	m, err := a.S.Ent.Meter.Get(ctx, id.Uuid())
	if err != nil {
		fail(w, r, err)
		return
	}

	answer(w, http.StatusCreated, meterOf(m))
}

func (a *Routes) publish(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, ok := a.product(w, r)
	if !ok {
		return
	}
	var in struct {
		Meter     string     `json:"meter"`
		Channel   string     `json:"channel"`
		Currency  string     `json:"currency"`
		Included  int64      `json:"included"`
		UnitPrice int64      `json:"unit_price"`
		Per       int64      `json:"per"`
		Bands     []Band     `json:"bands"`
		Effective *time.Time `json:"effective"`
		Existing  *time.Time `json:"existing"`
		Desc      string     `json:"desc"`
	}
	if !read(w, r, &in) {
		return
	}
	channel, ok := value(in.Channel, "CHANNEL_", levyan.Channel_value)
	if !ok {
		http.Error(w, "channel: mor or pg_kr", http.StatusBadRequest)
		return
	}

	req := levyan.PriceAddRequest_builder{
		Tenant: levyan.TenantRef_builder{Id: t.Id[:]}.Build(),
		Meter: levyan.MeterRef_builder{Keyed: levyan.MeterRefByKeyed_builder{
			Key:    proto.String(strings.TrimSpace(in.Meter)),
			Tenant: levyan.TenantRef_builder{Id: t.Id[:]}.Build(),
		}.Build()}.Build(),
		Channel:   levyan.Channel(channel),
		Currency:  strings.TrimSpace(in.Currency),
		Included:  in.Included,
		UnitPrice: in.UnitPrice,
		Per:       in.Per,
		Desc:      strings.TrimSpace(in.Desc),
	}.Build()
	if len(in.Bands) > 0 {
		bands := []*levyan.PriceBand{}
		for _, b := range in.Bands {
			v := levyan.PriceBand_builder{Fee: b.Fee}.Build()
			if b.UpTo != nil {
				v.SetUpTo(*b.UpTo)
			}
			bands = append(bands, v)
		}
		req.SetBands(levyan.PriceBands_builder{Bands: bands}.Build())
	}
	if in.Effective != nil {
		req.SetDateEffective(timestamppb.New(*in.Effective))
	}
	if in.Existing != nil {
		req.SetDateExisting(timestamppb.New(*in.Existing))
	}

	v, err := a.S.Ungated.Price().Add(ctx, req)
	if err != nil {
		fail(w, r, err)
		return
	}

	answer(w, http.StatusCreated, priceOf(v, strings.TrimSpace(in.Meter)))
}

// versions is every version of a product's meter, by its key, as stored.
func (a *Routes) versions(ctx context.Context, t *ent.Tenant, key string) (*ent.Meter, []*levyan.Price, error) {
	m, err := a.S.Ent.Meter.Query().Where(entmeter.TenantId(t.Id), entmeter.Key(key)).Only(ctx)
	if err != nil {
		return nil, nil, err
	}

	var out []*levyan.Price
	after := ""
	for {
		vs, err := a.S.Ungated.Price().List(ctx, levyan.PriceListRequest_builder{
			Filters: []*levyan.PriceFilter{levyan.PriceFilter_builder{Meter: levyan.MeterRef_builder{Id: m.Id[:]}.Build()}.Build()},
			Size:    200,
			After:   after,
		}.Build())
		if err != nil {
			return nil, nil, err
		}
		out = append(out, vs.GetItems()...)
		if after = vs.GetNext(); after == "" {
			break
		}
	}

	return m, out, nil
}

func (a *Routes) prices(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, ok := a.product(w, r)
	if !ok {
		return
	}

	keys := []string{}
	if k := strings.TrimSpace(r.URL.Query().Get("meter")); k != "" {
		keys = append(keys, k)
	} else {
		ms, err := a.S.Ent.Meter.Query().Where(entmeter.TenantId(t.Id)).Order(ent.Asc(entmeter.FieldKey)).All(ctx)
		if err != nil {
			fail(w, r, err)
			return
		}
		for _, m := range ms {
			keys = append(keys, m.Key)
		}
	}

	out := []Price{}
	for _, k := range keys {
		_, vs, err := a.versions(ctx, t, k)
		if ent.IsNotFound(err) {
			http.Error(w, "no meter of "+t.Alias+" is "+k, http.StatusNotFound)
			return
		}
		if err != nil {
			fail(w, r, err)
			return
		}
		for _, v := range vs {
			out = append(out, priceOf(v, k))
		}
	}

	answer(w, http.StatusOK, out)
}

// timeline answers which version of a meter's price was in force for a
// customer over a stretch of time: on a channel, in a currency, for a customer
// since `since` (from `from`, when it says nothing), over [from, to).
func (a *Routes) timeline(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, ok := a.product(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	channel, ok := value(q.Get("channel"), "CHANNEL_", levyan.Channel_value)
	if !ok {
		http.Error(w, "channel: mor or pg_kr", http.StatusBadRequest)
		return
	}
	at := map[string]time.Time{}
	for _, k := range []string{"since", "from", "to"} {
		v := strings.TrimSpace(q.Get(k))
		if v == "" {
			continue
		}
		u, err := time.Parse(time.RFC3339, v)
		if err != nil {
			http.Error(w, k+": a moment, as RFC 3339", http.StatusBadRequest)
			return
		}
		at[k] = u
	}
	from, okFrom := at["from"]
	to, okTo := at["to"]
	if !okFrom || !okTo || !from.Before(to) {
		http.Error(w, "from and to: a stretch of time, from before to", http.StatusBadRequest)
		return
	}
	since, ok := at["since"]
	if !ok {
		since = from
	}

	key := strings.TrimSpace(q.Get("meter"))
	m, vs, err := a.versions(ctx, t, key)
	if ent.IsNotFound(err) {
		http.Error(w, "no meter of "+t.Alias+" is "+key, http.StatusNotFound)
		return
	}
	if err != nil {
		fail(w, r, err)
		return
	}

	book := pricing.Book{}
	byId := map[pdid.Id]*levyan.Price{}
	for _, v := range vs {
		p, err := pricing.VersionOf(v, m.UnboundedZero)
		if err != nil {
			fail(w, r, err)
			return
		}
		book = append(book, p)
		byId[p.Id] = v
	}

	line := pricing.Line{Meter: pdid.Id(m.Id), Channel: levyan.Channel(channel), Currency: strings.TrimSpace(q.Get("currency"))}
	out := []Segment{}
	for _, s := range book.Timeline(line, since, from, to) {
		v := Segment{From: s.From, To: s.To}
		if s.Priced {
			p := priceOf(byId[s.Version.Id], key)
			v.Price = &p
		}
		out = append(out, v)
	}

	answer(w, http.StatusOK, out)
}

// Gap is a workspace that has been sending a stock meter's readings and has
// sent none for longer than it should have.
type Gap struct {
	Product   string    `json:"product"`
	Workspace string    `json:"workspace"`
	Meter     string    `json:"meter"`
	Last      time.Time `json:"last"`
}

// gaps is every workspace without a reading for longer than it should have
// gone, as the loop that looks for them finds them (`levyan.readings.gaps`):
// of one product, when the query names one.
func (a *Routes) gaps(w http.ResponseWriter, r *http.Request) {
	product := strings.TrimSpace(r.URL.Query().Get("product"))
	vs, _, err := ledger.Gaps(r.Context(), a.S.Ent, ledger.Moment(a.S.Clock()), a.Expect)
	if err != nil {
		fail(w, r, err)
		return
	}

	out := []Gap{}
	for _, v := range vs {
		if product != "" && v.Product != product {
			continue
		}
		out = append(out, Gap{Product: v.Product, Workspace: v.Workspace.String(), Meter: v.Meter, Last: v.Last})
	}

	answer(w, http.StatusOK, out)
}

// read decodes a body that is JSON, which is all a change is asked as.
func read(w http.ResponseWriter, r *http.Request, v any) bool {
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/json" {
		http.Error(w, "a change is asked for as JSON", http.StatusUnsupportedMediaType)
		return false
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		http.Error(w, "the body: "+err.Error(), http.StatusBadRequest)
		return false
	}

	return true
}

func answer(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// fail says what the servers refused in their words, naming the field a
// refusal is about, and anything else is this server's.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	if s, ok := status.FromError(err); ok {
		msg := s.Message()
		if vs := pderr.Violations(err); len(vs) > 0 {
			parts := []string{}
			for _, v := range vs {
				parts = append(parts, v.String())
			}
			msg = strings.Join(parts, "; ")
		}
		switch s.Code() {
		case codes.InvalidArgument:
			http.Error(w, msg, http.StatusBadRequest)
			return
		case codes.FailedPrecondition, codes.AlreadyExists, codes.Aborted:
			http.Error(w, msg, http.StatusConflict)
			return
		case codes.NotFound:
			http.Error(w, msg, http.StatusNotFound)
			return
		}
	}
	log.From(r.Context()).ErrorContext(r.Context(), "admin", "operator", Who(r.Context()), slog.Any("err", err))
	http.Error(w, "something went wrong", http.StatusInternalServerError)
}
