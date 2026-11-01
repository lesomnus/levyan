package admincli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/arg"
	"github.com/lesomnus/xli/flg"

	"github.com/lesomnus/levyan/internal/money"
	"github.com/lesomnus/levyan/server/admin"
)

// What an operator gives a product's workspaces beyond their prices (#43):
// `allowance` -- more of a meter each period for nothing, on top of what its
// prices include -- `pool`, a bundle of a meter what is billable is taken
// out of first, and `credit`, money taken off what is billed, with an
// expiry. Each write says why (`--reason`), and the trail says who.

func printAllowance(self *xli.Command, v admin.Allowance) {
	until := "until ended"
	if v.Until != nil {
		until = "until " + when(*v.Until)
	}
	extra := ""
	if v.EndedBy != "" {
		extra = "\tended by " + v.EndedBy + ": " + v.EndReason
	}
	self.Printf("%s\t%s\t%s\t+%d\tfrom %s %s\tby %s: %s%s\n", v.Id, v.Workspace, v.Meter, v.Quantity, when(v.From), until, v.GrantedBy, v.Reason, extra)
}

func newCmdAllowance() *xli.Command {
	return &xli.Command{
		Name:  "allowance",
		Brief: "more of a meter a product's workspaces are given each period, on top of what their prices include: give, end, list",
		Commands: xli.Commands{
			{
				Name:  "add",
				Brief: "give a workspace more of a meter each period for nothing, from now -- or the start of the period now open -- until --until, or until it is ended",
				Flags: flg.Flags{
					&flg.String{Name: "workspace", Brief: "the workspace, by the identifier of its payday tenant"},
					&flg.Int64{Name: "quantity", Brief: "how much more each period includes, in the meter's unit"},
					&flg.String{Name: "from", Brief: "when it begins, as 2006-01-02 or RFC 3339, never in a period whose close has begun; now by default"},
					&flg.String{Name: "until", Brief: "when it ends; by default when it is ended"},
					&flg.String{Name: "reason", Brief: "why it is given, in your words"},
					&flg.Switch{Name: "yes", Brief: "do it"},
				},
				Args: arg.Args{productArg, &arg.String{Name: "METER", Brief: "the meter it gives more of, by its key: loft.assets.active"}},
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					if err := yes(self, "allowance add"); err != nil {
						return err
					}
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					quantity, ok := flg.Find[int64](self, "quantity")
					if !ok {
						return errors.New("allowance add: --quantity, how much more each period includes")
					}
					body := map[string]any{
						"workspace": str(self, "workspace"), "meter": argOf(self, "METER"),
						"quantity": quantity, "reason": str(self, "reason"),
					}
					for _, k := range []string{"from", "until"} {
						if v := str(self, k); v != "" {
							t, err := instant(v)
							if err != nil {
								return fmt.Errorf("--%s: %w", k, err)
							}
							body[k] = t
						}
					}

					var v admin.Allowance
					if err := c.do(ctx, http.MethodPost, "/admin/products/"+url.PathEscape(argOf(self, "PRODUCT"))+"/allowances", body, &v); err != nil {
						return err
					}
					printAllowance(self, v)
					return next(ctx)
				}),
			},
			{
				Name:  "end",
				Brief: "end an allowance now: in force no longer, and what it gave of the time before stays given",
				Flags: flg.Flags{
					&flg.String{Name: "reason", Brief: "why it is ended, in your words"},
					&flg.Switch{Name: "yes", Brief: "do it"},
				},
				Args: arg.Args{&arg.String{Name: "ID", Brief: "the allowance, as `allowance ls` lists it"}},
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					if err := yes(self, "allowance end"); err != nil {
						return err
					}
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					var v admin.Allowance
					if err := c.do(ctx, http.MethodPost, "/admin/allowances/"+url.PathEscape(argOf(self, "ID"))+"/end", map[string]string{"reason": str(self, "reason")}, &v); err != nil {
						return err
					}
					printAllowance(self, v)
					return next(ctx)
				}),
			},
			{
				Name:  "ls",
				Brief: "a product's allowances, the latest given first: in force, and to come",
				Flags: flg.Flags{
					&flg.String{Name: "workspace", Brief: "of one workspace, by the identifier of its payday tenant"},
					&flg.Switch{Name: "all", Brief: "and those ended"},
				},
				Args: arg.Args{productArg},
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					q := url.Values{}
					if v := str(self, "workspace"); v != "" {
						q.Set("workspace", v)
					}
					if v, _ := flg.Find[bool](self, "all"); v {
						q.Set("all", "true")
					}
					var vs []admin.Allowance
					if err := c.do(ctx, http.MethodGet, "/admin/products/"+url.PathEscape(argOf(self, "PRODUCT"))+"/allowances?"+q.Encode(), nil, &vs); err != nil {
						return err
					}
					for _, v := range vs {
						printAllowance(self, v)
					}
					return next(ctx)
				}),
			},
		},
		Handler: xli.RequireSubcommand(),
	}
}

func printCredit(self *xli.Command, v admin.Credit) {
	to := "workspace " + v.Workspace
	if v.BillingAccount != "" {
		to = "billing account " + v.BillingAccount
	}
	self.Printf("%s\t%s\t%s of %s left\texpires %s\tby %s: %s\n",
		v.Id, to, money.String(v.Left, v.Currency), money.String(v.Granted, v.Currency), when(v.Expires), v.GrantedBy, v.Reason)
}

func newCmdCredit() *xli.Command {
	return &xli.Command{
		Name:  "credit",
		Brief: "money a product's workspaces or billing accounts are granted, taken off what they are billed before anything is collected: grant, expire, list",
		Commands: xli.Commands{
			{
				Name:  "grant",
				Brief: "grant credit to a workspace, whose lines alone it pays whoever pays for it, or to a billing account, the whole of it in the product",
				Flags: flg.Flags{
					&flg.String{Name: "workspace", Brief: "the workspace, by the identifier of its payday tenant"},
					&flg.String{Name: "billing-account", Brief: "or the billing account, by its identifier"},
					&flg.Int64{Name: "amount", Brief: "how much, in the currency's minor unit: 100000 for 10만 원"},
					&flg.String{Name: "currency", Brief: "what in: a billing account's own; a workspace's, what it is billed in now by default"},
					&flg.String{Name: "expires", Brief: "when it expires, as 2006-01-02 or RFC 3339; a year from now by default"},
					&flg.String{Name: "reason", Brief: "why it is granted, in your words"},
					&flg.Switch{Name: "yes", Brief: "do it"},
				},
				Args: arg.Args{productArg},
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					if err := yes(self, "credit grant"); err != nil {
						return err
					}
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					amount, ok := flg.Find[int64](self, "amount")
					if !ok {
						return errors.New("credit grant: --amount, in the currency's minor unit")
					}
					body := map[string]any{
						"workspace": str(self, "workspace"), "billing_account": str(self, "billing-account"),
						"amount": amount, "currency": str(self, "currency"), "reason": str(self, "reason"),
					}
					if v := str(self, "expires"); v != "" {
						t, err := instant(v)
						if err != nil {
							return fmt.Errorf("--expires: %w", err)
						}
						body["expires"] = t
					}

					var v admin.Credit
					if err := c.do(ctx, http.MethodPost, "/admin/products/"+url.PathEscape(argOf(self, "PRODUCT"))+"/credits", body, &v); err != nil {
						return err
					}
					printCredit(self, v)
					return next(ctx)
				}),
			},
			{
				Name:  "expire",
				Brief: "end credit an operator granted now: what is left of it is taken off it, and nothing more of it is spent",
				Flags: flg.Flags{
					&flg.String{Name: "reason", Brief: "why it is ended, in your words"},
					&flg.Switch{Name: "yes", Brief: "do it"},
				},
				Args: arg.Args{&arg.String{Name: "ID", Brief: "the credit, as `credit ls` lists it"}},
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					if err := yes(self, "credit expire"); err != nil {
						return err
					}
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					var v admin.Credit
					if err := c.do(ctx, http.MethodPost, "/admin/credits/"+url.PathEscape(argOf(self, "ID"))+"/expire", map[string]string{"reason": str(self, "reason")}, &v); err != nil {
						return err
					}
					printCredit(self, v)
					return next(ctx)
				}),
			},
			{
				Name:  "ls",
				Brief: "what operators granted of a product, the soonest to expire first: what is left to spend",
				Flags: flg.Flags{
					&flg.String{Name: "workspace", Brief: "of one workspace, by the identifier of its payday tenant"},
					&flg.String{Name: "billing-account", Brief: "of one billing account, by its identifier"},
					&flg.Switch{Name: "all", Brief: "and what was spent or expired"},
				},
				Args: arg.Args{productArg},
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					q := url.Values{}
					if v := str(self, "workspace"); v != "" {
						q.Set("workspace", v)
					}
					if v := str(self, "billing-account"); v != "" {
						q.Set("billing_account", v)
					}
					if v, _ := flg.Find[bool](self, "all"); v {
						q.Set("all", "true")
					}
					var vs []admin.Credit
					if err := c.do(ctx, http.MethodGet, "/admin/products/"+url.PathEscape(argOf(self, "PRODUCT"))+"/credits?"+q.Encode(), nil, &vs); err != nil {
						return err
					}
					for _, v := range vs {
						printCredit(self, v)
					}
					return next(ctx)
				}),
			},
		},
		Handler: xli.RequireSubcommand(),
	}
}

func printPool(self *xli.Command, v admin.Pool) {
	extra := ""
	if v.EndedBy != "" {
		extra = "\tended by " + v.EndedBy + ": " + v.EndReason
	}
	self.Printf("%s\t%s\t%s\t%s of %d left\tfrom %s, expires %s\tby %s: %s%s\n",
		v.Id, v.Workspace, v.Meter, v.Left, v.Granted, when(v.From), when(v.Expires), v.GrantedBy, v.Reason, extra)
}

func newCmdPool() *xli.Command {
	return &xli.Command{
		Name:  "pool",
		Brief: "bundles of a meter a product's workspaces are given, which what they are billable for is taken out of before it is priced: grant, expire, list",
		Commands: xli.Commands{
			{
				Name:  "grant",
				Brief: "give a workspace a bundle of a meter, in its unit for a period -- asset-months of a time-weighted stock -- which every period it is in force for takes from, the soonest to expire first",
				Flags: flg.Flags{
					&flg.String{Name: "workspace", Brief: "the workspace, by the identifier of its payday tenant"},
					&flg.Int64{Name: "quantity", Brief: "how much it holds, in the meter's unit for a period"},
					&flg.String{Name: "from", Brief: "when it is in force from, as 2006-01-02 or RFC 3339, never in a period whose close has begun; now by default"},
					&flg.String{Name: "expires", Brief: "when it expires: no period that begins from then takes from it; a year from now by default"},
					&flg.String{Name: "reason", Brief: "why it is given, in your words"},
					&flg.Switch{Name: "yes", Brief: "do it"},
				},
				Args: arg.Args{productArg, &arg.String{Name: "METER", Brief: "the meter it is a bundle of, by its key: loft.assets.active"}},
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					if err := yes(self, "pool grant"); err != nil {
						return err
					}
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					quantity, ok := flg.Find[int64](self, "quantity")
					if !ok {
						return errors.New("pool grant: --quantity, how much it holds")
					}
					body := map[string]any{
						"workspace": str(self, "workspace"), "meter": argOf(self, "METER"),
						"quantity": quantity, "reason": str(self, "reason"),
					}
					for _, k := range []string{"from", "expires"} {
						if v := str(self, k); v != "" {
							t, err := instant(v)
							if err != nil {
								return fmt.Errorf("--%s: %w", k, err)
							}
							body[k] = t
						}
					}

					var v admin.Pool
					if err := c.do(ctx, http.MethodPost, "/admin/products/"+url.PathEscape(argOf(self, "PRODUCT"))+"/pools", body, &v); err != nil {
						return err
					}
					printPool(self, v)
					return next(ctx)
				}),
			},
			{
				Name:  "expire",
				Brief: "end a pool: from the start of the period now open no period takes from it, and what the periods before took stays taken",
				Flags: flg.Flags{
					&flg.String{Name: "reason", Brief: "why it is ended, in your words"},
					&flg.Switch{Name: "yes", Brief: "do it"},
				},
				Args: arg.Args{&arg.String{Name: "ID", Brief: "the pool, as `pool ls` lists it"}},
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					if err := yes(self, "pool expire"); err != nil {
						return err
					}
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					var v admin.Pool
					if err := c.do(ctx, http.MethodPost, "/admin/pools/"+url.PathEscape(argOf(self, "ID"))+"/expire", map[string]string{"reason": str(self, "reason")}, &v); err != nil {
						return err
					}
					printPool(self, v)
					return next(ctx)
				}),
			},
			{
				Name:  "ls",
				Brief: "a product's pools, the soonest to expire first, with what is left of each as the periods closed took of it",
				Flags: flg.Flags{
					&flg.String{Name: "workspace", Brief: "of one workspace, by the identifier of its payday tenant"},
					&flg.Switch{Name: "all", Brief: "and those expired"},
				},
				Args: arg.Args{productArg},
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					q := url.Values{}
					if v := str(self, "workspace"); v != "" {
						q.Set("workspace", v)
					}
					if v, _ := flg.Find[bool](self, "all"); v {
						q.Set("all", "true")
					}
					var vs []admin.Pool
					if err := c.do(ctx, http.MethodGet, "/admin/products/"+url.PathEscape(argOf(self, "PRODUCT"))+"/pools?"+q.Encode(), nil, &vs); err != nil {
						return err
					}
					for _, v := range vs {
						printPool(self, v)
					}
					return next(ctx)
				}),
			},
		},
		Handler: xli.RequireSubcommand(),
	}
}
