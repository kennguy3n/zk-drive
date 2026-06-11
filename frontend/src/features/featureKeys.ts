// Feature keys — the contract with the backend (internal/feature/flags.go).
// Keep this list in sync with AllFeatures on the Go side. Centralising the
// strings here means UI gating reads `Feature.SSO` instead of a stray
// "sso" literal that could drift from the backend key.
export const Feature = {
  Folders: "folders",
  Files: "files",
  ShareLinks: "share_links",
  BasicSearch: "basic_search",
  SSO: "sso",
  AuditLog: "audit_log",
  RetentionPolicies: "retention_policies",
  OnlyOffice: "onlyoffice",
  ClientRooms: "client_rooms",
  Webhooks: "webhooks",
  KChat: "kchat",
  StrictZK: "strict_zk",
  CMK: "cmk",
  DataResidency: "data_residency",
  AISummaries: "ai_summaries",
} as const;

export type FeatureKey = (typeof Feature)[keyof typeof Feature];

// Billing tiers, mirrored from internal/billing.
export const Tier = {
  Free: "free",
  Starter: "starter",
  Business: "business",
  SecureBusiness: "secure_business",
} as const;
