// multi-channel is a metadata-only integration probe, not an Agent or durable inbox.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/Icatme/wechatbot-go/channels/dingtalk"
	"github.com/Icatme/wechatbot-go/channels/feishu"
	"github.com/Icatme/wechatbot-go/channels/qqbot"
	"github.com/Icatme/wechatbot-go/channels/wecom"
	"github.com/Icatme/wechatbot-go/channels/wecomapp"
	"github.com/Icatme/wechatbot-go/gateway"
)

type principal struct {
	Platform string `json:"platform"`
	Account string `json:"account"`
	Tenant string `json:"tenant"`
	User string `json:"user"`
}
func policy(raw string) (gateway.Authorizer, error) {
	var allow []principal
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&allow); err != nil { return nil, errors.New("CHANNEL_ALLOW_JSON must be a JSON array of exact principals") }
	var extra any
	if err := d.Decode(&extra); err != io.EOF { return nil, errors.New("trailing allowlist data") }
	if len(allow) == 0 { return nil, errors.New("allowlist must not be empty") }
	for _, p := range allow { if p.Platform == "" || p.Account == "" || p.User == "" { return nil, errors.New("principal needs platform, account and user") } }
	return func(_ context.Context, e gateway.Event) error {
		for _, p := range allow {
			if e.Route.Platform == p.Platform && e.Route.ID == p.Account && e.Route.TenantID == p.Tenant && e.SenderID == p.User { return nil }
		}
		return gateway.ErrUnauthorized
	}, nil
}
func env(key, def string) string { if v := os.Getenv(key); v != "" { return v }; return def }
func build(ctx context.Context) (*gateway.Gateway, *http.ServeMux, error) {
	authorize, err := policy(os.Getenv("CHANNEL_ALLOW_JSON"))
	if err != nil { return nil, nil, err }
	if os.Getenv("CHANNEL_DEMO_ACK") != "1" { return nil, nil, errors.New("set CHANNEL_DEMO_ACK=1 only for a test bot: this probe ACKs messages without storing or answering them") }
	g, err := gateway.New(gateway.Options{Authorize: authorize, Handler: func(_ context.Context, e gateway.Event) error {
		log.Printf("demo accepted platform=%q account=%q event=%q (no storage/Agent)", e.Route.Platform, e.Route.ID, e.ID)
		return nil
	}})
	if err != nil { return nil, nil, err }
	mux := http.NewServeMux()
	count := 0
	add := func(a gateway.Adapter, err error) error { if err != nil { return err }; if err = g.Register(a); err != nil { return err }; count++; return nil }
	if id := os.Getenv("FEISHU_APP_ID"); id != "" {
		a, e := feishu.New(feishu.Config{AccountID: env("FEISHU_ACCOUNT_ID", id), AppID: id, AppSecret: os.Getenv("FEISHU_APP_SECRET"), Transport: env("FEISHU_TRANSPORT", "websocket"), BaseURL: os.Getenv("FEISHU_BASE_URL"), VerificationToken: os.Getenv("FEISHU_VERIFICATION_TOKEN"), EncryptKey: os.Getenv("FEISHU_ENCRYPT_KEY"), BotOpenID: os.Getenv("FEISHU_BOT_OPEN_ID")})
		if err = add(a, e); err != nil { return nil, nil, err }
		if env("FEISHU_TRANSPORT", "websocket") == "webhook" { mux.Handle("/callbacks/feishu", a.Handler(g.Bind(a.Account()))) }
	}
	if id := os.Getenv("DINGTALK_CLIENT_ID"); id != "" {
		a, e := dingtalk.New(dingtalk.Config{AccountID: env("DINGTALK_ACCOUNT_ID", id), ClientID: id, ClientSecret: os.Getenv("DINGTALK_CLIENT_SECRET"), RobotCode: os.Getenv("DINGTALK_ROBOT_CODE")})
		if err = add(a, e); err != nil { return nil, nil, err }
	}
	if id := os.Getenv("WECOM_BOT_ID"); id != "" {
		a, e := wecom.New(wecom.Config{AccountID: env("WECOM_ACCOUNT_ID", id), BotID: id, Secret: os.Getenv("WECOM_SECRET"), TenantID: os.Getenv("WECOM_TENANT_ID")})
		if err = add(a, e); err != nil { return nil, nil, err }
	}
	if id := os.Getenv("WECOM_CORP_ID"); id != "" {
		agent, e := strconv.Atoi(os.Getenv("WECOM_AGENT_ID"))
		if e != nil { return nil, nil, errors.New("WECOM_AGENT_ID must be an integer") }
		a, e := wecomapp.New(wecomapp.Config{AccountID: env("WECOM_APP_ACCOUNT_ID", id+":"+strconv.Itoa(agent)), CorpID: id, CorpSecret: os.Getenv("WECOM_CORP_SECRET"), AgentID: agent, Token: os.Getenv("WECOM_CALLBACK_TOKEN"), EncodingAESKey: os.Getenv("WECOM_ENCODING_AES_KEY")})
		if err = add(a, e); err != nil { return nil, nil, err }
		mux.Handle("/callbacks/wecom-app", a.Handler(g.Bind(a.Account())))
	}
	if id := os.Getenv("QQ_APP_ID"); id != "" {
		a, e := qqbot.New(qqbot.Config{AccountID: env("QQ_ACCOUNT_ID", id), AppID: id, ClientSecret: os.Getenv("QQ_CLIENT_SECRET")})
		if err = add(a, e); err != nil { return nil, nil, err }
		mux.Handle("/callbacks/qq", a.Handler(g.Bind(a.Account())))
	}
	if count == 0 { return nil, nil, errors.New("no channel credentials configured") }
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { if r.Method != "GET" { w.WriteHeader(405); return }; _, _ = io.WriteString(w, "alive\n") })
	return g, mux, nil // process liveness, not per-channel readiness
}
func run(ctx context.Context) error {
	g, mux, err := build(ctx)
	if err != nil { return err }
	life, cancel := context.WithCancel(ctx)
	defer cancel()
	srv := &http.Server{Addr: env("CHANNEL_LISTEN", "127.0.0.1:8640"), Handler: mux, ReadHeaderTimeout: 5*time.Second, ReadTimeout: 10*time.Second, WriteTimeout: 10*time.Second, IdleTimeout: 60*time.Second, MaxHeaderBytes: 16<<10}
	serverErr, gatewayErr := make(chan error, 1), make(chan error, 1)
	go func() { serverErr <- srv.ListenAndServe() }()
	go func() { gatewayErr <- g.Run(life) }()
	gotServer, gotGateway := false, false
	select { case err = <-serverErr: gotServer = true; case err = <-gatewayErr: gotGateway = true; case <-ctx.Done(): err = ctx.Err() }
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if closeErr := srv.Shutdown(shutdown); closeErr != nil { _ = srv.Close() }
	if !gotServer { <-serverErr }; if !gotGateway { <-gatewayErr }
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, context.Canceled) { return nil }
	return err
}
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := run(ctx); err != nil { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
}
