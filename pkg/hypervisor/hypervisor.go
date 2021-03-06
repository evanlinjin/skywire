// Package hypervisor implements management node
package hypervisor

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/rpc"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/google/uuid"
	"github.com/skycoin/dmsg"
	"github.com/skycoin/dmsg/cipher"
	"github.com/skycoin/dmsg/dmsgpty"
	"github.com/skycoin/dmsg/httputil"
	"github.com/skycoin/skycoin/src/util/logging"

	"github.com/skycoin/skywire/pkg/app"
	"github.com/skycoin/skywire/pkg/routing"
	"github.com/skycoin/skywire/pkg/skyenv"
	"github.com/skycoin/skywire/pkg/util/buildinfo"
	"github.com/skycoin/skywire/pkg/visor"
)

const (
	healthTimeout = 5 * time.Second
	httpTimeout   = 30 * time.Second
)

const (
	statusStop = iota
	statusStart
)

var (
	log = logging.MustGetLogger("hypervisor") // nolint: gochecknoglobals
)

// VisorConn represents a visor connection.
type VisorConn struct {
	Addr  dmsg.Addr
	RPC   visor.RPCClient
	PtyUI *dmsgpty.UI
}

// Hypervisor manages visors.
type Hypervisor struct {
	c      Config
	assets http.FileSystem             // Web UI.
	visors map[cipher.PubKey]VisorConn // connected remote visors.
	users  *UserManager
	mu     *sync.RWMutex
}

// New creates a new Hypervisor.
func New(assets http.FileSystem, config Config) (*Hypervisor, error) {
	config.Cookies.TLS = config.EnableTLS

	boltUserDB, err := NewBoltUserStore(config.DBPath)
	if err != nil {
		return nil, err
	}

	singleUserDB := NewSingleUserStore("admin", boltUserDB)

	return &Hypervisor{
		c:      config,
		assets: assets,
		visors: make(map[cipher.PubKey]VisorConn),
		users:  NewUserManager(singleUserDB, config.Cookies),
		mu:     new(sync.RWMutex),
	}, nil
}

// ServeRPC serves RPC of a Hypervisor.
func (hv *Hypervisor) ServeRPC(dmsgC *dmsg.Client, lis *dmsg.Listener) error {
	for {
		conn, err := lis.AcceptStream()
		if err != nil {
			return err
		}
		addr := conn.RawRemoteAddr()
		ptyDialer := dmsgpty.DmsgUIDialer(dmsgC, dmsg.Addr{PK: addr.PK, Port: skyenv.DmsgPtyPort})
		visorConn := VisorConn{
			Addr:  addr,
			RPC:   visor.NewRPCClient(rpc.NewClient(conn), visor.RPCPrefix),
			PtyUI: dmsgpty.NewUI(ptyDialer, dmsgpty.DefaultUIConfig()),
		}
		log.WithField("remote_addr", addr).Info("Accepted.")
		hv.mu.Lock()
		hv.visors[addr.PK] = visorConn
		hv.mu.Unlock()
	}
}

// MockConfig configures how mock data is to be added.
type MockConfig struct {
	Visors            int
	MaxTpsPerVisor    int
	MaxRoutesPerVisor int
	EnableAuth        bool
}

// AddMockData adds mock data to Hypervisor.
func (hv *Hypervisor) AddMockData(config MockConfig) error {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))

	for i := 0; i < config.Visors; i++ {
		pk, client, err := visor.NewMockRPCClient(r, config.MaxTpsPerVisor, config.MaxRoutesPerVisor)
		if err != nil {
			return err
		}

		hv.mu.Lock()
		hv.visors[pk] = VisorConn{
			Addr: dmsg.Addr{
				PK:   pk,
				Port: uint16(i),
			},
			RPC: client,
		}
		hv.mu.Unlock()
	}

	hv.c.EnableAuth = config.EnableAuth

	return nil
}

// ServeHTTP implements http.Handler
func (hv *Hypervisor) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r := chi.NewRouter()
	r.Use(middleware.Logger)

	r.Route("/", func(r chi.Router) {
		r.Route("/api", func(r chi.Router) {
			r.Use(middleware.Timeout(httpTimeout))

			r.Get("/ping", hv.getPong())

			if hv.c.EnableAuth {
				r.Group(func(r chi.Router) {
					r.Post("/create-account", hv.users.CreateAccount())
					r.Post("/login", hv.users.Login())
					r.Post("/logout", hv.users.Logout())
				})
			}

			r.Group(func(r chi.Router) {
				if hv.c.EnableAuth {
					r.Use(hv.users.Authorize)
				}
				r.Get("/user", hv.users.UserInfo())
				r.Post("/change-password", hv.users.ChangePassword())
				r.Get("/about", hv.getAbout())
				r.Get("/visors", hv.getVisors())
				r.Get("/visors/{pk}", hv.getVisor())
				r.Get("/visors/{pk}/health", hv.getHealth())
				r.Get("/visors/{pk}/uptime", hv.getUptime())
				r.Get("/visors/{pk}/apps", hv.getApps())
				r.Get("/visors/{pk}/apps/{app}", hv.getApp())
				r.Put("/visors/{pk}/apps/{app}", hv.putApp())
				r.Get("/visors/{pk}/apps/{app}/logs", hv.appLogsSince())
				r.Get("/visors/{pk}/transport-types", hv.getTransportTypes())
				r.Get("/visors/{pk}/transports", hv.getTransports())
				r.Post("/visors/{pk}/transports", hv.postTransport())
				r.Get("/visors/{pk}/transports/{tid}", hv.getTransport())
				r.Delete("/visors/{pk}/transports/{tid}", hv.deleteTransport())
				r.Get("/visors/{pk}/routes", hv.getRoutes())
				r.Post("/visors/{pk}/routes", hv.postRoute())
				r.Get("/visors/{pk}/routes/{rid}", hv.getRoute())
				r.Put("/visors/{pk}/routes/{rid}", hv.putRoute())
				r.Delete("/visors/{pk}/routes/{rid}", hv.deleteRoute())
				r.Get("/visors/{pk}/routegroups", hv.getRouteGroups())
				r.Post("/visors/{pk}/restart", hv.restart())
				r.Post("/visors/{pk}/exec", hv.exec())
				r.Post("/visors/{pk}/update", hv.update())
				r.Get("/visors/{pk}/update/available", hv.updateAvailable())
			})
		})

		r.Route("/pty", func(r chi.Router) {
			if hv.c.EnableAuth {
				r.Use(hv.users.Authorize)
			}
			r.Get("/{pk}", hv.getPty())
		})

		r.Handle("/*", http.FileServer(hv.assets))
	})

	r.ServeHTTP(w, req)
}

func (hv *Hypervisor) getPong() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte(`"PONG!"`)); err != nil {
			log.WithError(err).Warn("getPong: Failed to send PONG!")
		}
	}
}

// About provides info about the hypervisor.
type About struct {
	PubKey cipher.PubKey   `json:"public_key"` // The hypervisor's public key.
	Build  *buildinfo.Info `json:"build"`
}

func (hv *Hypervisor) getAbout() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		httputil.WriteJSON(w, r, http.StatusOK, About{
			PubKey: hv.c.PK,
			Build:  buildinfo.Get(),
		})
	}
}

// VisorHealth represents a visor's health report attached to hypervisor to visor request status
type VisorHealth struct {
	Status int `json:"status"`
	*visor.HealthInfo
}

// provides summary of health information for every visor
func (hv *Hypervisor) getHealth() http.HandlerFunc {
	return hv.withCtx(hv.visorCtx, func(w http.ResponseWriter, r *http.Request, ctx *httpCtx) {
		vh := &VisorHealth{}

		type healthRes struct {
			h   *visor.HealthInfo
			err error
		}

		resCh := make(chan healthRes)
		tCh := time.After(healthTimeout)

		go func() {
			hi, err := ctx.RPC.Health()
			resCh <- healthRes{hi, err}
		}()

		select {
		case res := <-resCh:
			if res.err != nil {
				vh.Status = http.StatusInternalServerError
			} else {
				vh.HealthInfo = res.h
				vh.Status = http.StatusOK
			}

			httputil.WriteJSON(w, r, http.StatusOK, vh)
		case <-tCh:
			httputil.WriteJSON(w, r, http.StatusRequestTimeout, &VisorHealth{Status: http.StatusRequestTimeout})
		}
	})
}

// getUptime gets given visor's uptime
func (hv *Hypervisor) getUptime() http.HandlerFunc {
	return hv.withCtx(hv.visorCtx, func(w http.ResponseWriter, r *http.Request, ctx *httpCtx) {
		u, err := ctx.RPC.Uptime()
		if err != nil {
			httputil.WriteJSON(w, r, http.StatusInternalServerError, err)
			return
		}

		httputil.WriteJSON(w, r, http.StatusOK, u)
	})
}

type summaryResp struct {
	TCPAddr string `json:"tcp_addr"`
	Online  bool   `json:"online"`
	*visor.Summary
}

// provides summary of all visors.
func (hv *Hypervisor) getVisors() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hv.mu.RLock()
		wg := new(sync.WaitGroup)
		wg.Add(len(hv.visors))
		summaries, i := make([]summaryResp, len(hv.visors)), 0

		for pk, c := range hv.visors {
			go func(pk cipher.PubKey, c VisorConn, i int) {
				log := log.
					WithField("visor_addr", c.Addr).
					WithField("func", "getVisors")

				log.Debug("Requesting summary via RPC.")

				summary, err := c.RPC.Summary()
				if err != nil {
					log.WithError(err).
						Warn("Failed to obtain summary via RPC.")
					summary = &visor.Summary{PubKey: pk}
				} else {
					log.Debug("Obtained summary via RPC.")
				}
				summaries[i] = summaryResp{
					TCPAddr: c.Addr.String(),
					Online:  err == nil,
					Summary: summary,
				}
				wg.Done()
			}(pk, c, i)
			i++
		}

		wg.Wait()
		hv.mu.RUnlock()

		httputil.WriteJSON(w, r, http.StatusOK, summaries)
	}
}

// provides summary of single visor.
func (hv *Hypervisor) getVisor() http.HandlerFunc {
	return hv.withCtx(hv.visorCtx, func(w http.ResponseWriter, r *http.Request, ctx *httpCtx) {
		summary, err := ctx.RPC.Summary()
		if err != nil {
			httputil.WriteJSON(w, r, http.StatusInternalServerError, err)
			return
		}

		httputil.WriteJSON(w, r, http.StatusOK, summaryResp{
			TCPAddr: ctx.Addr.String(),
			Summary: summary,
		})
	})
}

func (hv *Hypervisor) getPty() http.HandlerFunc {
	return hv.withCtx(hv.visorCtx, func(w http.ResponseWriter, r *http.Request, ctx *httpCtx) {
		ctx.PtyUI.Handler()(w, r)
	})
}

// returns app summaries of a given node of pk
func (hv *Hypervisor) getApps() http.HandlerFunc {
	return hv.withCtx(hv.visorCtx, func(w http.ResponseWriter, r *http.Request, ctx *httpCtx) {
		apps, err := ctx.RPC.Apps()
		if err != nil {
			httputil.WriteJSON(w, r, http.StatusInternalServerError, err)
			return
		}

		httputil.WriteJSON(w, r, http.StatusOK, apps)
	})
}

// returns an app summary of a given visor's pk and app name
func (hv *Hypervisor) getApp() http.HandlerFunc {
	return hv.withCtx(hv.appCtx, func(w http.ResponseWriter, r *http.Request, ctx *httpCtx) {
		httputil.WriteJSON(w, r, http.StatusOK, ctx.App)
	})
}

// TODO: simplify
// nolint: funlen,gocognit,godox
func (hv *Hypervisor) putApp() http.HandlerFunc {
	return hv.withCtx(hv.appCtx, func(w http.ResponseWriter, r *http.Request, ctx *httpCtx) {
		var reqBody struct {
			AutoStart *bool          `json:"autostart,omitempty"`
			Status    *int           `json:"status,omitempty"`
			Passcode  *string        `json:"passcode,omitempty"`
			PK        *cipher.PubKey `json:"pk,omitempty"`
		}

		if err := httputil.ReadJSON(r, &reqBody); err != nil {
			if err != io.EOF {
				log.Warnf("putApp request: %v", err)
			}

			httputil.WriteJSON(w, r, http.StatusBadRequest, ErrMalformedRequest)

			return
		}

		if reqBody.AutoStart != nil {
			if *reqBody.AutoStart != ctx.App.AutoStart {
				if err := ctx.RPC.SetAutoStart(ctx.App.Name, *reqBody.AutoStart); err != nil {
					httputil.WriteJSON(w, r, http.StatusInternalServerError, err)
					return
				}
			}
		}

		const (
			skysocksName       = "skysocks"
			skysocksClientName = "skysocks-client"
		)

		if reqBody.Passcode != nil && ctx.App.Name == skysocksName {
			if err := ctx.RPC.SetSocksPassword(*reqBody.Passcode); err != nil {
				httputil.WriteJSON(w, r, http.StatusInternalServerError, err)
				return
			}
		}

		if reqBody.PK != nil && ctx.App.Name == skysocksClientName {
			log.Errorf("SETTING PK: %s", *reqBody.PK)
			if err := ctx.RPC.SetSocksClientPK(*reqBody.PK); err != nil {
				log.Errorf("ERROR SETTING PK")
				httputil.WriteJSON(w, r, http.StatusInternalServerError, err)
				return
			}
		}

		if reqBody.Status != nil {
			switch *reqBody.Status {
			case statusStop:
				if err := ctx.RPC.StopApp(ctx.App.Name); err != nil {
					httputil.WriteJSON(w, r, http.StatusInternalServerError, err)
					return
				}
			case statusStart:
				if err := ctx.RPC.StartApp(ctx.App.Name); err != nil {
					log.Errorf("ERROR STARTING APP")
					httputil.WriteJSON(w, r, http.StatusInternalServerError, err)
					return
				}
			default:
				errMsg := fmt.Errorf("value of 'status' field is %d when expecting 0 or 1", *reqBody.Status)
				httputil.WriteJSON(w, r, http.StatusBadRequest, errMsg)
				return
			}
		}

		httputil.WriteJSON(w, r, http.StatusOK, ctx.App)
	})
}

// LogsRes parses logs as json, along with the last obtained timestamp for use on subsequent requests
type LogsRes struct {
	LastLogTimestamp string   `json:"last_log_timestamp"`
	Logs             []string `json:"logs"`
}

func (hv *Hypervisor) appLogsSince() http.HandlerFunc {
	return hv.withCtx(hv.appCtx, func(w http.ResponseWriter, r *http.Request, ctx *httpCtx) {
		since := r.URL.Query().Get("since")
		since = strings.Replace(since, " ", "+", 1) // we need to put '+' again that was replaced in the query string

		// if time is not parsable or empty default to return all logs
		t, err := time.Parse(time.RFC3339Nano, since)
		if err != nil {
			t = time.Unix(0, 0)
		}

		logs, err := ctx.RPC.LogsSince(t, ctx.App.Name)
		if err != nil {
			httputil.WriteJSON(w, r, http.StatusInternalServerError, err)
			return
		}

		if len(logs) == 0 {
			httputil.WriteJSON(w, r, http.StatusInternalServerError, fmt.Errorf("no new available logs"))
			return
		}

		httputil.WriteJSON(w, r, http.StatusOK, &LogsRes{
			LastLogTimestamp: app.TimestampFromLog(logs[len(logs)-1]),
			Logs:             logs,
		})
	})
}

func (hv *Hypervisor) getTransportTypes() http.HandlerFunc {
	return hv.withCtx(hv.visorCtx, func(w http.ResponseWriter, r *http.Request, ctx *httpCtx) {
		types, err := ctx.RPC.TransportTypes()
		if err != nil {
			httputil.WriteJSON(w, r, http.StatusInternalServerError, err)
			return
		}

		httputil.WriteJSON(w, r, http.StatusOK, types)
	})
}

func (hv *Hypervisor) getTransports() http.HandlerFunc {
	return hv.withCtx(hv.visorCtx, func(w http.ResponseWriter, r *http.Request, ctx *httpCtx) {
		qTypes := strSliceFromQuery(r, "type", nil)

		qPKs, err := pkSliceFromQuery(r, "pk", nil)
		if err != nil {
			httputil.WriteJSON(w, r, http.StatusBadRequest, err)
			return
		}

		qLogs, err := httputil.BoolFromQuery(r, "logs", true)
		if err != nil {
			httputil.WriteJSON(w, r, http.StatusBadRequest, err)
			return
		}

		transports, err := ctx.RPC.Transports(qTypes, qPKs, qLogs)
		if err != nil {
			httputil.WriteJSON(w, r, http.StatusInternalServerError, err)
			return
		}
		httputil.WriteJSON(w, r, http.StatusOK, transports)
	})
}

func (hv *Hypervisor) postTransport() http.HandlerFunc {
	return hv.withCtx(hv.visorCtx, func(w http.ResponseWriter, r *http.Request, ctx *httpCtx) {
		var reqBody struct {
			TpType string        `json:"transport_type"`
			Remote cipher.PubKey `json:"remote_pk"`
			Public bool          `json:"public"`
		}

		if err := httputil.ReadJSON(r, &reqBody); err != nil {
			if err != io.EOF {
				log.Warnf("postTransport request: %v", err)
			}

			httputil.WriteJSON(w, r, http.StatusBadRequest, ErrMalformedRequest)

			return
		}

		const timeout = 30 * time.Second
		summary, err := ctx.RPC.AddTransport(reqBody.Remote, reqBody.TpType, reqBody.Public, timeout)
		if err != nil {
			httputil.WriteJSON(w, r, http.StatusInternalServerError, err)
			return
		}

		httputil.WriteJSON(w, r, http.StatusOK, summary)
	})
}

func (hv *Hypervisor) getTransport() http.HandlerFunc {
	return hv.withCtx(hv.tpCtx, func(w http.ResponseWriter, r *http.Request, ctx *httpCtx) {
		httputil.WriteJSON(w, r, http.StatusOK, ctx.Tp)
	})
}

func (hv *Hypervisor) deleteTransport() http.HandlerFunc {
	return hv.withCtx(hv.tpCtx, func(w http.ResponseWriter, r *http.Request, ctx *httpCtx) {
		if err := ctx.RPC.RemoveTransport(ctx.Tp.ID); err != nil {
			httputil.WriteJSON(w, r, http.StatusInternalServerError, err)
			return
		}

		httputil.WriteJSON(w, r, http.StatusOK, true)
	})
}

type routingRuleResp struct {
	Key     routing.RouteID      `json:"key"`
	Rule    string               `json:"rule"`
	Summary *routing.RuleSummary `json:"rule_summary,omitempty"`
}

func makeRoutingRuleResp(key routing.RouteID, rule routing.Rule, summary bool) routingRuleResp {
	resp := routingRuleResp{
		Key:  key,
		Rule: hex.EncodeToString(rule),
	}

	if summary {
		resp.Summary = rule.Summary()
	}

	return resp
}

func (hv *Hypervisor) getRoutes() http.HandlerFunc {
	return hv.withCtx(hv.visorCtx, func(w http.ResponseWriter, r *http.Request, ctx *httpCtx) {
		qSummary, err := httputil.BoolFromQuery(r, "summary", false)
		if err != nil {
			httputil.WriteJSON(w, r, http.StatusBadRequest, err)
			return
		}

		rules, err := ctx.RPC.RoutingRules()
		if err != nil {
			httputil.WriteJSON(w, r, http.StatusInternalServerError, err)
			return
		}

		resp := make([]routingRuleResp, len(rules))
		for i, rule := range rules {
			resp[i] = makeRoutingRuleResp(rule.KeyRouteID(), rule, qSummary)
		}

		httputil.WriteJSON(w, r, http.StatusOK, resp)
	})
}

func (hv *Hypervisor) postRoute() http.HandlerFunc {
	return hv.withCtx(hv.visorCtx, func(w http.ResponseWriter, r *http.Request, ctx *httpCtx) {
		var summary routing.RuleSummary
		if err := httputil.ReadJSON(r, &summary); err != nil {
			if err != io.EOF {
				log.Warnf("postRoute request: %v", err)
			}

			httputil.WriteJSON(w, r, http.StatusBadRequest, ErrMalformedRequest)

			return
		}

		rule, err := summary.ToRule()
		if err != nil {
			httputil.WriteJSON(w, r, http.StatusBadRequest, err)
			return
		}

		if err := ctx.RPC.SaveRoutingRule(rule); err != nil {
			httputil.WriteJSON(w, r, http.StatusInternalServerError, err)
			return
		}

		httputil.WriteJSON(w, r, http.StatusOK, makeRoutingRuleResp(rule.KeyRouteID(), rule, true))
	})
}

func (hv *Hypervisor) getRoute() http.HandlerFunc {
	return hv.withCtx(hv.routeCtx, func(w http.ResponseWriter, r *http.Request, ctx *httpCtx) {
		qSummary, err := httputil.BoolFromQuery(r, "summary", true)
		if err != nil {
			httputil.WriteJSON(w, r, http.StatusBadRequest, err)
			return
		}

		rule, err := ctx.RPC.RoutingRule(ctx.RtKey)
		if err != nil {
			httputil.WriteJSON(w, r, http.StatusNotFound, err)
			return
		}

		httputil.WriteJSON(w, r, http.StatusOK, makeRoutingRuleResp(ctx.RtKey, rule, qSummary))
	})
}

func (hv *Hypervisor) putRoute() http.HandlerFunc {
	return hv.withCtx(hv.routeCtx, func(w http.ResponseWriter, r *http.Request, ctx *httpCtx) {
		var summary routing.RuleSummary
		if err := httputil.ReadJSON(r, &summary); err != nil {
			if err != io.EOF {
				log.Warnf("putRoute request: %v", err)
			}

			httputil.WriteJSON(w, r, http.StatusBadRequest, ErrMalformedRequest)

			return
		}

		rule, err := summary.ToRule()
		if err != nil {
			httputil.WriteJSON(w, r, http.StatusBadRequest, err)
			return
		}

		if err := ctx.RPC.SaveRoutingRule(rule); err != nil {
			httputil.WriteJSON(w, r, http.StatusInternalServerError, err)
			return
		}

		httputil.WriteJSON(w, r, http.StatusOK, makeRoutingRuleResp(ctx.RtKey, rule, true))
	})
}

func (hv *Hypervisor) deleteRoute() http.HandlerFunc {
	return hv.withCtx(hv.routeCtx, func(w http.ResponseWriter, r *http.Request, ctx *httpCtx) {
		if err := ctx.RPC.RemoveRoutingRule(ctx.RtKey); err != nil {
			httputil.WriteJSON(w, r, http.StatusNotFound, err)
			return
		}

		httputil.WriteJSON(w, r, http.StatusOK, true)
	})
}

type routeGroupResp struct {
	routing.RuleConsumeFields
	FwdRule routing.RuleForwardFields `json:"resp"`
}

func makeRouteGroupResp(info visor.RouteGroupInfo) routeGroupResp {
	if len(info.FwdRule) == 0 || len(info.ConsumeRule) == 0 {
		return routeGroupResp{}
	}

	return routeGroupResp{
		RuleConsumeFields: *info.ConsumeRule.Summary().ConsumeFields,
		FwdRule:           *info.FwdRule.Summary().ForwardFields,
	}
}

func (hv *Hypervisor) getRouteGroups() http.HandlerFunc {
	return hv.withCtx(hv.visorCtx, func(w http.ResponseWriter, r *http.Request, ctx *httpCtx) {
		routegroups, err := ctx.RPC.RouteGroups()
		if err != nil {
			httputil.WriteJSON(w, r, http.StatusInternalServerError, err)
			return
		}

		resp := make([]routeGroupResp, len(routegroups))
		for i, l := range routegroups {
			resp[i] = makeRouteGroupResp(l)
		}

		httputil.WriteJSON(w, r, http.StatusOK, resp)
	})
}

// NOTE: Reply comes with a delay, because of check if new executable is started successfully.
func (hv *Hypervisor) restart() http.HandlerFunc {
	return hv.withCtx(hv.visorCtx, func(w http.ResponseWriter, r *http.Request, ctx *httpCtx) {
		if err := ctx.RPC.Restart(); err != nil {
			httputil.WriteJSON(w, r, http.StatusInternalServerError, err)
			return
		}

		httputil.WriteJSON(w, r, http.StatusOK, true)
	})
}

// executes a command and returns its output
func (hv *Hypervisor) exec() http.HandlerFunc {
	return hv.withCtx(hv.visorCtx, func(w http.ResponseWriter, r *http.Request, ctx *httpCtx) {
		var reqBody struct {
			Command string `json:"command"`
		}

		if err := httputil.ReadJSON(r, &reqBody); err != nil {
			if err != io.EOF {
				log.Warnf("exec request: %v", err)
			}

			httputil.WriteJSON(w, r, http.StatusBadRequest, ErrMalformedRequest)

			return
		}

		out, err := ctx.RPC.Exec(reqBody.Command)
		if err != nil {
			httputil.WriteJSON(w, r, http.StatusInternalServerError, err)
			return
		}

		output := struct {
			Output string `json:"output"`
		}{string(out)}

		httputil.WriteJSON(w, r, http.StatusOK, output)
	})
}

func (hv *Hypervisor) update() http.HandlerFunc {
	return hv.withCtx(hv.visorCtx, func(w http.ResponseWriter, r *http.Request, ctx *httpCtx) {
		updated, err := ctx.RPC.Update()
		if err != nil {
			httputil.WriteJSON(w, r, http.StatusInternalServerError, err)
			return
		}

		output := struct {
			Updated bool `json:"updated"`
		}{updated}

		httputil.WriteJSON(w, r, http.StatusOK, output)
	})
}

func (hv *Hypervisor) updateAvailable() http.HandlerFunc {
	return hv.withCtx(hv.visorCtx, func(w http.ResponseWriter, r *http.Request, ctx *httpCtx) {
		version, err := ctx.RPC.UpdateAvailable()
		if err != nil {
			httputil.WriteJSON(w, r, http.StatusInternalServerError, err)
			return
		}

		output := struct {
			Available        bool   `json:"available"`
			CurrentVersion   string `json:"current_version"`
			AvailableVersion string `json:"available_version,omitempty"`
		}{
			Available:      version != nil,
			CurrentVersion: buildinfo.Version(),
		}

		if version != nil {
			output.AvailableVersion = version.String()
		}

		httputil.WriteJSON(w, r, http.StatusOK, output)
	})
}

/*
	<<< Helper functions >>>
*/

func (hv *Hypervisor) visorConn(pk cipher.PubKey) (VisorConn, bool) {
	hv.mu.RLock()
	conn, ok := hv.visors[pk]
	hv.mu.RUnlock()

	return conn, ok
}

type httpCtx struct {
	// Hypervisor
	VisorConn

	// App
	App *visor.AppState

	// Transport
	Tp *visor.TransportSummary

	// Route
	RtKey routing.RouteID
}

type (
	valuesFunc  func(w http.ResponseWriter, r *http.Request) (*httpCtx, bool)
	handlerFunc func(w http.ResponseWriter, r *http.Request, ctx *httpCtx)
)

func (hv *Hypervisor) withCtx(vFunc valuesFunc, hFunc handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if rv, ok := vFunc(w, r); ok {
			hFunc(w, r, rv)
		}
	}
}

func (hv *Hypervisor) visorCtx(w http.ResponseWriter, r *http.Request) (*httpCtx, bool) {
	pk, err := pkFromParam(r, "pk")
	if err != nil {
		httputil.WriteJSON(w, r, http.StatusBadRequest, err)
		return nil, false
	}

	visor, ok := hv.visorConn(pk)

	if !ok {
		httputil.WriteJSON(w, r, http.StatusNotFound, fmt.Errorf("visor of pk '%s' not found", pk))
		return nil, false
	}

	return &httpCtx{
		VisorConn: visor,
	}, true
}

func (hv *Hypervisor) appCtx(w http.ResponseWriter, r *http.Request) (*httpCtx, bool) {
	ctx, ok := hv.visorCtx(w, r)
	if !ok {
		return nil, false
	}

	appName := chi.URLParam(r, "app")

	apps, err := ctx.RPC.Apps()
	if err != nil {
		httputil.WriteJSON(w, r, http.StatusInternalServerError, err)
		return nil, false
	}

	for _, a := range apps {
		if a.Name == appName {
			ctx.App = a
			return ctx, true
		}
	}

	errMsg := fmt.Errorf("can not find app of name %s from visor %s", appName, ctx.Addr.PK)
	httputil.WriteJSON(w, r, http.StatusNotFound, errMsg)

	return nil, false
}

func (hv *Hypervisor) tpCtx(w http.ResponseWriter, r *http.Request) (*httpCtx, bool) {
	ctx, ok := hv.visorCtx(w, r)
	if !ok {
		return nil, false
	}

	tid, err := uuidFromParam(r, "tid")
	if err != nil {
		httputil.WriteJSON(w, r, http.StatusBadRequest, err)
		return nil, false
	}

	tp, err := ctx.RPC.Transport(tid)
	if err != nil {
		if err.Error() == visor.ErrNotFound.Error() {
			errMsg := fmt.Errorf("transport of ID %s is not found", tid)
			httputil.WriteJSON(w, r, http.StatusNotFound, errMsg)

			return nil, false
		}

		httputil.WriteJSON(w, r, http.StatusInternalServerError, err)

		return nil, false
	}

	ctx.Tp = tp

	return ctx, true
}

func (hv *Hypervisor) routeCtx(w http.ResponseWriter, r *http.Request) (*httpCtx, bool) {
	ctx, ok := hv.visorCtx(w, r)
	if !ok {
		return nil, false
	}

	rid, err := ridFromParam(r, "rid")
	if err != nil {
		httputil.WriteJSON(w, r, http.StatusBadRequest, err)
		return nil, false
	}

	ctx.RtKey = rid

	return ctx, true
}

func pkFromParam(r *http.Request, key string) (cipher.PubKey, error) {
	pk := cipher.PubKey{}
	err := pk.UnmarshalText([]byte(chi.URLParam(r, key)))

	return pk, err
}

func uuidFromParam(r *http.Request, key string) (uuid.UUID, error) {
	return uuid.Parse(chi.URLParam(r, key))
}

func ridFromParam(r *http.Request, key string) (routing.RouteID, error) {
	rid, err := strconv.ParseUint(chi.URLParam(r, key), 10, 32)
	if err != nil {
		return 0, errors.New("invalid route ID provided")
	}

	return routing.RouteID(rid), nil
}

func strSliceFromQuery(r *http.Request, key string, defaultVal []string) []string {
	slice, ok := r.URL.Query()[key]
	if !ok {
		return defaultVal
	}

	return slice
}

func pkSliceFromQuery(r *http.Request, key string, defaultVal []cipher.PubKey) ([]cipher.PubKey, error) {
	qPKs, ok := r.URL.Query()[key]
	if !ok {
		return defaultVal, nil
	}

	pks := make([]cipher.PubKey, len(qPKs))

	for i, qPK := range qPKs {
		pk := cipher.PubKey{}
		if err := pk.UnmarshalText([]byte(qPK)); err != nil {
			return nil, err
		}

		pks[i] = pk
	}

	return pks, nil
}
