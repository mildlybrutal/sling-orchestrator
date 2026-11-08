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
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/mildlybrutal/sling-orchestrator/internal/deploy"
	"github.com/mildlybrutal/sling-orchestrator/internal/proxy"
	"github.com/mildlybrutal/sling-orchestrator/internal/webhook"
)

const usage = `usage:
  %[1]s <repo-url-or-path> <40-char-commit-sha>   build and deploy one commit
  %[1]s rollback [sha]                            go back to the previous version, or to <sha> (full or short) if its image exists
  %[1]s status                                    show what is live and what can be rolled back to
  %[1]s serve                                     run the webhook server
(a local repo literally named "rollback", "status" or "serve": write ./name)`

func main() {
	log.SetFlags(0) // CLI errors read better without a timestamp
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
	case len(args) >= 1 && len(args) <= 2 && args[0] == "rollback":
		d := &deploy.Deployment{}
		if len(args) == 2 {
			d.CommitSHA = args[1]
		}
		finish(d, deploy.Rollback(d))
	case len(args) == 2:
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

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
