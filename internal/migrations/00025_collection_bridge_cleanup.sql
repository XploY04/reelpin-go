-- migrate:up

CREATE TABLE reelpin.collection_bridge_deletions (
    user_id UUID NOT NULL,
    save_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, save_id)
);
GRANT SELECT, INSERT ON reelpin.collection_bridge_deletions TO reelpin_app;

CREATE FUNCTION reelpin.delete_python_collection_saves(actor UUID, save UUID DEFAULT NULL)
RETURNS VOID
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE
    removed UUID[];
BEGIN
    WITH deleted AS (
        DELETE FROM reelpin.user_saves s
        USING reelpin.contents c, reelpin.content_versions v
        WHERE s.user_id = actor
          AND (save IS NULL OR s.id = save)
          AND c.id = s.content_id
          AND v.id = c.current_version_id
          AND (v.processor_version = 'python-collection-bridge-v1'
               OR EXISTS (SELECT 1 FROM reelpin.backfill_audit a
                          WHERE a.source_table = 'reels' AND a.source_id = s.id
                            AND a.content_id = s.content_id AND a.action = 'copied'))
        RETURNING s.content_id
    )
    SELECT array_agg(content_id) INTO removed FROM deleted;

    DELETE FROM reelpin.contents c
    WHERE c.id = ANY(removed)
      AND c.access_scope_hash <> 'public'
      AND NOT EXISTS (SELECT 1 FROM reelpin.user_saves s WHERE s.content_id = c.id);
END;
$$;

REVOKE ALL ON FUNCTION reelpin.delete_python_collection_saves(UUID, UUID) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION reelpin.delete_python_collection_saves(UUID, UUID) TO reelpin_app;

-- migrate:down

DROP TABLE reelpin.collection_bridge_deletions;

DROP FUNCTION reelpin.delete_python_collection_saves(UUID, UUID);
