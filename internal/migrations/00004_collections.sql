-- migrate:up

-- A collection is one owner's named set of saves, optionally shared. Members
-- and share links grant access to the collection, never to the saves
-- themselves: an item row references reelpin.user_saves, so what a viewer sees
-- is exactly the saves the owner or an editor filed, and deleting a save
-- removes it from every collection by cascade.
CREATE TABLE reelpin.collections (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id      UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
    name          TEXT NOT NULL
        CONSTRAINT collections_name_check CHECK (length(trim(name)) > 0),
    description   TEXT NOT NULL DEFAULT '',
    cover_save_id UUID REFERENCES reelpin.user_saves(id) ON DELETE SET NULL,
    -- The share link is a capability: only its hash is stored, minting returns
    -- the token once, and revoking forgets the hash. One active link per
    -- collection; re-minting replaces it, which also revokes the old one.
    visibility      TEXT NOT NULL DEFAULT 'private'
        CONSTRAINT collections_visibility_check CHECK (visibility IN ('private', 'link')),
    link_token_hash TEXT,
    link_expires_at TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX collections_owner_idx ON reelpin.collections (owner_id, updated_at DESC);
CREATE UNIQUE INDEX collections_link_token_key
    ON reelpin.collections (link_token_hash) WHERE link_token_hash IS NOT NULL;
CREATE INDEX collections_cover_save_idx ON reelpin.collections (cover_save_id);

-- Membership below the owner: editors change items, viewers read. The owner is
-- never a member row, so ownership cannot be demoted by deleting one.
CREATE TABLE reelpin.collection_members (
    collection_id UUID NOT NULL REFERENCES reelpin.collections(id) ON DELETE CASCADE,
    user_id       UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
    role          TEXT NOT NULL
        CONSTRAINT collection_members_role_check CHECK (role IN ('editor', 'viewer')),
    invited_by    UUID REFERENCES auth.users(id) ON DELETE SET NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (collection_id, user_id)
);

CREATE INDEX collection_members_user_idx ON reelpin.collection_members (user_id);
CREATE INDEX collection_members_invited_by_idx ON reelpin.collection_members (invited_by);

-- Item membership references the save, not global content: two users saving
-- the same reel have two distinct saves, and a collection holds one of them.
-- The primary key is what makes filing idempotent.
CREATE TABLE reelpin.collection_items (
    collection_id UUID NOT NULL REFERENCES reelpin.collections(id) ON DELETE CASCADE,
    save_id       UUID NOT NULL REFERENCES reelpin.user_saves(id) ON DELETE CASCADE,
    added_by      UUID REFERENCES auth.users(id) ON DELETE SET NULL,
    added_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (collection_id, save_id)
);

CREATE INDEX collection_items_save_idx ON reelpin.collection_items (save_id);
CREATE INDEX collection_items_added_by_idx ON reelpin.collection_items (added_by);
-- The item page order: added_at DESC, save_id DESC, matching the cursor shape.
CREATE INDEX collection_items_page_idx
    ON reelpin.collection_items (collection_id, added_at DESC, save_id DESC);

-- An invite is a second capability, also stored only as a hash, with an expiry
-- and an optional use cap. Accepting one creates a membership; accepting it
-- twice leaves one membership and spends one use.
CREATE TABLE reelpin.collection_invites (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    collection_id UUID NOT NULL REFERENCES reelpin.collections(id) ON DELETE CASCADE,
    token_hash    TEXT NOT NULL,
    role          TEXT NOT NULL
        CONSTRAINT collection_invites_role_check CHECK (role IN ('editor', 'viewer')),
    created_by    UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
    expires_at    TIMESTAMPTZ NOT NULL,
    max_uses      INTEGER,
    uses          INTEGER NOT NULL DEFAULT 0,
    revoked       BOOLEAN NOT NULL DEFAULT false,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT collection_invites_token_key UNIQUE (token_hash)
);

CREATE INDEX collection_invites_collection_idx ON reelpin.collection_invites (collection_id);
CREATE INDEX collection_invites_created_by_idx ON reelpin.collection_invites (created_by);

GRANT SELECT, INSERT, UPDATE, DELETE ON reelpin.collections TO reelpin_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON reelpin.collection_members TO reelpin_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON reelpin.collection_items TO reelpin_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON reelpin.collection_invites TO reelpin_app;

GRANT SELECT, DELETE ON reelpin.collections,
                        reelpin.collection_members,
                        reelpin.collection_items,
                        reelpin.collection_invites TO reelpin_maintenance;

-- migrate:down

-- Restores the pre-collections shape. Only ever used against a disposable
-- database: in production this is corrected forward, never rolled back.
DROP TABLE IF EXISTS reelpin.collection_invites;
DROP TABLE IF EXISTS reelpin.collection_items;
DROP TABLE IF EXISTS reelpin.collection_members;
DROP TABLE IF EXISTS reelpin.collections;
