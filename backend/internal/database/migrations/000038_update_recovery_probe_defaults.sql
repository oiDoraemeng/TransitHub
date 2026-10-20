-- Upgrade saved defaults while preserving explicitly customized timeouts.
DO $$
BEGIN
    IF to_regclass('strategy_settings') IS NOT NULL THEN
        UPDATE strategy_settings
        SET settings = jsonb_set(settings, '{recoveryProbeFirstTimeoutSeconds}', '90'::jsonb)
        WHERE COALESCE(settings->>'recoveryProbeFirstTimeoutSeconds', '60') IN ('0', '60');
        UPDATE strategy_settings
        SET settings = jsonb_set(settings, '{recoveryProbeSubsequentTimeoutSeconds}', '60'::jsonb)
        WHERE COALESCE(settings->>'recoveryProbeSubsequentTimeoutSeconds', '15') IN ('0', '15');
    END IF;
END $$;
