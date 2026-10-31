package admincli

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/arg"
	"github.com/lesomnus/xli/flg"

	"github.com/lesomnus/levyan/internal/money"
	"github.com/lesomnus/levyan/server/admin"
)

func printFinding(self *xli.Command, v admin.Finding) {
	product := v.Product
	if product == "" {
		product = "-"
	}
	amount := ""
	if v.Currency != "" && v.Amount != 0 {
		amount = "\t" + money.String(v.Amount, v.Currency)
	}
	self.Printf("%s\t%s\t%s\t%s\t%s%s\t%s\n", v.Id, v.State, v.Severity, v.Kind, product, amount, v.Summary)
}

func printFindingShown(self *xli.Command, v admin.Finding) {
	printFinding(self, v)
	self.Printf("  subject %s, found %s, last changed %s\n", v.Subject, when(v.Found), when(v.Updated))
	for _, r := range v.Rows {
		self.Printf("  row\t%s\t%s\n", r.Entity, r.Ref)
	}
	if v.Accepted != nil {
		self.Printf("  accepted %s by %s: %s\n", when(*v.Accepted), v.AcceptedBy, v.Reason)
	}
	if v.Resolved != nil {
		self.Printf("  resolved %s: %s\n", when(*v.Resolved), v.Resolution)
	}
	self.Printf("  correction: %s\n", v.Correction)
	if v.Command != "" {
		self.Printf("  %s\n", v.Command)
	}
}

func newCmdReconcile() *xli.Command {
	idArg := &arg.String{Name: "ID", Brief: "the finding, as `reconcile ls` lists it"}
	attemptArg := &arg.String{Name: "ATTEMPT", Brief: "the attempt, by its identifier or its key (lv-…), as `invoice show` lists it"}

	return &xli.Command{
		Name:  "reconcile",
		Brief: "what reconciliation found drifted, and what an operator does about it: make a pass now, list, look, accept, give back, settle, follow a rule again",
		Commands: xli.Commands{
			{
				Name:  "run",
				Brief: "make every check now, as you: what is found, changed or resolved is written, and what is open is said",
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					var v admin.Pass
					if err := c.do(ctx, http.MethodPost, "/admin/reconcile", map[string]string{}, &v); err != nil {
						return err
					}
					for _, k := range v.Checks {
						switch {
						case k.Skipped:
							self.Printf("check\t%s\tpassed over: no merchant of record is configured\n", k.Name)
						case k.Err != "":
							self.Printf("check\t%s\tnot made: %s\n", k.Name, k.Err)
						default:
							self.Printf("check\t%s\t%d found\n", k.Name, k.Found)
						}
					}
					self.Printf("statements with a billing account: %d, on one invoice %d, on none %d, on more than one %d\n",
						v.Statements.WithAccount, v.Statements.OnOne, v.Statements.OnNone, v.Statements.OnMore)
					self.Printf("findings: %d open, %d accepted; %d new, %d changed, %d resolved\n", v.Open, v.Found-v.Open, v.New, v.Changed, v.Resolved)
					for _, f := range v.Findings {
						printFinding(self, f)
					}
					return next(ctx)
				}),
			},
			{
				Name:  "ls",
				Brief: "what reconciliation found that is so -- open or accepted -- the oldest first",
				Flags: flg.Flags{
					&flg.String{Name: "state", Brief: "in one state: open, accepted or resolved; or all"},
					&flg.String{Name: "kind", Brief: "of one kind: twice, charge_unmatched, attempt_stale, …"},
					&flg.String{Name: "product", Brief: "of one product, by its alias"},
				},
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					q := url.Values{}
					for _, k := range []string{"state", "kind", "product"} {
						if v := str(self, k); v != "" {
							q.Set(k, v)
						}
					}
					var vs []admin.Finding
					if err := c.do(ctx, http.MethodGet, "/admin/findings?"+q.Encode(), nil, &vs); err != nil {
						return err
					}
					for _, v := range vs {
						printFinding(self, v)
					}
					return next(ctx)
				}),
			},
			{
				Name:  "show",
				Brief: "one finding: what it says, the rows it is about, and what to do about it",
				Args:  arg.Args{idArg},
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					var v admin.Finding
					if err := c.do(ctx, http.MethodGet, "/admin/findings/"+url.PathEscape(argOf(self, "ID")), nil, &v); err != nil {
						return err
					}
					printFindingShown(self, v)
					return next(ctx)
				}),
			},
			{
				Name:  "accept",
				Brief: "accept a finding as it is, with your reason: it stays accepted while it says what it says now",
				Flags: flg.Flags{
					&flg.String{Name: "reason", Brief: "why it is accepted as it is, in your words"},
					&flg.Switch{Name: "yes", Brief: "do it"},
				},
				Args: arg.Args{idArg},
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					if err := yes(self, "reconcile accept"); err != nil {
						return err
					}
					if str(self, "reason") == "" {
						return errors.New("reconcile accept: --reason, why it is accepted as it is")
					}
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					var v admin.Finding
					if err := c.do(ctx, http.MethodPost, "/admin/findings/"+url.PathEscape(argOf(self, "ID"))+"/accept", map[string]string{"reason": str(self, "reason")}, &v); err != nil {
						return err
					}
					printFindingShown(self, v)
					return next(ctx)
				}),
			},
			{
				Name:  "refund",
				Brief: "give back what a charge's key was charged past what the ledger accounts for, by the merchant of record: a refund of each extra payment, sent now",
				Flags: flg.Flags{&flg.Switch{Name: "yes", Brief: "do it"}},
				Args:  arg.Args{attemptArg},
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					if err := yes(self, "reconcile refund"); err != nil {
						return err
					}
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					var vs []admin.Attempt
					if err := c.do(ctx, http.MethodPost, "/admin/attempts/"+url.PathEscape(argOf(self, "ATTEMPT"))+"/refund", map[string]string{}, &vs); err != nil {
						return err
					}
					for _, a := range vs {
						printAttempt(self, a)
					}
					return next(ctx)
				}),
			},
			{
				Name:  "settle",
				Brief: "settle an attempt from what its channel answers: asked of by its key, or of the merchant's transaction you name",
				Flags: flg.Flags{
					&flg.String{Name: "transaction", Brief: "the merchant of record's transaction of it (txn_…), as its own pages show it"},
					&flg.Switch{Name: "yes", Brief: "do it"},
				},
				Args: arg.Args{attemptArg},
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					if err := yes(self, "reconcile settle"); err != nil {
						return err
					}
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					var a admin.Attempt
					if err := c.do(ctx, http.MethodPost, "/admin/attempts/"+url.PathEscape(argOf(self, "ATTEMPT"))+"/settle", map[string]string{"transaction": str(self, "transaction")}, &a); err != nil {
						return err
					}
					printAttempt(self, a)
					return next(ctx)
				}),
			},
			{
				Name:  "rule",
				Brief: "follow a product's rules again now: what they want given, what they no longer want -- and what a rule there is no longer gave -- ended",
				Flags: flg.Flags{
					&flg.String{Name: "rule", Brief: "one rule, by its name: payers.grace; every one by default"},
					&flg.Switch{Name: "yes", Brief: "do it"},
				},
				Args: arg.Args{productArg},
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					if err := yes(self, "reconcile rule"); err != nil {
						return err
					}
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					var vs []admin.Grant
					if err := c.do(ctx, http.MethodPost, "/admin/products/"+url.PathEscape(argOf(self, "PRODUCT"))+"/rules", map[string]string{"rule": str(self, "rule")}, &vs); err != nil {
						return err
					}
					for _, v := range vs {
						printGrant(self, v)
					}
					return next(ctx)
				}),
			},
		},
		Handler: xli.RequireSubcommand(),
	}
}

func printAttempt(self *xli.Command, a admin.Attempt) {
	extra := []string{}
	if a.Failure != "" {
		extra = append(extra, a.Failure)
	}
	if a.ChannelRef != "" {
		extra = append(extra, a.ChannelRef)
	}
	self.Printf("attempt\t%s\t%s\t%s\t%s\t%d sent\t%s\n", a.Key, a.Kind, money.String(a.Amount, a.Currency), a.State, a.Sends, strings.Join(extra, "; "))
}
