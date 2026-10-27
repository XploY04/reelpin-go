-- migrate:up

-- What the app said about the build that registered each token, refreshed on
-- every registration, so a forced update can see which builds are still live
-- before it cuts one off. All nullable: an older build sends none of them.
ALTER TABLE reelpin.device_push_tokens
    ADD COLUMN app_version TEXT,
    ADD COLUMN app_build   INTEGER,
    ADD COLUMN timezone    TEXT,
    ADD COLUMN locale      TEXT;

-- migrate:down

ALTER TABLE reelpin.device_push_tokens
    DROP COLUMN locale,
    DROP COLUMN timezone,
    DROP COLUMN app_build,
    DROP COLUMN app_version;
