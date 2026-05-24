-- Reverse migration: outbound webhook subscriptions.
--
-- DROP order: deliveries first (FK -> subscriptions), then subscriptions.
-- RLS policies, indexes, and the tables themselves all drop in a single
-- DROP TABLE CASCADE — the explicit DROP POLICY/INDEX calls would be
-- redundant.

DROP TABLE IF EXISTS webhook_deliveries CASCADE;
DROP TABLE IF EXISTS webhook_subscriptions CASCADE;
