// Barrel for the shared UI primitives. Import from "components/ui" so the
// per-module workstreams pull in tokenised, KChat-themed components without
// reaching for inline styles or duplicating primitives.

export { Button, type ButtonProps } from "./Button";
export { Modal, type ModalProps } from "./Modal";
export { EmptyState, type EmptyStateProps } from "./EmptyState";
export {
  Skeleton,
  FileListSkeleton,
  FolderTreeSkeleton,
  PagePreviewSkeleton,
} from "./Skeleton";

export { AppShell, type AppShellProps } from "./AppShell";
export { PageHeader, type PageHeaderProps } from "./PageHeader";

export {
  Input,
  Textarea,
  Select,
  Field,
  RadioCard,
  type FieldProps,
  type RadioCardProps,
} from "./Field";
export { Badge, type BadgeProps } from "./Badge";
export { Table, THead, TBody, Tr, Th, Td } from "./Table";
export { Tabs, type TabItem, type TabsProps } from "./Tabs";

export { ToastProvider, useToast, type ToastOptions, type ToastVariant } from "./toast";
export {
  DialogsProvider,
  useConfirm,
  usePrompt,
  useResourcePicker,
  type ConfirmOptions,
  type PromptOptions,
  type PickerItem,
  type PickerOptions,
} from "./dialogs";
