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
		Long: `Show access authorization and aggregate setup progress for a Datum Cloud
service, including the current blocking stage and elapsed setup time.`,
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
	project, err := resolveProject(opts)
	if err != nil {
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

	state, entitlement, err := activation.Observe(ctx, ec, info)
	if err != nil {
		return fmt.Errorf("checking %s access: %w", info.DisplayName, err)
	}

	activation.RenderStatus(opts.IOStreams.Out, info, project, state, entitlement)
	return nil
}
