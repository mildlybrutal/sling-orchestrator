package webhook

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mildlybrutal/sling-orchestrator/internal/deploy"
	"github.com/mildlybrutal/sling-orchestrator/internal/history"
	"github.com/mildlybrutal/sling-orchestrator/internal/proxy"
)

// Serve runs Sling as a webhook server: a push to the configured branch of
// the configured repo deploys that commit.
func Serve() error {
	cfg, err := loadWebhookConfig()
	if err != nil {
		return err
	}

	// History is required here: it's what GET /deploys serves and how a
	// deploy cut off by a crash is found later.
	path, err := history.DefaultPath()
	if err != nil {
		return err
	}
	store, err := history.Open(path)
	if err != nil {
		return err
	}
	defer store.Close()
	deploy.SetRecorder(store)
	defer deploy.SetRecorder(nil)
	log.Printf("deploy history: %s", path)

	q := newDeployQueue(deployAndLog)
	stop := make(chan struct{})
	workerDone := make(chan struct{})
	go func() {
		q.worker(stop)
		close(workerDone)
	}()

	submit := func(d *deploy.Deployment) { q.submit(d) }

	hook := &webhookHandler{cfg: cfg, submit: submit}
	mux := http.NewServeMux()
	mux.Handle("/webhook", hook)
	mux.Handle("/deploys", &deploysHandler{token: cfg.StatusToken, recent: store.Recent, live: proxy.CurrentUpstream})
	servers := []*http.Server{newServer(cfg.Listen, mux)}

	// Public mode: Caddy forwards https://<domain>/webhook to the sling-net
	// gateway (the host as containers see it). That listener serves
	// /webhook only, so /deploys is never one proxy hop from the internet.
	if proxy.Current().Public() {
		if err := proxy.Ensure(context.Background()); err != nil {
			return fmt.Errorf("proxy: %w", err)
		}
		addr, err := proxy.WebhookListenAddr()
		if err != nil {
			return err
		}
		public := http.NewServeMux()
		public.Handle("/webhook", hook)
		servers = append(servers, newServer(addr, public))
		log.Printf("public webhook: %s/webhook (Caddy → %s)", proxy.PublicURL, addr)
	}

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	errc := make(chan error, len(servers))
	for _, s := range servers[1:] {
		go func(s *http.Server) { errc <- s.ListenAndServe() }(s)
	}
	srv := servers[0]
	go func() {
		log.Printf("Sling listening on http://%s/webhook (repo %s, branch %s)", cfg.Listen, cfg.Repo, cfg.Branch)
		if len(cfg.StatusToken) > 0 {
			log.Printf("deploy history at http://%s/deploys (Authorization: Bearer $SLING_STATUS_TOKEN)", cfg.Listen)
		} else {
			log.Printf("GET /deploys is off; set SLING_STATUS_TOKEN to enable it")
		}
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}

	// Restore default signal handling: a second Ctrl-C kills immediately.
	stopSignals()
	log.Println("shutting down: refusing new webhooks, finishing any deploy in progress (Ctrl-C again to force)")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdownCtx)
	}
	close(stop)
	<-workerDone
	log.Println("stopped")
	return nil
}

func deployAndLog(d *deploy.Deployment) {
	sha := d.CommitSHA[:7]
	log.Printf("deploying %s", sha)
	err := deploy.Run(d)
	switch {
	case errors.Is(err, deploy.ErrAlreadyLive):
		log.Printf("%s is already live, nothing to do", sha)
		return
	case err != nil:
		log.Printf("deploy %s failed: %v", sha, err)
	default:
		log.Printf("deploy %s is live at %s", sha, proxy.PublicURL)
	}
	d.PrintTimeline()
}

func newServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}
