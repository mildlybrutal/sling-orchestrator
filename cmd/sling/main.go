// Command sling deploys one commit of an app with zero downtime, or, as
// "sling serve", deploys every push GitHub sends to its webhook.
package main

import (
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/mildlybrutal/sling-orchestrator/internal/deploy"
	"github.com/mildlybrutal/sling-orchestrator/internal/proxy"
	"github.com/mildlybrutal/sling-orchestrator/internal/webhook"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "serve" {
		if err := webhook.Serve(); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) != 3 {
		log.Fatalf("usage:\n  %[1]s <repo-url-or-path> <40-char-commit-sha>   deploy one commit\n  %[1]s serve                                     run the webhook server", os.Args[0])
	}

	d := &deploy.Deployment{
		RepoURL:        os.Args[1],
		CommitSHA:      os.Args[2],
		AllowLocalRepo: true, // the person at the terminal already has the files
	}

	err := deploy.Run(d)
	if errors.Is(err, deploy.ErrAlreadyLive) {
		fmt.Printf("Nothing to do: %s is already live at %s\n", d.CommitSHA[:7], proxy.PublicURL)
		return
	}
	if err != nil {
		d.PrintTimeline()
		log.Fatalf("Deployment failed: %v", err)
	}

	fmt.Printf("\n✅ Live\n")
	fmt.Printf("   Container ID : %s\n", d.ContainerID[:12])
	fmt.Printf("   Public URL   : %s\n", proxy.PublicURL)
	fmt.Printf("   Direct URL   : http://127.0.0.1:%s (bypasses the proxy)\n", d.HostPort)
	if d.PreviousStopped {
		fmt.Printf("   Previous     : %s (stopped; start it and switch to roll back)\n", d.PreviousUpstream)
	}
	d.PrintTimeline()
}
