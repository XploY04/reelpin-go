-- migrate:up

-- A pin is idempotent by place: tapping "save" twice must not produce two pins.
-- The index is created only when the table exists, so a fresh CI database, which
-- has no legacy tables, still migrates.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = 'public' AND tablename = 'manual_map_pins') THEN
        CREATE UNIQUE INDEX IF NOT EXISTS manual_map_pins_user_place_key
            ON public.manual_map_pins (user_id, google_place_id)
            WHERE google_place_id IS NOT NULL;

        ALTER TABLE public.manual_map_pins ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT now();
    END IF;

    IF EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = 'public' AND tablename = 'hidden_reel_map_pins') THEN
        -- Hiding the same place twice is the same fact, not two.
        CREATE UNIQUE INDEX IF NOT EXISTS hidden_reel_map_pins_key
            ON public.hidden_reel_map_pins (user_id, reel_id, location_index);
    END IF;
END
$$;

-- migrate:down

DROP INDEX IF EXISTS public.manual_map_pins_user_place_key;
DROP INDEX IF EXISTS public.hidden_reel_map_pins_key;
