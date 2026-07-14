package cmd

import (
	"fmt"

	"go.miloapis.com/service-catalog/pkg/activation"
)

// resolveProject runs the Project resolver and reports an error when no
// project is in scope. ServiceEntitlement only exists inside a project's own
// virtual control plane, so every subcommand here needs one to do anything
// project-scoped. The resolver runs here, at RunE time, so a host CLI's
// --project flag is reflected rather than a value captured at construction.
func resolveProject(opts ServicesCommandOptions) (string, error) {
	if opts.Project == nil {
		return "", fmt.Errorf("services: Project is not configured")
	}
	project, err := opts.Project()
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", fmt.Errorf("no project set — pass --project or select one with datumctl")
	}
	return project, nil
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
