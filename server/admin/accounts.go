package admin

import (
	"net/http"
	"strings"
	"time"

	"github.com/lesomnus/payday/pdid"

	levyan "github.com/lesomnus/levyan"
)

// BillingAccount is a billing account as an operator opens one.
type BillingAccount struct {
	Id       string    `json:"id"`
	Owner    string    `json:"owner"`
	Name     string    `json:"name,omitempty"`
	Country  string    `json:"country"`
	Channel  string    `json:"channel"`
	Currency string    `json:"currency"`
	Created  time.Time `json:"created"`
}

// Business is the business a billing account bills, for its tax invoices.
type Business struct {
	Registration   string `json:"registration"`
	Name           string `json:"name"`
	Representative string `json:"representative,omitempty"`
	Address        string `json:"address,omitempty"`
	Kind           string `json:"kind,omitempty"`
	Item           string `json:"item,omitempty"`
	Email          string `json:"email"`
}

// openBillingAccount opens a billing account paid by transfer for an account
// of roster's, after a contract (#9): an operator's alone, on the trail as
// theirs. Every other billing account is its account's own to make, on what
// the deployment sells its country through.
func (a *Routes) openBillingAccount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Owner     string   `json:"owner"`
		Channel   string   `json:"channel"`
		Name      string   `json:"name"`
		Recipient string   `json:"recipient"`
		Email     string   `json:"email"`
		Address   string   `json:"address"`
		Business  Business `json:"business"`
	}
	if !read(w, r, &in) {
		return
	}
	owner, err := pdid.Parse(strings.TrimSpace(in.Owner))
	if err != nil || owner.IsZero() {
		http.Error(w, "owner: the account at roster the billing account is of, by its identifier", http.StatusBadRequest)
		return
	}
	if v, ok := value(in.Channel, "CHANNEL_", levyan.Channel_value); !ok || levyan.Channel(v) != levyan.Channel_CHANNEL_BANK_KR {
		http.Error(w, "channel: bank_kr; a billing account on any other channel is its account's own to make", http.StatusBadRequest)
		return
	}

	v, err := a.S.Ungated.BillingAccount().Add(r.Context(), levyan.BillingAccountAddRequest_builder{
		Owner:     owner.Bytes(),
		Name:      strings.TrimSpace(in.Name),
		Country:   "KR",
		Channel:   levyan.Channel_CHANNEL_BANK_KR.String(),
		Recipient: strings.TrimSpace(in.Recipient),
		Email:     strings.TrimSpace(in.Email),
		Address:   strings.TrimSpace(in.Address),
		Business: levyan.BillingBusiness_builder{
			Registration:   strings.TrimSpace(in.Business.Registration),
			Name:           strings.TrimSpace(in.Business.Name),
			Representative: strings.TrimSpace(in.Business.Representative),
			Address:        strings.TrimSpace(in.Business.Address),
			Kind:           strings.TrimSpace(in.Business.Kind),
			Item:           strings.TrimSpace(in.Business.Item),
			Email:          strings.TrimSpace(in.Business.Email),
		}.Build(),
	}.Build())
	if err != nil {
		fail(w, r, err)
		return
	}

	answer(w, http.StatusCreated, BillingAccount{
		Id: text(v.GetId()), Owner: owner.String(), Name: v.GetName(), Country: v.GetCountry(),
		Channel: named(v.GetChannel(), "CHANNEL_"), Currency: v.GetCurrency(), Created: v.GetDateCreated().AsTime(),
	})
}
