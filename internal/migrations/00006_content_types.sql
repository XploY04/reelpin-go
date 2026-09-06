-- migrate:up

-- The identity resolver's real vocabulary. The first constraint was written
-- from the plan's examples; the resolver also emits link, pin, profile and
-- short, and the constraint exists to catch exactly this drift.
ALTER TABLE reelpin.contents
    DROP CONSTRAINT contents_content_type_check;
ALTER TABLE reelpin.contents
    ADD CONSTRAINT contents_content_type_check CHECK (source_content_type IN
        ('reel', 'post', 'video', 'image', 'carousel', 'article', 'page',
         'place', 'link', 'pin', 'profile', 'short', 'other'));

-- migrate:down

ALTER TABLE reelpin.contents
    DROP CONSTRAINT contents_content_type_check;
ALTER TABLE reelpin.contents
    ADD CONSTRAINT contents_content_type_check CHECK (source_content_type IN
        ('reel', 'post', 'video', 'image', 'carousel', 'article', 'page',
         'place', 'other'));
