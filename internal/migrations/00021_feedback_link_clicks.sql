-- migrate:up

-- Website feedback. user_id is set to NULL, not cascaded, when the account is
-- deleted: the message is not personal data once the link to the account is
-- gone.
CREATE TABLE reelpin.feedback (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID REFERENCES auth.users(id) ON DELETE SET NULL,
    topic      TEXT NOT NULL
        CONSTRAINT feedback_topic_check CHECK (topic IN
            ('Feedback', 'Support', 'Partnership', 'Privacy request')),
    email      TEXT,
    message    TEXT NOT NULL
        CONSTRAINT feedback_message_check CHECK (char_length(message) BETWEEN 1 AND 2000),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX feedback_created_idx ON reelpin.feedback (created_at DESC);

-- Store-link taps counted by the website. Anonymous by design: no user id and
-- no address, only what the redirect already saw.
CREATE TABLE reelpin.link_clicks (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    target     TEXT NOT NULL
        CONSTRAINT link_clicks_target_check CHECK (target IN ('play', 'ios')),
    referer    TEXT,
    user_agent TEXT,
    country    TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX link_clicks_created_idx ON reelpin.link_clicks (created_at DESC);

GRANT SELECT, INSERT ON reelpin.feedback TO reelpin_app;
GRANT SELECT, INSERT ON reelpin.link_clicks TO reelpin_app;
GRANT SELECT, DELETE ON reelpin.feedback TO reelpin_maintenance;
GRANT SELECT, DELETE ON reelpin.link_clicks TO reelpin_maintenance;

-- migrate:down

DROP TABLE reelpin.link_clicks;
DROP TABLE reelpin.feedback;
