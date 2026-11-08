package webhook

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mildlybrutal/sling-orchestrator/internal/deploy"
	"github.com/mildlybrutal/sling-orchestrator/internal/proxy"
)

// Serve runs Sling as a webhook server: a push to the configured branch of
// the configured repo deploys that commit.
func Serve() error {
	cfg, err := loadWebhookConfig()
	if err != nil {
		return err
	}

	q := newDeployQueue(deployAndLog)
	stop := make(chan struct{})
	workerDone := make(chan struct{})
	go func() {
		q.worker(stop)
		close(workerDone)
	}()

	mux := http.NewServeMux()
	mux.Handle("/webhook", &webhookHandler{cfg: cfg, submit: q.submit})
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	errc := make(chan error, 1)
	go func() {
		log.Printf("Sling listening on http://%s/webhook (repo %s, branch %s)", cfg.Listen, cfg.Repo, cfg.Branch)
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
	_ = srv.Shutdown(shutdownCtx)
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
