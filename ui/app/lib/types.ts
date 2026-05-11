export type Phase = "Draft" | "Published" | "Deprecated" | "Retired";

export interface KubeMeta {
  name: string;
  creationTimestamp: string;
}

export interface KubeList<T> {
  items: T[];
}

export interface ServiceCondition {
  type: string;
  status: string;
  reason?: string;
  message?: string;
  lastTransitionTime?: string;
}

export interface Service {
  metadata: KubeMeta;
  spec: {
    serviceName: string;
    phase: Phase;
    displayName: string;
    description?: string;
    owner: { producerProjectRef: { name: string } };
  };
  status?: {
    publishedAt?: string;
    conditions?: ServiceCondition[];
  };
}

export interface MonitoredResourceLabel {
  name: string;
  description?: string;
}

export interface GVKRef {
  group: string;
  kind: string;
}

export interface MonitoredResourceTypeSpec {
  type: string;
  displayName?: string;
  description?: string;
  gvk: GVKRef;
  labels?: MonitoredResourceLabel[];
}

export interface MeterMeasurement {
  aggregation: string;
  unit: string;
  unitDisplayName?: string;
}

export interface MeterBilling {
  consumedUnit: string;
  consumedUnitDisplayName?: string;
  pricingUnit: string;
  pricingUnitDisplayName?: string;
}

export interface MeterSpec {
  name: string;
  displayName?: string;
  description?: string;
  measurement: MeterMeasurement;
  billing: MeterBilling;
  monitoredResourceTypes: string[];
}

export interface ServiceConfiguration {
  metadata: KubeMeta;
  spec: {
    serviceRef: { name: string };
    phase: Phase;
    version?: string;
    monitoredResourceTypes?: MonitoredResourceTypeSpec[];
    meters?: MeterSpec[];
  };
  status?: {
    publishedAt?: string;
    serviceName?: string;
    conditions?: ServiceCondition[];
  };
}
