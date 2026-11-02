-- migrate:up

ALTER TABLE reelpin.notifications ADD COLUMN delivery_attempts INTEGER NOT NULL DEFAULT 0
    CHECK (delivery_attempts >= 0);

CREATE TABLE reelpin.notification_delivery_receipts (
    notification_id UUID NOT NULL REFERENCES reelpin.notifications(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL,
    provider_message_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (notification_id, token_hash)
);

GRANT SELECT, INSERT ON reelpin.notification_delivery_receipts TO reelpin_app;
GRANT SELECT, DELETE ON reelpin.notification_delivery_receipts TO reelpin_maintenance;

-- migrate:down

DROP TABLE reelpin.notification_delivery_receipts;
ALTER TABLE reelpin.notifications DROP COLUMN delivery_attempts;
