package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"go.miloapis.com/service-catalog/pkg/activation"
)

func newStatusCommand(opts ServicesCommandOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status <name>",
		Short: "Show the current entitlement state for a service",
		Long: `Show whether a Datum Cloud service is enabled, pending approval, denied, or
not yet requested for the current project.`,
		Example: `  # Check on a service's access state
  datumctl services status compute`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatus(cmd, opts, args[0])
		},
	}
	return cmd
}

func runStatus(cmd *cobra.Command, opts ServicesCommandOptions, name string) error {
	if err := requireProject(opts); err != nil {
		return err
	}
	ctx := cmd.Context()

	cc, err := newCatalogClient(opts)
	if err != nil {
		return err
	}
	services, err := cc.ListServices(ctx)
	if err != nil {
		return fmt.Errorf("listing services: %w", err)
	}
	info, err := resolveService(services, name)
	if err != nil {
		return err
	}

	ec, err := newEntitlementClient(opts)
	if err != nil {
		return err
	}

	cfg := activation.ConfigFromService(info)
	state, entitlement, err := activation.Observe(ctx, ec, cfg)
	if err != nil {
		return fmt.Errorf("checking %s access: %w", info.DisplayName, err)
	}

	activation.RenderStatus(opts.IOStreams.Out, cfg, opts.Project, state, entitlement)
	return nil
}
