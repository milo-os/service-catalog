package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"go.miloapis.com/service-catalog/pkg/activation"
)

func newListCommand(opts ServicesCommandOptions) *cobra.Command {
	var output string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List available services and their entitlement state for the current project",
		Long: `List every published Datum Cloud service alongside its entitlement state
for the current project: enabled, pending approval, not requested, or denied.

The SERVICE NAME column carries the value to pass to
"datumctl services enable" and "datumctl services status".`,
		Example: `  # See what's available and what's already enabled
  datumctl services list

  # Machine-readable output
  datumctl services list -o json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runList(cmd, opts, output)
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "", "Output format: json, yaml (default table)")

	return cmd
}

func runList(cmd *cobra.Command, opts ServicesCommandOptions, output string) error {
	project, err := resolveProject(opts)
	if err != nil {
		return err
	}
	ctx := cmd.Context()

	cc, err := newCatalogClient(opts)
	if err != nil {
		return err
	}
	ec, err := newEntitlementClient(opts)
	if err != nil {
		return err
	}

	services, err := cc.ListServices(ctx)
	if err != nil {
		return fmt.Errorf("listing services: %w", err)
	}
	entitlements, err := ec.List(ctx)
	if err != nil {
		return fmt.Errorf("listing entitlements: %w", err)
	}

	entries := activation.JoinCatalog(services, entitlements)

	switch output {
	case "json":
		return printJSON(opts.IOStreams.Out, activation.NewCatalogReport(project, entries))
	case "yaml":
		return printYAML(opts.IOStreams.Out, activation.NewCatalogReport(project, entries))
	case "", "table":
		activation.RenderList(opts.IOStreams.Out, entries)
		return nil
	default:
		return fmt.Errorf("unsupported output format %q: must be table, json, or yaml", output)
	}
}
