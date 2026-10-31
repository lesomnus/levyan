// Package taxinvoicetest is an issuer of tax invoices to test against: it
// keeps what it issued by its key, refuses what the law would not take, and
// answers as a test tells it to -- an answer lost after it issued, a request
// lost before it arrived.
//
// Linking it registers the kind "test" ([taxinvoice.Register]): a test names
// that kind in its configuration, and finds the issuer the deployment made
// with [Of]. levyan's binaries do not link it (cmd/levyan's test).
package taxinvoicetest

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/lesomnus/levyan/internal/taxinvoice"
)

// Kind is the kind this registers.
const Kind = "test"

func init() {
	taxinvoice.Register(Kind, func() (taxinvoice.Issuer, error) { return New(), nil })
}

// Answer is how the next request is answered.
type Answer int

const (
	// Issue issues it, and says so.
	Issue Answer = iota

	// Lost issues it, and the answer never comes: Unknown.
	Lost

	// Dropped never arrives: Unknown, and nothing issued.
	Dropped
)

// Issuer is an issuer in a test.
type Issuer struct {
	mu     sync.Mutex
	script []Answer
	issued map[string]issued
	order  []string
	asks   int
	next   int
}

type issued struct {
	v   taxinvoice.Invoice
	out taxinvoice.Issued
}

var _ taxinvoice.Issuer = (*Issuer)(nil)

// New is an issuer that issues what it is asked for.
func New() *Issuer { return &Issuer{issued: map[string]issued{}} }

// Of is the issuer `v` is: what a deployment opened of kind [Kind].
func Of(v taxinvoice.Issuer) *Issuer {
	i, _ := v.(*Issuer)
	return i
}

// Then has the next requests answered so, in order.
func (i *Issuer) Then(as ...Answer) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.script = append(i.script, as...)
}

// Issued is every tax invoice issued, in the order it was.
func (i *Issuer) Issued() []taxinvoice.Invoice {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := []taxinvoice.Invoice{}
	for _, k := range i.order {
		out = append(out, i.issued[k].v)
	}
	return out
}

// Asked is how many times it was asked to issue, the same again included.
func (i *Issuer) Asked() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.asks
}

func (i *Issuer) Name() string { return Kind }

func (i *Issuer) Issue(_ context.Context, v taxinvoice.Invoice) (taxinvoice.Issued, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.asks++
	if have, ok := i.issued[v.Key]; ok {
		return have.out, nil
	}
	if err := v.Check(); err != nil {
		return taxinvoice.Issued{}, err
	}
	if v.Amends != nil {
		if _, ok := i.issued[v.Amends.Key]; !ok {
			return taxinvoice.Issued{}, &taxinvoice.Error{Op: "issue", Kind: taxinvoice.Invalid, Message: "amends: no tax invoice is " + v.Amends.Key}
		}
	}

	a := Issue
	if len(i.script) > 0 {
		a, i.script = i.script[0], i.script[1:]
	}
	if a == Dropped {
		return taxinvoice.Issued{}, &taxinvoice.Error{Op: "issue", Kind: taxinvoice.Unknown, Message: "the request never came"}
	}

	// An approval number has the shape of the NTS's -- the day, and
	// sixteen digits -- and is nobody's.
	i.next++
	out := taxinvoice.Issued{Key: v.Key, Approval: v.Written.Format("20060102") + fmt.Sprintf("00000000%08d", i.next), At: time.Now()}
	i.issued[v.Key] = issued{v: v, out: out}
	i.order = append(i.order, v.Key)
	if a == Lost {
		return taxinvoice.Issued{}, &taxinvoice.Error{Op: "issue", Kind: taxinvoice.Unknown, Message: "the answer never came"}
	}

	return out, nil
}

func (i *Issuer) Get(_ context.Context, key string) (taxinvoice.Issued, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if have, ok := i.issued[key]; ok {
		return have.out, nil
	}
	return taxinvoice.Issued{}, &taxinvoice.Error{Op: "get", Kind: taxinvoice.NotFound}
}
