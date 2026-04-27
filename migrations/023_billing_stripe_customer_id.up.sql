-- Stripe customer linkage. Stored on workspace_plans rather than a
-- separate join table because each workspace has at most one Stripe
-- customer (1:1 with the plan row) and the lookup happens on every
-- portal-session request, so colocating avoids an extra query.
ALTER TABLE workspace_plans
    ADD COLUMN IF NOT EXISTS stripe_customer_id TEXT;

-- Lookups by Stripe customer ID happen on the webhook path when we
-- only know the customer (e.g. customer.subscription.updated without
-- workspace_id metadata for older subscriptions). Partial index keeps
-- the cost low because most rows are still NULL.
CREATE INDEX IF NOT EXISTS idx_workspace_plans_stripe_customer
    ON workspace_plans(stripe_customer_id)
    WHERE stripe_customer_id IS NOT NULL;
