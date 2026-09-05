-- migrate:up

-- The send path depends on three guarantees the original tables did not make.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = 'public' AND tablename = 'device_push_tokens') THEN
        -- One row per device. A token that moves between accounts follows the
        -- newest sign-in instead of notifying two people.
        DELETE FROM public.device_push_tokens a
        USING public.device_push_tokens b
        WHERE a.fcm_token = b.fcm_token AND a.ctid < b.ctid;

        CREATE UNIQUE INDEX IF NOT EXISTS device_push_tokens_token_key
            ON public.device_push_tokens (fcm_token);

        ALTER TABLE public.device_push_tokens
            ADD COLUMN IF NOT EXISTS revoked BOOLEAN NOT NULL DEFAULT false;

        CREATE INDEX IF NOT EXISTS device_push_tokens_user_idx
            ON public.device_push_tokens (user_id) WHERE revoked = false;
    END IF;

    IF EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = 'public' AND tablename = 'notifications') THEN
        -- One notification per event. This is what stops a redelivered message
        -- from buzzing a phone twice.
        CREATE UNIQUE INDEX IF NOT EXISTS notifications_event_key
            ON public.notifications (event_key);

        -- The rendered text is stored so a resend, an audit, or a support
        -- question does not have to guess what the user actually saw.
        ALTER TABLE public.notifications ADD COLUMN IF NOT EXISTS title TEXT NOT NULL DEFAULT '';
        ALTER TABLE public.notifications ADD COLUMN IF NOT EXISTS body TEXT NOT NULL DEFAULT '';
        ALTER TABLE public.notifications ADD COLUMN IF NOT EXISTS data JSONB NOT NULL DEFAULT '{}'::jsonb;
    END IF;
END
$$;

-- Per-recipient campaign status, so a partial send is resumable and countable.
CREATE TABLE IF NOT EXISTS reelpin.campaign_targets (
    campaign_id     UUID NOT NULL,
    user_id         TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending',
    notification_id UUID,
    failure_reason  TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (campaign_id, user_id)
);

CREATE INDEX IF NOT EXISTS campaign_targets_pending_idx
    ON reelpin.campaign_targets (campaign_id) WHERE status = 'pending';

-- migrate:down

DROP TABLE IF EXISTS reelpin.campaign_targets;
DROP INDEX IF EXISTS public.notifications_event_key;
DROP INDEX IF EXISTS public.device_push_tokens_token_key;
DROP INDEX IF EXISTS public.device_push_tokens_user_idx;
