import { useState, type ReactNode } from "react";
import { Link, useLocation, useNavigation } from "@remix-run/react";
import {
  Sidebar,
  SidebarContent,
  SidebarGroup,
  SidebarGroupContent,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarInset,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarProvider,
  SidebarTrigger,
} from "@datum-cloud/datum-ui/sidebar";
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from "@datum-cloud/datum-ui/breadcrumb";
import { ArrowLeftIcon, BoxIcon, LayoutDashboardIcon, SettingsIcon, SlidersIcon, StoreIcon } from "lucide-react";

interface AppLayoutProps {
  children: ReactNode;
}

interface Crumb {
  label: string;
  to?: string;
}

function buildCrumbs(pathname: string): Crumb[] {
  const segments = pathname.split("/").filter(Boolean);
  const crumbs: Crumb[] = [{ label: "Home", to: "/" }];

  if (segments[0] === "catalog") {
    crumbs.push({ label: "Catalog", to: "/catalog" });
    return crumbs;
  }

  if (segments[0] === "services") {
    crumbs.push({ label: "Services", to: "/services" });
    if (segments[1] === "new") {
      crumbs.push({ label: "New service" });
      return crumbs;
    }
    if (segments[1]) {
      const serviceName = decodeURIComponent(segments[1]);
      if (segments[2] === "configurations") {
        crumbs.push({
          label: serviceName,
          to: `/services/${segments[1]}?tab=configurations`,
        });
        if (segments[3]) {
          const last = decodeURIComponent(segments[3]);
          const label =
            last === "compare"
              ? "Compare"
              : last === "new"
                ? "New configuration"
                : last;
          crumbs.push({ label });
        }
      } else {
        crumbs.push({ label: serviceName });
      }
    }
  }
  return crumbs;
}

export function AppLayout({ children }: AppLayoutProps) {
  const location = useLocation();
  const navigation = useNavigation();
  const isNavigating = navigation.state !== "idle";
  const crumbs = buildCrumbs(location.pathname);
  const onCatalog = location.pathname.startsWith("/catalog");
  const [sidebarOpen, setSidebarOpen] = useState(true);

  // Detect service context: /services/:name (but not /services/new)
  const pathSegments = location.pathname.split("/").filter(Boolean);
  const inServiceContext = pathSegments[0] === "services" && pathSegments.length >= 2 && pathSegments[1] !== "new";
  const serviceNameParam = inServiceContext ? pathSegments[1] : null;

  const searchParams = new URLSearchParams(location.search);
  const currentTab = searchParams.get("tab") ?? "overview";

  function isActiveTab(tab: string) {
    if (!inServiceContext) return false;
    // On sub-routes like /configurations/:name, highlight configurations
    if (pathSegments.length >= 3 && pathSegments[2] === "configurations") return tab === "configurations";
    return currentTab === tab;
  }

  return (
    <SidebarProvider open={sidebarOpen} onOpenChange={setSidebarOpen}>
      <Sidebar collapsible="icon">
        <SidebarHeader className="px-4 py-3 font-semibold text-sm">
          Service Catalog
        </SidebarHeader>
        {inServiceContext ? (
          <SidebarContent>
            <SidebarGroup>
              <SidebarGroupContent>
                <SidebarMenu>
                  <SidebarMenuItem>
                    <SidebarMenuButton asChild>
                      <Link to="/services">
                        <ArrowLeftIcon className="size-4" />
                        Services
                      </Link>
                    </SidebarMenuButton>
                  </SidebarMenuItem>
                </SidebarMenu>
              </SidebarGroupContent>
            </SidebarGroup>
            <SidebarGroup>
              <SidebarGroupLabel className="truncate">{decodeURIComponent(serviceNameParam ?? "")}</SidebarGroupLabel>
              <SidebarGroupContent>
                <SidebarMenu>
                  <SidebarMenuItem>
                    <SidebarMenuButton asChild isActive={isActiveTab("overview")}>
                      <Link to={`/services/${serviceNameParam}?tab=overview`}>
                        <LayoutDashboardIcon className="size-4" />
                        Overview
                      </Link>
                    </SidebarMenuButton>
                  </SidebarMenuItem>
                  <SidebarMenuItem>
                    <SidebarMenuButton asChild isActive={isActiveTab("configurations")}>
                      <Link to={`/services/${serviceNameParam}?tab=configurations`}>
                        <SlidersIcon className="size-4" />
                        Configurations
                      </Link>
                    </SidebarMenuButton>
                  </SidebarMenuItem>
                  <SidebarMenuItem>
                    <SidebarMenuButton asChild isActive={isActiveTab("settings")}>
                      <Link to={`/services/${serviceNameParam}?tab=settings`}>
                        <SettingsIcon className="size-4" />
                        Settings
                      </Link>
                    </SidebarMenuButton>
                  </SidebarMenuItem>
                </SidebarMenu>
              </SidebarGroupContent>
            </SidebarGroup>
          </SidebarContent>
        ) : (
          <SidebarContent>
            <SidebarGroup>
              <SidebarGroupLabel>Browse</SidebarGroupLabel>
              <SidebarGroupContent>
                <SidebarMenu>
                  <SidebarMenuItem>
                    <SidebarMenuButton asChild isActive={onCatalog}>
                      <Link to="/catalog">
                        <StoreIcon className="size-4" />
                        Catalog
                      </Link>
                    </SidebarMenuButton>
                  </SidebarMenuItem>
                </SidebarMenu>
              </SidebarGroupContent>
            </SidebarGroup>
            <SidebarGroup>
              <SidebarGroupLabel>Manage</SidebarGroupLabel>
              <SidebarGroupContent>
                <SidebarMenu>
                  <SidebarMenuItem>
                    <SidebarMenuButton asChild isActive={location.pathname === "/services" || location.pathname === "/services/"}>
                      <Link to="/services">
                        <BoxIcon className="size-4" />
                        Services
                      </Link>
                    </SidebarMenuButton>
                  </SidebarMenuItem>
                </SidebarMenu>
              </SidebarGroupContent>
            </SidebarGroup>
          </SidebarContent>
        )}
      </Sidebar>
      <SidebarInset>
        <div className={`h-0.5 bg-primary transition-opacity duration-200 ${isNavigating ? "opacity-100" : "opacity-0"}`} />
        <header className="flex h-12 shrink-0 items-center gap-2 border-b border-border/50 px-4">
          <SidebarTrigger className="-ml-1" />
          <Breadcrumb>
            <BreadcrumbList>
              {crumbs.map((c, i) => {
                const isLast = i === crumbs.length - 1;
                return (
                  <span key={i} className="contents">
                    <BreadcrumbItem>
                      {isLast || !c.to ? (
                        <BreadcrumbPage>{c.label}</BreadcrumbPage>
                      ) : (
                        <BreadcrumbLink asChild>
                          <Link to={c.to}>{c.label}</Link>
                        </BreadcrumbLink>
                      )}
                    </BreadcrumbItem>
                    {!isLast && <BreadcrumbSeparator />}
                  </span>
                );
              })}
            </BreadcrumbList>
          </Breadcrumb>
        </header>
        <div className="flex-1 min-h-0 overflow-auto">{children}</div>
      </SidebarInset>
    </SidebarProvider>
  );
}
