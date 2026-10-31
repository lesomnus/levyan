// Package taxinvoice is what levyan asks of a service that issues Korean
// electronic tax invoices (전자세금계산서) for it, on a channel where levyan
// is the seller and the invoice is the legal one: the bank transfer an
// institution pays by (#9), and a Korean gateway's later.
//
// It is the boundary, and nothing behind it. Which service issues them --
// one of the NTS's certified issuers (발행 대행), Popbill, Barobill and the
// like -- is the owner's decision and has not been made (levyan#1, the fourth
// open question). A service is a package implementing [Issuer] and
// registering its kind ([Register]); nothing that uses one names it, and the
// fake (package taxinvoicetest) is what tests use.
//
// # What it promises
//
//   - **Once by its key.** A tax invoice is issued under a key of the
//     seller's own (an issuer's 문서관리번호): issued again under the same
//     key, it is answered with the document it already is, and nothing is
//     issued twice. levyan's key is its invoice's or credit note's number,
//     which is written down and committed before the issuer is asked, so an
//     answer that never came is asked for again under it.
//   - **What the law asks of it** (부가가치세법 제32조): the supplier's
//     registration number and name, the buyer's registration number -- or
//     the 고유번호 of a buyer that is no business -- the supply value and the
//     tax apart, and the day it is written for. An issuer refuses one that
//     lacks any ([Invalid]).
//   - **An amendment is an invoice of its own** (부가가치세법 시행령
//     제70조): the amounts less than nothing, written for the day the cause
//     arose, and naming the one it amends and why ([Amends]).
//   - The request failing is not the invoice failing: an error's [Kind] says
//     what is known, as package pg's and package mor's do, and only
//     [Unknown] may have issued it.
package taxinvoice

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Issuer issues Korean electronic tax invoices.
type Issuer interface {
	// Name is the service's: "popbill".
	Name() string

	// Issue issues a tax invoice, and answers what it is at the NTS: its
	// approval number. One issued under its key already is answered as it
	// is, and nothing more is issued.
	Issue(ctx context.Context, v Invoice) (Issued, error)

	// Get is the tax invoice issued under `key`: [NotFound] when the issuer
	// has none, which is the issuer saying it was never issued.
	Get(ctx context.Context, key string) (Issued, error)
}

// Party is a business on a tax invoice: the supplier, or the buyer.
type Party struct {
	// Registration is its 사업자등록번호, or a 고유번호 for one that is no
	// business: ten digits, written with or without hyphens.
	Registration string

	// Name is its 상호, and Representative who represents it.
	Name           string
	Representative string

	Address string

	// Kind and Item are its 업태 and 종목.
	Kind string
	Item string

	// Email is where its tax invoices go.
	Email string
}

// Invoice is a tax invoice to issue.
type Invoice struct {
	// Key keeps it once at the issuer: the seller's own, at most 24 of
	// letters, digits, '-' and '_'.
	Key string

	// Written is the day it is written for (작성일자), in Korea.
	Written time.Time

	Supplier Party
	Buyer    Party

	// Supply and Tax are the supply value and the VAT, in won, apart: less
	// than nothing on an amendment that takes back.
	Supply int64
	Tax    int64

	// Items are what was supplied, each with its supply and its tax.
	Items []Item

	// Note is the 비고: on an amendment, the original's day.
	Note string

	// Amends is the tax invoice this one amends, and why; nil for one that
	// amends nothing.
	Amends *Amends
}

// Item is one thing a tax invoice is for.
type Item struct {
	Name   string
	Supply int64
	Tax    int64
}

// Amends is what an amendment amends.
type Amends struct {
	// Key and Approval are the original's: its key, and its NTS approval
	// number.
	Key      string
	Approval string

	// Written is the original's day.
	Written time.Time

	Cause Cause
}

// Cause is why a tax invoice is amended, as 부가가치세법 시행령 제70조 ①
// says them.
type Cause int

const (
	// CauseUnspecified amends nothing.
	CauseUnspecified Cause = iota

	// Cancelled is the contract cancelled (계약의 해제, 2호): what was
	// supplied is taken back in whole, on the day it was cancelled.
	Cancelled

	// Changed is an amount added to the supply value or taken from it
	// (공급가액에 추가되거나 차감되는 금액, 3호), on the day the cause arose.
	Changed
)

func (c Cause) String() string {
	switch c {
	case Cancelled:
		return "cancelled"
	case Changed:
		return "changed"
	default:
		return "none"
	}
}

// Issued is a tax invoice the issuer has: its key, the NTS's approval number
// (승인번호), and when it was issued.
type Issued struct {
	Key      string
	Approval string
	At       time.Time
}

// Check refuses a tax invoice that lacks what the law asks of one: the
// supplier's registration and name, the buyer's registration, a supply value
// and its tax, and a day; and a key an issuer would not keep it by.
func (v Invoice) Check() error {
	switch {
	case !key(v.Key):
		return &Error{Op: "issue", Kind: Invalid, Message: fmt.Sprintf("key: %q is not 1 to 24 of letters, digits, '-' and '_'", v.Key)}
	case !Registration(v.Supplier.Registration):
		return &Error{Op: "issue", Kind: Invalid, Message: "supplier: a registration number of ten digits"}
	case strings.TrimSpace(v.Supplier.Name) == "":
		return &Error{Op: "issue", Kind: Invalid, Message: "supplier: a name"}
	case !Registration(v.Buyer.Registration):
		return &Error{Op: "issue", Kind: Invalid, Message: "buyer: a registration number, or a 고유번호, of ten digits"}
	case v.Written.IsZero():
		return &Error{Op: "issue", Kind: Invalid, Message: "written: the day it is written for"}
	case v.Supply == 0:
		return &Error{Op: "issue", Kind: Invalid, Message: "supply: a supply value"}
	case v.Supply < 0 && v.Amends == nil:
		return &Error{Op: "issue", Kind: Invalid, Message: "supply: less than nothing only on an amendment that takes back"}
	case v.Amends != nil && v.Amends.Cause == CauseUnspecified:
		return &Error{Op: "issue", Kind: Invalid, Message: "amends: why"}
	}

	var supply, tax int64
	for _, it := range v.Items {
		supply += it.Supply
		tax += it.Tax
	}
	if len(v.Items) > 0 && (supply != v.Supply || tax != v.Tax) {
		return &Error{Op: "issue", Kind: Invalid, Message: "items: they add up to the invoice's supply value and tax"}
	}

	return nil
}

// Registration says whether `v` is a registration number's shape: ten digits,
// with or without hyphens (123-45-67890). The check digit is not asked: a
// 고유번호 is written in the same shape, and its number is the tax office's
// to give (implementation decision).
func Registration(v string) bool {
	d := strings.ReplaceAll(strings.TrimSpace(v), "-", "")
	if len(d) != 10 {
		return false
	}
	_, err := strconv.ParseUint(d, 10, 64)
	return err == nil
}

func key(v string) bool {
	if len(v) < 1 || len(v) > 24 {
		return false
	}
	for _, r := range v {
		if !(r == '-' || r == '_' || ('0' <= r && r <= '9') || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z')) {
			return false
		}
	}
	return true
}

// Kind is what a request to an issuer that failed is known to have done, as
// package pg's kinds are.
type Kind int

const (
	// Unknown may have issued it: ask ([Issuer.Get]) before issuing again.
	Unknown Kind = iota

	// Invalid is a tax invoice the issuer will not issue as it is.
	Invalid

	// Auth is the issuer refusing levyan: its key, its certificate, its
	// contract.
	Auth

	// RateLimited is not now.
	RateLimited

	// NotFound is the issuer having nothing under the key.
	NotFound
)

func (k Kind) String() string {
	switch k {
	case Unknown:
		return "unknown"
	case Invalid:
		return "invalid"
	case Auth:
		return "auth"
	case RateLimited:
		return "rate limited"
	case NotFound:
		return "not found"
	default:
		return "kind " + strconv.Itoa(int(k))
	}
}

// Error is a request to an issuer that failed.
type Error struct {
	Op   string
	Kind Kind

	// Code and Message are the issuer's.
	Code    string
	Message string

	Err error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("taxinvoice: ")
	b.WriteString(e.Op)
	b.WriteString(": ")
	b.WriteString(e.Kind.String())
	if e.Code != "" {
		b.WriteString(": ")
		b.WriteString(e.Code)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// KindOf is what is known of the request `err` says failed: [Unknown] for an
// error that is no [Error].
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return Unknown
}

// Opener makes an issuer of a kind.
type Opener func() (Issuer, error)

var (
	mu    sync.Mutex
	kinds = map[string]Opener{}
)

// Register makes issuers of `kind` openable ([Open]): what an issuer's
// package does as it is linked, as a database driver does.
func Register(kind string, open Opener) {
	mu.Lock()
	defer mu.Unlock()
	kinds[kind] = open
}

// Open is an issuer of `kind`. None is linked into levyan's binaries yet:
// which service issues its tax invoices is the owner's open question.
func Open(kind string) (Issuer, error) {
	mu.Lock()
	open, ok := kinds[kind]
	names := slices.Sorted(maps.Keys(kinds))
	mu.Unlock()
	if !ok {
		if len(names) == 0 {
			return nil, fmt.Errorf("taxinvoice: no issuer of kind %q is linked, and none is: which service issues levyan's tax invoices is the owner's to choose", kind)
		}
		return nil, fmt.Errorf("taxinvoice: no issuer of kind %q is linked (%s)", kind, strings.Join(names, ", "))
	}

	return open()
}
