package cmd

import (
	"fmt"

	"go.miloapis.com/service-catalog/pkg/activation"
)

// requireProject reports an error when no project is in scope.
// ServiceEntitlement only exists inside a project's own virtual control
// plane, so every subcommand here needs one to do anything project-scoped.
func requireProject(opts ServicesCommandOptions) error {
	if opts.Project == "" {
		return fmt.Errorf("no project set — pass --project or select one with datumctl")
	}
	return nil
}

// newCatalogClient resolves opts.CatalogRESTConfig and builds a CatalogClient
// against the platform-wide root API server, where Service lives.
func newCatalogClient(opts ServicesCommandOptions) (activation.CatalogClient, error) {
	if opts.CatalogRESTConfig == nil {
		return nil, fmt.Errorf("services: CatalogRESTConfig is not configured")
	}
	cfg, err := opts.CatalogRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("resolving platform-wide connection: %w", err)
	}
	cc, err := activation.NewCatalogRESTClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("building catalog client: %w", err)
	}
	return cc, nil
}

// newEntitlementClient resolves opts.ProjectRESTConfig and builds an
// EntitlementClient against the active project's own virtual control plane,
// where ServiceEntitlement lives.
func newEntitlementClient(opts ServicesCommandOptions) (activation.EntitlementClient, error) {
	if opts.ProjectRESTConfig == nil {
		return nil, fmt.Errorf("services: ProjectRESTConfig is not configured")
	}
	cfg, err := opts.ProjectRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("resolving project connection: %w", err)
	}
	ec, err := activation.NewRESTClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("building entitlement client: %w", err)
	}
	return ec, nil
}
