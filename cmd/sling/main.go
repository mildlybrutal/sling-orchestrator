// Command sling deploys one commit of an app with zero downtime, rolls back
// to an earlier one, shows what is deployed, or, as "sling serve", deploys
// every push GitHub sends to its webhook.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/mildlybrutal/sling-orchestrator/internal/deploy"
	"github.com/mildlybrutal/sling-orchestrator/internal/history"
	"github.com/mildlybrutal/sling-orchestrator/internal/proxy"
	"github.com/mildlybrutal/sling-orchestrator/internal/webhook"
)

const usage = `usage:
  %[1]s <repo-url-or-path> <40-char-commit-sha>   build and deploy one commit
  %[1]s rollback [sha]                            go back to the previous version, or to <sha> (full or short) if its image exists
  %[1]s status                                    show what is live and what can be rolled back to
  %[1]s history [n]                               the last n deploys and rollbacks (default 20), from any Sling process
  %[1]s serve                                     run the webhook server
(a local repo literally named "rollback", "status", "history" or "serve": write ./name)`

func main() {
	log.SetFlags(0) // CLI errors read better without a timestamp
	ps, err := proxy.LoadSettings()
	if err != nil {
		log.Fatal(err)
	}
	proxy.Configure(ps)

	args := os.Args[1:]
	switch {
	case len(args) == 1 && args[0] == "serve":
		log.SetFlags(log.LstdFlags) // a long-running server's log wants timestamps
		if err := webhook.Serve(); err != nil {
			log.Fatal(err)
		}
	case len(args) == 1 && args[0] == "status":
		if err := status(); err != nil {
			log.Fatal(err)
		}
	case len(args) >= 1 && len(args) <= 2 && args[0] == "history":
		n := 20
		if len(args) == 2 {
			v, err := strconv.Atoi(args[1])
			if err != nil || v < 1 || v > 1000 {
				log.Fatalf("history: n must be a number from 1 to 1000, got %q", args[1])
			}
			n = v
		}
		if err := showHistory(n); err != nil {
			log.Fatal(err)
		}
	case len(args) >= 1 && len(args) <= 2 && args[0] == "rollback":
		defer recordHistory()()
		d := &deploy.Deployment{}
		if len(args) == 2 {
			d.CommitSHA = args[1]
		}
		finish(d, deploy.Rollback(d))
	case len(args) == 2:
		defer recordHistory()()
		d := &deploy.Deployment{
			RepoURL:        args[0],
			CommitSHA:      args[1],
			AllowLocalRepo: true, // the person at the terminal already has the files
		}
		finish(d, deploy.Run(d))
	default:
		log.Fatalf(usage, os.Args[0])
	}
}

// finish reports how a deploy or rollback ended.
func finish(d *deploy.Deployment, err error) {
	if errors.Is(err, deploy.ErrAlreadyLive) {
		fmt.Printf("Nothing to do: %s is already live at %s\n", short(d.CommitSHA), proxy.PublicURL)
		return
	}
	if err != nil {
		d.PrintTimeline()
		log.Fatalf("%s failed: %v", d.Info().Kind, err)
	}

	fmt.Printf("\n✅ Live: %s\n", short(d.CommitSHA))
	fmt.Printf("   Container ID : %s\n", d.ContainerID[:12])
	fmt.Printf("   Public URL   : %s\n", proxy.PublicURL)
	fmt.Printf("   Direct URL   : http://127.0.0.1:%s (bypasses the proxy)\n", d.HostPort)
	if d.PreviousStopped {
		fmt.Printf("   Previous     : %s (stopped; undo with: sling rollback)\n", d.PreviousUpstream)
	}
	d.PrintTimeline()
}

// status prints what is deployed. It only reads: it never starts the proxy
// or takes the deploy lock, so it is safe to run during a deploy.
func status() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := deploy.CurrentState(ctx)
	if err != nil {
		return err
	}

	switch {
	case st.LiveErr != nil && errors.Is(st.LiveErr, syscall.ECONNREFUSED):
		fmt.Printf("Live     : nothing, the proxy isn't running (any deploy starts it)\n")
	case st.LiveErr != nil:
		fmt.Printf("Live     : unknown, proxy not reachable: %v\n", st.LiveErr)
	case st.LiveUpstream == "":
		fmt.Printf("Live     : nothing (%s serves 503)\n", proxy.PublicURL)
	default:
		fmt.Printf("Live     : %s at %s\n", st.LiveUpstream, proxy.PublicURL)
	}
	if st.RollbackTarget != "" {
		fmt.Printf("Rollback : sling rollback  →  %s\n", short(st.RollbackTarget))
	} else {
		fmt.Printf("Rollback : no previous version kept\n")
	}
	if len(st.Versions) == 0 {
		fmt.Println("\nNo Sling containers or images on this host yet.")
		return nil
	}

	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "\tCOMMIT\tCONTAINER\tIMAGE")
	for _, v := range st.Versions {
		mark := ""
		switch {
		case v.Live:
			mark = "● live"
		case v.SHA == st.RollbackTarget:
			mark = "↩ previous"
		}
		container := "-"
		if v.Container != "" {
			container = v.ContainerStatus
		}
		image := "pruned"
		if v.HasImage {
			image = "built " + v.ImageAge
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", mark, short(v.SHA), container, image)
	}
	return w.Flush()
}

// recordHistory saves this command's deploy to the shared history file.
// History is a convenience for the CLI: if it can't be opened, warn and
// deploy anyway. It returns the function that closes it.
func recordHistory() func() {
	path, err := history.DefaultPath()
	if err == nil {
		var store *history.Store
		if store, err = history.Open(path); err == nil {
			deploy.SetRecorder(store)
			return func() { deploy.SetRecorder(nil); store.Close() }
		}
	}
	fmt.Printf("⚠️  deploy history is off: %v\n", err)
	return func() {}
}

func showHistory(n int) error {
	path, err := history.DefaultPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		fmt.Println("No deploys recorded yet.")
		return nil
	}
	store, err := history.Open(path)
	if err != nil {
		return err
	}
	defer store.Close()
	list, err := store.Recent(n)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("No deploys recorded yet.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "STARTED\tKIND\tCOMMIT\tSTATUS\tTOOK\tERROR")
	for _, in := range list {
		start, end := in.Timeline[0].At, in.Timeline[len(in.Timeline)-1].At
		took := end.Sub(start).Round(time.Second).String()
		errText := in.Error
		if i := strings.IndexByte(errText, '\n'); i >= 0 {
			errText = errText[:i] // first line; container logs follow it
		}
		if len(errText) > 70 {
			errText = errText[:67] + "..."
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			start.Local().Format("Jan 02 15:04:05"), in.Kind, short(in.Commit), in.Status, took, errText)
	}
	return w.Flush()
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
