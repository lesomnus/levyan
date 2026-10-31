package billing

import (
	"context"
	"slices"
	"time"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	"github.com/lesomnus/levyan/internal/pricing"
)

// Gap is a meter of a product with no price on a channel and currency a
// statement of it may be priced on, while it has one on another: what such a
// statement would come to nothing for, for want of a price (#9).
type Gap struct {
	Meter    string
	Channel  levyan.Channel
	Currency string

	// Priced is the lines the meter is priced on then.
	Priced []pricing.Line
}

// Lines is every channel and currency a statement of `product` may be priced
// on: the product's terms, and every billing account's -- whoever pays for a
// workspace, it is on their billing account's.
func Lines(ctx context.Context, db *ent.Client, product *ent.Tenant) ([][2]string, error) {
	seen := map[[2]string]bool{}
	if product.Channel != "" && product.Currency != "" {
		seen[[2]string{product.Channel, product.Currency}] = true
	}

	bs, err := db.BillingAccount.Query().All(ctx)
	if err != nil {
		return nil, err
	}
	for _, b := range bs {
		if b.Channel != "" && b.Currency != "" {
			seen[[2]string{b.Channel, b.Currency}] = true
		}
	}

	out := make([][2]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	slices.SortFunc(out, func(a, b [2]string) int {
		if c := compare(a[0], b[0]); c != 0 {
			return c
		}
		return compare(a[1], b[1])
	})

	return out, nil
}

// Gaps is what of `product`'s price book is missing on the channels and
// currencies in use ([Lines]) at `at`, for a customer since `since`: each
// meter in force on some line then and on none of these. A meter priced on no
// line is not sold, and is no gap.
func Gaps(ctx context.Context, db *ent.Client, product *ent.Tenant, since, at time.Time) ([]Gap, error) {
	b, err := Rater{Db: db}.Book(ctx, product.Id)
	if err != nil {
		return nil, err
	}
	lines, err := Lines(ctx, db, product)
	if err != nil {
		return nil, err
	}

	out := []Gap{}
	for _, m := range b.Meters {
		id := pdid.Id(m.Id)
		priced := []pricing.Line{}
		seen := map[pricing.Line]bool{}
		for _, v := range b.Versions {
			l := v.Line()
			if l.Meter != id || seen[l] {
				continue
			}
			seen[l] = true
			if _, ok := b.Versions.InForce(l, since, at); ok {
				priced = append(priced, l)
			}
		}
		if len(priced) == 0 {
			continue
		}

		for _, k := range lines {
			c, err := Channel(k[0])
			if err != nil {
				continue
			}
			l := pricing.Line{Meter: id, Channel: c, Currency: k[1]}
			if _, ok := b.Versions.InForce(l, since, at); ok {
				continue
			}
			out = append(out, Gap{Meter: m.Key, Channel: c, Currency: k[1], Priced: priced})
		}
	}

	return out, nil
}

func compare(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
