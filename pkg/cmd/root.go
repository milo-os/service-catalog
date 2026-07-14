// Package cmd provides the `services` cobra command group: list available
// Datum Cloud services, request access to one, and check on its entitlement
// state for the current project. It is the generic replacement for a
// per-plugin access command (e.g. a hypothetical `compute access`), mirroring
// how go.miloapis.com/activity/pkg/cmd is hosted in its own repo and imported
// into a host CLI's root command tree rather than duplicated per caller.
package cmd

import (
	"os"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"
)

// ServicesCommandOptions contains options for creating the services command.
type ServicesCommandOptions struct {
	// IOStreams for command input/output. If a stream is nil, it defaults to
	// the corresponding os.Std{in,out,err}.
	IOStreams genericclioptions.IOStreams

	// CatalogRESTConfig resolves a REST config scoped to the platform-wide root
	// API server, where Service lives. It is a resolver func rather than an
	// eager *rest.Config because building one may involve a token or auth
	// lookup that shouldn't run speculatively before cobra has even parsed
	// args (e.g. on `--help`); it is only called by a subcommand that actually
	// needs the catalog scope.
	CatalogRESTConfig func() (*rest.Config, error)

	// ProjectRESTConfig resolves a REST config scoped to the active project's
	// own virtual control plane, where ServiceEntitlement lives. Same
	// laziness rationale as CatalogRESTConfig. Every subcommand here needs
	// both resolvers in practice: Service and ServiceEntitlement live in two
	// different scopes with no API or controller joining them, so a name
	// given on the command line is always resolved against the catalog scope
	// even when the rest of the subcommand's work is project-scoped.
	ProjectRESTConfig func() (*rest.Config, error)

	// Project resolves the project ID/name the entitlement client is scoped to,
	// used for display and for validating a project is actually in scope
	// (ServiceEntitlement does not exist at user/org scope). It is a resolver
	// func rather than an eager string for the same reason as CatalogRESTConfig:
	// it must run at RunE time so a --project flag the host CLI parses on the
	// actual invocation is visible, rather than being captured once when the
	// command tree is constructed, before any flags are parsed.
	Project func() (string, error)
}

// NewServicesCommand creates the root `services` command with the provided
// options. Pass an empty ServicesCommandOptions{} only for construction
// convenience — CatalogRESTConfig and ProjectRESTConfig must be set by the
// caller for any subcommand to function.
//
// Subcommands never call os.Exit. On failure, RunE returns an error that may
// be an *activation.Error carrying a process exit code (activation.ExitCodeOf
// maps it); mapping that to os.Exit is the host CLI's job at its own
// top-level Execute wrapper, the same way datumctl's main.go already maps
// errors from other embedded command groups (e.g. activity) through its own
// error-formatting path rather than letting the command itself exit.
func NewServicesCommand(opts ServicesCommandOptions) *cobra.Command {
	ioStreams := opts.IOStreams
	if ioStreams.In == nil {
		ioStreams.In = os.Stdin
	}
	if ioStreams.Out == nil {
		ioStreams.Out = os.Stdout
	}
	if ioStreams.ErrOut == nil {
		ioStreams.ErrOut = os.Stderr
	}
	opts.IOStreams = ioStreams

	cmd := &cobra.Command{
		Use:   "services",
		Short: "See and manage Datum Cloud service access for the current project",
		Long: `See which Datum services are available and enabled for your current
project, and request access to any of them.

Subcommands:
  list             List available services and their entitlement state
  enable <name>    Request access to a service for the current project
  status <name>    Show the current entitlement state for a service`,
		Example: `  # See what's available and what's already enabled
  datumctl services list

  # Request access to a service
  datumctl services enable compute

  # Check on a pending request
  datumctl services status compute`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.AddCommand(newListCommand(opts))
	cmd.AddCommand(newEnableCommand(opts))
	cmd.AddCommand(newStatusCommand(opts))

	return cmd
}
