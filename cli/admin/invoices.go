package admincli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/arg"
	"github.com/lesomnus/xli/flg"

	"github.com/lesomnus/levyan/internal/money"
	"github.com/lesomnus/levyan/server/admin"
)

func printInvoice(self *xli.Command, v admin.Invoice) {
	number := ""
	if v.Number != 0 {
		number = fmt.Sprintf("\tNo. %d", v.Number)
	}
	if v.ChannelNumber != "" {
		number = "\t" + v.ChannelNumber
	}
	state := v.State
	if v.Carried > 0 && v.State == "draft" {
		state = fmt.Sprintf("draft, waited %d time(s)", v.Carried)
	}
	self.Printf("%s\t%s\t%s\t%s\t%s\t%s %s\t%s, %s left%s\n",
		v.Id, v.Product, v.BillingAccount, when(v.Start), when(v.End), v.Channel, v.Currency,
		state, money.String(v.Remaining, v.Currency), number)
}

func printAmounts(self *xli.Command, v admin.Invoice) {
	tax := ""
	if v.TaxRate != "" {
		tax = " at " + v.TaxRate
	}
	self.Printf("  subtotal %s, tax %s%s, total %s; credit paid %s, due %s, paid %s, taken off %s, left %s\n",
		money.String(v.Subtotal, v.Currency), money.String(v.Tax, v.Currency), tax, money.String(v.Total, v.Currency),
		money.String(v.Credited, v.Currency), money.String(v.Due, v.Currency), money.String(v.Paid, v.Currency),
		money.String(v.Reduced, v.Currency), money.String(v.Remaining, v.Currency))
	if v.Finalized != nil {
		extra := ""
		if v.DueDate != nil {
			extra = ", due " + when(*v.DueDate)
		}
		if v.Issued != nil {
			extra += ", issued " + when(*v.Issued) + " as " + v.Document
		}
		self.Printf("  finalized %s%s\n", when(*v.Finalized), extra)
	}
	if v.Settled != nil {
		self.Printf("  settled %s\t%s\n", when(*v.Settled), v.Reason)
	}
}

func newCmdInvoice() *xli.Command {
	idArg := &arg.String{Name: "ID", Brief: "the invoice, as `invoice ls` lists it"}

	return &xli.Command{
		Name:  "invoice",
		Brief: "what billing accounts are asked to pay: list them, look at one, void one, credit one, record a transfer",
		Commands: xli.Commands{
			{
				Name:  "ls",
				Brief: "a product's invoices, the latest first",
				Flags: flg.Flags{
					&flg.String{Name: "state", Brief: "in one state: draft, open, paid, uncollectible, void, written_off or shadow"},
					&flg.String{Name: "billing-account", Brief: "of one billing account, by its identifier"},
				},
				Args: arg.Args{productArg},
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					q := url.Values{}
					if v := str(self, "state"); v != "" {
						q.Set("state", v)
					}
					if v := str(self, "billing-account"); v != "" {
						q.Set("billing_account", v)
					}
					var vs []admin.Invoice
					if err := c.do(ctx, http.MethodGet, "/admin/products/"+url.PathEscape(argOf(self, "PRODUCT"))+"/invoices?"+q.Encode(), nil, &vs); err != nil {
						return err
					}
					for _, v := range vs {
						printInvoice(self, v)
					}
					return next(ctx)
				}),
			},
			{
				Name:  "show",
				Brief: "one invoice: its amounts, its lines, its credit notes, its attempts, and its billing account's credit",
				Args:  arg.Args{idArg},
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					var v admin.Invoice
					if err := c.do(ctx, http.MethodGet, "/admin/invoices/"+url.PathEscape(argOf(self, "ID")), nil, &v); err != nil {
						return err
					}
					printShown(self, v)
					return next(ctx)
				}),
			},
			{
				Name:  "void",
				Brief: "void an open invoice nothing was paid of: what is left of it is taken back, and what credit paid of it is credit again",
				Flags: flg.Flags{
					&flg.String{Name: "reason", Brief: "why, in your words"},
					&flg.Switch{Name: "yes", Brief: "do it"},
				},
				Args: arg.Args{idArg},
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					if err := yes(self, "invoice void"); err != nil {
						return err
					}
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					var v admin.Invoice
					if err := c.do(ctx, http.MethodPost, "/admin/invoices/"+url.PathEscape(argOf(self, "ID"))+"/void", map[string]string{"reason": str(self, "reason")}, &v); err != nil {
						return err
					}
					printShown(self, v)
					return next(ctx)
				}),
			},
			{
				Name:  "credit",
				Brief: "write a credit note against a finalized invoice: what it takes back comes off what is due, and the rest is given back or credited",
				Flags: flg.Flags{
					&flg.Int64{Name: "amount", Brief: "how much of the invoice's subtotal, in the currency's minor unit: won, cents; its tax is worked out"},
					&flg.String{Name: "reason", Brief: "correction or refund; correction by default"},
					&flg.Switch{Name: "refund", Brief: "give back through the channel what is past what is due, as far as was paid, rather than credit it; a refund always does"},
					&flg.String{Name: "words", Brief: "why, in your words"},
					&flg.String{Name: "reference", Brief: "on a channel that moves no money itself: the transfer you made back, as the bank says it"},
					&flg.String{Name: "date", Brief: "and the day you made it, as 2006-01-02; today by default"},
					&flg.Switch{Name: "yes", Brief: "do it"},
				},
				Args: arg.Args{idArg},
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					if err := yes(self, "invoice credit"); err != nil {
						return err
					}
					amount, ok := flg.Find[int64](self, "amount")
					if !ok || amount <= 0 {
						return errors.New("invoice credit: --amount, more than nothing")
					}
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					refund, _ := flg.Find[bool](self, "refund")
					body := map[string]any{
						"amount": amount, "reason": str(self, "reason"), "refund": refund,
						"words": str(self, "words"), "reference": str(self, "reference"),
					}
					if v := str(self, "date"); v != "" {
						t, err := instant(v)
						if err != nil {
							return fmt.Errorf("--date: %w", err)
						}
						body["date"] = t
					}
					var v admin.Invoice
					if err := c.do(ctx, http.MethodPost, "/admin/invoices/"+url.PathEscape(argOf(self, "ID"))+"/credit", body, &v); err != nil {
						return err
					}
					printShown(self, v)
					return next(ctx)
				}),
			},
			{
				Name:  "paid",
				Brief: "record a transfer that came for an invoice, on a channel that moves no money itself",
				Flags: flg.Flags{
					&flg.Int64{Name: "amount", Brief: "what came, in the currency's minor unit"},
					&flg.String{Name: "date", Brief: "the day it came, as 2006-01-02; today by default"},
					&flg.String{Name: "reference", Brief: "what the bank says identifies it"},
					&flg.Switch{Name: "yes", Brief: "do it"},
				},
				Args: arg.Args{idArg},
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					if err := yes(self, "invoice paid"); err != nil {
						return err
					}
					amount, ok := flg.Find[int64](self, "amount")
					if !ok || amount <= 0 {
						return errors.New("invoice paid: --amount, more than nothing")
					}
					if str(self, "reference") == "" {
						return errors.New("invoice paid: --reference, as the bank says the transfer")
					}
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					body := map[string]any{"amount": amount, "reference": str(self, "reference")}
					if v := str(self, "date"); v != "" {
						t, err := instant(v)
						if err != nil {
							return fmt.Errorf("--date: %w", err)
						}
						body["date"] = t
					}
					var v admin.Invoice
					if err := c.do(ctx, http.MethodPost, "/admin/invoices/"+url.PathEscape(argOf(self, "ID"))+"/paid", body, &v); err != nil {
						return err
					}
					printShown(self, v)
					return next(ctx)
				}),
			},
		},
		Handler: xli.RequireSubcommand(),
	}
}

func printShown(self *xli.Command, v admin.Invoice) {
	printInvoice(self, v)
	printAmounts(self, v)
	for _, l := range v.Lines {
		label := l.Label
		if label == "" {
			label = l.Workspace
		}
		self.Printf("  line\t%s\t%s\t%s\t%s\t%s\n", l.Statement, label, when(l.Start), when(l.End), money.String(l.Total, v.Currency))
	}
	for _, cn := range v.CreditNotes {
		number := ""
		if cn.Number != 0 {
			number = fmt.Sprintf("\tNo. %d", cn.Number)
		}
		self.Printf("  credit note\t%s\t%s\t%s (tax %s): off due %s, given back %s, credited %s%s\t%s\n",
			cn.Id, cn.Reason, money.String(cn.Total, v.Currency), money.String(cn.Tax, v.Currency),
			money.String(cn.Reduced, v.Currency), money.String(cn.Refunded, v.Currency), money.String(cn.Credited, v.Currency),
			number, cn.Words)
	}
	for _, a := range v.Attempts {
		extra := []string{}
		if a.Failure != "" {
			extra = append(extra, a.Failure)
		}
		if a.Recorded {
			extra = append(extra, "recorded, "+a.Reference)
		}
		if a.Twice != nil {
			extra = append(extra, "the channel holds more of its key than this, since "+a.Twice.UTC().Format(time.RFC3339)+": reconciliation's")
		}
		self.Printf("  attempt\t%s\t%s\t%s\t%s\t%d sent\t%s\n", a.Key, a.Kind, money.String(a.Amount, a.Currency), a.State, a.Sends, strings.Join(extra, "; "))
	}
	if v.Balance != nil {
		self.Printf("  credit of the billing account in %s: %s\n", v.Product, money.String(*v.Balance, v.Currency))
	}
	if d := v.Dunning; d != nil {
		next := "none"
		if d.NextRetry != nil {
			next = d.NextRetry.UTC().Format(time.RFC3339)
		}
		self.Printf("  dunning\t%s since %s\tnext retry %s\tgrace ends %s\trestricted from %s\tread-only from %s\n",
			d.Kind, d.Failed.UTC().Format(time.RFC3339), next, d.GraceEnds.UTC().Format(time.RFC3339), d.RestrictedFrom.UTC().Format(time.RFC3339), d.ReadonlyFrom.UTC().Format(time.RFC3339))
	}
}
