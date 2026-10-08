// Command scaleset-probe answers the #10 spike questions that don't need real
// jobs, using the same scale-set client as fireactions. Throwaway: it is a
// spike tool, not part of the product.
//
//	go run ./hack/scaleset-probe -repo OWNER/REPO -app-id ID -key /etc/fireactions/app.pem <command>
//
// Commands:
//
//	group                    look up the "default" runner group (personal-account repos)
//	labels NAME LABEL...     create scale set NAME with LABELs, print what GitHub kept, delete it
//	session NAME             open a second message session on existing scale set NAME
//	ratelimit                print the App installation's REST rate limit
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/actions/scaleset"
	"github.com/hostinger/fireactions/helper/github"
)

func main() {
	repo := flag.String("repo", "", "OWNER/REPO of a throwaway PRIVATE repository")
	appID := flag.Int64("app-id", 0, "GitHub App ID")
	keyFile := flag.String("key", "", "GitHub App private key (PEM)")
	flag.Parse()

	if err := run(*repo, *appID, *keyFile, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(repo string, appID int64, keyFile string, args []string) error {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || appID == 0 || keyFile == "" || len(args) == 0 {
		return fmt.Errorf("usage: scaleset-probe -repo OWNER/REPO -app-id ID -key FILE group|labels|session|ratelimit ...")
	}

	key, err := os.ReadFile(keyFile)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	gh, err := github.NewClient(appID, string(key))
	if err != nil {
		return err
	}
	installation, _, err := gh.Apps.FindRepositoryInstallation(ctx, owner, name)
	if err != nil {
		return fmt.Errorf("finding installation: %w", err)
	}

	r, _, err := gh.Installation(installation.GetID()).Repositories.Get(ctx, owner, name)
	if err != nil {
		return err
	}
	if !r.GetPrivate() {
		return fmt.Errorf("%s is public; use a throwaway private repository", repo)
	}

	if args[0] == "ratelimit" {
		limits, _, err := gh.Installation(installation.GetID()).RateLimit.Get(ctx)
		if err != nil {
			return err
		}
		core := limits.GetCore()
		fmt.Printf("core: %d/%d remaining, resets %s\n", core.Remaining, core.Limit, core.Reset.Format(time.RFC3339))
		return nil
	}

	client, err := scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
		GitHubConfigURL: "https://github.com/" + repo,
		GitHubAppAuth: scaleset.GitHubAppAuth{
			ClientID:       strconv.FormatInt(appID, 10),
			InstallationID: installation.GetID(),
			PrivateKey:     string(key),
		},
		SystemInfo: scaleset.SystemInfo{System: "fireactions-spike"},
	})
	if err != nil {
		return err
	}

	switch args[0] {
	case "group":
		group, err := client.GetRunnerGroupByName(ctx, scaleset.DefaultRunnerGroup)
		if err != nil {
			return fmt.Errorf("default group lookup FAILED: %w", err)
		}
		fmt.Printf("default group: id=%d name=%q isDefault=%v\n", group.ID, group.Name, group.IsDefault)

	case "labels":
		if len(args) < 3 {
			return fmt.Errorf("usage: labels NAME LABEL...")
		}
		want := &scaleset.RunnerScaleSet{Name: args[1], RunnerGroupID: 1}
		for _, l := range args[2:] {
			want.Labels = append(want.Labels, scaleset.Label{Name: l})
		}
		ss, err := client.CreateRunnerScaleSet(ctx, want)
		if err != nil {
			return fmt.Errorf("create with labels %v FAILED: %w", args[2:], err)
		}
		defer func() { _ = client.DeleteRunnerScaleSet(context.WithoutCancel(ctx), ss.ID) }()

		got, err := client.GetRunnerScaleSetByID(ctx, ss.ID)
		if err != nil {
			return err
		}
		fmt.Printf("scale set %d created; asked for %v, GitHub kept:\n", ss.ID, args[2:])
		for _, l := range got.Labels {
			fmt.Printf("  %s (%s)\n", l.Name, l.Type)
		}
		fmt.Println("(deleted again)")

	case "session":
		if len(args) != 2 {
			return fmt.Errorf("usage: session NAME")
		}
		ss, err := client.GetRunnerScaleSet(ctx, 1, args[1])
		if err != nil || ss == nil {
			return fmt.Errorf("scale set %q not found: %v", args[1], err)
		}
		sess, err := client.MessageSessionClient(ctx, ss.ID, "scaleset-probe")
		if err != nil {
			fmt.Printf("second session on scale set %d REFUSED: %v\n", ss.ID, err)
			return nil
		}
		defer func() { _ = sess.Close(context.WithoutCancel(ctx)) }()
		fmt.Printf("second session on scale set %d OPENED (id %s); closing it\n", ss.ID, sess.Session().SessionID)

	default:
		return fmt.Errorf("unknown command %q", args[0])
	}

	return nil
}
