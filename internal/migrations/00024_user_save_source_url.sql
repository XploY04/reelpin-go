-- migrate:up

-- The link a backfilled save was submitted as. Only the last 60 days of legacy
-- jobs are copied, so for older saves nothing else keeps it; a save the Go app
-- makes leaves it null and its job holds the link.
ALTER TABLE reelpin.user_saves ADD COLUMN source_url TEXT;

-- migrate:down

ALTER TABLE reelpin.user_saves DROP COLUMN source_url;
