package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/hostinger/fireactions/fleet"
	"github.com/spf13/cobra"
)

func newFleetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "fleet",
		Short:   "Render host configs from fleet.yaml",
		GroupID: "main",
	}

	render := &cobra.Command{
		Use:   "render",
		Short: "Render every host's fireactions, cache-proxy and systemd files into OUTDIR/<host>/",
		Long: `Render every host's files from fleet.yaml into OUTDIR/<host>/.

Secrets are read from the environment, never from fleet.yaml:
  ` + fleet.EnvCacheUpstreamToken + `   token the cache proxy presents upstream
  ` + fleet.EnvCacheTokenRW + `         proxy token for pools with cache: rw
  ` + fleet.EnvCacheTokenRO + `         proxy token for pools with cache: ro
  plus any $VAR a pool's env refers to.

Files holding secrets are written 0600. Never commit OUTDIR.`,
		Args: cobra.NoArgs,
		RunE: runFleetRender,
	}
	render.Flags().StringP("file", "f", "fleet.yaml", "fleet file")
	render.Flags().StringP("out", "o", "rendered", "output directory")
	render.Flags().String("host", "", "render only this host")

	cmd.AddCommand(render)
	return cmd
}

func runFleetRender(cmd *cobra.Command, _ []string) error {
	file, _ := cmd.Flags().GetString("file")
	out, _ := cmd.Flags().GetString("out")
	only, _ := cmd.Flags().GetString("host")

	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	f, err := fleet.ParseBytes(b)
	if err != nil {
		return err
	}

	hosts, err := fleet.Render(f, os.Getenv)
	if err != nil {
		return err
	}

	if only != "" {
		files, ok := hosts[only]
		if !ok {
			return fmt.Errorf("host %q is not in %s", only, file)
		}
		hosts = map[string][]fleet.RenderedFile{only: files}
	}

	names := make([]string, 0, len(hosts))
	for host := range hosts {
		names = append(names, host)
	}
	sort.Strings(names)

	for _, host := range names {
		files := hosts[host]
		dir := filepath.Join(out, host)
		if err := fleet.WriteHost(dir, files); err != nil {
			return err
		}
		cmd.Printf("%s: %d files in %s\n", host, len(files), dir)
	}

	return nil
}
