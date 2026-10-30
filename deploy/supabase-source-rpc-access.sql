BEGIN;
SET LOCAL lock_timeout = '3s';
SET LOCAL statement_timeout = '30s';
SET LOCAL idle_in_transaction_session_timeout = '30s';
SET LOCAL ROLE postgres;

DO $$
DECLARE
    signature text;
    target oid;
BEGIN
    IF (SELECT count(*) FROM pg_roles WHERE rolname IN ('anon', 'authenticated', 'service_role')) <> 3 THEN
        RAISE EXCEPTION 'expected Supabase caller roles are missing';
    END IF;

    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname IN ('anon', 'authenticated') AND rolsuper) THEN
        RAISE EXCEPTION 'unexpected caller superuser status requires review';
    END IF;

    IF EXISTS (
        SELECT 1 FROM pg_auth_members m
        JOIN pg_roles r ON r.oid = m.member
        WHERE r.rolname IN ('anon', 'authenticated')
    ) THEN
        RAISE EXCEPTION 'unexpected caller role membership requires review';
    END IF;

    IF (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
        WHERE n.nspname = 'public' AND p.proname IN (
            'delete_user_account_data', 'claim_notification_campaign', 'claim_notification_event'
        )) <> 3 THEN
        RAISE EXCEPTION 'expected source RPC inventory differs';
    END IF;

    FOREACH signature IN ARRAY ARRAY[
        'public.delete_user_account_data(text)',
        'public.claim_notification_campaign(uuid)',
        'public.claim_notification_event(uuid,text,text,text,text,text,uuid,boolean)'
    ] LOOP
        target := to_regprocedure(signature);
        IF target IS NULL OR NOT EXISTS (
            SELECT 1 FROM pg_proc
            WHERE oid = target AND prosecdef AND prokind = 'f'
                AND prorettype = 'jsonb'::regtype
                AND proowner = 'postgres'::regrole
        ) THEN
            RAISE EXCEPTION 'source RPC contract differs: %', signature;
        END IF;
    END LOOP;
END;
$$;

REVOKE EXECUTE ON FUNCTION public.delete_user_account_data(text)
    FROM PUBLIC, anon, authenticated;
REVOKE EXECUTE ON FUNCTION public.claim_notification_campaign(uuid)
    FROM PUBLIC, anon, authenticated;
REVOKE EXECUTE ON FUNCTION public.claim_notification_event(uuid,text,text,text,text,text,uuid,boolean)
    FROM PUBLIC, anon, authenticated;

GRANT EXECUTE ON FUNCTION public.delete_user_account_data(text) TO service_role;
GRANT EXECUTE ON FUNCTION public.claim_notification_campaign(uuid) TO service_role;
GRANT EXECUTE ON FUNCTION public.claim_notification_event(uuid,text,text,text,text,text,uuid,boolean)
    TO service_role;

DO $$
DECLARE
    signature text;
BEGIN
    FOREACH signature IN ARRAY ARRAY[
        'public.delete_user_account_data(text)',
        'public.claim_notification_campaign(uuid)',
        'public.claim_notification_event(uuid,text,text,text,text,text,uuid,boolean)'
    ] LOOP
        IF has_function_privilege('anon', signature, 'EXECUTE')
            OR has_function_privilege('authenticated', signature, 'EXECUTE')
            OR NOT has_function_privilege('service_role', signature, 'EXECUTE') THEN
            RAISE EXCEPTION 'source RPC access verification failed: %', signature;
        END IF;
    END LOOP;
END;
$$;

COMMIT;
