DROP INDEX IF EXISTS idx_workspace_plans_stripe_customer;
ALTER TABLE workspace_plans DROP COLUMN IF EXISTS stripe_customer_id;
