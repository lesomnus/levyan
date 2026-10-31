// Package admincli is `levyan-admin`, the operator's tool. `serve` stands the
// operator's entry point up beside a deployment, on a listener of its own
// (`server/admin`); every other command asks it, from wherever the operator
// is, with their key -- the entry point's address in `--url` or
// LEVYAN_ADMIN_URL, the key in `--key` or LEVYAN_ADMIN_KEY.
//
// It is a binary of its own so that `levyan`, the one products reach, links
// none of it -- and it links none of that one's door either: no service keys
// are read here and no `levyan.v1` is served (cmd/levyan's test).
package admincli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lesomnus/otx/log"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/arg"
	"github.com/lesomnus/xli/cfg"
	"github.com/lesomnus/xli/flg"
	entschema "github.com/protobuf-orm/ent/dialect/sql/schema"
	"golang.org/x/sync/errgroup"

	"github.com/lesomnus/payday/pdcmd"
	"github.com/lesomnus/payday/spin"

	"github.com/lesomnus/levyan/cmd"
	entmigrate "github.com/lesomnus/levyan/internal/ent/migrate"
	"github.com/lesomnus/levyan/internal/money"
	"github.com/lesomnus/levyan/server/admin"
)

// Cmd is the root of `levyan-admin`.
func Cmd(c *cmd.Config) *xli.Command {
	l := cfg.New(cmd.Name, c)
	version := pdcmd.NewCmdVersion()
	product, key, meter, price := newCmdProduct(), newCmdKey(), newCmdMeter(), newCmdPrice()

	return &xli.Command{
		Name:  "levyan-admin",
		Brief: "the operator's entry point, and what an operator asks of it",

		Flags: flg.Flags{
			cfg.ConfigFlag(),
			&flg.String{Name: "url", Brief: "the entry point, as `levyan-admin serve` listens on it; LEVYAN_ADMIN_URL, or http://127.0.0.1:8081"},
			&flg.String{Name: "key", Brief: "your key, as ${env:NAME}, ${file:PATH} or itself; LEVYAN_ADMIN_KEY"},
		},

		Commands: []*xli.Command{
			version,
			cfg.NewCmdConfig(l),
			newCmdServe(c),
			product,
			key,
			meter,
			price,
		},

		// What asks the entry point reads none of a deployment's
		// configuration: an operator runs it from their own machine.
		Handler: xli.Chain(cfg.Load(l, version, product, key, meter, price), xli.RequireSubcommand()),
	}
}

func newCmdServe(c *cmd.Config) *xli.Command {
	return &xli.Command{
		Name:  "serve",
		Brief: "serve the operator's entry point beside this deployment, on admin.addr",
		Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
			ops, err := admin.Operators(c.Admin.Keys)
			if err != nil {
				return err
			}
			if len(ops) == 0 {
				return errors.New("admin.keys: an entry point nobody holds a key to is one nobody can use")
			}

			// Before the server, as `levyan serve` does: what it builds logs
			// into the providers it finds on ctx.
			ctx, done, err := cmd.Telemetry(ctx, c)
			if err != nil {
				return err
			}
			defer done()

			s, err := cmd.Build(ctx, *c)
			if err != nil {
				return err
			}
			defer s.Close()

			// The database is what this build says, or nothing is written to
			// it: migrating is `levyan migrate`'s, and this binary alters no
			// table.
			if err := entschema.Check(ctx, s.Db, s.Dialect, entmigrate.Tables); err != nil {
				return err
			}

			routes := &admin.Routes{S: s, Operators: ops}
			if err := routes.Sync(ctx); err != nil {
				return err
			}

			l, err := net.Listen("tcp", c.Admin.ListenAddr())
			if err != nil {
				return err
			}
			log.From(ctx).InfoContext(ctx, "admin", slog.String("addr", l.Addr().String()), slog.Int("operators", len(ops)))

			srv := &http.Server{
				Handler:           routes.Handler(),
				ReadHeaderTimeout: 10 * time.Second,
				IdleTimeout:       2 * time.Minute,
				BaseContext:       func(net.Listener) context.Context { return ctx },
			}

			// The one loop this process runs is its own: the access record's
			// writer (`payday.access`), for what it read. The deployment's
			// loops are `levyan serve`'s, under its lease.
			g, ctx := errgroup.WithContext(ctx)
			g.Go(func() error { return spin.Run(ctx, slices.Values([]any{s.Access})) })
			g.Go(func() error {
				go func() {
					<-ctx.Done()
					shut, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
					defer cancel()
					_ = srv.Shutdown(shut)
				}()
				if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
					return err
				}
				return nil
			})
			if err := g.Wait(); err != nil {
				return err
			}

			return next(ctx)
		}),
	}
}

// client is what a command asks the entry point with.
type client struct {
	base string
	key  string
}

func clientOf(self *xli.Command) (client, error) {
	base, _ := flg.Find[string](self, "url")
	if base = strings.TrimSpace(base); base == "" {
		base = os.Getenv("LEVYAN_ADMIN_URL")
	}
	if base == "" {
		base = "http://127.0.0.1:8081"
	}

	ref, _ := flg.Find[string](self, "key")
	if ref = strings.TrimSpace(ref); ref == "" {
		ref = os.Getenv("LEVYAN_ADMIN_KEY")
	}
	if ref == "" {
		return client{}, errors.New("--key or LEVYAN_ADMIN_KEY: the entry point answers an operator's key")
	}

	// As a secret of the configuration is read: `${env:NAME}`, `${file:PATH}`,
	// or the key itself.
	var key cfg.Secret
	if err := key.UnmarshalText([]byte(ref)); err != nil {
		return client{}, fmt.Errorf("--key: %w", err)
	}
	v, err := key.Value()
	if err != nil {
		return client{}, fmt.Errorf("--key: %w", err)
	}

	return client{base: strings.TrimSuffix(base, "/"), key: v}, nil
}

func (c client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("User-Agent", "levyan-admin")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", res.Status, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}

	return nil
}

// yes refuses a change nobody said yes to.
func yes(self *xli.Command, what string) error {
	if ok, _ := flg.Find[bool](self, "yes"); !ok {
		return fmt.Errorf("%s: say --yes", what)
	}

	return nil
}

func str(self *xli.Command, name string) string {
	v, _ := flg.Find[string](self, name)
	return strings.TrimSpace(v)
}

func argOf(self *xli.Command, name string) string {
	v, _ := arg.Get[string](self, name)
	return strings.TrimSpace(v)
}

// instant reads a moment as RFC 3339, or a date as the start of that day in
// UTC -- the day a command says it back as, whatever zone the operator's own
// machine is in.
func instant(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if t, err := time.Parse(time.DateOnly, v); err == nil {
		return t, nil
	}

	return time.Parse(time.RFC3339, v)
}

// when is a moment as the commands print one: in UTC.
func when(t time.Time) string { return t.UTC().Format(time.RFC3339) }

var productArg = &arg.String{Name: "PRODUCT", Brief: "by its alias: loft"}

func printProduct(self *xli.Command, v admin.Product) {
	self.Printf("%s\t%s\t%s\tsince %s\n", v.Alias, v.Name, v.Id, when(v.Created))
}

func printKey(self *xli.Command, v admin.Key) {
	state := "live"
	if v.Revoked != nil {
		state = "revoked " + when(*v.Revoked)
	} else if v.Expires != nil {
		state = "until " + when(*v.Expires)
	}
	self.Printf("  key\t%s\t%s\t%s\n", v.Id, v.Alias, state)
}

func printMeter(self *xli.Command, v admin.Meter) {
	extra := ""
	if v.UnboundedZero {
		extra = "\t0 is without bound"
	}
	self.Printf("  meter\t%s\t%s\t%s, %s\t%s%s\n", v.Key, v.Unit, v.Kind, v.Aggregation, v.Name, extra)
}

func printPrice(self *xli.Command, v admin.Price) {
	rate := ""
	if len(v.Bands) > 0 {
		parts := []string{}
		for _, b := range v.Bands {
			bound := "the rest"
			if b.UpTo != nil {
				bound = "≤" + strconv.FormatInt(*b.UpTo, 10)
			}
			parts = append(parts, bound+" "+money.String(b.Fee, v.Currency))
		}
		rate = "bands " + strings.Join(parts, ", ")
	} else {
		per := ""
		if v.Per > 1 {
			per = " per " + strconv.FormatInt(v.Per, 10)
		}
		rate = fmt.Sprintf("%d included, then %s%s", v.Included, money.String(v.UnitPrice, v.Currency), per)
	}
	self.Printf("%s\t%s\t%s %s\t%s\tnew from %s, existing from %s\tpublished %s\n",
		v.Id, v.Meter, v.Channel, v.Currency, rate, when(v.Effective), when(v.Existing), when(v.Published))
}

func newCmdProduct() *xli.Command {
	return &xli.Command{
		Name:  "product",
		Brief: "the products levyan bills for: put one up, find them",
		Commands: xli.Commands{
			{
				Name:  "ls",
				Brief: "every product",
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					var vs []admin.Product
					if err := c.do(ctx, http.MethodGet, "/admin/products", nil, &vs); err != nil {
						return err
					}
					for _, v := range vs {
						printProduct(self, v)
					}
					return next(ctx)
				}),
			},
			{
				Name:  "add",
				Brief: "put a product up",
				Flags: flg.Flags{
					&flg.String{Name: "name", Brief: "what it is called in a sentence: Loft"},
					&flg.String{Name: "desc", Brief: "more about it"},
					&flg.Switch{Name: "yes", Brief: "do it"},
				},
				Args: arg.Args{&arg.String{Name: "ALIAS", Brief: "what it is called in a path or a command: loft"}},
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					if err := yes(self, "product add"); err != nil {
						return err
					}
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					var v admin.Detail
					body := map[string]string{"alias": argOf(self, "ALIAS"), "name": str(self, "name"), "desc": str(self, "desc")}
					if err := c.do(ctx, http.MethodPost, "/admin/products", body, &v); err != nil {
						return err
					}
					printProduct(self, v.Product)
					return next(ctx)
				}),
			},
			{
				Name:  "show",
				Brief: "one product: its keys and its meters",
				Args:  arg.Args{productArg},
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					var v admin.Detail
					if err := c.do(ctx, http.MethodGet, "/admin/products/"+url.PathEscape(argOf(self, "PRODUCT")), nil, &v); err != nil {
						return err
					}
					printProduct(self, v.Product)
					for _, k := range v.Keys {
						printKey(self, k)
					}
					for _, m := range v.Meters {
						printMeter(self, m)
					}
					return next(ctx)
				}),
			},
		},
		Handler: xli.RequireSubcommand(),
	}
}

func newCmdKey() *xli.Command {
	return &xli.Command{
		Name:  "key",
		Brief: "what a product calls levyan with: issue one, revoke one",
		Commands: xli.Commands{
			{
				Name:  "issue",
				Brief: "issue a product a key, which is said once and never again",
				Flags: flg.Flags{
					&flg.String{Name: "alias", Brief: "what it is called when deciding whether to revoke it: loft-prod"},
					&flg.String{Name: "desc", Brief: "more about it: where it is mounted"},
					&flg.String{Name: "expires", Brief: "when it stops working, as 2006-01-02 or RFC 3339; never by default"},
					&flg.Switch{Name: "yes", Brief: "do it"},
				},
				Args: arg.Args{productArg},
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					if err := yes(self, "key issue"); err != nil {
						return err
					}
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					body := map[string]any{"alias": str(self, "alias"), "desc": str(self, "desc")}
					if v := str(self, "expires"); v != "" {
						t, err := instant(v)
						if err != nil {
							return fmt.Errorf("--expires: %w", err)
						}
						body["expires"] = t
					}
					var v admin.Issued
					if err := c.do(ctx, http.MethodPost, "/admin/products/"+url.PathEscape(argOf(self, "PRODUCT"))+"/keys", body, &v); err != nil {
						return err
					}
					printKey(self, v.Key)
					// The key on a line of its own, for a pipe into the secret
					// it is kept in; nothing will say it again.
					self.Printf("%s\n", v.Token)
					return next(ctx)
				}),
			},
			{
				Name:  "revoke",
				Brief: "revoke a key: it opens nothing from now on",
				Flags: flg.Flags{&flg.Switch{Name: "yes", Brief: "do it"}},
				Args:  arg.Args{&arg.String{Name: "ID", Brief: "the key, as `product show` lists it"}},
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					if err := yes(self, "key revoke"); err != nil {
						return err
					}
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					var v admin.Key
					if err := c.do(ctx, http.MethodPost, "/admin/keys/"+url.PathEscape(argOf(self, "ID"))+"/revoke", map[string]string{}, &v); err != nil {
						return err
					}
					printKey(self, v)
					return next(ctx)
				}),
			},
		},
		Handler: xli.RequireSubcommand(),
	}
}

func newCmdMeter() *xli.Command {
	return &xli.Command{
		Name:  "meter",
		Brief: "what a product is priced by",
		Commands: xli.Commands{
			{
				Name:  "add",
				Brief: "write a product's meter, which is never changed or erased after",
				Flags: flg.Flags{
					&flg.String{Name: "unit", Brief: "what a reading counts in, as OpenTelemetry writes a unit: By, {asset}, d"},
					&flg.String{Name: "kind", Brief: "stock, for what is held, or flow, for what happens"},
					&flg.String{Name: "aggregation", Brief: "how a period's readings come to one number: time_weighted_average, max or last of a stock, sum of a flow"},
					&flg.Switch{Name: "unbounded-zero", Brief: "a reading of 0 is without bound, as a history kept forever is"},
					&flg.String{Name: "name", Brief: "what a person calls it"},
					&flg.String{Name: "desc", Brief: "more about it"},
					&flg.Switch{Name: "yes", Brief: "do it"},
				},
				Args: arg.Args{productArg, &arg.String{Name: "KEY", Brief: "what readings are filed under: loft.assets.active"}},
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					if err := yes(self, "meter add"); err != nil {
						return err
					}
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					unbounded, _ := flg.Find[bool](self, "unbounded-zero")
					body := map[string]any{
						"key": argOf(self, "KEY"), "unit": str(self, "unit"), "kind": str(self, "kind"),
						"aggregation": str(self, "aggregation"), "unbounded_zero": unbounded,
						"name": str(self, "name"), "desc": str(self, "desc"),
					}
					var v admin.Meter
					if err := c.do(ctx, http.MethodPost, "/admin/products/"+url.PathEscape(argOf(self, "PRODUCT"))+"/meters", body, &v); err != nil {
						return err
					}
					printMeter(self, v)
					return next(ctx)
				}),
			},
		},
		Handler: xli.RequireSubcommand(),
	}
}

// band reads `--band UPTO:FEE`, and `--band :FEE` for the last band.
func band(v string) (admin.Band, error) {
	upTo, fee, ok := strings.Cut(strings.TrimSpace(v), ":")
	if !ok {
		return admin.Band{}, fmt.Errorf("%q is not UPTO:FEE", v)
	}
	b := admin.Band{}
	n, err := strconv.ParseInt(strings.TrimSpace(fee), 10, 64)
	if err != nil {
		return admin.Band{}, fmt.Errorf("%q: the fee: %w", v, err)
	}
	b.Fee = n
	if upTo = strings.TrimSpace(upTo); upTo != "" {
		n, err := strconv.ParseInt(upTo, 10, 64)
		if err != nil {
			return admin.Band{}, fmt.Errorf("%q: the bound: %w", v, err)
		}
		b.UpTo = &n
	}

	return b, nil
}

func newCmdPrice() *xli.Command {
	line := func(fs ...flg.Flag) flg.Flags {
		return append(flg.Flags{
			&flg.String{Name: "channel", Brief: "who it is sold through: mor or pg_kr"},
			&flg.String{Name: "currency", Brief: "ISO 4217: KRW, USD"},
		}, fs...)
	}

	return &xli.Command{
		Name:  "price",
		Brief: "what a meter costs: publish a version, list them, ask which was in force when",
		Commands: xli.Commands{
			{
				Name:  "add",
				Brief: "publish a version of a meter's price, beside the ones before it, which stay",
				Flags: line(
					&flg.Int64{Name: "included", Brief: "how much of the meter a period includes for nothing"},
					&flg.Int64{Name: "unit-price", Brief: "what each --per units past that cost, in the currency's minor unit: won, cents"},
					&flg.Int64{Name: "per", Brief: "how many units --unit-price is for; 1 by default"},
					&flg.Strings{Name: "band", Brief: "a band, UPTO:FEE, in order, and :FEE for the last; in place of a unit price"},
					&flg.String{Name: "effective", Brief: "from when, for customers who start then or later; now by default"},
					&flg.String{Name: "existing", Brief: "from when, for the customers already there; --effective by default"},
					&flg.String{Name: "desc", Brief: "why this version is published"},
					&flg.Switch{Name: "yes", Brief: "do it"},
				),
				Args: arg.Args{productArg, &arg.String{Name: "METER", Brief: "by its key"}},
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					if err := yes(self, "price add"); err != nil {
						return err
					}
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					body := map[string]any{
						"meter": argOf(self, "METER"), "channel": str(self, "channel"), "currency": str(self, "currency"),
						"desc": str(self, "desc"),
					}
					for _, k := range []string{"included", "unit-price", "per"} {
						if v, ok := flg.Find[int64](self, k); ok {
							body[strings.ReplaceAll(k, "-", "_")] = v
						}
					}
					if vs, _ := flg.Find[[]string](self, "band"); len(vs) > 0 {
						bands := []admin.Band{}
						for _, v := range vs {
							b, err := band(v)
							if err != nil {
								return fmt.Errorf("--band: %w", err)
							}
							bands = append(bands, b)
						}
						body["bands"] = bands
					}
					for _, k := range []string{"effective", "existing"} {
						if v := str(self, k); v != "" {
							t, err := instant(v)
							if err != nil {
								return fmt.Errorf("--%s: %w", k, err)
							}
							body[k] = t
						}
					}
					var v admin.Price
					if err := c.do(ctx, http.MethodPost, "/admin/products/"+url.PathEscape(argOf(self, "PRODUCT"))+"/prices", body, &v); err != nil {
						return err
					}
					printPrice(self, v)
					return next(ctx)
				}),
			},
			{
				Name:  "ls",
				Brief: "every version of a product's prices, in the order they were published",
				Flags: flg.Flags{&flg.String{Name: "meter", Brief: "of one meter, by its key"}},
				Args:  arg.Args{productArg},
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					q := url.Values{}
					if v := str(self, "meter"); v != "" {
						q.Set("meter", v)
					}
					var vs []admin.Price
					if err := c.do(ctx, http.MethodGet, "/admin/products/"+url.PathEscape(argOf(self, "PRODUCT"))+"/prices?"+q.Encode(), nil, &vs); err != nil {
						return err
					}
					for _, v := range vs {
						printPrice(self, v)
					}
					return next(ctx)
				}),
			},
			{
				Name:  "timeline",
				Brief: "which version of a meter's price was in force for a customer, when",
				Flags: line(
					&flg.String{Name: "since", Brief: "when the customer started; --from by default"},
					&flg.String{Name: "from", Brief: "the start of the stretch, as 2006-01-02 or RFC 3339"},
					&flg.String{Name: "to", Brief: "its end, which is not in it"},
				),
				Args: arg.Args{productArg, &arg.String{Name: "METER", Brief: "by its key"}},
				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					c, err := clientOf(self)
					if err != nil {
						return err
					}
					q := url.Values{"meter": {argOf(self, "METER")}, "channel": {str(self, "channel")}, "currency": {str(self, "currency")}}
					for _, k := range []string{"since", "from", "to"} {
						if v := str(self, k); v != "" {
							t, err := instant(v)
							if err != nil {
								return fmt.Errorf("--%s: %w", k, err)
							}
							q.Set(k, t.Format(time.RFC3339))
						}
					}
					var vs []admin.Segment
					if err := c.do(ctx, http.MethodGet, "/admin/products/"+url.PathEscape(argOf(self, "PRODUCT"))+"/timeline?"+q.Encode(), nil, &vs); err != nil {
						return err
					}
					for _, v := range vs {
						if v.Price == nil {
							self.Printf("%s\t%s\tnothing in force\n", when(v.From), when(v.To))
							continue
						}
						self.Printf("%s\t%s\t", when(v.From), when(v.To))
						printPrice(self, *v.Price)
					}
					return next(ctx)
				}),
			},
		},
		Handler: xli.RequireSubcommand(),
	}
}
