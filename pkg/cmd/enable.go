package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"go.miloapis.com/service-catalog/pkg/activation"
)

func newEnableCommand(opts ServicesCommandOptions) *cobra.Command {
	var ropts activation.RequestOptions

	cmd := &cobra.Command{
		Use:   "enable <name>",
		Short: "Request access to a service for the current project",
		Long: `Submit a request to enable a Datum Cloud service for the current project.

Approval may be a manual step by the service provider — use --wait to block
until the platform reaches an answer, or check back later with
"datumctl services status <name>".`,
		Example: `  # Request access and return immediately
  datumctl services enable compute

  # Request access and wait for approval
  datumctl services enable compute --wait

  # Resubmit after a denial or revocation
  datumctl services enable compute --renew`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEnable(cmd, opts, args[0], ropts)
		},
	}

	cmd.Flags().StringVar(&ropts.Message, "message", "", "Justification sent to the service provider with the request")
	cmd.Flags().BoolVar(&ropts.Renew, "renew", false, "Delete a denied or revoked request and submit a new one")
	cmd.Flags().BoolVar(&ropts.Wait, "wait", false, "Wait until access becomes active (approval is a manual provider step)")
	cmd.Flags().DurationVar(&ropts.Timeout, "timeout", 0, "Maximum time to wait when --wait is set (0 uses the SDK's own default)")

	return cmd
}

// runEnable resolves name against the catalog, then hands off to
// activation.Requester for the request/renew/wait flow. On failure it
// returns whatever Requester.Request returns, which may be an
// *activation.Error carrying a process exit code — see NewServicesCommand's
// doc comment for who is responsible for mapping that to os.Exit.
func runEnable(cmd *cobra.Command, opts ServicesCommandOptions, name string, ropts activation.RequestOptions) error {
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

	requester := activation.Requester{
		Service: info,
		Client:  ec,
		IO:      activationIO(opts.IOStreams),
		Project: opts.Project,
	}
	return requester.Request(ctx, ropts)
}
