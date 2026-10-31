package payers

import (
	"context"
	"time"
	"uuid"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	"github.com/lesomnus/levyan/internal/ent"
	entbillingaccount "github.com/lesomnus/levyan/internal/ent/billingaccount"
	entlink "github.com/lesomnus/levyan/internal/ent/link"
	entoffer "github.com/lesomnus/levyan/internal/ent/offer"
	"github.com/lesomnus/levyan/server/ledger"
)

// What an account calls as itself, relayed by a product (`server/api`): each
// is about the account's own billing accounts, and of the product that relays
// the call, the offers made to it and the workspaces it pays for -- read with
// ent, of that product and that account and no other, and written through the
// stack as the deployment within the product's tenant.

// First is the first billing account `account` made that it still holds, and
// the zero identifier when it has made none.
func First(ctx context.Context, db *ent.Client, account pdid.Id) (uuid.UUID, error) {
	v, err := db.BillingAccount.Query().
		Where(entbillingaccount.Owner(account.Uuid()), entbillingaccount.DateForgottenIsNil()).
		Order(ent.Asc(entbillingaccount.FieldDateCreated), ent.Asc(entbillingaccount.FieldId)).
		First(ctx)
	if ent.IsNotFound(err) {
		return uuid.UUID{}, nil
	}
	if err != nil {
		return uuid.UUID{}, err
	}

	return v.Id, nil
}

// own is billing account `id` when `account` holds it, and NotFound when it
// does not: somebody else's is no billing account of theirs.
func own(ctx context.Context, db *ent.Client, account, id pdid.Id) (*ent.BillingAccount, error) {
	v, err := db.BillingAccount.Query().
		Where(entbillingaccount.Id(id.Uuid()), entbillingaccount.Owner(account.Uuid()), entbillingaccount.DateForgottenIsNil()).
		Only(ctx)
	if ent.IsNotFound(err) {
		return nil, status.Errorf(codes.NotFound, "no billing account of the account's is %s", id)
	}

	return v, err
}

// Offers is what is offered to `account` of `product`'s workspaces and not
// answered, with each workspace.
func (p Payers) Offers(ctx context.Context, product, account pdid.Id) ([]*ent.Offer, error) {
	return p.db().Offer.Query().
		Where(
			entoffer.TenantId(product.Uuid()),
			entoffer.Account(account.Uuid()),
			entoffer.Answer(int32(levyan.OfferAnswer_OFFER_ANSWER_UNSPECIFIED)),
		).
		WithWorkspace().
		Order(ent.Asc(entoffer.FieldDateCreated), ent.Asc(entoffer.FieldId)).
		All(ctx)
}

// Links is what `account` pays for of `product`'s workspaces, or will from
// the end of the period, with each workspace.
func (p Payers) Links(ctx context.Context, product, account pdid.Id) ([]*ent.Link, error) {
	now := p.now()
	vs, err := p.db().Link.Query().
		Where(
			entlink.TenantId(product.Uuid()),
			entlink.Payer(account.Uuid()),
			entlink.Or(entlink.DateEndIsNil(), entlink.DateEndGT(now)),
		).
		WithWorkspace().
		Order(ent.Asc(entlink.FieldDateStart), ent.Asc(entlink.FieldId)).
		All(ctx)
	if err != nil {
		return nil, err
	}

	out := vs[:0]
	for _, v := range vs {
		if !empty(v) {
			out = append(out, v)
		}
	}

	return out, nil
}

// offer is offer `id` of `product` when it is made to `account` and not
// answered.
func (p Payers) offer(ctx context.Context, db *ent.Client, product, account, id pdid.Id) (*ent.Offer, error) {
	v, err := db.Offer.Query().
		Where(entoffer.Id(id.Uuid()), entoffer.TenantId(product.Uuid()), entoffer.Account(account.Uuid())).
		WithWorkspace().
		Only(ctx)
	if ent.IsNotFound(err) {
		return nil, status.Errorf(codes.NotFound, "no offer to the account is %s", id)
	}
	if err != nil {
		return nil, err
	}
	if v.Answer != int32(levyan.OfferAnswer_OFFER_ANSWER_UNSPECIFIED) {
		return nil, status.Errorf(codes.FailedPrecondition, "the offer was answered already: %s", levyan.OfferAnswer(v.Answer))
	}

	return v, nil
}

// Accept is `account` accepting offer `id`: it pays for the workspace, with
// billing account `billing` -- its own -- or none when zero, from the end of
// the period, and whoever would have paid from then pays no longer.
func (p Payers) Accept(ctx context.Context, product, account, id, billing pdid.Id) (*levyan.Link, error) {
	o, err := p.offer(ctx, p.db(), product, account, id)
	if err != nil {
		return nil, err
	}
	if !billing.IsZero() {
		if _, err := own(ctx, p.db(), account, billing); err != nil {
			return nil, err
		}
	}

	var out *levyan.Link
	err = p.held(ctx, o.WorkspaceId, func(s levyan.Server, db *ent.Client) error {
		o, err := p.offer(ctx, db, product, account, id)
		if err != nil {
			return err
		}
		now := p.now()
		b, err := Boundary(ctx, db, product.Uuid(), now)
		if err != nil {
			return err
		}

		// Whoever would pay from the end of the period pays until then.
		ls, err := links(ctx, db, product.Uuid(), o.WorkspaceId)
		if err != nil {
			return err
		}
		for _, l := range ls {
			if _, err := end(ctx, s, l, b, levyan.LinkEnded_LINK_ENDED_REPLACED); err != nil {
				return err
			}
		}

		out, err = add(ctx, s, product.Uuid(), o.WorkspaceId, account, billing.Uuid(), b, levyan.LinkBegan_LINK_BEGAN_ACCEPTED, o.Id)
		if err != nil {
			return err
		}

		return answer(ctx, s, o.Id, levyan.OfferAnswer_OFFER_ANSWER_ACCEPTED, now)
	})

	return out, err
}

// Decline is `account` declining offer `id`.
func (p Payers) Decline(ctx context.Context, product, account, id pdid.Id) error {
	o, err := p.offer(ctx, p.db(), product, account, id)
	if err != nil {
		return err
	}

	return p.held(ctx, o.WorkspaceId, func(s levyan.Server, db *ent.Client) error {
		if _, err := p.offer(ctx, db, product, account, id); err != nil {
			return err
		}

		return answer(ctx, s, o.Id, levyan.OfferAnswer_OFFER_ANSWER_DECLINED, p.now())
	})
}

// paid is the link `account` pays for workspace `key` of `product` by from
// the end of the period, and the end.
func (p Payers) paid(ctx context.Context, db *ent.Client, product, account, key pdid.Id) (uuid.UUID, *ent.Link, time.Time, error) {
	ws, ok, err := Workspace(ctx, db, product.Uuid(), key)
	if err != nil {
		return ws, nil, time.Time{}, err
	}
	if !ok {
		return ws, nil, time.Time{}, status.Errorf(codes.NotFound, "the account pays for no workspace %s", key)
	}
	b, err := Boundary(ctx, db, product.Uuid(), p.now())
	if err != nil {
		return ws, nil, time.Time{}, err
	}
	ls, err := links(ctx, db, product.Uuid(), ws)
	if err != nil {
		return ws, nil, time.Time{}, err
	}
	l := at(ls, b)
	if l == nil || l.Payer != account.Uuid() {
		return ws, nil, time.Time{}, status.Errorf(codes.FailedPrecondition, "the account does not pay for workspace %s from the end of the period", key)
	}

	return ws, l, b, nil
}

// Use is `account` paying for workspace `key` with its billing account
// `billing` from the end of the period: the link it pays by until then ends
// there, and another begins.
func (p Payers) Use(ctx context.Context, product, account, key, billing pdid.Id) (*levyan.Link, error) {
	if _, err := own(ctx, p.db(), account, billing); err != nil {
		return nil, err
	}
	ws, _, _, err := p.paid(ctx, p.db(), product, account, key)
	if err != nil {
		return nil, err
	}

	var out *levyan.Link
	err = p.held(ctx, ws, func(s levyan.Server, db *ent.Client) error {
		_, l, b, err := p.paid(ctx, db, product, account, key)
		if err != nil {
			return err
		}
		if l.BillingAccountId == billing.Uuid() {
			out = l.Proto()
			return nil
		}
		if _, err := end(ctx, s, l, b, levyan.LinkEnded_LINK_ENDED_REPLACED); err != nil {
			return err
		}

		out, err = add(ctx, s, product.Uuid(), ws, account, billing.Uuid(), b, levyan.LinkBegan_LINK_BEGAN_USED, uuid.UUID{})
		return err
	})

	return out, err
}

// Stop is `account` no longer paying for workspace `key`, as the period ends.
func (p Payers) Stop(ctx context.Context, product, account, key pdid.Id) (*levyan.Link, error) {
	ws, _, _, err := p.paid(ctx, p.db(), product, account, key)
	if err != nil {
		return nil, err
	}

	var out *levyan.Link
	err = p.held(ctx, ws, func(s levyan.Server, db *ent.Client) error {
		_, l, b, err := p.paid(ctx, db, product, account, key)
		if err != nil {
			return err
		}
		v, err := end(ctx, s, l, b, levyan.LinkEnded_LINK_ENDED_STOPPED)
		if v == nil && err == nil {
			v = l.Proto()
		}
		out = v
		return err
	})

	return out, err
}

// Create is a billing account of `account`'s made through the stack: in a
// country, which says its channel and its currency as the deployment sells
// there now ([Layer]).
func (p Payers) Create(ctx context.Context, account pdid.Id, v *levyan.BillingAccount) (*levyan.BillingAccount, error) {
	req := levyan.BillingAccountAddRequest_builder{
		Name:      v.GetName(),
		Owner:     account.Bytes(),
		Country:   v.GetCountry(),
		Recipient: v.GetRecipient(),
		Email:     v.GetEmail(),
		Address:   v.GetAddress(),
	}.Build()
	if v.HasBusiness() {
		req.SetBusiness(v.GetBusiness())
	}

	return p.S.BillingAccount().Add(ctx, req)
}

// Update says again, through the stack, who receives what billing account
// `id` of `account`'s is billed and the business it is billed to: what `v`
// says, and nothing it leaves out.
func (p Payers) Update(ctx context.Context, account, id pdid.Id, req *levyan.BillingAccountPatchRequest) (*levyan.BillingAccount, error) {
	if _, err := own(ctx, p.db(), account, id); err != nil {
		return nil, err
	}

	req = proto.CloneOf(req)
	req.SetRef(levyan.BillingAccountRef_builder{Id: id.Bytes()}.Build())
	req.SetDateUpdatedForce(true)

	return p.S.BillingAccount().Patch(ctx, req)
}

func tsOf(t time.Time) *timestamppb.Timestamp { return timestamppb.New(ledger.Moment(t)) }

func boolOf(v bool) *bool { return &v }
