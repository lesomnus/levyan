package admincli

import (
	"context"
	"net/http"

	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"

	"github.com/lesomnus/levyan/server/admin"
)

func newCmdBillingAccount() *xli.Command {
	return &xli.Command{
		Name:  "billing-account",
		Brief: "what an account pays with: open one paid by transfer, after a contract",
		Commands: xli.Commands{
			{
				Name:  "add",
				Brief: "open a billing account paid by bank transfer with a tax invoice, for an account of roster's: an operator's alone, after a contract",
				Flags: flg.Flags{
					&flg.String{Name: "channel", Brief: "bank_kr: the one an operator opens"},
					&flg.String{Name: "owner", Brief: "the account at roster it is of, by its identifier"},
					&flg.String{Name: "name", Brief: "what the account calls it: 본사"},
					&flg.String{Name: "recipient", Brief: "who receives what it is billed"},
					&flg.String{Name: "email", Brief: "where what it is billed goes"},
					&flg.String{Name: "address", Brief: "where it is billed"},
					&flg.String{Name: "registration", Brief: "the business's 사업자등록번호, or a 고유번호: ten digits"},
					&flg.String{Name: "business-name", Brief: "the business's 상호"},
					&flg.String{Name: "representative", Brief: "who represents it"},
					&flg.String{Name: "business-address", Brief: "where it does business"},
					&flg.String{Name: "kind", Brief: "its 업태"},
					&flg.String{Name: "item", Brief: "its 종목"},
					&flg.String{Name: "tax-email", Brief: "where its tax invoices go"},
					&flg.Switch{Name: "yes", Brief: "do it"},
				},
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					if err := yes(self, "billing-account add"); err != nil {
						return err
					}
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					body := map[string]any{
						"owner": str(self, "owner"), "channel": str(self, "channel"), "name": str(self, "name"),
						"recipient": str(self, "recipient"), "email": str(self, "email"), "address": str(self, "address"),
						"business": admin.Business{
							Registration: str(self, "registration"), Name: str(self, "business-name"),
							Representative: str(self, "representative"), Address: str(self, "business-address"),
							Kind: str(self, "kind"), Item: str(self, "item"), Email: str(self, "tax-email"),
						},
					}
					var v admin.BillingAccount
					if err := c.do(ctx, http.MethodPost, "/admin/billing-accounts", body, &v); err != nil {
						return err
					}
					self.Printf("%s\t%s\t%s\t%s %s\tsince %s\t%s\n", v.Id, v.Owner, v.Country, v.Channel, v.Currency, when(v.Created), v.Name)
					return next(ctx)
				}),
			},
		},
		Handler: xli.RequireSubcommand(),
	}
}
