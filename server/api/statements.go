package api

import (
	"context"
	"uuid"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
	levyanv1 "github.com/lesomnus/levyan/api/levyan/v1"
	entbillingaccount "github.com/lesomnus/levyan/internal/ent/billingaccount"
	entperiod "github.com/lesomnus/levyan/internal/ent/period"
	"github.com/lesomnus/levyan/internal/ent/predicate"
	entstatement "github.com/lesomnus/levyan/internal/ent/statement"
	entworkspace "github.com/lesomnus/levyan/internal/ent/workspace"
	"github.com/lesomnus/levyan/server/pd"
)

// What an account reads of what its billing accounts paid for (#47): of the
// product that relays the call, the statements of its workspaces whose
// period one of the account's billing accounts paid for. That is the billing
// account the statement names -- the one of the link in force through its
// period, of which a period has one at most -- so a period before somebody
// else began paying for the workspace is the payer's then, and one after the
// account stopped is the payer's after. A statement is shown whatever became
// of the invoice it is a line of: it says what the period came to and who
// paid for it, and what was asked of whom is the invoice's to say.

func theirStatements(product pdid.Id) func(context.Context, pdid.Id) (predicate.Statement, error) {
	return func(_ context.Context, account pdid.Id) (predicate.Statement, error) {
		return entstatement.And(
			entstatement.TenantId(product.Uuid()),
			entstatement.HasBillingAccountWith(entbillingaccount.Owner(account.Uuid())),
		), nil
	}
}

func (h Payers) ListStatements(ctx context.Context, req *levyanv1.PayerServiceListStatementsRequest) (*levyanv1.PayerServiceListStatementsResponse, error) {
	ctx, _, product, err := as(ctx, levyan.PayerService_ListStatements_FullMethodName)
	if err != nil {
		return nil, err
	}
	ctx, err = pd.AccountReads(ctx, levyan.PayerService_ListStatements_FullMethodName, pd.Reads{
		Statement:      theirStatements(product),
		BillingAccount: ownBillingAccounts,
	})
	if err != nil {
		return nil, err
	}

	size := req.GetPageSize()
	switch {
	case size <= 0:
		size = 20
	case size > 100:
		size = 100
	}
	list := levyan.StatementListRequest_builder{Size: size, After: req.GetPageToken(), Reverse: true}.Build()
	if v := req.GetBillingAccount(); v != "" {
		id, err := pdid.Parse(v)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "billing_account: %q is not a billing account's identifier", v)
		}

		// One of the account's own: read through the wall, which answers
		// another's as it answers one that is not there.
		ref := levyan.BillingAccountRef_builder{Id: id.Bytes()}.Build()
		if _, err := h.S.BillingAccount().Get(ctx, levyan.BillingAccountGetRequest_builder{Ref: ref}.Build()); err != nil {
			if status.Code(err) == codes.NotFound {
				return nil, status.Errorf(codes.NotFound, "the account has no billing account %s", id)
			}
			return nil, err
		}
		list.SetFilters([]*levyan.StatementFilter{levyan.StatementFilter_builder{BillingAccount: ref}.Build()})
	}

	vs, err := h.S.Statement().List(ctx, list)
	if err != nil {
		return nil, err
	}
	out, err := h.statements(ctx, product, vs.GetItems())
	if err != nil {
		return nil, err
	}

	return levyanv1.PayerServiceListStatementsResponse_builder{Statements: out, NextPageToken: vs.GetNext()}.Build(), nil
}

func (h Payers) GetStatement(ctx context.Context, req *levyanv1.PayerServiceGetStatementRequest) (*levyanv1.PayerServiceGetStatementResponse, error) {
	ctx, _, product, err := as(ctx, levyan.PayerService_GetStatement_FullMethodName)
	if err != nil {
		return nil, err
	}
	if ctx, err = pd.AccountReads(ctx, levyan.PayerService_GetStatement_FullMethodName, pd.Reads{Statement: theirStatements(product)}); err != nil {
		return nil, err
	}
	id, err := pdid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "id: %q is not a statement's identifier", req.GetId())
	}

	v, err := h.S.Statement().Get(ctx, levyan.StatementGetRequest_builder{Ref: levyan.StatementRef_builder{Id: id.Bytes()}.Build()}.Build())
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, status.Errorf(codes.NotFound, "no statement is %s", id)
		}
		return nil, err
	}
	out, err := h.statements(ctx, product, []*levyan.Statement{v})
	if err != nil {
		return nil, err
	}

	return levyanv1.PayerServiceGetStatementResponse_builder{Statement: out[0]}.Build(), nil
}

// statements is what the wall answered of statements, as the API says them:
// the workspace each is of, as the product names it, and the zone of its
// period, read with ent of the product and of what the wall answered alone.
func (h Payers) statements(ctx context.Context, product pdid.Id, vs []*levyan.Statement) ([]*levyanv1.Statement, error) {
	out := make([]*levyanv1.Statement, 0, len(vs))
	if len(vs) == 0 {
		return out, nil
	}

	wids, pids := make([]uuid.UUID, len(vs)), make([]uuid.UUID, len(vs))
	for i, v := range vs {
		w, err := pdid.From(v.GetWorkspace().GetId())
		if err != nil {
			return nil, err
		}
		p, err := pdid.From(v.GetPeriod().GetId())
		if err != nil {
			return nil, err
		}
		wids[i], pids[i] = w.Uuid(), p.Uuid()
	}

	ws, err := h.Db.Workspace.Query().Where(entworkspace.TenantId(product.Uuid()), entworkspace.IdIn(wids...)).All(ctx)
	if err != nil {
		return nil, err
	}
	keys := map[uuid.UUID]pdid.Id{}
	for _, w := range ws {
		key, err := pdid.From(w.Key)
		if err != nil {
			return nil, err
		}
		keys[w.Id] = key
	}
	ps, err := h.Db.Period.Query().Where(entperiod.TenantId(product.Uuid()), entperiod.IdIn(pids...)).All(ctx)
	if err != nil {
		return nil, err
	}
	zones := map[uuid.UUID]string{}
	for _, p := range ps {
		zones[p.Id] = p.Zone
	}

	for i, v := range vs {
		out = append(out, statement(v, keys[wids[i]], zones[pids[i]]))
	}

	return out, nil
}
